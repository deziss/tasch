package scheduler

import (
	"encoding/json"
	"sync"
	"testing"
)

// TestFairshareSnapshotUnderConcurrentUsage is the regression test for the defect that killed
// the master: the persistence tick marshaled the live UserUsage map every 60 seconds while
// job completions wrote to it, producing an unrecoverable "concurrent map read and map write"
// fatal error that recover() cannot catch.
//
// Run with -race. Before the fix, marshaling fc.UserUsage here fails; Snapshot must not.
func TestFairshareSnapshotUnderConcurrentUsage(t *testing.T) {
	fc := NewFairshareCalculator()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: job completions recording usage.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					fc.RecordUsage("alice", 1, 1, 0, 0)
					fc.RecordUsage("bob", 2, 1, 0, 0)
				}
			}
		}(i)
	}

	// Reader: the persistence tick, marshaling a snapshot.
	for i := 0; i < 200; i++ {
		if _, err := json.Marshal(fc.Snapshot()); err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
		fc.DecayUsage(0.95)
		fc.CalculatePenalty("alice")
	}

	close(stop)
	wg.Wait()

	// Record once more with the writers stopped, so what is asserted below cannot depend on how
	// the two loops interleaved. The earlier version checked the snapshot straight after the
	// decay loop and assumed both users were still in it — but 200 rounds of 0.95 leaves usage
	// at three hundred-thousandths of what it was, below the point at which decay prunes an
	// entry, so whether they survived came down to how many writes had landed. It passed
	// normally and failed under -cover, which slows the writers relative to the reader: a test
	// that fails only when the build changes is worse than no test.
	fc.RecordUsage("alice", 1, 1, 0, 0)
	fc.RecordUsage("bob", 2, 1, 0, 0)

	got := fc.Snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot has %d users, want alice and bob", len(got))
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
}

// TestFairshareSnapshotIsDetached confirms the snapshot does not alias the live map, so a
// later write cannot mutate a map that is mid-marshal.
func TestFairshareSnapshotIsDetached(t *testing.T) {
	fc := NewFairshareCalculator()
	fc.RecordUsage("alice", 500, 1, 0, 0)

	snap := fc.Snapshot()
	fc.RecordUsage("alice", 500, 1, 0, 0)

	if snap["alice"] != 500 {
		t.Fatalf("snapshot mutated by a later write: alice = %v, want 500", snap["alice"])
	}
}

// TestFairshareRestoreIsDetached confirms restoring from persisted state copies the map rather
// than adopting the caller's.
func TestFairshareRestoreIsDetached(t *testing.T) {
	fc := NewFairshareCalculator()
	persisted := map[string]float64{"alice": 100}

	fc.Restore(persisted)
	persisted["alice"] = 999

	if got := fc.Snapshot()["alice"]; got != 100 {
		t.Fatalf("alice = %v, want 100 — Restore adopted the caller's map instead of copying", got)
	}
}
