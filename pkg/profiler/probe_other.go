//go:build !windows

package profiler

import "os/exec"

// hideConsoleWindow is a no-op outside Windows, which has no console window to hide.
func hideConsoleWindow(cmd *exec.Cmd) {}
