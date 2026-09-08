//go:build !windows

package daemon

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// processGroupGrace is how long a job's process group has to exit after SIGTERM before it is
// killed outright.
const processGroupGrace = 10 * time.Second

// prepareCommand builds the shell command for a job, in its own process group.
//
// The process group matters: exec.CommandContext's default cancellation signals only the
// direct child, which is the `sh` wrapper. `sh` does not exec-optimise a compound command, so
// anything the job backgrounded — `python train.py & wait`, nohup, setsid, make -j, torchrun's
// per-rank children — was reparented to init and kept running after a walltime kill or a
// cancel, holding the GPUs the scheduler had just marked free. Signalling the whole group
// reclaims them.
func prepareCommand(ctx context.Context, cmdStr string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative PID addresses the process group. SIGTERM first so a job can checkpoint,
		// then SIGKILL for anything that ignores it.
		pgid := -cmd.Process.Pid
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		time.AfterFunc(processGroupGrace, func() {
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		})
		return nil
	}
	// Stop waiting on inherited pipes once the grace period has passed, so a leaked descriptor
	// in a grandchild cannot keep Wait blocked forever.
	cmd.WaitDelay = processGroupGrace + 2*time.Second

	return cmd
}
