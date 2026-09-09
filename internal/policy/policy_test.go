package policy

import (
	"errors"
	"strings"
	"testing"

	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/pkg/matchmaker"
	"github.com/deziss/tasch/pkg/scheduler"
)

// research → ml-team and vision-team. The department's ceiling is what the teams share.
func testConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Accounts = []config.AccountConfig{
		{Name: "research", MaxGPUs: 8, MaxRunningJobs: 10},
		{Name: "ml-team", Parent: "research", Users: []string{"alice"}, MaxGPUs: 6, MaxQueuedJobs: 2},
		{Name: "vision-team", Parent: "research", Users: []string{"bob"}, MaxGPUs: 6},
	}
	cfg.Partitions = []config.PartitionConfig{
		{Name: "gpu", NodeSelector: "ad.gpu_count > 0", MaxWalltimeSeconds: 3600,
			DefaultWalltimeSeconds: 600, PriorityBoost: -5, AllowedAccounts: []string{"research"}},
		{Name: "cpu", NodeSelector: "ad.gpu_count == 0", Default: true, MaxRunningJobs: 2},
	}
	return cfg
}

func newTestPolicy(t *testing.T) *Policy {
	t.Helper()
	eval, err := matchmaker.NewEvaluator()
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	p, err := New(testConfig(), eval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func job(id, account, partition string, gpus int) *scheduler.Job {
	return &scheduler.Job{ID: id, Account: account, Partition: partition, GPUsRequired: gpus}
}

// With nothing configured the policy must be inert: this is what keeps an upgrade from
// changing where existing jobs run.
func TestEmptyPolicyAdmitsEverything(t *testing.T) {
	p, err := New(config.DefaultConfig(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Enabled() {
		t.Fatal("a config with no partitions or accounts should leave the policy inert")
	}
	if account, err := p.ResolveAccount("anyone", ""); err != nil || account != "" {
		t.Fatalf("ResolveAccount = %q, %v; want empty and no error", account, err)
	}
	part, err := p.ResolvePartition("", "anyone", "")
	if err != nil || part != nil {
		t.Fatalf("ResolvePartition = %v, %v; want nil and no error", part, err)
	}
	if ok, reason := p.AdmitDispatch(job("j", "", "", 8), nil, nil); !ok {
		t.Fatalf("AdmitDispatch denied with no quotas configured: %s", reason)
	}
}

func TestResolveAccount(t *testing.T) {
	p := newTestPolicy(t)

	if got, err := p.ResolveAccount("alice", ""); err != nil || got != "ml-team" {
		t.Fatalf("alice's default account = %q, %v; want ml-team", got, err)
	}
	if got, err := p.ResolveAccount("alice", "ml-team"); err != nil || got != "ml-team" {
		t.Fatalf("explicit own account = %q, %v", got, err)
	}
	// Without this check quotas are advisory: anyone out of budget would just name a fuller one.
	_, err := p.ResolveAccount("alice", "vision-team")
	if err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("naming another team's account = %v, want a membership error", err)
	}
	if _, err := p.ResolveAccount("alice", "nope"); err == nil {
		t.Fatal("an unknown account should be rejected")
	}
	if got, err := p.ResolveAccount("stranger", ""); err != nil || got != "" {
		t.Fatalf("a user in no account = %q, %v; want empty", got, err)
	}
}

// The point of nesting: work inside a team counts against the department, so two teams cannot
// between them exceed what the department was given.
func TestQuotaRollsUpToTheParent(t *testing.T) {
	p := newTestPolicy(t)

	running := []*scheduler.Job{
		job("a", "ml-team", "gpu", 4),
		job("b", "vision-team", "gpu", 3),
	}
	usage := p.AccountUsage(running)

	if usage["research"].GPUs != 7 {
		t.Fatalf("research usage = %d GPUs, want 7 (both teams roll up)", usage["research"].GPUs)
	}
	if usage["ml-team"].GPUs != 4 {
		t.Fatalf("ml-team usage = %d GPUs, want 4", usage["ml-team"].GPUs)
	}

	// ml-team is at 4 of its own 6, so its own quota would allow 2 more — but research has only
	// 1 GPU left, and that is the limit that must bind.
	ok, reason := p.AdmitDispatch(job("c", "ml-team", "gpu", 2), usage, nil)
	if ok {
		t.Fatal("a job fitting the team's quota but not the department's was admitted")
	}
	if !strings.Contains(reason, "research") {
		t.Fatalf("reason %q should name the account that ran out", reason)
	}

	// One GPU still fits both ceilings.
	if ok, reason := p.AdmitDispatch(job("d", "ml-team", "gpu", 1), usage, nil); !ok {
		t.Fatalf("a job inside both quotas was denied: %s", reason)
	}
}

func TestPartitionRunningLimit(t *testing.T) {
	p := newTestPolicy(t)
	running := map[string]int{"cpu": 2}

	ok, reason := p.AdmitDispatch(job("j", "", "cpu", 0), nil, running)
	if ok {
		t.Fatal("the cpu partition is at its limit of 2 and should not admit a third job")
	}
	if !strings.Contains(reason, "cpu") {
		t.Fatalf("reason %q should name the partition", reason)
	}
}

func TestPartitionAccessInheritsFromTheParentAccount(t *testing.T) {
	p := newTestPolicy(t)

	// The gpu partition allows the "research" account; ml-team is under it and must inherit,
	// or granting a department access would mean re-listing every team inside it.
	if _, err := p.ResolvePartition("gpu", "alice", "ml-team"); err != nil {
		t.Fatalf("ml-team should inherit research's access to gpu: %v", err)
	}
	_, err := p.ResolvePartition("gpu", "stranger", "")
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("an account outside research should be refused the gpu partition, got %v", err)
	}
}

func TestUnnamedJobTakesTheDefaultPartition(t *testing.T) {
	p := newTestPolicy(t)
	part, err := p.ResolvePartition("", "alice", "ml-team")
	if err != nil {
		t.Fatalf("ResolvePartition: %v", err)
	}
	if part == nil || part.Name != "cpu" {
		t.Fatalf("default partition = %v, want cpu", part)
	}
	if _, err := p.ResolvePartition("ghost", "alice", "ml-team"); err == nil {
		t.Fatal("an unknown partition should be rejected at submit, not at dispatch")
	}
}

func TestPartitionWalltimeAndPriority(t *testing.T) {
	p := newTestPolicy(t)
	gpu, err := p.ResolvePartition("gpu", "alice", "ml-team")
	if err != nil {
		t.Fatalf("ResolvePartition: %v", err)
	}

	// A job asking for nothing takes the partition's default rather than running forever.
	walltime, priority, err := ApplyPartitionDefaults(gpu, 0, 10)
	if err != nil {
		t.Fatalf("ApplyPartitionDefaults: %v", err)
	}
	if walltime != 600 {
		t.Fatalf("walltime = %d, want the partition default of 600", walltime)
	}
	if priority != 5 {
		t.Fatalf("priority = %d, want 10 with the -5 boost applied", priority)
	}

	// Asking for more than the ceiling is refused at submit, where the user can see it.
	if _, _, err := ApplyPartitionDefaults(gpu, 7200, 10); err == nil {
		t.Fatal("a walltime above the partition ceiling should be rejected")
	}

	// A ceiling with no default still has to bound the job.
	ceilingOnly := &config.PartitionConfig{Name: "p", MaxWalltimeSeconds: 100}
	if walltime, _, err := ApplyPartitionDefaults(ceilingOnly, 0, 10); err != nil || walltime != 100 {
		t.Fatalf("walltime = %d, %v; want the ceiling applied as the default", walltime, err)
	}
}

func TestMatchesPartition(t *testing.T) {
	p := newTestPolicy(t)

	gpuNode := `{"gpu_count": 4}`
	cpuNode := `{"gpu_count": 0}`

	if !p.MatchesPartition("gpu", gpuNode) {
		t.Fatal("a GPU node should belong to the gpu partition")
	}
	if p.MatchesPartition("gpu", cpuNode) {
		t.Fatal("a CPU node must not be offered to the gpu partition")
	}
	if !p.MatchesPartition("", cpuNode) {
		t.Fatal("a job with no partition should match any node")
	}
	// A partition deleted from the config while a job that named it was queued: refusing every
	// node is right, because widening it to the whole cluster would put the job exactly where
	// the operator had decided it should not go.
	if p.MatchesPartition("deleted", gpuNode) {
		t.Fatal("a job naming a partition that no longer exists must not match every node")
	}
}

// A runaway loop should be stopped at the door rather than after it has filled the queue for
// everyone else.
func TestQueuedQuotaIsCheckedAtSubmit(t *testing.T) {
	p := newTestPolicy(t)

	queued := p.AccountUsage([]*scheduler.Job{
		job("q1", "ml-team", "cpu", 0),
		job("q2", "ml-team", "cpu", 0),
	})
	if err := p.AdmitQueued("ml-team", queued); err == nil {
		t.Fatal("ml-team is at its queued limit of 2 and should not accept a third")
	}
	if err := p.AdmitQueued("vision-team", queued); err != nil {
		t.Fatalf("vision-team has no queued limit and should be admitted: %v", err)
	}
}

// An invalid node selector must fail the start. Left alone it matches nothing, silently, on
// every scheduling cycle forever — the same failure an invalid job requirement used to have.
func TestInvalidNodeSelectorFailsStartup(t *testing.T) {
	eval, err := matchmaker.NewEvaluator()
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Partitions = []config.PartitionConfig{{Name: "bad", NodeSelector: "this is not CEL"}}

	if _, err := New(cfg, eval); err == nil {
		t.Fatal("an invalid node_selector should be refused at startup")
	}
}

// "There is no such partition" and "you may not use that partition" are different answers, and
// the caller turns them into different gRPC codes. Conflating them tells a user to fix a name
// that was never wrong.
func TestAccessRefusalsAreDistinguishable(t *testing.T) {
	p := newTestPolicy(t)

	var access AccessDenied

	_, err := p.ResolvePartition("ghost", "alice", "ml-team")
	if err == nil {
		t.Fatal("an unknown partition should be an error")
	}
	if errors.As(err, &access) {
		t.Fatalf("a nonexistent partition is a malformed request, not a permission problem: %v", err)
	}

	_, err = p.ResolvePartition("gpu", "stranger", "outsiders")
	if !errors.As(err, &access) {
		t.Fatalf("being refused a partition should be a permission problem, got %v", err)
	}

	_, err = p.ResolveAccount("alice", "nope")
	if errors.As(err, &access) {
		t.Fatalf("a nonexistent account is a malformed request, not a permission problem: %v", err)
	}

	_, err = p.ResolveAccount("alice", "vision-team")
	if !errors.As(err, &access) {
		t.Fatalf("submitting to another team's account should be a permission problem, got %v", err)
	}
}
