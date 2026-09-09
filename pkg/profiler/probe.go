package profiler

import (
	"context"
	"os/exec"
	"time"
)

// probeTimeout bounds how long a hardware-detection command may run.
//
// Every GPU probe shells out to a vendor tool, and all of them used plain exec.Command with no
// deadline. An nvidia-smi wedged in uninterruptible sleep — the routine outcome when a GPU falls
// off the bus, which is exactly when you want the node to still report in — blocked ClassAd
// generation forever. StartWorker never returned, and `tasch start` hung with no message and no
// watchdog. On Windows the PowerShell probe is slow enough to need a generous bound rather than
// no bound at all.
//
// It is a variable rather than a constant only so tests can shorten it.
var probeTimeout = 10 * time.Second

// probe runs a detection command and returns its stdout, giving up after probeTimeout.
//
// The context also kills the process, so a hung vendor tool is reaped rather than left behind
// on every worker restart.
func probe(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideConsoleWindow(cmd)
	return cmd.Output()
}
