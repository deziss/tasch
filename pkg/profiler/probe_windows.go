//go:build windows

package profiler

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow keeps a detection command from flashing a console window on a desktop.
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
