package scheduler

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// enqueueDep is a terse helper: the tests care about dependency wiring, not about job fields.
func enqueueDep(t *testing.T, gs *GlobalScheduler, id string, mutate ...func(*Job)) *Job {
	t.Helper()
	job := &Job{ID: id, Command: "true", Priority: 10, SubmitTime: time.Now()}
	for _, m := range mutate {
		m(job)
	}
	if err := gs.Enqueue(job); err != nil {
		t.Fatalf("Enqueue(%s): %v", id, err)
	}
	return job
}

func finish(t *testing.T, gs *GlobalScheduler, id string, success bool) {
	t.Helper()
	gs.RemoveByID(id)
	if _, ok := gs.MarkRunning(id, "node-1"); !ok {
		t.Fatalf("MarkRunning(%s) failed", id)
	}
	gs.MarkCompleted(id, success, "", "")
}

// The default mode holds a job until its dependency succeeds.
func TestDependencyAfterOKWaitsThenReleases(t *testing.T) {
	gs := NewGlobalScheduler()
	enqueueDep(t, gs, "first")
	enqueueDep(t, gs, "second", func(j *Job) { j.DependsOn = []string{"first"} })

	// "second" is behind "first" in the queue, but the point is that it is not merely behind:
	// it is ineligible, so even with "first" gone it would not be offered.
	if head := gs.PeekRunnable(); head == nil || head.ID != "first" {
		t.Fatalf("expected first to be runnable, got %v", head)
	}
	if state, reason := gs.Eligibility("second"); state != EligibleWaiting {
		t.Fatalf("second should be waiting, got state %v (%s)", state, reason)
	}

	finish(t, gs, "first", true)

	if state, reason := gs.Eligibility("second"); state != EligibleNow {
		t.Fatalf("second should be runnable once first succeeded, got %v (%s)", state, reason)
	}
	if head := gs.PeekRunnable(); head == nil || head.ID != "second" {
		t.Fatalf("expected second to be runnable, got %v", head)
	}
}

// A job waiting on something that failed is not waiting for anything: no future event can
// release it, so it must be reported as doomed rather than left queued forever.
func TestDependencyAfterOKDoomedOnFailure(t *testing.T) {
	gs := NewGlobalScheduler()
	enqueueDep(t, gs, "first")
	enqueueDep(t, gs, "second", func(j *Job) { j.DependsOn = []string{"first"} })

	finish(t, gs, "first", false)

	state, reason := gs.Eligibility("second")
	if state != EligibleDoomed {
		t.Fatalf("second should be doomed, got %v (%s)", state, reason)
	}
	if !strings.Contains(reason, "first") {
		t.Fatalf("reason %q should name the dependency", reason)
	}

	doomed := gs.DoomedQueued()
	if _, ok := doomed["second"]; !ok {
		t.Fatalf("DoomedQueued() = %v, want it to include second", doomed)
	}

	job, ok := gs.FailQueued("second", "dependency not satisfiable: "+reason)
	if !ok || job.State != StateFailed {
		t.Fatalf("FailQueued did not fail the job: %v %v", job, ok)
	}
	if gs.QueueLen() != 0 {
		t.Fatalf("a failed job is still in the queue (len %d)", gs.QueueLen())
	}
}

func TestDependencyModes(t *testing.T) {
	tests := []struct {
		mode      string
		depFailed bool
		want      Eligibility
	}{
		{mode: DependAfterAny, depFailed: false, want: EligibleNow},
		{mode: DependAfterAny, depFailed: true, want: EligibleNow},
		{mode: DependAfterNotOK, depFailed: true, want: EligibleNow},
		// A cleanup step that only runs on failure must not run when the job succeeded.
		{mode: DependAfterNotOK, depFailed: false, want: EligibleDoomed},
		{mode: DependAfterOK, depFailed: false, want: EligibleNow},
		{mode: DependAfterOK, depFailed: true, want: EligibleDoomed},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s/failed=%v", tc.mode, tc.depFailed), func(t *testing.T) {
			gs := NewGlobalScheduler()
			enqueueDep(t, gs, "dep")
			enqueueDep(t, gs, "job", func(j *Job) {
				j.DependsOn = []string{"dep"}
				j.DependencyMode = tc.mode
			})
			finish(t, gs, "dep", !tc.depFailed)

			if got, reason := gs.Eligibility("job"); got != tc.want {
				t.Fatalf("eligibility = %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

// The regression that matters: a blocked job sitting at the head of the queue must not stop
// everything behind it from dispatching. This is the same failure mode a gang rank at the head
// once caused, and it starves the whole cluster rather than one job.
func TestBlockedHeadDoesNotStarveTheQueue(t *testing.T) {
	gs := NewGlobalScheduler()
	enqueueDep(t, gs, "dep", func(j *Job) { j.Priority = 100 })
	// Priority 1 puts the blocked job at the head, ahead of the runnable one.
	enqueueDep(t, gs, "blocked", func(j *Job) { j.Priority = 1; j.DependsOn = []string{"dep"} })
	enqueueDep(t, gs, "runnable", func(j *Job) { j.Priority = 5 })

	if head := gs.Peek(); head == nil || head.ID != "blocked" {
		t.Fatalf("test setup: expected blocked at the head, got %v", head)
	}
	head := gs.PeekRunnable()
	if head == nil || head.ID != "runnable" {
		t.Fatalf("PeekRunnable() = %v, want runnable to be offered past the blocked head", head)
	}
}

// An array's whole reason to exist over a loop of submissions is that the scheduler can hold
// back the tail. If the throttle did not apply, a thousand tasks would hit the cluster at once.
func TestArrayConcurrencyThrottle(t *testing.T) {
	gs := NewGlobalScheduler()
	for i := 0; i < 4; i++ {
		enqueueDep(t, gs, fmt.Sprintf("task-%d", i), func(j *Job) {
			j.ArrayID = "arr"
			j.ArrayIndex = i
			j.ArrayMaxConcurrent = 2
		})
	}

	// Start two tasks; the array is now at its limit.
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("task-%d", i)
		gs.RemoveByID(id)
		if _, ok := gs.MarkRunning(id, "node-1"); !ok {
			t.Fatalf("MarkRunning(%s) failed", id)
		}
	}

	if head := gs.PeekRunnable(); head != nil {
		t.Fatalf("PeekRunnable() = %s, want nothing while the array is at its limit", head.ID)
	}
	state, reason := gs.Eligibility("task-2")
	if state != EligibleWaiting || !strings.Contains(reason, "limit") {
		t.Fatalf("task-2 eligibility = %v (%s), want waiting on the array limit", state, reason)
	}

	// One finishes, one slot opens.
	gs.MarkCompleted("task-0", true, "", "")
	head := gs.PeekRunnable()
	if head == nil || head.ArrayID != "arr" {
		t.Fatalf("PeekRunnable() = %v, want a task once a slot freed", head)
	}
}

// An array with no cap runs as wide as the cluster allows; the throttle must be opt-in.
func TestArrayWithoutLimitIsNotThrottled(t *testing.T) {
	gs := NewGlobalScheduler()
	for i := 0; i < 3; i++ {
		enqueueDep(t, gs, fmt.Sprintf("t-%d", i), func(j *Job) { j.ArrayID = "arr"; j.ArrayIndex = i })
	}
	gs.RemoveByID("t-0")
	if _, ok := gs.MarkRunning("t-0", "node-1"); !ok {
		t.Fatal("MarkRunning failed")
	}
	if head := gs.PeekRunnable(); head == nil {
		t.Fatal("an uncapped array should keep offering tasks while others run")
	}
}

// Half an array is worse than none: the submitter cannot ask for "the rest" without working out
// which rest, while the tasks that landed already occupy the cluster.
func TestEnqueueBatchIsAllOrNothing(t *testing.T) {
	gs := NewGlobalScheduler()
	gs.MaxQueueSize = 3

	batch := make([]*Job, 5)
	for i := range batch {
		batch[i] = &Job{ID: fmt.Sprintf("b-%d", i), Command: "true", SubmitTime: time.Now()}
	}
	if err := gs.EnqueueBatch(batch); err == nil {
		t.Fatal("EnqueueBatch should refuse a batch that does not fit")
	}
	if gs.QueueLen() != 0 {
		t.Fatalf("a rejected batch left %d jobs behind", gs.QueueLen())
	}

	if err := gs.EnqueueBatch(batch[:3]); err != nil {
		t.Fatalf("EnqueueBatch(3 of 3 slots): %v", err)
	}
	if gs.QueueLen() != 3 {
		t.Fatalf("queue length = %d, want 3", gs.QueueLen())
	}
}

// A duplicate ID anywhere in the batch must reject the whole batch, not enqueue the prefix.
func TestEnqueueBatchRejectsDuplicates(t *testing.T) {
	gs := NewGlobalScheduler()
	enqueueDep(t, gs, "existing")

	batch := []*Job{
		{ID: "fresh", Command: "true", SubmitTime: time.Now()},
		{ID: "existing", Command: "true", SubmitTime: time.Now()},
	}
	if err := gs.EnqueueBatch(batch); err == nil {
		t.Fatal("EnqueueBatch should reject a batch containing an existing ID")
	}
	if gs.KnownJob("fresh") {
		t.Fatal("a rejected batch enqueued its prefix")
	}
}

// A pruned dependency has to be treated as satisfied. Anything waiting on it was released or
// failed when it finished, hours before pruning could reach it; refusing to run would strand
// the job forever with no way to tell why.
func TestPrunedDependencyIsTreatedAsSatisfied(t *testing.T) {
	gs := NewGlobalScheduler()
	enqueueDep(t, gs, "waiter", func(j *Job) { j.DependsOn = []string{"long-gone"} })

	if state, reason := gs.Eligibility("waiter"); state != EligibleNow {
		t.Fatalf("eligibility = %v (%s), want runnable", state, reason)
	}
}
