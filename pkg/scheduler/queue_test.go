package scheduler

import (
	"testing"
	"time"
)

func TestEnqueueDequeue(t *testing.T) {
	gs := NewGlobalScheduler()

	gs.Enqueue(&Job{ID: "low", Priority: 20, SubmitTime: time.Now(), Command: "echo low"})
	gs.Enqueue(&Job{ID: "high", Priority: 1, SubmitTime: time.Now(), Command: "echo high"})
	gs.Enqueue(&Job{ID: "mid", Priority: 10, SubmitTime: time.Now(), Command: "echo mid"})

	// Should dequeue in priority order: high (1), mid (10), low (20)
	j := gs.Dequeue()
	if j.ID != "high" {
		t.Errorf("Expected 'high', got '%s'", j.ID)
	}
	j = gs.Dequeue()
	if j.ID != "mid" {
		t.Errorf("Expected 'mid', got '%s'", j.ID)
	}
	j = gs.Dequeue()
	if j.ID != "low" {
		t.Errorf("Expected 'low', got '%s'", j.ID)
	}
	j = gs.Dequeue()
	if j != nil {
		t.Errorf("Expected nil on empty queue, got %v", j)
	}
}

func TestFIFOSamePriority(t *testing.T) {
	gs := NewGlobalScheduler()

	t1 := time.Now()
	t2 := t1.Add(1 * time.Second)
	t3 := t1.Add(2 * time.Second)

	gs.Enqueue(&Job{ID: "c", Priority: 10, SubmitTime: t3})
	gs.Enqueue(&Job{ID: "a", Priority: 10, SubmitTime: t1})
	gs.Enqueue(&Job{ID: "b", Priority: 10, SubmitTime: t2})

	order := []string{"a", "b", "c"}
	for _, expected := range order {
		j := gs.Dequeue()
		if j.ID != expected {
			t.Errorf("Expected '%s', got '%s'", expected, j.ID)
		}
	}
}

func TestJobStateTracking(t *testing.T) {
	gs := NewGlobalScheduler()
	gs.Enqueue(&Job{ID: "j1", Priority: 10, SubmitTime: time.Now(), Command: "echo test"})

	job, ok := gs.GetJob("j1")
	if !ok || job.State != StateQueued {
		t.Fatalf("Expected QUEUED state, got %s", job.State)
	}

	gs.MarkRunning("j1", "worker-1")
	job, _ = gs.GetJob("j1")
	if job.State != StateRunning {
		t.Fatalf("Expected RUNNING state, got %s", job.State)
	}

	gs.MarkCompleted("j1", true, "hello", "")
	job, _ = gs.GetJob("j1")
	if job.State != StateCompleted {
		t.Fatalf("Expected COMPLETED state, got %s", job.State)
	}
	if job.Output != "hello" {
		t.Fatalf("Expected output 'hello', got '%s'", job.Output)
	}
}

func TestCancel(t *testing.T) {
	gs := NewGlobalScheduler()
	gs.Enqueue(&Job{ID: "j1", Priority: 10, SubmitTime: time.Now()})
	gs.Enqueue(&Job{ID: "j2", Priority: 10, SubmitTime: time.Now()})

	_, ok := gs.Cancel("j1")
	if !ok {
		t.Fatal("Expected cancel to succeed")
	}

	job, _ := gs.GetJob("j1")
	if job.State != StateCancelled {
		t.Fatalf("Expected CANCELLED, got %s", job.State)
	}

	// Queue should only have j2
	if gs.QueueLen() != 1 {
		t.Fatalf("Expected queue length 1, got %d", gs.QueueLen())
	}

	j := gs.Dequeue()
	if j.ID != "j2" {
		t.Fatalf("Expected 'j2', got '%s'", j.ID)
	}
}

func TestBackfill(t *testing.T) {
	gs := NewGlobalScheduler()

	gs.Enqueue(&Job{ID: "big", Priority: 1, SubmitTime: time.Now(), Requirement: "ad.cpu_cores >= 64"})
	gs.Enqueue(&Job{ID: "small", Priority: 10, SubmitTime: time.Now(), Requirement: "ad.cpu_cores >= 1"})

	// Backfill: skip "big" (can't match), take "small"
	found := gs.Backfill(func(j *Job) bool {
		return j.Requirement == "ad.cpu_cores >= 1"
	})

	if found == nil || found.ID != "small" {
		t.Fatalf("Expected backfill to find 'small', got %v", found)
	}

	// Only "big" should remain
	if gs.QueueLen() != 1 {
		t.Fatalf("Expected queue length 1, got %d", gs.QueueLen())
	}
}

func TestFairshareCalculator(t *testing.T) {
	fc := NewFairshareCalculator()

	if p := fc.CalculatePenalty("alice"); p != 0 {
		t.Fatalf("Expected 0 penalty for unknown user, got %d", p)
	}

	fc.RecordUsage("alice", 250)
	if p := fc.CalculatePenalty("alice"); p != 2 {
		t.Fatalf("Expected penalty 2 for 250 units, got %d", p)
	}

	fc.DecayUsage(0.5)
	if p := fc.CalculatePenalty("alice"); p != 1 {
		t.Fatalf("Expected penalty 1 after 50%% decay, got %d", p)
	}
}
