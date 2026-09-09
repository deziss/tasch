package ha

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/deziss/tasch/pkg/scheduler"
	"github.com/hashicorp/raft"
)

func newTestFSM() *FSM {
	return NewFSM(scheduler.NewGlobalScheduler(), scheduler.NewFairshareCalculator(), NewCordons())
}

func apply(t *testing.T, f *FSM, cmd *Command) interface{} {
	t.Helper()
	data, err := cmd.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", cmd.Type, err)
	}
	result := f.Apply(&raft.Log{Data: data})
	if err, isErr := result.(error); isErr {
		t.Fatalf("apply %s: %v", cmd.Type, err)
	}
	return result
}

func testJob(id string) *scheduler.Job {
	return &scheduler.Job{
		ID: id, Command: "true", Requirement: "true", User: "alice",
		Priority: 10, BasePriority: 10, SubmitTime: time.Now(),
	}
}

// TestFSMIsDeterministic is the property the whole design rests on: two replicas fed the same
// commands in the same order must reach the same state. If they can diverge, a follower promoted
// to leader schedules against a different reality than the one clients were promised.
func TestFSMIsDeterministic(t *testing.T) {
	commands := []*Command{
		{Type: CmdEnqueue, Job: testJob("j1")},
		{Type: CmdEnqueue, Job: testJob("j2")},
		{Type: CmdEnqueue, Job: testJob("j3")},
		{Type: CmdDispatch, JobID: "j1", Node: "node-a"},
		{Type: CmdComplete, JobID: "j1", Success: true, Output: "done"},
		{Type: CmdCancel, JobID: "j2"},
		{Type: CmdRecordUsage, User: "alice", Seconds: 100, CPUs: 2, GPUs: 1},
		{Type: CmdCordon, Node: "node-b", Reason: "maintenance", At: time.Unix(1700000000, 0)},
	}

	first, second := newTestFSM(), newTestFSM()
	for _, cmd := range commands {
		apply(t, first, cmd)
		apply(t, second, cmd)
	}

	for _, id := range []string{"j1", "j2", "j3"} {
		a, aok := first.Queue().GetJob(id)
		b, bok := second.Queue().GetJob(id)
		if aok != bok {
			t.Fatalf("%s present in one replica but not the other", id)
		}
		if a.State != b.State {
			t.Errorf("%s state diverged: %s vs %s", id, a.State, b.State)
		}
		if a.WorkerNode != b.WorkerNode {
			t.Errorf("%s node diverged: %q vs %q", id, a.WorkerNode, b.WorkerNode)
		}
		if a.Attempt != b.Attempt {
			t.Errorf("%s attempt diverged: %d vs %d", id, a.Attempt, b.Attempt)
		}
	}
	if first.Fairshare().Snapshot()["alice"] != second.Fairshare().Snapshot()["alice"] {
		t.Error("fairshare usage diverged")
	}
	if first.Cordons().IsCordoned("node-b") != second.Cordons().IsCordoned("node-b") {
		t.Error("cordon state diverged")
	}
}

// TestFSMDispatchTransitionsJob confirms a replicated dispatch has the same effect as the local
// one: the job leaves the queue and becomes RUNNING on the chosen node.
func TestFSMDispatchTransitionsJob(t *testing.T) {
	f := newTestFSM()
	apply(t, f, &Command{Type: CmdEnqueue, Job: testJob("j1")})

	result := apply(t, f, &Command{Type: CmdDispatch, JobID: "j1", Node: "node-a"})
	dispatch, ok := result.(DispatchResult)
	if !ok || !dispatch.OK {
		t.Fatalf("dispatch failed: %+v", result)
	}
	if dispatch.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", dispatch.Attempt)
	}

	job, _ := f.Queue().GetJob("j1")
	if job.State != scheduler.StateRunning {
		t.Errorf("state = %s, want RUNNING", job.State)
	}
	if job.WorkerNode != "node-a" {
		t.Errorf("node = %q, want node-a", job.WorkerNode)
	}
	if f.Queue().QueueLen() != 0 {
		t.Errorf("queue length = %d, want 0", f.Queue().QueueLen())
	}
}

// TestFSMDispatchOfMissingJobIsHarmless covers a dispatch replicated for a job that a
// previously-applied cancel already removed. It must not panic or corrupt state.
func TestFSMDispatchOfMissingJobIsHarmless(t *testing.T) {
	f := newTestFSM()
	result := apply(t, f, &Command{Type: CmdDispatch, JobID: "ghost", Node: "node-a"})
	if dispatch, ok := result.(DispatchResult); !ok || dispatch.OK {
		t.Errorf("dispatching a missing job reported success: %+v", result)
	}
}

// TestFSMSnapshotRoundTrip is what lets a new or lagging replica catch up without replaying the
// entire log. Everything a promoted leader needs must survive the round trip.
func TestFSMSnapshotRoundTrip(t *testing.T) {
	original := newTestFSM()
	apply(t, original, &Command{Type: CmdEnqueue, Job: testJob("queued-1")})
	apply(t, original, &Command{Type: CmdEnqueue, Job: testJob("running-1")})
	apply(t, original, &Command{Type: CmdDispatch, JobID: "running-1", Node: "node-a"})
	apply(t, original, &Command{Type: CmdRecordUsage, User: "alice", Seconds: 250, CPUs: 4, GPUs: 2})
	apply(t, original, &Command{Type: CmdCordon, Node: "node-b", Reason: "disk", At: time.Unix(1700000000, 0)})
	apply(t, original, &Command{Type: CmdRegisterGroup, Group: &scheduler.JobGroup{
		GroupID: "dj-1", JobIDs: []string{"running-1"}, NumNodes: 1, State: "PENDING",
	}})

	snapshot, err := original.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := &memorySink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	restored := newTestFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The queued job must be back in the heap, not merely in the map, or a promoted leader
	// would never dispatch it.
	if restored.Queue().QueueLen() != 1 {
		t.Errorf("queue length = %d, want 1", restored.Queue().QueueLen())
	}
	if head := restored.Queue().Peek(); head == nil || head.ID != "queued-1" {
		t.Errorf("head of queue = %v, want queued-1", head)
	}

	running, ok := restored.Queue().GetJob("running-1")
	if !ok || running.State != scheduler.StateRunning || running.WorkerNode != "node-a" {
		t.Errorf("running job did not survive: %+v", running)
	}
	if got := restored.Fairshare().Snapshot()["alice"]; got != original.Fairshare().Snapshot()["alice"] {
		t.Errorf("fairshare usage = %v, want %v", got, original.Fairshare().Snapshot()["alice"])
	}
	if !restored.Cordons().IsCordoned("node-b") {
		t.Error("cordon did not survive the snapshot")
	}
	if restored.Cordons().Reason("node-b") != "disk" {
		t.Errorf("cordon reason = %q, want %q", restored.Cordons().Reason("node-b"), "disk")
	}
	if len(restored.Queue().ListGroups()) != 1 {
		t.Error("job group did not survive the snapshot")
	}
}

// TestFSMRejectsUnknownCommand confirms an unrecognised entry is reported rather than silently
// skipped: skipping would let replicas diverge, which is the one thing this must never do.
func TestFSMRejectsUnknownCommand(t *testing.T) {
	f := newTestFSM()
	data, _ := (&Command{Type: "no-such-command"}).Encode()
	if err, isErr := f.Apply(&raft.Log{Data: data}).(error); !isErr {
		t.Fatalf("unknown command was accepted, got %v", err)
	}
}

func TestFSMRejectsMalformedEntry(t *testing.T) {
	f := newTestFSM()
	if _, isErr := f.Apply(&raft.Log{Data: []byte("{not json")}).(error); !isErr {
		t.Fatal("a malformed log entry was accepted")
	}
}

// TestReprioritizeUsesLeaderComputedPenalties confirms penalties travel in the command. Deriving
// them inside Apply would depend on each replica's usage snapshot at apply time and could
// diverge.
func TestReprioritizeUsesLeaderComputedPenalties(t *testing.T) {
	f := newTestFSM()
	hog := testJob("hog-1")
	hog.User = "hog"
	light := testJob("light-1")
	light.User = "light"
	apply(t, f, &Command{Type: CmdEnqueue, Job: hog})
	apply(t, f, &Command{Type: CmdEnqueue, Job: light})

	apply(t, f, &Command{Type: CmdReprioritize, Penalties: map[string]int{"hog": 20}})

	head := f.Queue().Peek()
	if head == nil || head.User != "light" {
		t.Fatalf("head of queue = %v, want the light user's job", head)
	}
	job, _ := f.Queue().GetJob("hog-1")
	if job.Priority != 30 {
		t.Errorf("priority = %d, want 30 (base 10 + penalty 20)", job.Priority)
	}
}

// memorySink is an in-memory raft.SnapshotSink.
type memorySink struct {
	bytes.Buffer
	cancelled bool
}

func (s *memorySink) ID() string    { return "test-snapshot" }
func (s *memorySink) Close() error  { return nil }
func (s *memorySink) Cancel() error { s.cancelled = true; return nil }
