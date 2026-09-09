package scheduler

import (
	"math"
	"testing"
	"time"
)

// TestRecordUsageWeightsResources is the regression test for unweighted billing: usage was raw
// wall-clock seconds, so a job holding 64 GPUs accrued exactly as much as one running sleep.
// A user could saturate the cluster's accelerators and be charged like an idle one.
func TestRecordUsageWeightsResources(t *testing.T) {
	fc := NewFairshareCalculator()

	// Same duration, wildly different resources.
	fc.RecordUsage("light", 100, 1, 0, 0)
	fc.RecordUsage("heavy", 100, 1, 8, 0)

	usage := fc.Snapshot()
	if usage["heavy"] <= usage["light"] {
		t.Fatalf("heavy=%v light=%v — a GPU job must cost more than a CPU job of equal duration",
			usage["heavy"], usage["light"])
	}

	// With the default weights, 8 GPU-seconds cost 32x a CPU-second each.
	wantRatio := (1.0 + 8*32) / 1.0
	gotRatio := usage["heavy"] / usage["light"]
	if math.Abs(gotRatio-wantRatio) > 0.01 {
		t.Errorf("cost ratio = %.2f, want %.2f", gotRatio, wantRatio)
	}
}

// TestRecordUsageBillsUnreservedJobsAsOneCore confirms a job that reserved nothing is not free.
func TestRecordUsageBillsUnreservedJobsAsOneCore(t *testing.T) {
	fc := NewFairshareCalculator()
	fc.RecordUsage("nobody", 100, 0, 0, 0)

	if got := fc.Snapshot()["nobody"]; got <= 0 {
		t.Errorf("usage = %v, want a job with no reservation to still be billed", got)
	}
}

// TestCalculatePenaltyReflectsShare confirms the penalty tracks a user's share of usage rather
// than an absolute count of seconds, which made it meaningless on a small cluster and
// overwhelming on a large one.
func TestCalculatePenaltyReflectsShare(t *testing.T) {
	fc := NewFairshareCalculator()

	// hog consumes three quarters of the cluster.
	fc.RecordUsage("hog", 300, 1, 0, 0)
	fc.RecordUsage("light", 100, 1, 0, 0)

	hog := fc.CalculatePenalty("hog")
	light := fc.CalculatePenalty("light")

	if hog <= light {
		t.Fatalf("hog=%d light=%d — the heavier user must be penalised more", hog, light)
	}
	if hog > fc.MaxPenalty {
		t.Errorf("penalty %d exceeds MaxPenalty %d", hog, fc.MaxPenalty)
	}
	// 300/400 of MaxPenalty.
	if want := fc.MaxPenalty * 3 / 4; hog != want {
		t.Errorf("hog penalty = %d, want %d", hog, want)
	}
}

func TestCalculatePenaltyUnknownUserIsZero(t *testing.T) {
	fc := NewFairshareCalculator()
	fc.RecordUsage("someone", 100, 1, 0, 0)

	if got := fc.CalculatePenalty("newcomer"); got != 0 {
		t.Errorf("penalty = %d, want 0 for a user with no usage", got)
	}
}

// TestDecayFactorHalvesOverHalfLife is the regression test for the decay rate: it was a
// hardcoded 0.95 per minute, a half-life of about thirteen minutes, so a user could saturate the
// cluster all morning and carry no penalty by lunchtime.
func TestDecayFactorHalvesOverHalfLife(t *testing.T) {
	const interval = time.Minute
	const halfLife = 24 * time.Hour

	factor := DecayFactorFor(interval, halfLife)

	// Applying it for exactly one half-life must halve the usage.
	usage := 1.0
	for i := 0; i < int(halfLife/interval); i++ {
		usage *= factor
	}
	if math.Abs(usage-0.5) > 0.001 {
		t.Errorf("usage after one half-life = %.4f, want 0.5", usage)
	}

	// The old behaviour, for contrast: 0.95 per minute reaches half in well under an hour.
	old := 1.0
	for i := 0; i < 60; i++ {
		old *= 0.95
	}
	if old > 0.5 {
		t.Errorf("sanity check failed: 0.95^60 = %.4f, expected well below 0.5", old)
	}
}

func TestDecayFactorHandlesZeroes(t *testing.T) {
	if got := DecayFactorFor(time.Minute, 0); got != 1 {
		t.Errorf("factor = %v, want 1 (no decay) for a zero half-life", got)
	}
	if got := DecayFactorFor(0, time.Hour); got != 1 {
		t.Errorf("factor = %v, want 1 for a zero interval", got)
	}
}

// TestDecayForgetsExhaustedAccounts confirms decayed-to-nothing users are dropped, so a cluster
// that has seen many one-off users does not accumulate entries forever.
func TestDecayForgetsExhaustedAccounts(t *testing.T) {
	fc := NewFairshareCalculator()
	fc.RecordUsage("transient", 1, 1, 0, 0)

	for i := 0; i < 100; i++ {
		fc.DecayUsage(0.5)
	}

	if _, still := fc.Snapshot()["transient"]; still {
		t.Error("an account decayed to nothing is still retained")
	}
}

// TestReprioritizeQueuedAppliesPenaltyToBacklog is the regression test for the frozen penalty:
// it was applied once at submission, so a user who filled the queue and only then became the
// heaviest consumer kept their whole backlog at its original priority.
func TestReprioritizeQueuedAppliesPenaltyToBacklog(t *testing.T) {
	gs := NewGlobalScheduler()

	for _, id := range []string{"hog-1", "hog-2"} {
		if err := gs.Enqueue(&Job{ID: id, User: "hog", Priority: 10, BasePriority: 10, SubmitTime: time.Now()}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if err := gs.Enqueue(&Job{ID: "light-1", User: "light", Priority: 10, BasePriority: 10, SubmitTime: time.Now()}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	changed := gs.ReprioritizeQueued(func(job *Job) int {
		if job.User == "hog" {
			return 20
		}
		return 0
	})
	if changed != 2 {
		t.Errorf("changed %d jobs, want 2", changed)
	}

	// The light user's job must now come first despite being submitted last.
	head := gs.Peek()
	if head == nil || head.User != "light" {
		t.Fatalf("head of queue = %v, want the light user's job", head)
	}

	// Reprioritising again must not compound the penalty.
	gs.ReprioritizeQueued(func(job *Job) int {
		if job.User == "hog" {
			return 20
		}
		return 0
	})
	job, _ := gs.GetJob("hog-1")
	if job.Priority != 30 {
		t.Errorf("priority = %d, want 30 (base 10 + penalty 20) — the penalty compounded", job.Priority)
	}
}
