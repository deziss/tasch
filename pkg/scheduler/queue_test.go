package scheduler

import (
	"testing"
	"time"
)

func enqueue(t *testing.T, gs *GlobalScheduler, job *Job) {
	t.Helper()
	if err := gs.Enqueue(job); err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
}

func TestEnqueueDequeue(t *testing.T) {
	gs := NewGlobalScheduler()

	enqueue(t, gs, &Job{ID: "low", Priority: 20, SubmitTime: time.Now(), Command: "echo low"})
	enqueue(t, gs, &Job{ID: "high", Priority: 1, SubmitTime: time.Now(), Command: "echo high"})
	enqueue(t, gs, &Job{ID: "mid", Priority: 10, SubmitTime: time.Now(), Command: "echo mid"})

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

	enqueue(t, gs, &Job{ID: "c", Priority: 10, SubmitTime: t3})
	enqueue(t, gs, &Job{ID: "a", Priority: 10, SubmitTime: t1})
	enqueue(t, gs, &Job{ID: "b", Priority: 10, SubmitTime: t2})

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
	enqueue(t, gs, &Job{ID: "j1", Priority: 10, SubmitTime: time.Now(), Command: "echo test"})

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
	enqueue(t, gs, &Job{ID: "j1", Priority: 10, SubmitTime: time.Now()})
	enqueue(t, gs, &Job{ID: "j2", Priority: 10, SubmitTime: time.Now()})

	_, ok := gs.Cancel("j1")
	if !ok {
		t.Fatal("Expected cancel to succeed")
	}

	job, _ := gs.GetJob("j1")
	if job.State != StateCancelled {
		t.Fatalf("Expected CANCELLED, got %s", job.State)
	}

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

	enqueue(t, gs, &Job{ID: "big", Priority: 1, SubmitTime: time.Now(), Requirement: "ad.cpu_cores >= 64"})
	enqueue(t, gs, &Job{ID: "small", Priority: 10, SubmitTime: time.Now(), Requirement: "ad.cpu_cores >= 1"})

	found := gs.Backfill(func(j *Job) bool {
		return j.Requirement == "ad.cpu_cores >= 1"
	})

	if found == nil || found.ID != "small" {
		t.Fatalf("Expected backfill to find 'small', got %v", found)
	}

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

func TestQueueLimit(t *testing.T) {
	gs := NewGlobalScheduler()
	gs.MaxQueueSize = 2

	err := gs.Enqueue(&Job{ID: "j1", Priority: 10, SubmitTime: time.Now()})
	if err != nil {
		t.Fatalf("First enqueue should succeed: %v", err)
	}
	err = gs.Enqueue(&Job{ID: "j2", Priority: 10, SubmitTime: time.Now()})
	if err != nil {
		t.Fatalf("Second enqueue should succeed: %v", err)
	}
	err = gs.Enqueue(&Job{ID: "j3", Priority: 10, SubmitTime: time.Now()})
	if err == nil {
		t.Fatal("Third enqueue should fail (queue full)")
	}
}

func TestPersistenceHooks(t *testing.T) {
	gs := NewGlobalScheduler()
	var jobChanges, groupChanges int
	gs.OnJobChange = func(job *Job) { jobChanges++ }
	gs.OnGroupChange = func(group *JobGroup) { groupChanges++ }

	enqueue(t, gs, &Job{ID: "j1", Priority: 10, SubmitTime: time.Now()})
	gs.MarkRunning("j1", "w1")
	gs.MarkCompleted("j1", true, "", "")

	if jobChanges != 3 {
		t.Fatalf("Expected 3 job change callbacks, got %d", jobChanges)
	}

	gs.RegisterGroup(&JobGroup{GroupID: "g1", State: "PENDING"})
	gs.SetGroupState("g1", "RUNNING")

	if groupChanges != 2 {
		t.Fatalf("Expected 2 group change callbacks, got %d", groupChanges)
	}
}
