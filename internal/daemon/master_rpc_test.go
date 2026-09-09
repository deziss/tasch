package daemon

import (
	"strings"
	"testing"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/pkg/scheduler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func codeOf(err error) codes.Code {
	return status.Code(err)
}

// An expression that does not compile used to be accepted, then silently matched nothing on
// every scheduling cycle forever: the job sat QUEUED with no error and no log line, and the
// submitter had no way to find out why.
func TestSubmitJobRejectsAnInvalidRequirement(t *testing.T) {
	m := newTestMaster(t)

	_, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "this is not CEL",
		Command:        "true",
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("SubmitJob = %v, want InvalidArgument", err)
	}
}

func TestSubmitJobRefusesWhileDraining(t *testing.T) {
	m := newTestMaster(t)
	m.draining.Store(true)

	_, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	if codeOf(err) != codes.Unavailable {
		t.Fatalf("SubmitJob while draining = %v, want Unavailable", err)
	}
}

// Identity comes from the authenticated principal, never from the request. Honouring the field
// would let a client evade its fairshare penalty by inventing a name on every submit, inflate
// someone else's usage, or blow up the metric label cardinality.
func TestSubmitJobTakesTheUserFromThePrincipal(t *testing.T) {
	m := newTestMaster(t)

	id := m.submit(t, userCtx("alice"), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", User: "bob",
	})

	job, ok := m.queue.GetJob(id)
	if !ok {
		t.Fatal("job was not queued")
	}
	if job.User != "alice" {
		t.Fatalf("job owner = %q, want alice: the --user flag must not override the principal", job.User)
	}
}

// A dependency must already exist. Besides catching a typo at submit rather than leaving the
// job queued forever, it is what makes a cycle impossible to express.
func TestSubmitJobRejectsAnUnknownDependency(t *testing.T) {
	m := newTestMaster(t)

	_, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", DependsOn: []string{"does-not-exist"},
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("SubmitJob with a missing dependency = %v, want InvalidArgument", err)
	}
	if !strings.Contains(status.Convert(err).Message(), "does-not-exist") {
		t.Fatalf("the error should name the missing job, got %q", err)
	}
}

func TestSubmitJobRejectsAnUnknownDependencyMode(t *testing.T) {
	m := newTestMaster(t)
	first := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})

	_, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true",
		DependsOn: []string{first}, DependencyMode: "whenever",
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("SubmitJob = %v, want InvalidArgument", err)
	}
}

// An array is one submission that becomes many jobs, each with its own index in the
// environment — that index is the only thing distinguishing them.
func TestSubmitJobExpandsAnArray(t *testing.T) {
	m := newTestMaster(t)

	resp, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "echo $TASCH_ARRAY_TASK_ID", Array: "1-4%2",
	})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}

	if len(resp.JobIds) != 4 {
		t.Fatalf("array produced %d tasks, want 4", len(resp.JobIds))
	}
	if resp.ArrayId == "" {
		t.Fatal("the response carries no array id")
	}
	if resp.JobId != resp.JobIds[0] {
		t.Fatal("job_id should carry the first task, so a client unaware of arrays still gets one")
	}

	seen := map[string]bool{}
	for _, id := range resp.JobIds {
		job, ok := m.queue.GetJob(id)
		if !ok {
			t.Fatalf("task %s was not queued", id)
		}
		if job.ArrayMaxConcurrent != 2 {
			t.Fatalf("task concurrency cap = %d, want 2", job.ArrayMaxConcurrent)
		}
		index := job.EnvVars["TASCH_ARRAY_TASK_ID"]
		if index == "" {
			t.Fatalf("task %s has no TASCH_ARRAY_TASK_ID", id)
		}
		if seen[index] {
			t.Fatalf("two tasks share index %s, so they would run the same work twice", index)
		}
		seen[index] = true
		if job.EnvVars["TASCH_ARRAY_TASK_COUNT"] != "4" {
			t.Fatalf("task count = %q, want 4", job.EnvVars["TASCH_ARRAY_TASK_COUNT"])
		}
	}
}

func TestSubmitJobRejectsAMalformedArray(t *testing.T) {
	m := newTestMaster(t)

	_, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", Array: "9-1",
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("SubmitJob with a backwards range = %v, want InvalidArgument", err)
	}
}

// Naming a partition that does not exist is a malformed request; naming one you may not use is
// not. Conflating them tells a user to fix a name that was never wrong.
func TestSubmitJobDistinguishesUnknownFromForbiddenPartitions(t *testing.T) {
	m := newTestMaster(t, func(cfg *config.Config) {
		cfg.Partitions = []config.PartitionConfig{
			{Name: "gpu", AllowedUsers: []string{"root"}},
			{Name: "cpu", Default: true},
		}
	})

	_, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", Partition: "ghost",
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("unknown partition = %v, want InvalidArgument", err)
	}

	_, err = m.SubmitJob(userCtx("mallory"), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", Partition: "gpu",
	})
	if codeOf(err) != codes.PermissionDenied {
		t.Fatalf("forbidden partition = %v, want PermissionDenied", err)
	}
}

// A partition's walltime ceiling only means something if it is applied where the user can see
// it, which is at submit.
func TestSubmitJobAppliesPartitionWalltime(t *testing.T) {
	m := newTestMaster(t, func(cfg *config.Config) {
		cfg.Partitions = []config.PartitionConfig{{
			Name: "short", Default: true,
			DefaultWalltimeSeconds: 600, MaxWalltimeSeconds: 1800,
		}}
	})

	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	job, _ := m.queue.GetJob(id)
	if job.WalltimeSeconds != 600 {
		t.Fatalf("walltime = %d, want the partition default of 600", job.WalltimeSeconds)
	}
	if job.Partition != "short" {
		t.Fatalf("partition = %q, want short", job.Partition)
	}

	_, err := m.SubmitJob(adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", WalltimeSeconds: 7200,
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("a walltime above the ceiling = %v, want InvalidArgument", err)
	}
}

// Cancelling someone else's job must not be possible, and the answer must not confirm that the
// job exists — that is the enumeration step which makes forging results practical.
func TestCancelJobEnforcesOwnership(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, userCtx("alice"), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})

	_, err := m.CancelJob(userCtx("mallory"), &pb.CancelJobRequest{JobId: id})
	if codeOf(err) != codes.NotFound {
		t.Fatalf("cancelling another user's job = %v, want NotFound", err)
	}

	resp, err := m.CancelJob(userCtx("alice"), &pb.CancelJobRequest{JobId: id})
	if err != nil {
		t.Fatalf("the owner could not cancel their own job: %v", err)
	}
	if resp.Status != "CANCELLED" {
		t.Fatalf("status = %q, want CANCELLED", resp.Status)
	}
}

func TestCancelJobOnSomethingAlreadyFinished(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	m.startJob(t, id, "node-1")
	m.state.Complete(id, true, "done", "")

	resp, err := m.CancelJob(adminCtx(), &pb.CancelJobRequest{JobId: id})
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if resp.Status != scheduler.StateCompleted {
		t.Fatalf("status = %q, want the job's real state", resp.Status)
	}
}

func TestGetJobStatusHidesOtherPeoplesJobs(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, userCtx("alice"), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})

	_, err := m.GetJobStatus(userCtx("mallory"), &pb.GetJobStatusRequest{JobId: id})
	if codeOf(err) != codes.NotFound {
		t.Fatalf("reading another user's job = %v, want NotFound", err)
	}
}

// "QUEUED" on its own does not say whether the cluster is busy or the job is waiting on
// something specific. Saying which turns a support question into an answer.
func TestGetJobStatusExplainsWhyAJobIsWaiting(t *testing.T) {
	m := newTestMaster(t)

	first := m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	second := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", DependsOn: []string{first},
	})

	resp, err := m.GetJobStatus(adminCtx(), &pb.GetJobStatusRequest{JobId: second})
	if err != nil {
		t.Fatalf("GetJobStatus: %v", err)
	}
	if !strings.Contains(resp.BlockedReason, first) {
		t.Fatalf("blocked reason = %q, want it to name the dependency", resp.BlockedReason)
	}
	if len(resp.DependsOn) != 1 || resp.DependsOn[0] != first {
		t.Fatalf("depends_on = %v, want [%s]", resp.DependsOn, first)
	}
}

func TestGetJobStatusOnAnUnknownJob(t *testing.T) {
	m := newTestMaster(t)
	resp, err := m.GetJobStatus(adminCtx(), &pb.GetJobStatusRequest{JobId: "nope"})
	if err != nil {
		t.Fatalf("GetJobStatus: %v", err)
	}
	if resp.State != "NOT_FOUND" {
		t.Fatalf("state = %q, want NOT_FOUND", resp.State)
	}
}

// An unscoped list handed out every job ID, user and command in the cluster.
func TestListJobsIsScopedToWhatTheCallerOwns(t *testing.T) {
	m := newTestMaster(t)
	m.submit(t, userCtx("alice"), &pb.SubmitJobRequest{CelRequirement: "true", Command: "alice-job"})
	m.submit(t, userCtx("bob"), &pb.SubmitJobRequest{CelRequirement: "true", Command: "bob-job"})

	resp, err := m.ListJobs(userCtx("alice"), &pb.ListJobsRequest{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0].Command != "alice-job" {
		t.Fatalf("alice saw %d jobs, want only her own", len(resp.Jobs))
	}

	all, err := m.ListJobs(adminCtx(), &pb.ListJobsRequest{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(all.Jobs) != 2 {
		t.Fatalf("admin saw %d jobs, want 2", len(all.Jobs))
	}
}

// The user filter narrows what the caller may already see; it must never widen it.
func TestListJobsUserFilterCannotWidenAccess(t *testing.T) {
	m := newTestMaster(t)
	m.submit(t, userCtx("alice"), &pb.SubmitJobRequest{CelRequirement: "true", Command: "alice-job"})
	m.submit(t, userCtx("bob"), &pb.SubmitJobRequest{CelRequirement: "true", Command: "bob-job"})

	resp, err := m.ListJobs(userCtx("alice"), &pb.ListJobsRequest{UserFilter: "bob"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(resp.Jobs) != 0 {
		t.Fatalf("alice reached bob's jobs by filtering for them: %d returned", len(resp.Jobs))
	}

	asAdmin, err := m.ListJobs(adminCtx(), &pb.ListJobsRequest{UserFilter: "bob"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(asAdmin.Jobs) != 1 || asAdmin.Jobs[0].User != "bob" {
		t.Fatalf("admin filter returned %d jobs, want bob's one", len(asAdmin.Jobs))
	}
}

func TestListJobsPaginates(t *testing.T) {
	m := newTestMaster(t)
	for i := 0; i < 5; i++ {
		m.submit(t, adminCtx(), &pb.SubmitJobRequest{CelRequirement: "true", Command: "true"})
	}

	first, err := m.ListJobs(adminCtx(), &pb.ListJobsRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(first.Jobs) != 2 || first.NextPageToken == "" {
		t.Fatalf("first page had %d jobs, token %q", len(first.Jobs), first.NextPageToken)
	}
	if first.TotalMatching != 5 {
		t.Fatalf("total = %d, want 5 across all pages", first.TotalMatching)
	}

	second, err := m.ListJobs(adminCtx(), &pb.ListJobsRequest{PageSize: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatalf("ListJobs page 2: %v", err)
	}
	for _, a := range first.Jobs {
		for _, b := range second.Jobs {
			if a.JobId == b.JobId {
				t.Fatalf("job %s appeared on two pages", a.JobId)
			}
		}
	}
}

func TestListJobsRejectsAGarbagePageToken(t *testing.T) {
	m := newTestMaster(t)
	_, err := m.ListJobs(adminCtx(), &pb.ListJobsRequest{PageToken: "not-a-token"})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("ListJobs with a bad token = %v, want InvalidArgument", err)
	}
}

func TestListJobsReportsTimingAndPlacement(t *testing.T) {
	m := newTestMaster(t)
	id := m.submit(t, adminCtx(), &pb.SubmitJobRequest{
		CelRequirement: "true", Command: "true", GpusRequired: 2, CpusRequired: 4,
	})
	m.startJob(t, id, "node-1")

	resp, err := m.ListJobs(adminCtx(), &pb.ListJobsRequest{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	info := resp.Jobs[0]
	if info.StartTime == 0 {
		t.Fatal("a running job reported no start time, so no client can show how long it has run")
	}
	// Not started means unset, not 1970.
	if info.EndTime != 0 {
		t.Fatalf("end time = %d, want 0 while the job is still running", info.EndTime)
	}
	if info.GpusRequired != 2 || info.CpusRequired != 4 {
		t.Fatalf("reservations = %d GPUs / %d CPUs, want 2/4", info.GpusRequired, info.CpusRequired)
	}
}

// Taking a node out of service affects everyone's work, not just the caller's.
func TestCordonNodeRequiresAdmin(t *testing.T) {
	m := newTestMaster(t)

	_, err := m.CordonNode(userCtx("alice"), &pb.CordonNodeRequest{NodeName: "node-1", Cordon: true})
	if codeOf(err) != codes.PermissionDenied {
		t.Fatalf("cordon as a user = %v, want PermissionDenied", err)
	}

	if _, err := m.CordonNode(adminCtx(), &pb.CordonNodeRequest{
		NodeName: "node-1", Cordon: true, Reason: "firmware",
	}); err != nil {
		t.Fatalf("cordon as admin: %v", err)
	}
	if !m.cordons.IsCordoned("node-1") {
		t.Fatal("the node was not cordoned")
	}
	if m.cordons.Reason("node-1") != "firmware" {
		t.Fatalf("reason = %q, want firmware", m.cordons.Reason("node-1"))
	}

	if _, err := m.CordonNode(adminCtx(), &pb.CordonNodeRequest{NodeName: "node-1"}); err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if m.cordons.IsCordoned("node-1") {
		t.Fatal("the node is still cordoned after being returned to service")
	}
}

func TestCordonNodeNeedsAName(t *testing.T) {
	m := newTestMaster(t)
	_, err := m.CordonNode(adminCtx(), &pb.CordonNodeRequest{Cordon: true})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("cordon with no node = %v, want InvalidArgument", err)
	}
}

// Reserving nodes takes capacity from everyone, so it is an admin action; and a window that has
// already closed would take effect on nothing.
func TestCreateReservationValidatesTheWindow(t *testing.T) {
	m := newTestMaster(t)

	_, err := m.CreateReservation(userCtx("alice"), &pb.CreateReservationRequest{
		Nodes: []string{"node-1"}, EndTime: 1 << 40,
	})
	if codeOf(err) != codes.PermissionDenied {
		t.Fatalf("reserve as a user = %v, want PermissionDenied", err)
	}

	_, err = m.CreateReservation(adminCtx(), &pb.CreateReservationRequest{EndTime: 1 << 40})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("reserve with no nodes = %v, want InvalidArgument", err)
	}

	_, err = m.CreateReservation(adminCtx(), &pb.CreateReservationRequest{
		Nodes: []string{"node-1"}, StartTime: 2000, EndTime: 1000,
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("reserve ending before it starts = %v, want InvalidArgument", err)
	}

	_, err = m.CreateReservation(adminCtx(), &pb.CreateReservationRequest{
		Nodes: []string{"node-1"}, StartTime: 1000, EndTime: 2000,
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("reserve in the past = %v, want InvalidArgument", err)
	}
}
