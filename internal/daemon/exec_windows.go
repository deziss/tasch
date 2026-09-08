//go:build windows

package daemon

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// processGroupGrace is how long a job's process tree has to exit before it is killed outright.
const processGroupGrace = 10 * time.Second

// prepareCommand builds the shell command for a job.
//
// As on Unix, cancelling must reap the job's descendants and not just the `cmd.exe` wrapper,
// or a backgrounded process survives a walltime kill and keeps holding its GPU. Windows has no
// process groups to signal, so the tree is torn down with `taskkill /T`.
func prepareCommand(ctx context.Context, cmdStr string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		// A new process group keeps the job's console signals from reaching the daemon.
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// /T includes the whole tree, /F forces termination.
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := kill.Run(); err != nil {
			// taskkill is unavailable or the tree is already gone; fall back to the wrapper.
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = processGroupGrace + 2*time.Second

	return cmd
}
