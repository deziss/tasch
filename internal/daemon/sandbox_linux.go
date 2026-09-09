//go:build linux

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/deziss/tasch/internal/config"
)

// sandbox is the host-side half of one job's isolation: the scratch directories the worker
// creates and removes, and the plan the re-executed helper carries out from inside the
// namespaces. See sandbox.go for why the work is split that way.
type sandbox struct {
	spec sandboxSpec
	// dirs are removed when the job ends, innermost first.
	dirs []string
}

// sandboxReexec reports the binary and argv the worker re-enters itself with. It is a variable
// so tests can point it at a helper inside the test binary, which has no `tasch` subcommands.
var sandboxReexec = func() (string, []string, error) {
	self, err := os.Executable()
	return self, []string{"tasch", sandboxInitCommand}, err
}

// gpuEnvKeys are the variables the master sets when it pins a job to accelerators. Their
// presence is what tells the worker a job needs device nodes; the dispatch message carries no
// GPU count of its own.
var gpuEnvKeys = []string{"CUDA_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES", "ONEAPI_DEVICE_SELECTOR"}

// newSandbox prepares the host state for one job and returns its plan.
//
// It returns (nil, nil) when isolation is off, which is what makes the caller's handling
// uniform: a nil sandbox runs the job exactly as Tasch always did.
func newSandbox(cfg *config.Config, jobID string, env map[string]string) (*sandbox, error) {
	mode := cfg.Sandbox.Mode
	if mode == "" || mode == config.SandboxNone {
		return nil, nil
	}
	if err := sandboxSupportErr(); err != nil {
		return nil, err
	}

	root := cfg.Sandbox.ResolvedScratchDir()
	// 0700: a job's scratch must not be readable by anything else sharing the account.
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create scratch root %s: %w", root, err)
	}

	sb := &sandbox{}
	scratch := filepath.Join(root, "job-"+jobID)
	if err := os.MkdirAll(scratch, 0700); err != nil {
		return nil, fmt.Errorf("create job scratch %s: %w", scratch, err)
	}
	sb.dirs = append(sb.dirs, scratch)

	wantsGPU := false
	if cfg.Sandbox.WantsGPUDevices() {
		for _, k := range gpuEnvKeys {
			if _, ok := env[k]; ok {
				wantsGPU = true
				break
			}
		}
	}

	sb.spec = sandboxSpec{
		Mode:        mode,
		Hostname:    cfg.Sandbox.Hostname,
		Scratch:     scratch,
		WorkdirIn:   scratch,
		Writable:    cfg.Sandbox.WritablePaths,
		Masked:      cfg.Sandbox.MaskedPaths,
		TmpfsSizeMB: cfg.Sandbox.TmpfsSizeMB,
		NoNewPrivs:  !cfg.Sandbox.AllowNewPrivileges,
		GPUDevices:  wantsGPU,
		PrivateNet:  cfg.Sandbox.Network == "none",
	}

	if mode == config.SandboxStrict {
		// pivot_root needs the new root to be a mount point of its own, so the helper mounts a
		// tmpfs over this directory before assembling anything into it.
		stage := scratch + ".root"
		if err := os.MkdirAll(stage, 0700); err != nil {
			return nil, fmt.Errorf("create rootfs staging dir %s: %w", stage, err)
		}
		// Removed before the scratch it sits beside, so the slice stays innermost-first.
		sb.dirs = append([]string{stage}, sb.dirs...)
		sb.spec.RootStage = stage
		sb.spec.WorkdirIn = sandboxWorkdir
		sb.spec.ReadOnly = cfg.Sandbox.ResolvedReadOnlyPaths()
	} else {
		// In private mode the host filesystem stays visible, so the account's home and Tasch's
		// own state directory are masked explicitly. Without this a job could read the token it
		// would need to impersonate the worker.
		sb.spec.Masked = append(defaultMaskedPaths(), sb.spec.Masked...)
	}

	return sb, nil
}

// defaultMaskedPaths are the host directories private mode hides even when the operator
// configures nothing: the service account's home, and wherever Tasch keeps its keys and state.
func defaultMaskedPaths() []string {
	var masked []string
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		masked = append(masked, home)
	}
	// cgroupfs last: the job's own cgroup is owned by the account it runs as, so without this a
	// job in private mode could raise its own memory.max and undo the limits the master set.
	for _, p := range []string{"/var/lib/tasch", "/etc/tasch", "/root", "/sys/fs/cgroup"} {
		if _, err := os.Stat(p); err == nil {
			masked = append(masked, p)
		}
	}
	return masked
}

// Workdir is the job's working directory as the job sees it.
func (s *sandbox) Workdir() string {
	if s == nil {
		return ""
	}
	return s.spec.WorkdirIn
}

// HostScratch is the job's working directory on the host, for the worker to collect artefacts
// from before Close removes it.
func (s *sandbox) HostScratch() string {
	if s == nil {
		return ""
	}
	return s.spec.Scratch
}

// applyTo turns a plain job command into a re-execution of this binary inside new namespaces.
//
// The command string moves into the spec, because the process this binary is re-executed as
// takes no arguments: everything it needs to know arrives in the environment, and it is the
// helper — not this exec.Cmd — that finally runs the job.
func (s *sandbox) applyTo(cmd *exec.Cmd, command string) error {
	if s == nil {
		return nil
	}
	s.spec.Command = command
	self, args, err := sandboxReexec()
	if err != nil {
		return fmt.Errorf("locate own binary for sandbox re-exec: %w", err)
	}
	spec, err := json.Marshal(s.spec)
	if err != nil {
		return fmt.Errorf("encode sandbox spec: %w", err)
	}

	cmd.Path = self
	cmd.Args = args
	cmd.Env = append(cmd.Env, sandboxSpecEnv+"="+string(spec))

	flags := syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS
	if s.spec.PrivateNet {
		flags |= syscall.CLONE_NEWNET
	}
	if os.Geteuid() != 0 {
		// An unprivileged worker has none of the capabilities mounting requires. A user
		// namespace grants them *within the namespace*: the account's own uid is mapped to 0
		// there, so the helper can mount and pivot_root, while outside the namespace it remains
		// exactly as unprivileged as before. Files it creates stay owned by the service account.
		flags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Geteuid(), Size: 1},
		}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getegid(), Size: 1},
		}
		// setgroups stays denied, which the kernel requires before an unprivileged process may
		// write a gid map at all.
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}
	cmd.SysProcAttr.Cloneflags |= uintptr(flags)
	return nil
}

// Close removes the job's scratch directories.
func (s *sandbox) Close() error {
	if s == nil {
		return nil
	}
	var firstErr error
	for _, d := range s.dirs {
		if err := os.RemoveAll(d); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

var (
	sandboxSupportOnce sync.Once
	sandboxSupportBad  error
)

// sandboxSupportErr reports why namespaces cannot be used here, or nil if they can.
//
// The check is worth doing up front rather than letting the first job fail with a bare EPERM:
// the usual cause is a sysctl an administrator can flip, and the error says which one.
func sandboxSupportErr() error {
	sandboxSupportOnce.Do(func() { sandboxSupportBad = detectSandboxSupport() })
	return sandboxSupportBad
}

func detectSandboxSupport() error {
	if os.Geteuid() == 0 {
		// root already holds every capability the helper needs.
		return nil
	}
	// Debian and Ubuntu shipped this switch for years; when it is 0 an unprivileged clone with
	// CLONE_NEWUSER fails with EPERM and nothing explains why.
	if data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		if strings.TrimSpace(string(data)) == "0" {
			return fmt.Errorf("unprivileged user namespaces are disabled; enable them with " +
				"`sysctl -w kernel.unprivileged_userns_clone=1`, or run the worker as root")
		}
	}
	if data, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil {
		if strings.TrimSpace(string(data)) == "0" {
			return fmt.Errorf("user namespaces are disabled; raise " +
				"`sysctl user.max_user_namespaces`, or run the worker as root")
		}
	}
	return nil
}

// sandboxSupportErrForLog is the startup-time form of sandboxSupportErr, kept separate so the
// non-Linux build can report the platform reason instead.
func sandboxSupportErrForLog() error { return sandboxSupportErr() }
