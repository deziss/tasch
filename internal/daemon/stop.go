package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/deziss/tasch/internal/config"
)

// WritePID writes the current process PID to the pid file.
func WritePID() error {
	path := config.PidPath()
	dir := path[:strings.LastIndex(path, "/")]
	os.MkdirAll(dir, 0755)
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0644)
}

// RemovePID removes the pid file.
func RemovePID() {
	os.Remove(config.PidPath())
}

// StopDaemon reads the PID file and sends SIGTERM.
func StopDaemon() error {
	path := config.PidPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("tasch is not running (no pid file at %s)", path)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("invalid pid file: %w", err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("process %d not found: %w", pid, err)
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		os.Remove(path)
		return fmt.Errorf("failed to stop process %d: %w", pid, err)
	}

	os.Remove(path)
	fmt.Printf("Tasch (PID %d) stopped.\n", pid)
	return nil
}
