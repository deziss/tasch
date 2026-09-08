//go:build !linux

package daemon

import "syscall"

// Resource limits are a Linux cgroup v2 feature. On other platforms jobs run unconfined: the
// reservations the master tracks remain bookkeeping only, and a job can use the whole machine.

type jobCgroup struct{}

func newJobCgroup(jobID string, cpus, memMB, maxPIDs int) (*jobCgroup, error) {
	return nil, nil
}

func (c *jobCgroup) apply(attr *syscall.SysProcAttr) {}

func (c *jobCgroup) PeakMemoryBytes() int64 { return 0 }

func (c *jobCgroup) WasOOMKilled() bool { return false }

func (c *jobCgroup) Close() error { return nil }

func cgroupsSupported() bool { return false }

func cgroupUnavailableReason() string { return "resource limits require Linux with cgroup v2" }
