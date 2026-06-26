//go:build !windows

package daemon

import (
	"context"
	"os/exec"
)

func prepareCommand(ctx context.Context, cmdStr string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", cmdStr)
}
