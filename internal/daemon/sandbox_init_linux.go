//go:build linux

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// prSetNoNewPrivs is prctl(2)'s PR_SET_NO_NEW_PRIVS. Once set it cannot be unset, and it is
// inherited across exec, so a job cannot regain privilege through a setuid binary or a file
// capability even if one is reachable inside the sandbox.
const prSetNoNewPrivs = 38

// SandboxInit is the entry point for the re-executed helper. It runs as PID 1 inside the job's
// new namespaces: it builds the filesystem view, then starts and supervises the job.
//
// It does not return: the process exits with the job's status, so the worker's exec.Cmd reports
// what the job did rather than what the helper did. A setup failure exits 126 — "found but not
// executable" — which is the closest conventional code for "the job never started".
func SandboxInit() {
	code, err := sandboxInit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tasch sandbox: %v\n", err)
		os.Exit(126)
	}
	os.Exit(code)
}

// sandboxInit builds the sandbox and runs the job, returning the job's exit code.
func sandboxInit() (int, error) {
	raw := os.Getenv(sandboxSpecEnv)
	if raw == "" {
		return 0, fmt.Errorf("%s is not set; this command is internal and is run by the worker",
			sandboxInitCommand)
	}
	var spec sandboxSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return 0, fmt.Errorf("decode sandbox spec: %w", err)
	}
	if err := requireOwnNamespaces(); err != nil {
		return 0, err
	}
	if err := buildSandbox(&spec); err != nil {
		return 0, err
	}
	return superviseJob(&spec)
}

// requireOwnNamespaces refuses to continue unless this process really is in namespaces of its
// own. Everything below mounts, masks and pivots the root; run by hand on a host, it would do
// all of that to the host itself.
//
// Being PID 1 is the check, because the worker always clones with CLONE_NEWPID: inside the new
// namespace this process is always the first, and anywhere else it never is.
func requireOwnNamespaces() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("%s is internal: it runs a job inside namespaces the worker creates, "+
			"and running it directly would reconfigure this host's own filesystem", sandboxInitCommand)
	}
	return nil
}

// buildSandbox turns the freshly cloned namespaces into the view the job runs in.
func buildSandbox(spec *sandboxSpec) error {
	// Detach mount propagation first. Without this every mount below would propagate back to
	// the host's mount namespace, so the sandbox would rearrange the worker's own filesystem.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	if spec.Hostname != "" {
		if err := syscall.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("set hostname: %w", err)
		}
	}
	if spec.PrivateNet {
		// A fresh network namespace has a loopback device, but it is down. Jobs that talk to
		// 127.0.0.1 — single-node PyTorch rendezvous, a local test server — need it up.
		if err := bringLoopbackUp(); err != nil {
			fmt.Fprintf(os.Stderr, "tasch: could not bring up loopback in the sandbox: %v\n", err)
		}
	}

	var err error
	switch spec.Mode {
	case "strict":
		err = buildStrictRoot(spec)
	default:
		err = buildPrivateView(spec)
	}
	if err != nil {
		return err
	}

	if spec.NoNewPrivs {
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
			return fmt.Errorf("set no_new_privs: %w", errno)
		}
	}
	return nil
}

// buildPrivateView keeps the host filesystem visible but gives the job its own scratch, /tmp
// and /dev/shm, a process table showing only its own processes, and no view of the paths the
// worker keeps its state and credentials in.
func buildPrivateView(spec *sandboxSpec) error {
	// Hold the scratch directory open before mounting anything. Every mount below can bury it —
	// a private /tmp buries a scratch configured under /tmp, and masking the account's home
	// buries the default one. An open descriptor survives all of it, and /proc/self/fd is how
	// the original directory is bound back afterwards.
	scratchFD, err := os.Open(spec.Scratch)
	if err != nil {
		return fmt.Errorf("open job scratch: %w", err)
	}
	defer func() { _ = scratchFD.Close() }()
	fd := int(scratchFD.Fd())

	// /proc must be remounted for it to reflect the new PID namespace; the inherited one still
	// shows every process on the host. It also has to come first, because restoring the scratch
	// goes through /proc/self/fd.
	if err := mountProc("/proc"); err != nil {
		return err
	}

	if err := mountTmpfs("/tmp", spec.TmpfsSizeMB, "1777"); err != nil {
		return err
	}
	if err := restoreScratchUnder("/tmp", spec.Scratch, fd); err != nil {
		return err
	}
	if err := mountTmpfs("/dev/shm", spec.TmpfsSizeMB, "1777"); err != nil {
		// A host without /dev/shm is unusual but not a reason to fail the job.
		fmt.Fprintf(os.Stderr, "tasch: could not give the job a private /dev/shm: %v\n", err)
	} else if err := restoreScratchUnder("/dev/shm", spec.Scratch, fd); err != nil {
		return err
	}

	for _, p := range spec.Masked {
		if err := maskPath(p, spec.Scratch, fd); err != nil {
			return err
		}
	}
	return nil
}

// restoreScratchUnder puts the job's scratch directory back after a mount covered it.
//
// Recreating the path inside the covering filesystem and binding the held descriptor over it
// is what keeps the working directory reachable at the path the job was told to use.
func restoreScratchUnder(covered, scratch string, scratchFD int) error {
	if !under(scratch, covered) {
		return nil
	}
	if err := os.MkdirAll(scratch, 0700); err != nil {
		return fmt.Errorf("recreate scratch under %s: %w", covered, err)
	}
	src := fmt.Sprintf("/proc/self/fd/%d", scratchFD)
	if err := syscall.Mount(src, scratch, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("restore scratch under %s: %w", covered, err)
	}
	return nil
}

// maskPath covers a host path with an empty tmpfs so the job cannot read what is under it.
//
// If the job's scratch directory lives inside the masked path, it is recreated and bound back
// through fd before the tmpfs is sealed read-only — otherwise masking the account's home would
// take the job's own working directory with it.
func maskPath(path, scratch string, scratchFD int) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // nothing to hide
	}
	if !info.IsDir() {
		// Cover a file with /dev/null rather than a filesystem.
		return syscall.Mount("/dev/null", path, "", syscall.MS_BIND, "")
	}

	if err := syscall.Mount("tmpfs", path, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=0755,size=1m"); err != nil {
		return fmt.Errorf("mask %s: %w", path, err)
	}

	if err := restoreScratchUnder(path, scratch, scratchFD); err != nil {
		return err
	}

	// Seal the mask. Done last so the scratch could be recreated inside it.
	return syscall.Mount("", path, "",
		syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
}

// under reports whether path is inside dir.
func under(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// buildStrictRoot assembles a rootfs from read-only binds and pivots into it, so the job sees
// only what was explicitly put there.
func buildStrictRoot(spec *sandboxSpec) error {
	root := spec.RootStage
	if root == "" {
		return fmt.Errorf("strict sandbox has no staging directory")
	}
	// pivot_root requires the new root to be a mount point in its own right.
	if err := mountTmpfs(root, spec.TmpfsSizeMB, "0755"); err != nil {
		return fmt.Errorf("stage sandbox rootfs: %w", err)
	}

	for _, p := range spec.ReadOnly {
		if err := bindInto(root, p, true, true); err != nil {
			return err
		}
	}
	// /sys read-only: hardware discovery — including the driver's own device enumeration —
	// reads it, so a GPU job without /sys reports no devices. It has to be bound recursively,
	// because an unprivileged bind of a subtree containing submounts is refused outright: a
	// non-recursive bind would reveal whatever those submounts are hiding. The read-only
	// remount is then non-recursive, so the submounts keep the host's flags — which is why the
	// ones that matter are covered over immediately below.
	if err := bindInto(root, "/sys", true, true); err != nil {
		fmt.Fprintf(os.Stderr, "tasch: the job has no /sys: %v\n", err)
	} else {
		for _, sub := range sensitiveSysPaths {
			if err := coverPath(filepath.Join(root, strings.TrimPrefix(sub, "/"))); err != nil {
				fmt.Fprintf(os.Stderr, "tasch: could not hide %s from the job: %v\n", sub, err)
			}
		}
	}
	for _, p := range spec.Writable {
		if err := bindInto(root, p, false, true); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "proc"), 0555); err != nil {
		return err
	}
	if err := mountProc(filepath.Join(root, "proc")); err != nil {
		return err
	}
	if err := buildDev(root, spec); err != nil {
		return err
	}
	if err := mountTmpfs(filepath.Join(root, "tmp"), spec.TmpfsSizeMB, "1777"); err != nil {
		return err
	}

	// The job's working directory, the one place it can write that outlives a tmpfs.
	work := filepath.Join(root, strings.TrimPrefix(sandboxWorkdir, "/"))
	if err := os.MkdirAll(work, 0700); err != nil {
		return err
	}
	if err := syscall.Mount(spec.Scratch, work, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind job scratch at %s: %w", sandboxWorkdir, err)
	}

	return pivotRoot(root)
}

// buildDev creates the minimal device tree a job needs, plus accelerators when it has them.
//
// Device nodes are bind-mounted from the host rather than created with mknod: a process in an
// unprivileged user namespace cannot create device nodes at all, and binding is what gives the
// same result without needing that permission.
func buildDev(root string, spec *sandboxSpec) error {
	dev := filepath.Join(root, "dev")
	if err := mountTmpfs(dev, 64, "0755"); err != nil {
		return fmt.Errorf("stage /dev: %w", err)
	}

	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		if err := bindDevice(filepath.Join("/dev", name), filepath.Join(dev, name)); err != nil {
			return err
		}
	}
	if err := mountTmpfs(filepath.Join(dev, "shm"), spec.TmpfsSizeMB, "1777"); err != nil {
		return err
	}

	// A private devpts instance, so the job's terminals are not the host's.
	ptsDir := filepath.Join(dev, "pts")
	if err := os.MkdirAll(ptsDir, 0755); err == nil {
		if err := syscall.Mount("devpts", ptsDir, "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC,
			"newinstance,ptmxmode=0666,mode=0620"); err == nil {
			_ = os.Symlink("pts/ptmx", filepath.Join(dev, "ptmx"))
		}
	}

	for link, target := range map[string]string{
		"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2",
	} {
		_ = os.Symlink(target, filepath.Join(dev, link))
	}

	if !spec.GPUDevices {
		return nil
	}
	// Accelerators, for jobs the master pinned devices to. Absent patterns match nothing, so
	// the same list covers an NVIDIA box, an AMD one and a machine with neither.
	for _, pattern := range []string{
		"/dev/nvidia*", "/dev/nvidia-caps", "/dev/kfd", "/dev/dri", "/dev/accel*",
	} {
		matches, _ := filepath.Glob(pattern)
		for _, src := range matches {
			target := filepath.Join(dev, strings.TrimPrefix(src, "/dev/"))
			info, err := os.Stat(src)
			if err != nil {
				continue
			}
			if info.IsDir() {
				if err := os.MkdirAll(target, 0755); err != nil {
					continue
				}
				_ = syscall.Mount(src, target, "", syscall.MS_BIND|syscall.MS_REC, "")
				continue
			}
			if err := bindDevice(src, target); err != nil {
				fmt.Fprintf(os.Stderr, "tasch: could not expose %s to the job: %v\n", src, err)
			}
		}
	}
	return nil
}

// sensitiveSysPaths are the parts of /sys a job has no business reaching. cgroupfs is the one
// that matters most: a job's own cgroup is owned by the account it runs as, so a writable
// cgroupfs lets it raise the very limits the scheduler set for it.
var sensitiveSysPaths = []string{
	"/sys/fs/cgroup", "/sys/kernel/debug", "/sys/kernel/tracing",
	"/sys/kernel/security", "/sys/firmware",
}

// coverPath mounts an empty read-only filesystem over a path, hiding whatever is under it.
//
// Covering works where changing flags does not: a mount inherited from the host is locked
// inside a user namespace and cannot be unmounted or remounted, but nothing stops a new mount
// being stacked on top of it.
func coverPath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if err := syscall.Mount("tmpfs", path, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "mode=0555,size=1m"); err != nil {
		return err
	}
	return syscall.Mount("", path, "",
		syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
}

// bindDevice makes a host device node reachable at target.
func bindDevice(src, target string) error {
	if _, err := os.Stat(src); err != nil {
		return nil // the host does not have it either
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	// A bind mount needs something to mount onto; an empty regular file is enough.
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("stage device %s: %w", target, err)
	}
	if f != nil {
		_ = f.Close()
	}
	if err := syscall.Mount(src, target, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind device %s: %w", src, err)
	}
	return nil
}

// bindInto bind-mounts a host path at the same location under root, optionally read-only.
//
// Read-only takes two calls: the kernel ignores MS_RDONLY on the initial bind, so the mount has
// to be created first and then remounted with the flag. Skipping the remount is the classic way
// to end up with a "read-only" bind that is writable.
func bindInto(root, path string, readOnly, recursive bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // not present on this host; nothing to map
	}
	target := filepath.Join(root, strings.TrimPrefix(path, "/"))

	if info.IsDir() {
		if err := os.MkdirAll(target, 0755); err != nil {
			return fmt.Errorf("stage %s: %w", target, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return fmt.Errorf("stage %s: %w", filepath.Dir(target), err)
		}
		f, cerr := os.OpenFile(target, os.O_CREATE|os.O_RDONLY, 0600)
		if cerr != nil && !os.IsExist(cerr) {
			return fmt.Errorf("stage %s: %w", target, cerr)
		}
		if f != nil {
			_ = f.Close()
		}
	}

	flags := uintptr(syscall.MS_BIND)
	if recursive {
		flags |= syscall.MS_REC
	}
	if err := syscall.Mount(path, target, "", flags, ""); err != nil {
		return fmt.Errorf("bind %s: %w", path, err)
	}
	if !readOnly {
		return nil
	}
	// Two things make this remount fussy.
	//
	// It is not recursive: mounts inherited from the host are locked inside a user namespace,
	// so changing flags across a whole subtree fails as soon as one locked submount is in it —
	// which is what /sys always contains.
	//
	// And it has to carry the flags the mount already has. A user namespace may add locked
	// flags but never clear them, so a remount that asks only for MS_RDONLY is read as a
	// request to drop nosuid, nodev and noexec, and the kernel answers EPERM. Reading the
	// current flags back and OR-ing MS_RDONLY into them is what makes it an addition.
	existing, err := lockedMountFlags(target)
	if err != nil {
		return fmt.Errorf("read mount flags for %s: %w", path, err)
	}
	if err := syscall.Mount("", target, "",
		syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|existing, ""); err != nil {
		return fmt.Errorf("make %s read-only: %w", path, err)
	}
	return nil
}

// lockedMountFlags reports the flags a mount already carries that a user namespace forbids
// clearing, so a remount can preserve them.
func lockedMountFlags(target string) (uintptr, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, err
	}

	lockable := map[string]uintptr{
		"ro": syscall.MS_RDONLY, "nosuid": syscall.MS_NOSUID, "nodev": syscall.MS_NODEV,
		"noexec": syscall.MS_NOEXEC, "noatime": syscall.MS_NOATIME,
		"nodiratime": syscall.MS_NODIRATIME, "relatime": syscall.MS_RELATIME,
	}

	// The mount governing a path is the one with the longest matching mount point.
	var best string
	var flags uintptr
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		// id parent dev root mountpoint options ...
		if len(fields) < 6 {
			continue
		}
		point, opts := fields[4], fields[5]
		if point != abs && !strings.HasPrefix(abs, strings.TrimSuffix(point, "/")+"/") {
			continue
		}
		if len(point) < len(best) {
			continue
		}
		best = point
		flags = 0
		for _, opt := range strings.Split(opts, ",") {
			if f, ok := lockable[opt]; ok {
				flags |= f
			}
		}
	}
	if best == "" {
		return 0, fmt.Errorf("no mount covers %s", abs)
	}
	return flags, nil
}

func mountProc(target string) error {
	if err := syscall.Mount("proc", target, "proc",
		syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	return nil
}

func mountTmpfs(target string, sizeMB int, mode string) error {
	if err := os.MkdirAll(target, 0755); err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	opts := "mode=" + mode
	if sizeMB > 0 {
		opts += fmt.Sprintf(",size=%dm", sizeMB)
	}
	if err := syscall.Mount("tmpfs", target, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NODEV, opts); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %w", target, err)
	}
	return nil
}

// pivotRoot makes newroot the process's root and detaches the old one, so nothing outside the
// assembled rootfs remains reachable — not even through a stray file descriptor's ".." walk.
func pivotRoot(newroot string) error {
	old := filepath.Join(newroot, ".oldroot")
	if err := os.MkdirAll(old, 0700); err != nil {
		return fmt.Errorf("create pivot target: %w", err)
	}
	if err := syscall.Chdir(newroot); err != nil {
		return fmt.Errorf("chdir to new root: %w", err)
	}
	if err := syscall.PivotRoot(".", ".oldroot"); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir after pivot_root: %w", err)
	}
	// MNT_DETACH rather than a plain unmount: the old root is still busy at this point, and a
	// lazy unmount drops it as soon as the last reference goes.
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	return os.Remove("/.oldroot")
}

// ifreqFlags is struct ifreq as SIOCSIFFLAGS uses it: a 16-byte interface name followed by a
// 24-byte union, of which only the leading flags field matters here.
type ifreqFlags struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

// bringLoopbackUp raises lo in a freshly created network namespace.
func bringLoopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()

	var req ifreqFlags
	copy(req.Name[:], "lo")
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return errno
	}
	req.Flags |= syscall.IFF_UP | syscall.IFF_RUNNING
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return errno
	}
	return nil
}

// superviseJob starts the job and acts as init for its namespace: it forwards termination
// signals, reaps every orphan the job leaves behind, and reports the job's own exit status.
//
// Being init is what keeps cancellation working. A process that is PID 1 in a namespace ignores
// any signal it has no handler for, so a job execed directly here would survive the SIGTERM a
// walltime kill sends and only die to the SIGKILL that follows — losing the chance to
// checkpoint that the grace period exists to give it.
func superviseJob(spec *sandboxSpec) (int, error) {
	env := jobEnv(spec)

	attr := &syscall.ProcAttr{
		Dir:   spec.WorkdirIn,
		Env:   env,
		Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
		// Its own process group, so forwarding a signal to the job cannot loop back to init.
		Sys: &syscall.SysProcAttr{Setpgid: true},
	}
	pid, err := syscall.ForkExec("/bin/sh", []string{"sh", "-c", spec.Command}, attr)
	if err != nil {
		return 0, fmt.Errorf("start job inside sandbox: %w", err)
	}

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for sig := range sigCh {
			// Negative pid addresses the job's whole process group, so anything it spawned is
			// signalled too.
			_ = syscall.Kill(-pid, sig.(syscall.Signal))
		}
	}()

	// Reap everything, not just the job: orphaned grandchildren are reparented to PID 1, which
	// is this process. Waiting only on the job would leave them as zombies.
	for {
		var status syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &status, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("wait for job: %w", err)
		}
		if wpid != pid {
			continue
		}
		switch {
		case status.Signaled():
			// The shell convention, so a caller can tell which signal ended the job.
			return 128 + int(status.Signal()), nil
		default:
			return status.ExitStatus(), nil
		}
	}
}

// jobEnv is the environment the job runs with: everything the worker passed, minus the spec,
// with the paths that only make sense inside the sandbox pointed at their new locations.
func jobEnv(spec *sandboxSpec) []string {
	overrides := map[string]string{
		"HOME":   spec.WorkdirIn,
		"PWD":    spec.WorkdirIn,
		"TMPDIR": "/tmp",
	}

	out := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == sandboxSpecEnv {
			continue
		}
		if _, replaced := overrides[key]; replaced {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}
