//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Resource limits on the worker.
//
// The master's resource tracking was pure bookkeeping: it decided where a job fit, but nothing
// on the worker ever enforced the decision. A job that reserved one core and 1 GB could use
// every core and all the memory on the box, and `:(){ :|:& };:` could fork a worker to death.
// These limits make the reservation real on Linux, using cgroup v2.
//
// Two caveats worth stating plainly. First, this confines resource usage; it is not a security
// boundary. Jobs still run as the service account, share its filesystem and network, and can
// read anything it can — real isolation needs namespaces or containers. Second, GPUs are not
// covered: cgroup v2 has no GPU controller, and CUDA_VISIBLE_DEVICES remains advisory, so a job
// can still unset it and reach every card on the node.

// cgroupRoot is the cgroup v2 mount point.
const cgroupRoot = "/sys/fs/cgroup"

// defaultMaxPIDs caps a job's process count even when it reserved no CPU or memory. It is the
// cheapest protection against a fork bomb taking the worker down with it.
const defaultMaxPIDs = 4096

var (
	// cgroupBase is the directory this worker creates per-job cgroups under, resolved once.
	cgroupBase     string
	cgroupBaseErr  error
	cgroupBaseOnce sync.Once
)

// setupCgroupBase prepares a delegated subtree this worker can create job cgroups in.
//
// cgroup v2 forbids a cgroup from holding processes while its children have controllers
// enabled, so the daemon first moves itself into a leaf of its own cgroup, then enables the
// controllers it needs for sibling job cgroups.
func setupCgroupBase() (string, error) {
	own, err := ownCgroupPath()
	if err != nil {
		return "", err
	}
	base := filepath.Join(cgroupRoot, own)

	// Move ourselves into a leaf so the base can carry controllers for its children.
	daemonLeaf := filepath.Join(base, "tasch.daemon")
	if err := os.Mkdir(daemonLeaf, 0755); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create daemon cgroup: %w", err)
	}
	if err := os.WriteFile(filepath.Join(daemonLeaf, "cgroup.procs"),
		[]byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return "", fmt.Errorf("move daemon into its own cgroup: %w", err)
	}

	// Enable only the controllers actually delegated to us; asking for one we do not have
	// fails the whole write.
	available, err := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("read available controllers: %w", err)
	}
	var wanted []string
	for _, c := range []string{"cpu", "memory", "pids"} {
		if strings.Contains(string(available), c) {
			wanted = append(wanted, "+"+c)
		}
	}
	if len(wanted) == 0 {
		return "", fmt.Errorf("no cpu, memory or pids controller is delegated to this cgroup")
	}
	if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"),
		[]byte(strings.Join(wanted, " ")), 0644); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			// cgroup v2 refuses to enable controllers for children while the cgroup still holds
			// processes. That means this cgroup is shared with something else — the daemon has
			// already moved itself out. Running the daemon as a systemd service with
			// Delegate=yes gives it a cgroup of its own and resolves this.
			return "", fmt.Errorf("cgroup %s is shared with other processes, so controllers "+
				"cannot be delegated to job cgroups; run tasch as a systemd service with "+
				"Delegate=yes", base)
		}
		return "", fmt.Errorf("enable controllers %v: %w", wanted, err)
	}

	return base, nil
}

// ownCgroupPath reads this process's cgroup v2 path, relative to the mount point.
func ownCgroupPath() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2 always appears as the single "0::<path>" entry.
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry in /proc/self/cgroup (is this a cgroup v1 system?)")
}

// jobCgroup is one job's resource confinement.
type jobCgroup struct {
	path string
	dir  *os.File
}

// newJobCgroup creates a cgroup for a job and applies its reservations.
//
// It returns nil without error when cgroups are unavailable or not delegated: enforcement is
// best-effort by design, because a worker started outside systemd, in a container without a
// delegated subtree, or on a cgroup v1 host must still be able to run jobs.
func newJobCgroup(jobID string, cpus, memMB, maxPIDs int) (*jobCgroup, error) {
	cgroupBaseOnce.Do(func() { cgroupBase, cgroupBaseErr = setupCgroupBase() })
	if cgroupBaseErr != nil {
		return nil, cgroupBaseErr
	}

	path := filepath.Join(cgroupBase, "job-"+jobID)
	if err := os.Mkdir(path, 0755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create job cgroup: %w", err)
	}

	c := &jobCgroup{path: path}
	if err := c.applyLimits(cpus, memMB, maxPIDs); err != nil {
		_ = c.Close()
		return nil, err
	}

	// Hold a directory handle so the child can be placed into this cgroup at clone time,
	// which avoids the window between fork and a write to cgroup.procs during which the job
	// would run unconfined.
	dir, err := os.Open(path)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("open job cgroup: %w", err)
	}
	c.dir = dir
	return c, nil
}

func (c *jobCgroup) applyLimits(cpus, memMB, maxPIDs int) error {
	// cpu.max is "<quota> <period>" in microseconds. One core is a full period of quota.
	if cpus > 0 {
		const period = 100000
		quota := cpus * period
		if err := c.write("cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			return err
		}
	}
	if memMB > 0 {
		if err := c.write("memory.max", strconv.FormatInt(int64(memMB)*1024*1024, 10)); err != nil {
			return err
		}
	}
	if maxPIDs <= 0 {
		maxPIDs = defaultMaxPIDs
	}
	// Always set a process cap, even for a job that reserved nothing.
	return c.write("pids.max", strconv.Itoa(maxPIDs))
}

func (c *jobCgroup) write(file, value string) error {
	if err := os.WriteFile(filepath.Join(c.path, file), []byte(value), 0644); err != nil {
		return fmt.Errorf("set %s=%s: %w", file, value, err)
	}
	return nil
}

// apply attaches the cgroup to a command so the child starts inside it.
func (c *jobCgroup) apply(attr *syscall.SysProcAttr) {
	if c == nil || c.dir == nil {
		return
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = int(c.dir.Fd())
}

// PeakMemoryBytes reports the job's high-water memory usage, or 0 if unavailable.
func (c *jobCgroup) PeakMemoryBytes() int64 {
	if c == nil {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(c.path, "memory.peak"))
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// WasOOMKilled reports whether the kernel killed anything in this cgroup for exceeding
// memory.max, which turns an opaque "signal: killed" into an actionable reason.
func (c *jobCgroup) WasOOMKilled() bool {
	if c == nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(c.path, "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			return err == nil && n > 0
		}
	}
	return false
}

// Close removes the cgroup. It is safe to call on a nil receiver.
func (c *jobCgroup) Close() error {
	if c == nil {
		return nil
	}
	if c.dir != nil {
		_ = c.dir.Close()
		c.dir = nil
	}
	// rmdir only succeeds once every process in the cgroup has exited, which is the point at
	// which we are called.
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		slog.Debug("could not remove job cgroup", "path", c.path, "error", err)
	}
	return nil
}

// cgroupsSupported reports whether resource limits can be enforced here.
func cgroupsSupported() bool {
	cgroupBaseOnce.Do(func() { cgroupBase, cgroupBaseErr = setupCgroupBase() })
	return cgroupBaseErr == nil
}

// cgroupUnavailableReason explains why limits cannot be enforced.
func cgroupUnavailableReason() string {
	if cgroupBaseErr == nil {
		return ""
	}
	return cgroupBaseErr.Error()
}
