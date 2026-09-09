package daemon

import (
	"context"
	"testing"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/ha"
	"github.com/deziss/tasch/internal/policy"
	"github.com/deziss/tasch/pkg/scheduler"
)

// newTestStore wires the scheduling paths to the single-master store, which is what a test
// without replication needs.
func newTestStore(srv *schedulerServer) ha.Store {
	return ha.NewDirect(srv.queue, scheduler.NewFairshareCalculator(), ha.NewCordons(), ha.NewReservations())
}

// preemptTestServer builds a server whose "bulk" partition is preemptible and whose "protected"
// one is not.
func preemptTestServer(t *testing.T, tune func(*config.Config)) *schedulerServer {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.NodeName = "n1"
	cfg.Preemption = config.PreemptionConfig{
		Enabled: true, PriorityMargin: 5, MinRuntimeSeconds: 60, MaxVictimsPerJob: 4,
	}
	cfg.Partitions = []config.PartitionConfig{
		{Name: "bulk", Preemptible: true},
		{Name: "protected"},
	}
	if tune != nil {
		tune(cfg)
	}
	pol, err := policy.New(cfg, nil)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	srv := newTestServer()
	srv.cfg = cfg
	srv.policy = pol
	return srv
}

// runningJob puts a job on a node in RUNNING state, started ago seconds in the past.
func runningJob(t *testing.T, srv *schedulerServer, id, partition string, priority int,
	gpus int, ago time.Duration) *scheduler.Job {
	t.Helper()

	job := &scheduler.Job{
		ID: id, Command: "true", Requirement: "true", Priority: priority,
		Partition: partition, GPUsRequired: gpus, SubmitTime: time.Now().Add(-ago),
	}
	if err := srv.queue.Enqueue(job); err != nil {
		t.Fatalf("Enqueue(%s): %v", id, err)
	}
	srv.queue.RemoveByID(id)
	if _, ok := srv.queue.MarkRunning(id, "node-1"); !ok {
		t.Fatalf("MarkRunning(%s) failed", id)
	}
	if !srv.queue.AdoptRunning(id, "node-1", 1, time.Now().Add(-ago)) {
		t.Fatalf("could not backdate %s", id)
	}
	return job
}

func incoming(priority int, gpus int, partition string) *scheduler.Job {
	return &scheduler.Job{
		ID: "urgent", Command: "true", Requirement: "true",
		Priority: priority, GPUsRequired: gpus, Partition: partition,
		SubmitTime: time.Now(),
	}
}

// The basic case: bulk work at priority 20 gives way to urgent work at priority 1.
func TestEligibleVictimsPicksLowerPriorityWork(t *testing.T) {
	srv := preemptTestServer(t, nil)
	runningJob(t, srv, "bulk-1", "bulk", 20, 1, 10*time.Minute)

	victims := eligibleVictims(srv, incoming(1, 1, "bulk"), "node-1", time.Now())
	if len(victims) != 1 || victims[0].job.ID != "bulk-1" {
		t.Fatalf("victims = %v, want bulk-1", victims)
	}
}

// The margin exists so ordinary priority jitter does not become eviction churn. A job one step
// higher must not be able to evict.
func TestPriorityMarginIsRespected(t *testing.T) {
	srv := preemptTestServer(t, nil)
	runningJob(t, srv, "bulk-1", "bulk", 10, 1, 10*time.Minute)

	// Margin is 5, so an incoming job must be at priority 5 or better to evict a 10.
	if got := eligibleVictims(srv, incoming(6, 1, "bulk"), "node-1", time.Now()); len(got) != 0 {
		t.Fatalf("a job only 4 steps higher evicted work: %v", got)
	}
	if got := eligibleVictims(srv, incoming(5, 1, "bulk"), "node-1", time.Now()); len(got) != 1 {
		t.Fatalf("a job exactly at the margin should evict, got %v", got)
	}
}

// Without a minimum runtime a loaded cluster can spend its time starting and killing the same
// jobs, making no progress at all.
func TestJobsThatJustStartedAreProtected(t *testing.T) {
	srv := preemptTestServer(t, nil)
	runningJob(t, srv, "fresh", "bulk", 20, 1, 5*time.Second)

	if got := eligibleVictims(srv, incoming(1, 1, "bulk"), "node-1", time.Now()); len(got) != 0 {
		t.Fatalf("a job five seconds old was evicted: %v", got)
	}
}

// Preemptibility is the operator's decision, not the submitter's. Work in a partition that was
// not marked preemptible is never a victim.
func TestNonPreemptiblePartitionIsNeverEvicted(t *testing.T) {
	srv := preemptTestServer(t, nil)
	runningJob(t, srv, "safe", "protected", 50, 1, time.Hour)

	if got := eligibleVictims(srv, incoming(1, 1, "protected"), "node-1", time.Now()); len(got) != 0 {
		t.Fatalf("work in a non-preemptible partition was evicted: %v", got)
	}
}

// A job with no partition has no operator decision behind it, so it must not be evicted on a
// default — that would surprise every cluster that upgrades into this feature.
func TestJobWithoutPartitionIsNotPreemptible(t *testing.T) {
	srv := preemptTestServer(t, nil)
	runningJob(t, srv, "legacy", "", 50, 1, time.Hour)

	if got := eligibleVictims(srv, incoming(1, 1, ""), "node-1", time.Now()); len(got) != 0 {
		t.Fatalf("a job with no partition was evicted: %v", got)
	}
}

// Evicting a gang rank alone fails every other rank with it, so gangs are excluded on both
// sides of the decision.
func TestGangRanksAreNeitherVictimNorBeneficiary(t *testing.T) {
	srv := preemptTestServer(t, nil)
	job := &scheduler.Job{
		ID: "rank-0", Command: "true", Priority: 20, Partition: "bulk",
		GroupID: "grp", SubmitTime: time.Now().Add(-time.Hour),
	}
	if err := srv.queue.Enqueue(job); err != nil {
		t.Fatal(err)
	}
	srv.queue.RemoveByID("rank-0")
	if _, ok := srv.queue.MarkRunning("rank-0", "node-1"); !ok {
		t.Fatal("MarkRunning failed")
	}
	srv.queue.AdoptRunning("rank-0", "node-1", 1, time.Now().Add(-time.Hour))

	if got := eligibleVictims(srv, incoming(1, 1, "bulk"), "node-1", time.Now()); len(got) != 0 {
		t.Fatalf("a gang rank was chosen as a victim: %v", got)
	}

	gangIncoming := incoming(1, 1, "bulk")
	gangIncoming.GroupID = "other"
	if preemptFor(srv, gangIncoming, nil) {
		t.Fatal("preemption ran for a gang job, which cannot be placed one rank at a time")
	}
}

// Turning preemption off must make it inert, whatever the partitions say.
func TestPreemptionDisabledDoesNothing(t *testing.T) {
	srv := preemptTestServer(t, func(cfg *config.Config) { cfg.Preemption.Enabled = false })
	runningJob(t, srv, "bulk-1", "bulk", 50, 1, time.Hour)

	if preemptFor(srv, incoming(1, 1, "bulk"), nil) {
		t.Fatal("preemption acted while disabled")
	}
}

func TestPreemptionConfigured(t *testing.T) {
	cfg := config.DefaultConfig()
	if preemptionConfigured(cfg) {
		t.Fatal("preemption is off by default and nothing is preemptible")
	}
	cfg.Preemption.Enabled = true
	if preemptionConfigured(cfg) {
		t.Fatal("enabling preemption with no preemptible partition should still be inert")
	}
	cfg.Partitions = []config.PartitionConfig{{Name: "bulk", Preemptible: true}}
	if !preemptionConfigured(cfg) {
		t.Fatal("preemption enabled with a preemptible partition should be reported as active")
	}
}

// The bug this pins: preemption requeues a victim, the worker's kill then reports a failure
// for the same attempt, and the attempt fence lets it through because a requeue does not change
// the attempt. Applying it marked the requeued job FAILED — turning a job that was only delayed
// into one that was destroyed, which is the opposite of what preemption promises.
func TestResultForARequeuedJobIsIgnored(t *testing.T) {
	srv := preemptTestServer(t, nil)
	srv.state = newTestStore(srv)

	runningJob(t, srv, "victim", "bulk", 20, 0, 10*time.Minute)
	job, _ := srv.queue.GetJob("victim")

	// Preemption puts it back in the queue, keeping its attempt.
	if _, ok := srv.state.RequeueRunning("victim"); !ok {
		t.Fatal("RequeueRunning failed")
	}

	// The worker's kill now reports the failure, for that same attempt.
	_, err := srv.ReportResult(context.Background(), &pb.ReportResultRequest{
		JobId: "victim", WorkerNode: "node-1", Success: false,
		Error: "cancelled", Attempt: job.Attempt,
	})
	if err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	after, ok := srv.queue.GetJob("victim")
	if !ok {
		t.Fatal("the job disappeared")
	}
	if after.State != scheduler.StateQueued {
		t.Fatalf("a preempted job ended %s; it must stay QUEUED so it runs again", after.State)
	}
}
