package daemon

import (
	"strings"
	"testing"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/ha"
	"github.com/deziss/tasch/pkg/scheduler"
)

// Job IDs key both the in-memory map and the database, and both were last-write-wins. A
// collision silently cross-linked two users' jobs: results, logs and resource releases landed on
// the wrong one. The old IDs were 32 bits, which collide about half the time by 77,000 jobs.
func TestJobIDsDoNotCollide(t *testing.T) {
	const count = 20000
	seen := make(map[string]bool, count)
	for i := 0; i < count; i++ {
		id := newJobID()
		if seen[id] {
			t.Fatalf("job ID %s was generated twice within %d draws", id, count)
		}
		seen[id] = true
	}
	if len(newJobID()) < 16 {
		t.Fatalf("job IDs are %d characters, which is not enough entropy to key a database on",
			len(newJobID()))
	}
}

// A failure the job itself caused says nothing about the node's health, so it must not count
// toward blocking that node — otherwise one user's broken script takes a worker out of service.
func TestCircuitBreakerBlocksAfterConsecutiveFailures(t *testing.T) {
	cb := newCircuitBreaker()

	if cb.IsBlocked("node-1") {
		t.Fatal("a node with no history should not be blocked")
	}

	cb.RecordFailure("node-1", "job-1")
	cb.RecordFailure("node-1", "job-2")
	if cb.IsBlocked("node-1") {
		t.Fatal("two failures should not be enough to block a node")
	}

	cb.RecordFailure("node-1", "job-3")
	if !cb.IsBlocked("node-1") {
		t.Fatal("three consecutive failures should block the node")
	}

	// One success means the node is working, so the count must not persist.
	cb.RecordSuccess("node-1")
	if cb.IsBlocked("node-1") {
		t.Fatal("a success should clear the breaker")
	}
}

// The same job failing repeatedly is one broken job, not a broken node. Counting it again each
// time it is retried onto the same worker would eventually take that worker out of service.
func TestCircuitBreakerIgnoresRepeatsOfTheSameJob(t *testing.T) {
	cb := newCircuitBreaker()
	for i := 0; i < 5; i++ {
		cb.RecordFailure("node-1", "same-job")
	}
	if cb.IsBlocked("node-1") {
		t.Fatal("one job failing five times blocked the node it ran on")
	}
}

func TestUserErrorsAreNotTheNodesFault(t *testing.T) {
	tests := map[string]bool{
		"exit status 1":             true,
		"walltime exceeded (30s)":   true,
		"cancelled":                 true,
		"out of memory: exceeded":   true,
		"connection refused":        false,
		"no such file or directory": false,
	}
	for message, isUsers := range tests {
		if got := isUserError(message); got != isUsers {
			t.Errorf("isUserError(%q) = %v, want %v", message, got, isUsers)
		}
	}
}

// A cancellation or a walltime kill is a deliberate stop, not a transient failure. Retrying one
// re-runs a job the user explicitly killed; retrying the other burns the whole limit again.
func TestDeliberateStopsAreNotRetried(t *testing.T) {
	job := &scheduler.Job{ID: "j", MaxRetries: 3}

	if isRetryable(job, "walltime exceeded (30s)") {
		t.Fatal("a walltime kill was retried, which burns the whole limit again")
	}
	if isRetryable(job, "cancelled") {
		t.Fatal("a cancelled job was retried")
	}
	// Retrying an OOM spends three attempts reaching the same outcome: the reservation that was
	// exceeded is unchanged, and each attempt occupies a node to prove it again.
	if isRetryable(job, "out of memory: exceeded the 512 MB reservation") {
		t.Fatal("a job killed for exceeding its own memory reservation was retried")
	}
	if !isRetryable(job, "connection refused") {
		t.Fatal("a transient failure should be retried")
	}
}

// A worker that reconnects reports what it is running. Without adoption those jobs are declared
// failed while the processes keep running, holding resources the scheduler thinks are free.
func TestAdoptReportedJobs(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", GpusRequired: 1,
	})
	job := m.startJob(t, id, "node-1")

	// Simulate a master restart: the job is still RUNNING in the queue, but this master has
	// never seen the worker claim it.
	m.adoptedMu.Lock()
	m.adopted = make(map[string]bool)
	m.adoptedMu.Unlock()

	m.adoptReportedJobs("node-1", []*pb.RunningJob{
		{JobId: id, Attempt: job.Attempt, StartTime: time.Now().Add(-time.Minute).Unix()},
	})

	m.adoptedMu.Lock()
	claimed := m.adopted[id]
	m.adoptedMu.Unlock()
	if !claimed {
		t.Fatal("the worker's claim was not recorded, so the reaper would fail a live job")
	}

	after, _ := m.queue.GetJob(id)
	if after.State != scheduler.StateRunning {
		t.Fatalf("adopted job is %s, want it left RUNNING", after.State)
	}
}

// A job nobody claims is genuinely lost, and leaving it RUNNING holds its resources forever.
func TestReapUnclaimedJobsFailsWhatNobodyClaimed(t *testing.T) {
	m := newTestMaster(t)
	claimed := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "kept"})
	lost := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "lost"})
	m.startJob(t, claimed, "node-1")
	m.startJob(t, lost, "node-1")

	m.adoptedMu.Lock()
	m.adopted[claimed] = true
	m.adoptedMu.Unlock()

	// The real grace period is ninety seconds, which is right in production and absurd in a
	// test; what is under test is the decision, not the wait.
	restore := adoptionGracePeriod
	adoptionGracePeriod = 10 * time.Millisecond
	defer func() { adoptionGracePeriod = restore }()

	reapUnclaimedJobs(m.schedulerServer, []string{claimed, lost})

	if job, _ := m.queue.GetJob(claimed); job.State != scheduler.StateRunning {
		t.Fatalf("a claimed job was reaped: %s", job.State)
	}
	if job, _ := m.queue.GetJob(lost); job.State == scheduler.StateRunning {
		t.Fatal("a job nobody claimed was left RUNNING, holding its resources forever")
	}
}

// A job whose dependency failed is not waiting for anything: no future event can release it.
// Left alone it sits QUEUED forever, telling its submitter nothing.
func TestFailDoomedDependents(t *testing.T) {
	m := newTestMaster(t)

	first := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	second := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", DependsOn: []string{first},
	})

	m.startJob(t, first, "node-1")
	m.state.Complete(first, false, "", "exit status 1")

	failDoomedDependents(m.schedulerServer)

	job, _ := m.queue.GetJob(second)
	if job.State != scheduler.StateFailed {
		t.Fatalf("the dependent is %s, want FAILED rather than queued forever", job.State)
	}
	if !strings.Contains(job.Error, first) {
		t.Fatalf("error %q should name the dependency that ended it", job.Error)
	}
}

// A chain resolves: failing one job dooms its dependents, and failing those dooms theirs.
func TestFailDoomedDependentsResolvesAChain(t *testing.T) {
	m := newTestMaster(t)

	a := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "a"})
	b := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "b", DependsOn: []string{a},
	})
	c := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "c", DependsOn: []string{b},
	})

	m.startJob(t, a, "node-1")
	m.state.Complete(a, false, "", "exit status 1")

	// One pass per link, as the scheduling ticks would run them.
	failDoomedDependents(m.schedulerServer)
	failDoomedDependents(m.schedulerServer)

	for _, id := range []string{b, c} {
		job, _ := m.queue.GetJob(id)
		if job.State != scheduler.StateFailed {
			t.Fatalf("job %s is %s, want the failure to have propagated down the chain", id, job.State)
		}
	}
}

// A closed window constrains nothing, and a months-old reservation left in the list is the kind
// of clutter that gets acted on.
func TestExpireReservations(t *testing.T) {
	m := newTestMaster(t)
	now := time.Now()

	m.reservations.Add(ha.Reservation{
		ID: "old", Nodes: []string{"node-1"},
		Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour),
	})
	m.reservations.Add(ha.Reservation{
		ID: "current", Nodes: []string{"node-2"},
		Start: now.Add(-time.Minute), End: now.Add(time.Hour),
	})

	m.expireReservations()

	if _, ok := m.reservations.Get("old"); ok {
		t.Fatal("a closed window was left in the list")
	}
	if _, ok := m.reservations.Get("current"); !ok {
		t.Fatal("an open window was removed")
	}
}

// A gang either runs whole or not at all: a rank failing while its siblings keep running wastes
// the nodes they hold, since the job cannot complete without the missing rank.
func TestGroupFailureCancelsTheSiblings(t *testing.T) {
	m := newTestMaster(t)

	group := &scheduler.JobGroup{
		GroupID: "grp", NumNodes: 2, State: "RUNNING", CreatedAt: time.Now(),
		JobIDs: []string{"rank-0", "rank-1"},
	}
	for _, id := range group.JobIDs {
		job := &scheduler.Job{
			ID: id, Command: "true", Requirement: "true", GroupID: "grp",
			SubmitTime: time.Now(), User: "root",
		}
		if err := m.queue.Enqueue(job); err != nil {
			t.Fatal(err)
		}
		m.startJob(t, id, "node-1")
	}
	if err := m.state.RegisterGroup(group); err != nil {
		t.Fatal(err)
	}

	m.handleGroupCompletion("grp", "rank-0", false)

	sibling, _ := m.queue.GetJob("rank-1")
	if sibling.State == scheduler.StateRunning {
		t.Fatal("a sibling was left running after its gang lost a rank")
	}
	if g, ok := m.queue.GetGroup("grp"); !ok || g.State != scheduler.StateFailed {
		t.Fatalf("group state = %v, want FAILED", g)
	}
}

// Truncation must mark that it cut something, or a log line reads as complete when it is not.
func TestTruncate(t *testing.T) {
	// The marker is added to the kept prefix rather than fitted inside the limit, so a reader
	// can always tell a cut line from a complete one.
	if got := truncate("abcdefghij", 5); got != "abcde..." {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Fatalf("a short string was altered: %q", got)
	}
}

// Reservations must survive a master restart, or a restart quietly lets jobs back onto nodes
// that are about to be taken away.
func TestReservationsSurviveASnapshotRoundTrip(t *testing.T) {
	m := newTestMaster(t)
	now := time.Now().Truncate(time.Second)

	if err := m.state.AddReservation(ha.Reservation{
		ID: "r1", Nodes: []string{"node-1"}, Start: now, End: now.Add(time.Hour),
		Reason: "firmware",
	}); err != nil {
		t.Fatalf("AddReservation: %v", err)
	}

	restored := ha.NewReservations()
	restored.Restore(m.reservations.Snapshot())

	got, ok := restored.Get("r1")
	if !ok || got.Reason != "firmware" || !got.Start.Equal(now) {
		t.Fatalf("restored reservation = %+v, ok=%v", got, ok)
	}
}
