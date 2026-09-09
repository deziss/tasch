package daemon

import (
	"testing"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/ha"
	"github.com/deziss/tasch/pkg/scheduler"
)

// A node advertising unparseable metadata must not be treated as one with unlimited free
// resources, which is what an all-zero class ad amounted to.
func TestCanDispatchRejectsAnUnreadableClassAd(t *testing.T) {
	m := newTestMaster(t)
	job := &scheduler.Job{ID: "j", GPUsRequired: 1, CPUsRequired: 1}

	if canDispatchResources(m.schedulerServer, member("broken", "{not json"), job) {
		t.Fatal("a node with an unreadable class ad was offered work")
	}
}

func TestCanDispatchChecksEveryResource(t *testing.T) {
	m := newTestMaster(t)
	node := member("node-1", nodeAd(8, 16000, 2))

	tests := []struct {
		name string
		job  *scheduler.Job
		want bool
	}{
		{name: "fits", job: &scheduler.Job{ID: "a", CPUsRequired: 4, MemoryRequiredMB: 8000, GPUsRequired: 1}, want: true},
		{name: "too many GPUs", job: &scheduler.Job{ID: "b", GPUsRequired: 4}, want: false},
		{name: "too many cores", job: &scheduler.Job{ID: "c", CPUsRequired: 16}, want: false},
		{name: "too much memory", job: &scheduler.Job{ID: "d", MemoryRequiredMB: 64000}, want: false},
		{name: "asks for nothing", job: &scheduler.Job{ID: "e"}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canDispatchResources(m.schedulerServer, node, tc.job); got != tc.want {
				t.Fatalf("canDispatchResources = %v, want %v", got, tc.want)
			}
		})
	}
}

// Already-allocated capacity has to count, or two dispatches in the same tick both fit into
// room only one of them can have.
func TestCanDispatchCountsWhatIsAlreadyAllocated(t *testing.T) {
	m := newTestMaster(t)
	node := member("node-1", nodeAd(8, 16000, 2))

	if _, ok := m.gpuTracker.Allocate("node-1", "first", 2, 0, 0, 2); !ok {
		t.Fatal("could not allocate the node's GPUs")
	}

	if canDispatchResources(m.schedulerServer, node, &scheduler.Job{ID: "second", GPUsRequired: 1}) {
		t.Fatal("a GPU job was offered a node whose GPUs are all allocated")
	}
}

// A partition is a promise about which nodes a job runs on. It must hold even when the excluded
// node happens to have room.
func TestCanDispatchHonoursThePartition(t *testing.T) {
	m := newTestMaster(t, func(cfg *config.Config) {
		cfg.Partitions = []config.PartitionConfig{
			{Name: "gpu", NodeSelector: "ad.gpu_count > 0"},
			{Name: "cpu", NodeSelector: "ad.gpu_count == 0", Default: true},
		}
	})

	cpuNode := member("cpu-1", nodeAd(64, 128000, 0))
	gpuJob := &scheduler.Job{ID: "j", Partition: "gpu"}

	if canDispatchResources(m.schedulerServer, cpuNode, gpuJob) {
		t.Fatal("a gpu-partition job was offered a node with no GPUs, which has plenty of room")
	}
	if !canDispatchResources(m.schedulerServer, cpuNode, &scheduler.Job{ID: "k", Partition: "cpu"}) {
		t.Fatal("a cpu-partition job was refused its own partition's node")
	}
}

// Before a window opens, a job that could still be running when it does must not be placed.
// This is what drains a node in time without anyone cordoning it early.
func TestCanDispatchRespectsAnUpcomingReservation(t *testing.T) {
	m := newTestMaster(t)
	node := member("node-1", nodeAd(8, 16000, 0))

	start := time.Now().Add(20 * time.Minute)
	m.reservations.Add(ha.Reservation{
		ID: "r1", Nodes: []string{"node-1"}, Start: start, End: start.Add(time.Hour),
	})

	short := &scheduler.Job{ID: "short", WalltimeSeconds: 60}
	if !canDispatchResources(m.schedulerServer, node, short) {
		t.Fatal("a job finishing well before the window was refused")
	}

	long := &scheduler.Job{ID: "long", WalltimeSeconds: 7200}
	if canDispatchResources(m.schedulerServer, node, long) {
		t.Fatal("a job that would run into the window was placed on the node")
	}

	unbounded := &scheduler.Job{ID: "unbounded"}
	if canDispatchResources(m.schedulerServer, node, unbounded) {
		t.Fatal("a job with no walltime was placed ahead of a reservation it cannot be shown to precede")
	}
}

// Two 1-GPU jobs on one node must get different devices. Handing both device 0 makes them
// collide and OOM while the other GPUs idle.
func TestGPUVisibilityPinsRealDevices(t *testing.T) {
	tests := []struct {
		vendor  string
		devices []int
		key     string
		want    string
	}{
		{vendor: "nvidia", devices: []int{2, 3}, key: "CUDA_VISIBLE_DEVICES", want: "2,3"},
		{vendor: "amd", devices: []int{1}, key: "HIP_VISIBLE_DEVICES", want: "1"},
	}

	for _, tc := range tests {
		t.Run(tc.vendor, func(t *testing.T) {
			env := map[string]string{}
			setGPUVisibility(env, tc.vendor, tc.devices)
			if env[tc.key] != tc.want {
				t.Fatalf("%s = %q, want %q", tc.key, env[tc.key], tc.want)
			}
		})
	}
}

// A CPU job must not be told it has GPU 0. The variable is inherited by everything the job
// spawns, and an empty value is what actually hides the devices.
func TestGPUVisibilityWithNoDevices(t *testing.T) {
	env := map[string]string{}
	setGPUVisibility(env, "nvidia", nil)
	if got, ok := env["CUDA_VISIBLE_DEVICES"]; ok && got != "" {
		t.Fatalf("CUDA_VISIBLE_DEVICES = %q for a job with no GPUs", got)
	}
}

// Resource requests used to be scraped out of the CEL string with a regular expression, so any
// expression the regex did not literally match reserved nothing and the node was freely
// oversubscribed. Explicit fields win; the scrape is only a fallback.
func TestResourceRequestPrefersExplicitFields(t *testing.T) {
	cpus, mem := resourceRequest("ad.cpu_cores >= 8", 4, 2048)
	if cpus != 4 || mem != 2048 {
		t.Fatalf("explicit request = %d cpus / %d MB, want 4/2048", cpus, mem)
	}

	// With nothing explicit, the fallback reads what it can from the expression.
	cpus, _ = resourceRequest("ad.cpu_cores >= 8", 0, 0)
	if cpus != 8 {
		t.Fatalf("inferred cpus = %d, want 8 from the expression", cpus)
	}

	// An expression the scrape cannot read must reserve nothing rather than guess.
	cpus, mem = resourceRequest("ad.cpu_cores in [8, 16]", 0, 0)
	if cpus != 0 || mem != 0 {
		t.Fatalf("unreadable expression reserved %d cpus / %d MB, want nothing", cpus, mem)
	}
}

// A gang either starts whole or waits. Dispatching a rank at a time deadlocks the cluster: each
// group holds part of what the others need.
func TestGangGroupWaitsForEnoughNodes(t *testing.T) {
	m := newTestMaster(t)

	group := &scheduler.JobGroup{
		GroupID: "grp", NumNodes: 3, State: "PENDING", CreatedAt: time.Now(),
	}
	for i := 0; i < 3; i++ {
		id := "rank-" + itoa(i)
		job := &scheduler.Job{
			ID: id, Command: "true", Requirement: "true", GroupID: "grp",
			SubmitTime: time.Now(), User: "root",
		}
		if err := m.queue.Enqueue(job); err != nil {
			t.Fatal(err)
		}
		group.JobIDs = append(group.JobIDs, id)
	}
	if err := m.state.RegisterGroup(group); err != nil {
		t.Fatal(err)
	}

	// Only one node exists, so nothing may start.
	tryDispatchGroup(m.schedulerServer, group)

	for _, id := range group.JobIDs {
		job, _ := m.queue.GetJob(id)
		if job.State != scheduler.StateQueued {
			t.Fatalf("rank %s is %s; a gang must not start partially", id, job.State)
		}
	}
}

// The dispatch bus is what replaced a broadcast that handed every worker every job's command
// and environment variables.
func TestDispatchJobReachesOnlyItsTargetNode(t *testing.T) {
	m := newTestMaster(t)

	target, stopTarget := m.bus.Subscribe("node-1")
	defer stopTarget()
	other, stopOther := m.bus.Subscribe("node-2")
	defer stopOther()

	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "secret-command",
		EnvVars: map[string]string{"HF_TOKEN": "super-secret"},
	})
	job := m.startJob(t, id, "node-1")

	dispatchJob(m.schedulerServer, job, "node-1", job.Attempt)

	select {
	case msg := <-target:
		if msg.Command != "secret-command" {
			t.Fatalf("target received %q", msg.Command)
		}
		if msg.Attempt != job.Attempt {
			t.Fatalf("dispatch attempt = %d, want %d: results are fenced on it", msg.Attempt, job.Attempt)
		}
	case <-time.After(time.Second):
		t.Fatal("the target node received no dispatch")
	}

	select {
	case msg := <-other:
		t.Fatalf("another node received the job's command and environment: %v", msg)
	default:
	}
}

// The handshake exists so a dispatch nobody acknowledged can be re-driven. Recording the start
// is what stops it being re-dispatched to a worker already running it.
func TestAcknowledgeStartClearsThePendingDispatch(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	job := m.startJob(t, id, "node-1")

	m.dispatchPendingMu.Lock()
	m.dispatchPending[id] = time.Now()
	m.dispatchPendingMu.Unlock()

	if _, err := m.AcknowledgeStart(adminCtx(), &pb.AcknowledgeStartRequest{
		JobId: id, WorkerNode: "node-1", Attempt: job.Attempt,
	}); err != nil {
		t.Fatalf("AcknowledgeStart: %v", err)
	}

	m.dispatchPendingMu.Lock()
	_, stillPending := m.dispatchPending[id]
	m.dispatchPendingMu.Unlock()
	if stillPending {
		t.Fatal("the job is still awaiting acknowledgement after the worker acknowledged it")
	}
}

// A result from a superseded dispatch must change nothing: acting on it releases the current
// node's allocation and overwrites the live result.
func TestReportResultIgnoresAStaleAttempt(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})

	// Dispatch, lose the worker, dispatch again: the first attempt is now superseded. Attempt 0
	// would not do here — it is reserved for workers predating the field and is accepted for
	// compatibility, so a test using it would exercise the wrong branch.
	first := m.startJob(t, id, "old-node")
	if _, ok := m.state.RequeueRunning(id); !ok {
		t.Fatal("could not requeue the job")
	}
	second := m.startJob(t, id, "node-1")
	if second.Attempt <= first.Attempt {
		t.Fatalf("attempt did not advance: %d then %d", first.Attempt, second.Attempt)
	}

	if _, err := m.ReportResult(adminCtx(), &pb.ReportResultRequest{
		JobId: id, WorkerNode: "old-node", Success: false,
		Error: "stale failure", Attempt: first.Attempt,
	}); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	after, _ := m.queue.GetJob(id)
	if after.State != scheduler.StateRunning {
		t.Fatalf("a superseded result changed the job to %s", after.State)
	}
}

func TestReportResultRecordsTheOutcome(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	job := m.startJob(t, id, "node-1")

	if _, err := m.ReportResult(adminCtx(), &pb.ReportResultRequest{
		JobId: id, WorkerNode: "node-1", Success: true, Output: "done",
		Attempt: job.Attempt, StartTime: time.Now().Add(-time.Second).Unix(),
		EndTime: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("ReportResult: %v", err)
	}

	after, _ := m.queue.GetJob(id)
	if after.State != scheduler.StateCompleted {
		t.Fatalf("job is %s, want COMPLETED", after.State)
	}
	if after.Output != "done" {
		t.Fatalf("output = %q", after.Output)
	}
	// The allocation must be released, or the node is permanently oversubscribed.
	if used := m.gpuTracker.UsageByNode()["node-1"]; used != 0 {
		t.Fatalf("node still holds %d GPUs for a finished job", used)
	}
}

// Followers must not schedule: two masters dispatching from the same queue would each send the
// same job to a different node and both would run it.
func TestSchedulingTickDoesNothingOnAFollower(t *testing.T) {
	m := newTestMaster(t)
	m.state = followerStore{Store: m.state}

	m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	schedulingTick(m.schedulerServer)

	if m.queue.QueueLen() != 1 {
		t.Fatal("a follower dispatched from the queue")
	}
}

// followerStore is the store with leadership denied, which is all schedulingTick consults.
type followerStore struct{ ha.Store }

func (followerStore) IsLeader() bool { return false }

// End to end through the scheduling tick: a queued job on a matching node is dispatched to it.
// The tick is the only path that puts work on a worker, so a test of the parts is not a test of
// the whole.
func TestSchedulingTickDispatchesToAMatchingNode(t *testing.T) {
	m := newTestMaster(t).withNodes(member("gpu-1", nodeAd(16, 64000, 4)))

	stream, stop := m.bus.Subscribe("gpu-1")
	defer stop()

	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "ad.gpu_count > 0", Command: "train.py", GpusRequired: 2,
	})

	schedulingTick(m.schedulerServer)

	select {
	case msg := <-stream:
		if msg.JobId != id {
			t.Fatalf("dispatched %s, want %s", msg.JobId, id)
		}
		// The device list is what actually stops two jobs colliding on GPU 0.
		if msg.EnvVars["CUDA_VISIBLE_DEVICES"] == "" {
			t.Fatal("a GPU job was dispatched with no device pinning")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduling tick dispatched nothing to a node that matches")
	}

	job, _ := m.queue.GetJob(id)
	if job.State != scheduler.StateRunning {
		t.Fatalf("job is %s after dispatch, want RUNNING", job.State)
	}
}

// A cordoned node must not receive work, however well it matches.
func TestSchedulingTickSkipsCordonedNodes(t *testing.T) {
	m := newTestMaster(t).withNodes(member("gpu-1", nodeAd(16, 64000, 4)))
	if err := m.state.Cordon("gpu-1", "firmware", time.Now()); err != nil {
		t.Fatalf("Cordon: %v", err)
	}

	m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	schedulingTick(m.schedulerServer)

	if m.queue.QueueLen() != 1 {
		t.Fatal("work was dispatched to a cordoned node")
	}
}

// A job whose requirement no node satisfies must stay queued rather than being placed anywhere.
func TestSchedulingTickLeavesUnmatchableWorkQueued(t *testing.T) {
	m := newTestMaster(t).withNodes(member("cpu-1", nodeAd(8, 16000, 0)))

	m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "ad.gpu_count >= 8", Command: "train.py",
	})
	schedulingTick(m.schedulerServer)

	if m.queue.QueueLen() != 1 {
		t.Fatal("a job was placed on a node that does not satisfy its requirement")
	}
}

// The regression that started all of this: a job the queue cannot dispatch must not stop the
// ones behind it. Here the head needs GPUs the cluster does not have, and a CPU job behind it
// has to keep moving.
func TestSchedulingTickBackfillsPastAnUnplaceableHead(t *testing.T) {
	m := newTestMaster(t).withNodes(member("cpu-1", nodeAd(8, 16000, 0)))

	stream, stop := m.bus.Subscribe("cpu-1")
	defer stop()

	// Priority 1 puts the impossible job at the head.
	m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "ad.gpu_count >= 8", Command: "needs-gpus", Priority: 1,
	})
	runnable := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "cpu-work", Priority: 5,
	})

	schedulingTick(m.schedulerServer)

	select {
	case msg := <-stream:
		if msg.JobId != runnable {
			t.Fatalf("dispatched %s, want the runnable job behind the head", msg.JobId)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an unplaceable job at the head starved everything behind it")
	}
}
