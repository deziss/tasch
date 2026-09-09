package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/auth"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/ha"
	"github.com/deziss/tasch/internal/policy"
	"github.com/deziss/tasch/pkg/matchmaker"
	"github.com/deziss/tasch/pkg/scheduler"
	"github.com/hashicorp/memberlist"
)

// A schedulerServer wired up enough to exercise the request handlers and the scheduling phases.
//
// The master is built by StartMaster, which binds ports, joins a gossip cluster and opens a
// database — none of which the logic under test needs, and all of which makes a test depend on
// the machine it runs on. This assembles the same struct from its parts instead, so the tests
// below drive real handlers against real queue, policy and reservation state.

// testMaster is a server plus the pieces a test needs to reach into.
type testMaster struct {
	*schedulerServer
	bus *dispatchBus

	// nodes is the cluster membership the server sees, so a test can present exactly the
	// hardware it means to reason about.
	nodes []*memberlist.Node
}

// withNodes makes the server see the given members.
func (m *testMaster) withNodes(members ...*memberlist.Node) *testMaster {
	m.nodes = members
	return m
}

type testMasterOption func(*config.Config)

func newTestMaster(t *testing.T, opts ...testMasterOption) *testMaster {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.NodeName = "test-master"
	cfg.MaxRetries = 3
	for _, opt := range opts {
		opt(cfg)
	}

	eval, err := matchmaker.NewEvaluator()
	if err != nil {
		t.Fatalf("evaluator: %v", err)
	}
	pol, err := policy.New(cfg, eval)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	queue := scheduler.NewGlobalScheduler()
	queue.MaxQueueSize = cfg.MaxQueueSize
	fairshare := scheduler.NewFairshareCalculator()
	cordons := ha.NewCordons()
	reservations := ha.NewReservations()
	bus := newDispatchBus()

	draining := &atomic.Bool{}
	srv := &schedulerServer{
		queue:           queue,
		eval:            eval,
		bus:             bus,
		fairshare:       fairshare,
		cfg:             cfg,
		draining:        draining,
		ctx:             context.Background(),
		cb:              newCircuitBreaker(),
		gpuTracker:      newGPUTracker(),
		state:           ha.NewDirect(queue, fairshare, cordons, reservations),
		cordons:         cordons,
		reservations:    reservations,
		policy:          pol,
		adopted:         make(map[string]bool),
		dispatchPending: make(map[string]time.Time),
		logStore:        make(map[string][]*pb.LogMessage),
		logChannels:     make(map[string][]chan *pb.LogMessage),
	}

	tm := &testMaster{schedulerServer: srv, bus: bus}
	srv.members = func() []*memberlist.Node { return tm.nodes }
	return tm
}

// adminCtx is a context carrying an admin principal, which is what the CLI's own token grants
// and what the handlers requiring elevated rights check for.
func adminCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Name: "root", Role: auth.RoleAdmin})
}

// userCtx is a context for an ordinary named user, used to prove that ownership is enforced.
func userCtx(name string) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Name: name, Role: auth.RoleUser})
}

// member builds a gossip member advertising the given class ad, as the matchmaker sees it.
func member(name string, ad string) *memberlist.Node {
	return &memberlist.Node{Name: name, Meta: []byte(ad)}
}

// nodeAd is a class ad for a node with the given capacity.
func nodeAd(cores, memoryMB, gpus int) string {
	return `{"os":"linux","architecture":"amd64","cpu_cores":` + itoa(cores) +
		`,"total_memory_mb":` + itoa(memoryMB) +
		`,"gpu_count":` + itoa(gpus) + `,"gpu_vendor":"nvidia"}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// submit puts a job through the real SubmitJob handler and returns its ID.
func (m *testMaster) submit(t *testing.T, ctx context.Context, req *pb.SubmitJobRequest) string {
	t.Helper()
	resp, err := m.SubmitJob(ctx, req)
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	return resp.JobId
}

// startJob moves a queued job to RUNNING on a node, as a dispatch would, and returns the job as
// it now stands.
//
// The attempt comes from the Dispatch call rather than from the returned job: Dispatch hands
// back the record as it was when it left the queue, and the attempt is incremented by the
// transition to RUNNING that follows. That is the same pair the real dispatch path threads
// through, and the attempt is the fencing token every later result is checked against.
func (m *testMaster) startJob(t *testing.T, jobID, node string) *scheduler.Job {
	t.Helper()
	_, attempt, ok := m.state.Dispatch(jobID, node)
	if !ok {
		t.Fatalf("could not dispatch %s", jobID)
	}
	running, found := m.queue.GetJob(jobID)
	if !found {
		t.Fatalf("job %s vanished on dispatch", jobID)
	}
	running.Attempt = attempt
	return running
}
