//go:build !windows

package profiler

import (
	"testing"
	"time"
)

// TestProbeTimesOutOnHungCommand is the regression test for a real hang: every GPU probe used
// plain exec.Command with no deadline, so an nvidia-smi stuck in uninterruptible sleep — the
// routine outcome when a GPU falls off the bus — blocked ClassAd generation forever. StartWorker
// never returned and `tasch start` hung with no message at all.
func TestProbeTimesOutOnHungCommand(t *testing.T) {
	// Shorten the budget so the test does not spend the real ten seconds waiting.
	original := probeTimeout
	probeTimeout = 200 * time.Millisecond
	defer func() { probeTimeout = original }()

	// Stand in for a wedged vendor tool: sleep far longer than the probe budget.
	start := time.Now()
	_, err := probe("sleep", "60")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("probe returned successfully from a command that should have been killed")
	}
	if elapsed > 5*time.Second {
		t.Errorf("probe took %v; it should give up at around %v", elapsed, probeTimeout)
	}
}

// TestProbeReturnsOutput confirms the timeout wrapper did not break the normal path.
func TestProbeReturnsOutput(t *testing.T) {
	out, err := probe("echo", "gpu-0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := string(out); got != "gpu-0\n" {
		t.Errorf("output = %q, want %q", got, "gpu-0\n")
	}
}

// TestProbeReportsMissingBinary confirms an absent vendor tool is an ordinary error, since a
// node with no nvidia-smi is the common case rather than a failure.
func TestProbeReportsMissingBinary(t *testing.T) {
	if _, err := probe("tasch-no-such-tool-exists"); err == nil {
		t.Error("probe reported success for a command that does not exist")
	}
}
