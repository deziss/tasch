package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/deziss/tasch/internal/config"
)

// WritePID writes the current process PID to the pid file.
//
// The file is 0600: it is world-readable no more, because a writable PID file lets whoever can
// edit it choose which process a later privileged `tasch stop` signals.
func WritePID() error {
	path := config.PidPath()
	// filepath.Dir, not a manual LastIndex("/"): PidPath returns a bare "tasch.pid" when the
	// home directory cannot be resolved, and a backslash-separated path on Windows. Slicing on
	// a missing "/" panicked with "slice bounds out of range [:-1]" after the daemon had
	// already started.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("cannot create pid file directory %s: %w", dir, err)
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
}

// IsRunning reports whether the PID file names a live process.
//
// A PID file alone means nothing: after an OOM kill, a SIGKILL, a panic, or power loss the file
// survives, and refusing to start on its mere existence left the service permanently unable to
// boot until someone deleted it by hand.
func IsRunning() (int, bool) {
	data, err := os.ReadFile(config.PidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return pid, false
	}
	// Signal 0 checks for existence without delivering anything.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return pid, false
	}
	return pid, true
}

// RemovePID removes the pid file.
func RemovePID() {
	_ = os.Remove(config.PidPath())
}

// StopDaemon reads the PID file, sends SIGTERM, waits up to waitSeconds, then SIGKILL.
//
// waitSeconds must cover the daemon's own drain: the wait was hardcoded to 15s while the
// shipped default drain_timeout is 60s, so the documented graceful shutdown was always cut
// short by a SIGKILL with buffered database writes still in flight.
func StopDaemon(waitSeconds int) error {
	path := config.PidPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("tasch is not running (no pid file at %s)", path)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("invalid pid file: %w", err)
	}

	// Confirm the PID really is a tasch process before signalling it.
	//
	// StopDaemon signals whatever PID the file names, and the file used to be world-readable
	// and written 0644. Combined with PID reuse — or with an attacker who reached code
	// execution as the service account and rewrote it — a later `sudo tasch stop` delivered
	// SIGTERM and then SIGKILL, as root, to a process of someone else's choosing.
	if !isTaschProcess(pid) {
		_ = os.Remove(path)
		return fmt.Errorf("PID %d is not a tasch process; removed the stale pid file", pid)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("process %d not found: %w", pid, err)
	}

	// Send SIGTERM for graceful shutdown
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("failed to stop process %d: %w", pid, err)
	}

	fmt.Printf("Sent SIGTERM to PID %d, waiting for graceful shutdown...\n", pid)

	if waitSeconds < 1 {
		waitSeconds = 1
	}
	fmt.Printf("Waiting up to %ds for graceful shutdown...\n", waitSeconds)
	for i := 0; i < waitSeconds; i++ {
		time.Sleep(1 * time.Second)
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			// Process is gone
			_ = os.Remove(path)
			fmt.Printf("Tasch (PID %d) stopped.\n", pid)
			return nil
		}
	}

	// Force kill
	fmt.Printf("Process %d did not exit, sending SIGKILL...\n", pid)
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		log.Printf("could not SIGKILL pid %d: %v", pid, err)
	}
	_ = os.Remove(path)
	fmt.Printf("Tasch (PID %d) force-killed.\n", pid)
	return nil
}

// isTaschProcess reports whether pid looks like a tasch daemon.
//
// On Linux this reads /proc/<pid>/cmdline. Where that is unavailable the check cannot be made,
// and it returns true so the stop path still works — the guarantee is best-effort by platform.
func isTaschProcess(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// Not Linux, or /proc is not mounted: fall back to permitting the stop.
		return true
	}
	// cmdline is NUL-separated; the executable is the first field.
	argv0 := string(bytes.SplitN(data, []byte{0}, 2)[0])
	return filepath.Base(argv0) == "tasch"
}
