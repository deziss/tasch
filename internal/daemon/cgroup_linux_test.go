//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestApplyLimitsWritesCgroupFiles checks the values written for a reservation, without needing
// a real delegated cgroup: the writes are ordinary file writes into the cgroup directory.
func TestApplyLimitsWritesCgroupFiles(t *testing.T) {
	dir := t.TempDir()
	c := &jobCgroup{path: dir}

	if err := c.applyLimits(2, 512, 100); err != nil {
		t.Fatalf("applyLimits: %v", err)
	}

	// cpu.max is "<quota> <period>"; two cores is two full periods of quota.
	assertFile(t, dir, "cpu.max", "200000 100000")
	assertFile(t, dir, "memory.max", "536870912")
	assertFile(t, dir, "pids.max", "100")
}

// TestApplyLimitsAlwaysCapsProcesses confirms a job that reserved nothing still gets a process
// cap. Without it a fork bomb takes the worker and every co-tenant job down with it.
func TestApplyLimitsAlwaysCapsProcesses(t *testing.T) {
	dir := t.TempDir()
	c := &jobCgroup{path: dir}

	if err := c.applyLimits(0, 0, 0); err != nil {
		t.Fatalf("applyLimits: %v", err)
	}

	assertFile(t, dir, "pids.max", strconv.Itoa(defaultMaxPIDs))

	// No reservation means no cpu or memory limit, so those files must not be written.
	for _, f := range []string{"cpu.max", "memory.max"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s was written for a job that reserved nothing", f)
		}
	}
}

// TestWasOOMKilledReadsMemoryEvents confirms an OOM kill is recognised, which is what turns an
// opaque "signal: killed" into a reason the submitter can act on.
func TestWasOOMKilledReadsMemoryEvents(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"killed", "low 0\nhigh 0\nmax 12\noom 3\noom_kill 1\n", true},
		{"not killed", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n", false},
		{"pressure but no kill", "low 0\nhigh 5\nmax 2\noom 0\noom_kill 0\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(tc.content), 0644); err != nil {
				t.Fatalf("write: %v", err)
			}
			c := &jobCgroup{path: dir}
			if got := c.WasOOMKilled(); got != tc.want {
				t.Errorf("WasOOMKilled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPeakMemoryBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("4620288\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := &jobCgroup{path: dir}
	if got := c.PeakMemoryBytes(); got != 4620288 {
		t.Errorf("PeakMemoryBytes = %d, want 4620288", got)
	}
}

// TestNilCgroupIsSafe confirms every accessor tolerates a nil receiver, which is what a worker
// holds whenever cgroups are unavailable — and enforcement is deliberately best-effort.
func TestNilCgroupIsSafe(t *testing.T) {
	var c *jobCgroup
	if c.PeakMemoryBytes() != 0 {
		t.Error("PeakMemoryBytes on nil should be 0")
	}
	if c.WasOOMKilled() {
		t.Error("WasOOMKilled on nil should be false")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close on nil: %v", err)
	}
}

// TestOwnCgroupPathParsesV2Entry covers the /proc/self/cgroup parsing.
func TestOwnCgroupPathParsesV2Entry(t *testing.T) {
	got, err := ownCgroupPath()
	if err != nil {
		t.Skipf("not a cgroup v2 host: %v", err)
	}
	if !strings.HasPrefix(got, "/") {
		t.Errorf("cgroup path = %q, want an absolute path", got)
	}
}

func assertFile(t *testing.T, dir, name, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}
