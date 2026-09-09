package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/pkg/scheduler"
)

// A walltime is a promise to the rest of the cluster that a node comes back. Without
// enforcement one runaway job holds a machine indefinitely and nothing says why.
func TestWalltimeEnforcerKillsAnOverrunningJob(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "sleep forever", WalltimeSeconds: 1,
	})
	m.startJob(t, id, "node-1")

	// Backdate the start so the limit has already passed.
	if !m.queue.AdoptRunning(id, "node-1", 1, time.Now().Add(-time.Hour)) {
		t.Fatal("could not backdate the job's start time")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go walltimeEnforcer(ctx, m.schedulerServer)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if job, ok := m.queue.GetJob(id); ok && job.State != scheduler.StateRunning {
			if job.State != scheduler.StateCancelled {
				t.Fatalf("job ended %s, want CANCELLED", job.State)
			}
			// The allocation must go back, or the node stays booked for a job that is gone.
			if m.gpuTracker.UsageByNode()["node-1"] != 0 {
				t.Fatal("the node still holds resources for a job killed on walltime")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a job past its walltime was still running after ten seconds")
}

// A job with no walltime must be left alone: zero means no limit, and killing it would break
// every long-running workload on the cluster.
func TestWalltimeEnforcerLeavesUnboundedJobsAlone(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	m.startJob(t, id, "node-1")
	m.queue.AdoptRunning(id, "node-1", 1, time.Now().Add(-24*time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go walltimeEnforcer(ctx, m.schedulerServer)
	time.Sleep(200 * time.Millisecond)

	job, _ := m.queue.GetJob(id)
	if job.State != scheduler.StateRunning {
		t.Fatalf("a job with no walltime was killed after %s", job.State)
	}
}

// A dispatch nobody acknowledged is a job that never started. Re-driving it is what stops a
// dropped message losing work silently.
func TestDispatchTimeoutRequeuesAnUnacknowledgedJob(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	m.startJob(t, id, "node-1")

	m.dispatchPendingMu.Lock()
	m.dispatchPending[id] = time.Now().Add(-time.Hour)
	m.dispatchPendingMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go dispatchTimeoutEnforcer(ctx, m.schedulerServer)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if job, ok := m.queue.GetJob(id); ok && job.State == scheduler.StateQueued {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a dispatch nobody acknowledged was never re-driven")
}

// Logs are held per job in memory. Without a bound, a chatty job and a long-lived master are
// enough to exhaust it — and the cap has to keep the most recent window, since the newest lines
// are the ones worth having when something has just gone wrong.
func TestAppendLogBoundsWhatIsKeptPerJob(t *testing.T) {
	m := newTestMaster(t)

	for i := 0; i < maxLogEntriesPerJob+50; i++ {
		m.appendLog("noisy", "INFO", "line "+itoa(i))
	}

	m.logMu.Lock()
	kept := append([]*pb.LogMessage(nil), m.logStore["noisy"]...)
	m.logMu.Unlock()

	if len(kept) != maxLogEntriesPerJob {
		t.Fatalf("kept %d lines, want the cap of %d", len(kept), maxLogEntriesPerJob)
	}
	if kept[len(kept)-1].Message != "line "+itoa(maxLogEntriesPerJob+49) {
		t.Fatalf("the last line kept is %q; the window must end at the newest line",
			kept[len(kept)-1].Message)
	}
}

// Logs of jobs that no longer exist are pure leak: nothing will ever ask for them again.
func TestPruneLogsDropsVanishedJobs(t *testing.T) {
	m := newTestMaster(t)
	m.appendLog("ghost", "INFO", "from a job that no longer exists")

	m.pruneLogs()

	m.logMu.Lock()
	_, present := m.logStore["ghost"]
	m.logMu.Unlock()
	if present {
		t.Fatal("logs were kept for a job the scheduler has no record of")
	}
}

// Streaming logs is what a console follows a running job with. It must deliver the backlog and
// then stop cleanly when the caller goes away, rather than leaking a goroutine per viewer.
func TestStreamLogsDeliversTheBacklogThenStops(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	m.appendLog(id, "INFO", "first line")

	ctx, cancel := context.WithCancel(adminCtx())
	stream := &captureLogStream{ctx: ctx}

	done := make(chan error, 1)
	go func() { done <- m.StreamLogs(&pb.LogStreamRequest{JobId: id}, stream) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && stream.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if stream.count() == 0 {
		cancel()
		t.Fatal("the stream delivered none of the existing log lines")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StreamLogs did not return when its caller went away")
	}
}

// Ownership applies to logs as much as to results: they carry the job's command and its output.
func TestStreamLogsEnforcesOwnership(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, userCtx("alice"), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})

	err := m.StreamLogs(&pb.LogStreamRequest{JobId: id}, &captureLogStream{ctx: userCtx("mallory")})
	if err == nil {
		t.Fatal("another user streamed the job's logs")
	}
}

// WorkerStatus is what every node view is built from, so it has to report both the hardware and
// why a node is or is not taking work.
func TestWorkerStatusReportsSchedulingState(t *testing.T) {
	m := newTestMaster(t).withNodes(member("node-1", nodeAd(8, 16000, 2)))
	if err := m.state.Cordon("node-1", "firmware", time.Now()); err != nil {
		t.Fatalf("Cordon: %v", err)
	}

	resp, err := m.WorkerStatus(adminCtx(), &pb.WorkerStatusRequest{})
	if err != nil {
		t.Fatalf("WorkerStatus: %v", err)
	}

	state, ok := resp.NodeState["node-1"]
	if !ok {
		t.Fatal("a cordoned node is missing from the status response")
	}
	if !state.Cordoned || state.CordonReason != "firmware" {
		t.Fatalf("node state = %+v, want cordoned with its reason", state)
	}
}

// With HA off there is one master and it is always the leader; saying otherwise would make
// every client look for a leader that does not exist.
func TestClusterStatusWithoutHA(t *testing.T) {
	m := newTestMaster(t)

	resp, err := m.ClusterStatus(adminCtx(), &pb.ClusterStatusRequest{})
	if err != nil {
		t.Fatalf("ClusterStatus: %v", err)
	}
	if resp.HaEnabled {
		t.Fatal("HA reported as on when it is not configured")
	}
	if !resp.IsLeader {
		t.Fatal("a single master must report itself as leader")
	}
}

// A distributed job is a group that starts whole or not at all, and every rank needs the
// rendezvous variables or the ranks cannot find each other.
func TestSubmitDistributedJobFormsAGroup(t *testing.T) {
	m := newTestMaster(t)

	resp, err := m.SubmitDistributedJob(adminCtx(), &pb.SubmitDistributedJobRequest{
		CelRequirement: "true", Command: "train.py", NumNodes: 3, GpusPerNode: 2,
	})
	if err != nil {
		t.Fatalf("SubmitDistributedJob: %v", err)
	}
	if len(resp.JobIds) != 3 {
		t.Fatalf("group has %d ranks, want 3", len(resp.JobIds))
	}

	group, ok := m.queue.GetGroup(resp.GroupId)
	if !ok {
		t.Fatal("the group was not registered")
	}
	if group.NumNodes != 3 {
		t.Fatalf("group wants %d nodes, want 3", group.NumNodes)
	}

	for rank, id := range resp.JobIds {
		job, found := m.queue.GetJob(id)
		if !found {
			t.Fatalf("rank %d was not queued", rank)
		}
		if job.GroupID != resp.GroupId {
			t.Fatalf("rank %d belongs to group %q", rank, job.GroupID)
		}
		if job.GPUsRequired != 2 {
			t.Fatalf("rank %d reserved %d GPUs, want 2", rank, job.GPUsRequired)
		}
		if job.EnvVars["WORLD_SIZE"] != "3" {
			t.Fatalf("rank %d has WORLD_SIZE=%q, want 3", rank, job.EnvVars["WORLD_SIZE"])
		}
		if job.EnvVars["RANK"] == "" {
			t.Fatalf("rank %d has no RANK, so the ranks cannot tell each other apart", rank)
		}
	}
}

func TestSubmitDistributedJobValidatesNodeCount(t *testing.T) {
	m := newTestMaster(t)
	_, err := m.SubmitDistributedJob(adminCtx(), &pb.SubmitDistributedJobRequest{
		CelRequirement: "true", Command: "train.py", NumNodes: 0,
	})
	if err == nil {
		t.Fatal("a distributed job across zero nodes was accepted")
	}
	if !strings.Contains(err.Error(), "num_nodes") {
		t.Fatalf("error %q should name the field that was wrong", err)
	}

	if _, err := m.SubmitDistributedJob(adminCtx(), &pb.SubmitDistributedJobRequest{
		CelRequirement: "true", Command: "train.py", NumNodes: 2, GpusPerNode: -1,
	}); err == nil {
		t.Fatal("a negative GPUs-per-node was accepted")
	}
}

// captureLogStream is a gRPC server stream that records what was sent to it.
//
// The mutex is a plain zero-valued one rather than something created on first use: the stream
// is written by the handler's goroutine and read by the test's, so a lazily initialised guard
// is itself the race it was meant to prevent.
type captureLogStream struct {
	pb.SchedulerService_StreamLogsServer
	ctx context.Context

	mu   sync.Mutex
	sent []*pb.LogMessage
}

func (s *captureLogStream) Context() context.Context { return s.ctx }

func (s *captureLogStream) Send(msg *pb.LogMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (s *captureLogStream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}
