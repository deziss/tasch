package scheduler

import (
	"testing"
	"time"
)

func newJob(id string) *Job {
	return &Job{ID: id, Command: "true", Requirement: "true", Priority: 10, SubmitTime: time.Now()}
}

// TestRequeueRejectsAlreadyQueuedJob is the regression test for heap corruption: Requeue pushed
// without checking state, so a duplicate result report put the same *Job into the heap twice.
// job.index then named only one of the two entries, and a later heap.Remove using that index
// evicted an unrelated job — which stayed marked QUEUED and was never scheduled again.
func TestRequeueRejectsAlreadyQueuedJob(t *testing.T) {
	gs := NewGlobalScheduler()
	if err := gs.Enqueue(newJob("j1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := gs.Enqueue(newJob("j2")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := gs.Requeue("j1", true); err == nil {
		t.Fatal("Requeue accepted a job that was already queued")
	}

	if got := gs.QueueLen(); got != 2 {
		t.Fatalf("queue length = %d, want 2 — the job was pushed onto the heap twice", got)
	}

	// Both jobs must still be reachable and distinct.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		job := gs.Dequeue()
		if job == nil {
			t.Fatalf("dequeue %d returned nil; a job was lost", i)
		}
		if seen[job.ID] {
			t.Fatalf("dequeued %s twice", job.ID)
		}
		seen[job.ID] = true
	}
}

// TestRequeueRejectsCancelledJob confirms a cancelled job cannot be resurrected by a retry.
func TestRequeueRejectsCancelledJob(t *testing.T) {
	gs := NewGlobalScheduler()
	if err := gs.Enqueue(newJob("j1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, ok := gs.Cancel("j1"); !ok {
		t.Fatal("cancel failed")
	}

	if _, err := gs.Requeue("j1", true); err == nil {
		t.Fatal("Requeue resurrected a cancelled job")
	}
	if job, _ := gs.GetJob("j1"); job.State != StateCancelled {
		t.Errorf("state = %s, want CANCELLED", job.State)
	}
}

// TestMarkRunningDoesNotOverwriteCancelled is the regression test for the lost-cancel race:
// Dequeue pops a job without changing its state, so a cancel can land between the pop and the
// dispatch. MarkRunning had no guard, so it overwrote CANCELLED with RUNNING — the user was
// told the job was cancelled, no cancel was ever sent to a worker, and the job ran anyway.
func TestMarkRunningDoesNotOverwriteCancelled(t *testing.T) {
	gs := NewGlobalScheduler()
	if err := gs.Enqueue(newJob("j1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	gs.Dequeue() // pop, as the dispatch loop does; state stays QUEUED
	if _, ok := gs.Cancel("j1"); !ok {
		t.Fatal("cancel failed")
	}

	if _, ok := gs.MarkRunning("j1", "worker-1"); ok {
		t.Fatal("MarkRunning reported success for a cancelled job")
	}
	job, _ := gs.GetJob("j1")
	if job.State != StateCancelled {
		t.Errorf("state = %s, want CANCELLED", job.State)
	}
	if job.WorkerNode != "" {
		t.Errorf("worker = %q, want empty — a cancelled job was assigned to a node", job.WorkerNode)
	}
}

// TestMarkRunningReportsUnknownJob confirms the return value distinguishes a missing job.
func TestMarkRunningReportsUnknownJob(t *testing.T) {
	gs := NewGlobalScheduler()
	if _, ok := gs.MarkRunning("nope", "worker-1"); ok {
		t.Fatal("MarkRunning reported success for an unknown job")
	}
}

// TestEnqueueRejectsDuplicateID confirms a colliding ID fails loudly instead of overwriting
// another user's job.
func TestEnqueueRejectsDuplicateID(t *testing.T) {
	gs := NewGlobalScheduler()
	first := newJob("dup")
	first.Command = "original"
	if err := gs.Enqueue(first); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	second := newJob("dup")
	second.Command = "impostor"
	if err := gs.Enqueue(second); err == nil {
		t.Fatal("Enqueue accepted a duplicate job ID")
	}

	job, _ := gs.GetJob("dup")
	if job.Command != "original" {
		t.Errorf("command = %q, want %q — the original job was overwritten", job.Command, "original")
	}
	if got := gs.QueueLen(); got != 1 {
		t.Errorf("queue length = %d, want 1", got)
	}
}

// TestDequeueIfIsAtomic confirms the head is only popped when the predicate accepts it, closing
// the window where a job was matched against a node and a different job got dispatched to it.
func TestDequeueIfIsAtomic(t *testing.T) {
	gs := NewGlobalScheduler()
	if err := gs.Enqueue(newJob("j1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if got := gs.DequeueIf(func(j *Job) bool { return false }); got != nil {
		t.Fatalf("DequeueIf popped %s despite the predicate rejecting it", got.ID)
	}
	if got := gs.QueueLen(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}

	var inspected string
	got := gs.DequeueIf(func(j *Job) bool {
		inspected = j.ID
		return true
	})
	if got == nil {
		t.Fatal("DequeueIf returned nil for an accepting predicate")
	}
	if got.ID != inspected {
		t.Fatalf("popped %s but the predicate inspected %s — they must be the same job", got.ID, inspected)
	}
	if gs.QueueLen() != 0 {
		t.Errorf("queue length = %d, want 0", gs.QueueLen())
	}
}

// TestDequeueIfOnEmptyQueue confirms the empty case is a clean nil.
func TestDequeueIfOnEmptyQueue(t *testing.T) {
	gs := NewGlobalScheduler()
	called := false
	if got := gs.DequeueIf(func(j *Job) bool { called = true; return true }); got != nil {
		t.Fatal("DequeueIf returned a job from an empty queue")
	}
	if called {
		t.Error("predicate ran against an empty queue")
	}
}

// TestMarkRunningIncrementsAttempt confirms every dispatch gets a fresh fencing token, so a
// result from a superseded dispatch can be told apart from the live one.
func TestMarkRunningIncrementsAttempt(t *testing.T) {
	gs := NewGlobalScheduler()
	if err := gs.Enqueue(newJob("j1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	first, ok := gs.MarkRunning("j1", "worker-1")
	if !ok {
		t.Fatal("first MarkRunning failed")
	}
	if first != 1 {
		t.Errorf("first attempt = %d, want 1", first)
	}

	// The job is requeued (dispatch timeout, retry) and dispatched again.
	if _, err := gs.Requeue("j1", false); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	second, ok := gs.MarkRunning("j1", "worker-2")
	if !ok {
		t.Fatal("second MarkRunning failed")
	}
	if second <= first {
		t.Errorf("second attempt = %d, want greater than %d", second, first)
	}

	job, _ := gs.GetJob("j1")
	if job.Attempt != second {
		t.Errorf("stored attempt = %d, want %d", job.Attempt, second)
	}
}

// TestRequeuePreservesAttempt confirms requeueing does not reset the fencing token, which would
// let a stale result look current again.
func TestRequeuePreservesAttempt(t *testing.T) {
	gs := NewGlobalScheduler()
	if err := gs.Enqueue(newJob("j1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	gs.MarkRunning("j1", "worker-1")
	if _, err := gs.Requeue("j1", true); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	job, _ := gs.GetJob("j1")
	if job.Attempt != 1 {
		t.Errorf("attempt = %d after requeue, want 1 — the fencing token must not reset", job.Attempt)
	}
}

// TestPruneTerminalReleasesFinishedJobs confirms finished jobs eventually leave memory. Nothing
// ever removed them, so gs.jobs grew with every job the cluster had ever run — each holding its
// captured output — and RunningJobs walked that whole map under the global lock once a second.
func TestPruneTerminalReleasesFinishedJobs(t *testing.T) {
	gs := NewGlobalScheduler()

	for _, id := range []string{"old", "recent", "queued"} {
		if err := gs.Enqueue(newJob(id)); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	gs.MarkCompleted("old", true, "done", "")
	gs.MarkCompleted("recent", true, "done", "")

	// Age "old" past the cutoff.
	if job, ok := gs.GetJob("old"); ok {
		_ = job
	}
	gs.mu.Lock()
	gs.jobs["old"].EndTime = time.Now().Add(-2 * time.Hour)
	gs.mu.Unlock()

	pruned := gs.PruneTerminal(time.Hour)
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if _, ok := gs.GetJob("old"); ok {
		t.Error("the aged terminal job is still resident")
	}
	if _, ok := gs.GetJob("recent"); !ok {
		t.Error("a recently finished job was pruned too early")
	}
	if _, ok := gs.GetJob("queued"); !ok {
		t.Error("a queued job was pruned")
	}
}

// TestPruneTerminalKeepsRunningJobs confirms pruning never touches live work.
func TestPruneTerminalKeepsRunningJobs(t *testing.T) {
	gs := NewGlobalScheduler()
	if err := gs.Enqueue(newJob("j1")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	gs.MarkRunning("j1", "worker-1")

	if pruned := gs.PruneTerminal(0); pruned != 0 {
		t.Fatalf("pruned = %d, want 0 — a running job was evicted", pruned)
	}
	if _, ok := gs.GetJob("j1"); !ok {
		t.Error("the running job is gone")
	}
}
