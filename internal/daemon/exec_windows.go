//go:build windows

package daemon

import (
	"context"
	"os/exec"
	"syscall"
)

func prepareCommand(ctx context.Context, cmdStr string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
	}
	return cmd
}
