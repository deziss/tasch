package daemon

import (
	"testing"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/pkg/scheduler"
)

// newTestServer builds the minimum schedulerServer the scheduling phases need. The phases
// under test here never reach discovery, matchmaking, or dispatch, so those stay nil.
func newTestServer() *schedulerServer {
	return &schedulerServer{
		queue:           scheduler.NewGlobalScheduler(),
		cb:              newCircuitBreaker(),
		gpuTracker:      newGPUTracker(),
		dispatchPending: make(map[string]time.Time),
		logStore:        make(map[string][]*pb.LogMessage),
		logChannels:     make(map[string][]chan *pb.LogMessage),
	}
}

func queuedJob(id string, priority int, groupID string) *scheduler.Job {
	return &scheduler.Job{
		ID:          id,
		Command:     "true",
		Requirement: "true",
		Priority:    priority,
		GroupID:     groupID,
		SubmitTime:  time.Now(),
	}
}

// TestDispatchTopJobYieldsToBackfillForGangRank is the regression test for the wedge: a gang
// rank at the head of the heap made the tick `continue`, which skipped the backfill phase
// along with the direct-match phase. No single job dispatched anywhere in the cluster for the
// duration of the gang timeout — and permanently if a restart orphaned the rank.
//
// dispatchTopJob must decline the rank and report false, so the caller proceeds to backfill.
func TestDispatchTopJobYieldsToBackfillForGangRank(t *testing.T) {
	srv := newTestServer()

	// The gang rank sorts first: same priority, earlier submit time.
	rank := queuedJob("rank-0", 10, "dj-abcd1234")
	rank.SubmitTime = time.Now().Add(-time.Minute)
	if err := srv.queue.Enqueue(rank); err != nil {
		t.Fatalf("enqueue rank: %v", err)
	}
	single := queuedJob("single-1", 10, "")
	if err := srv.queue.Enqueue(single); err != nil {
		t.Fatalf("enqueue single: %v", err)
	}

	if head := srv.queue.Peek(); head == nil || head.ID != "rank-0" {
		t.Fatalf("test setup: head is %v, want rank-0", head)
	}

	if dispatchTopJob(srv, nil) {
		t.Fatal("dispatchTopJob claimed it dispatched a gang rank")
	}

	// The single job must still be reachable — this is what the wedge prevented.
	got := srv.queue.Backfill(func(j *scheduler.Job) bool { return j.GroupID == "" })
	if got == nil {
		t.Fatal("backfill found nothing; the single job is unreachable behind the gang rank")
	}
	if got.ID != "single-1" {
		t.Fatalf("backfill returned %s, want single-1", got.ID)
	}
}

// TestDispatchTopJobDeclinesEmptyQueue confirms an empty queue is a clean no-op.
func TestDispatchTopJobDeclinesEmptyQueue(t *testing.T) {
	srv := newTestServer()
	if dispatchTopJob(srv, nil) {
		t.Fatal("dispatchTopJob claimed a dispatch from an empty queue")
	}
}

// TestDispatchTopJobDeclinesWhenNoNodeMatches confirms that with no members, the top job stays
// queued and backfill still gets its turn.
func TestDispatchTopJobDeclinesWhenNoNodeMatches(t *testing.T) {
	srv := newTestServer()
	if err := srv.queue.Enqueue(queuedJob("job-1", 10, "")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if dispatchTopJob(srv, nil) {
		t.Fatal("dispatchTopJob claimed a dispatch with no cluster members")
	}
	if got := srv.queue.QueueLen(); got != 1 {
		t.Errorf("queue length = %d, want 1 — the job should still be queued", got)
	}
}

// TestBackfillSkipsGangRanks confirms backfill never steals a rank out of a group, which would
// break co-scheduling by running one rank as a solo job.
func TestBackfillSkipsGangRanks(t *testing.T) {
	srv := newTestServer()
	if err := srv.queue.Enqueue(queuedJob("rank-0", 10, "dj-abcd1234")); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got := srv.queue.Backfill(func(j *scheduler.Job) bool { return j.GroupID == "" })
	if got != nil {
		t.Fatalf("backfill returned gang rank %s", got.ID)
	}
}
