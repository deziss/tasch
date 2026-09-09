package daemon

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/auth"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/ha"
	"github.com/deziss/tasch/internal/logging"
	"github.com/deziss/tasch/internal/policy"
	"github.com/deziss/tasch/internal/store"
	"github.com/deziss/tasch/pkg/discovery"
	"github.com/deziss/tasch/pkg/matchmaker"
	"github.com/deziss/tasch/pkg/scheduler"
	"github.com/google/uuid"
	"github.com/hashicorp/memberlist"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// MasterHandle is returned by StartMaster for shutdown orchestration.
type MasterHandle struct {
	Cancel   func()
	Draining *atomic.Bool
	Queue    *scheduler.GlobalScheduler
}

type schedulerServer struct {
	pb.UnimplementedSchedulerServiceServer
	disc      *discovery.NodeDiscovery
	queue     *scheduler.GlobalScheduler
	eval      *matchmaker.Evaluator
	bus       *dispatchBus
	fairshare *scheduler.FairshareCalculator
	store     *store.Store
	cfg       *config.Config
	draining  *atomic.Bool
	ctx       context.Context

	// Circuit breaker
	cb *circuitBreaker

	// GPU resource tracking
	gpuTracker *gpuTracker

	// state applies scheduler mutations, either locally or through replication.
	state ha.Store

	// raftNode is non-nil only when HA is enabled, for leadership reporting.
	raftNode *ha.Node

	// Nodes an operator has taken out of scheduling rotation.
	cordons *ha.Cordons

	// Nodes held aside for a window of time.
	reservations *ha.Reservations

	// policy resolves partitions and account quotas. It is never nil; with neither configured
	// it admits everything, which is the behaviour that predates it.
	policy *policy.Policy

	// Jobs a worker has claimed since this master started, used to decide which jobs left
	// RUNNING by a restart were genuinely lost.
	adoptedMu sync.Mutex
	adopted   map[string]bool

	// Dispatch acknowledgement tracking
	dispatchPendingMu sync.Mutex
	dispatchPending   map[string]time.Time

	// lastTick is the UnixNano of the most recent scheduling cycle, read by /health.
	lastTick atomic.Int64

	logMu       sync.Mutex
	logStore    map[string][]*pb.LogMessage
	subMu       sync.Mutex
	logChannels map[string][]chan *pb.LogMessage
}

// --- Circuit Breaker ---

type circuitBreaker struct {
	mu            sync.Mutex
	failures      map[string]int       // node → consecutive failures
	blocked       map[string]time.Time // node → blocked until
	lastFailedJob map[string]string    // node → last failed job ID
}

func newCircuitBreaker() *circuitBreaker {
	return &circuitBreaker{
		failures:      make(map[string]int),
		blocked:       make(map[string]time.Time),
		lastFailedJob: make(map[string]string),
	}
}

func (cb *circuitBreaker) RecordFailure(node string, jobID string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// If this is the same job that failed previously on this node, do not increment failures
	if cb.lastFailedJob[node] == jobID {
		return
	}
	cb.lastFailedJob[node] = jobID

	cb.failures[node]++
	if cb.failures[node] >= circuitBreakerThreshold {
		cb.blocked[node] = time.Now().Add(circuitBreakerBlockDuration)
		slog.Warn("node blocked by circuit breaker",
			"node", node, "consecutive_failures", cb.failures[node], "duration", circuitBreakerBlockDuration)
	}
}

func (cb *circuitBreaker) RecordSuccess(node string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.failures, node)
	delete(cb.blocked, node)
	delete(cb.lastFailedJob, node)
}

func (cb *circuitBreaker) IsBlocked(node string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	until, ok := cb.blocked[node]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(cb.blocked, node)
		delete(cb.failures, node)
		delete(cb.lastFailedJob, node)
		return false
	}
	return true
}

// jobIDBytes is the entropy in a job ID, in bytes.
//
// IDs were an 8-character UUID prefix — 32 bits, which by the birthday bound collides with
// ~50% probability at only ~77k jobs, and nothing ever evicts a job from memory or from the
// database. 64 bits pushes that to billions of jobs while keeping the ID short enough to
// retype from a terminal. Enqueue rejects a duplicate outright, so a collision fails loudly
// instead of overwriting another job.
const jobIDBytes = 8

// newJobID returns a random 16-character hex job identifier.
func newJobID() string {
	b := make([]byte, jobIDBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable here, and a predictable ID would be worse
		// than none: fall back to a UUID, which is still unique.
		return strings.ReplaceAll(uuid.New().String(), "-", "")[:jobIDBytes*2]
	}
	return hex.EncodeToString(b)
}

// Circuit breaker tuning. These were bare literals at the point of use.
const (
	circuitBreakerThreshold     = 3
	circuitBreakerBlockDuration = 5 * time.Minute
)

// --- GPU Resource Tracker ---

// allocation records what one job holds on one node, including the physical GPU indices it
// was given.
type allocation struct {
	node    string
	devices []int // physical GPU indices, e.g. [2 3]
	cpus    int
	memMB   int
}

// nodeState is the per-node view of what is currently in use.
type nodeState struct {
	devices map[int]string // GPU index → job ID holding it
	cpus    int
	memMB   int
}

// gpuTracker tracks which resources each running job holds.
//
// Allocations are keyed by job ID rather than accumulated into per-node counters. That is what
// makes Release idempotent: releasing an unknown job is a no-op, so the several paths that can
// each end a job — completion, cancel, walltime kill, gang-sibling cancel, dispatch timeout,
// worker loss — can no longer double-release and drive a node's usage to zero while its jobs
// are still running.
//
// GPUs are tracked as individual device indices, not a count, so two jobs on the same node
// receive different physical devices.
type gpuTracker struct {
	mu    sync.Mutex
	nodes map[string]*nodeState
	byJob map[string]*allocation
}

func newGPUTracker() *gpuTracker {
	return &gpuTracker{
		nodes: make(map[string]*nodeState),
		byJob: make(map[string]*allocation),
	}
}

// nodeLocked returns the node's state, creating it if absent. Caller must hold gt.mu.
func (gt *gpuTracker) nodeLocked(node string) *nodeState {
	ns, ok := gt.nodes[node]
	if !ok {
		ns = &nodeState{devices: make(map[int]string)}
		gt.nodes[node] = ns
	}
	return ns
}

// Allocate reserves resources for jobID on node and returns the GPU indices assigned to it.
//
// totalGPUs is the node's physical GPU count, needed to pick free device indices. Allocating a
// job that already holds resources returns its existing devices unchanged, so a re-dispatch
// cannot double-book. Returns false if the node cannot satisfy the GPU request.
func (gt *gpuTracker) Allocate(node, jobID string, gpus, cpus, memMB, totalGPUs int) ([]int, bool) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	if existing, ok := gt.byJob[jobID]; ok {
		return append([]int(nil), existing.devices...), true
	}

	ns := gt.nodeLocked(node)

	devices := make([]int, 0, gpus)
	for idx := 0; idx < totalGPUs && len(devices) < gpus; idx++ {
		if _, taken := ns.devices[idx]; !taken {
			devices = append(devices, idx)
		}
	}
	if len(devices) < gpus {
		if ns.cpus == 0 && ns.memMB == 0 && len(ns.devices) == 0 {
			delete(gt.nodes, node)
		}
		return nil, false
	}

	for _, idx := range devices {
		ns.devices[idx] = jobID
	}
	ns.cpus += cpus
	ns.memMB += memMB

	gt.byJob[jobID] = &allocation{node: node, devices: devices, cpus: cpus, memMB: memMB}
	return append([]int(nil), devices...), true
}

// Release frees everything jobID holds. Releasing a job that holds nothing is a no-op, which
// is what makes the several concurrent end-of-job paths safe to call unconditionally.
func (gt *gpuTracker) Release(jobID string) {
	gt.mu.Lock()
	defer gt.mu.Unlock()

	alloc, ok := gt.byJob[jobID]
	if !ok {
		return
	}
	delete(gt.byJob, jobID)

	ns, ok := gt.nodes[alloc.node]
	if !ok {
		return
	}
	for _, idx := range alloc.devices {
		if holder, taken := ns.devices[idx]; taken && holder == jobID {
			delete(ns.devices, idx)
		}
	}
	ns.cpus -= alloc.cpus
	if ns.cpus < 0 {
		ns.cpus = 0
	}
	ns.memMB -= alloc.memMB
	if ns.memMB < 0 {
		ns.memMB = 0
	}
	if len(ns.devices) == 0 && ns.cpus == 0 && ns.memMB == 0 {
		delete(gt.nodes, alloc.node)
	}
}

// Reconcile drops allocations for jobs that are no longer running and books any running job
// that has none, returning the counts of each.
//
// The tracker is in-memory only: a master restart resets it to empty while workers keep running
// their jobs, so the new master believes every node is idle and oversubscribes it. Nothing
// rebuilt the tracker from the authoritative set of running jobs, so any drift — from a restart
// or from a missed release — was permanent.
//
// totalGPUs reports a node's physical GPU count, used when re-booking a job whose device
// assignment was lost.
func (gt *gpuTracker) Reconcile(running []*scheduler.Job, totalGPUs func(node string) int) (dropped, rebooked int) {
	live := make(map[string]*scheduler.Job, len(running))
	for _, job := range running {
		if job.WorkerNode != "" {
			live[job.ID] = job
		}
	}

	gt.mu.Lock()
	stale := make([]string, 0)
	for jobID := range gt.byJob {
		if _, ok := live[jobID]; !ok {
			stale = append(stale, jobID)
		}
	}
	missing := make([]*scheduler.Job, 0)
	for jobID, job := range live {
		if _, ok := gt.byJob[jobID]; !ok {
			missing = append(missing, job)
		}
	}
	gt.mu.Unlock()

	for _, jobID := range stale {
		gt.Release(jobID)
		dropped++
	}
	for _, job := range missing {
		if _, ok := gt.Allocate(job.WorkerNode, job.ID, job.GPUsRequired, job.CPUsRequired, job.MemoryRequiredMB, totalGPUs(job.WorkerNode)); ok {
			rebooked++
		}
	}
	return dropped, rebooked
}

// UsageByNode returns the number of GPUs currently allocated on each node.
func (gt *gpuTracker) UsageByNode() map[string]int {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	out := make(map[string]int, len(gt.nodes))
	for node, ns := range gt.nodes {
		out[node] = len(ns.devices)
	}
	return out
}

// DevicesFor returns the GPU indices held by jobID, or nil.
func (gt *gpuTracker) DevicesFor(jobID string) []int {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	alloc, ok := gt.byJob[jobID]
	if !ok {
		return nil
	}
	return append([]int(nil), alloc.devices...)
}

func (gt *gpuTracker) AvailableGPUs(node string, total int) int {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if ns, ok := gt.nodes[node]; ok {
		return total - len(ns.devices)
	}
	return total
}

func (gt *gpuTracker) AvailableCPUs(node string, total int) int {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if ns, ok := gt.nodes[node]; ok {
		return total - ns.cpus
	}
	return total
}

func (gt *gpuTracker) AvailableMemory(node string, total int) int {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if ns, ok := gt.nodes[node]; ok {
		return total - ns.memMB
	}
	return total
}

// --- Log helpers ---

// maxLogEntriesPerJob bounds the in-memory log ring for a single job. The log store was
// unbounded and never pruned, so every line of every job ever submitted stayed in master
// memory for the process lifetime.
const maxLogEntriesPerJob = 500

func (s *schedulerServer) appendLog(jobID, level, msg string) {
	entry := &pb.LogMessage{
		Timestamp: time.Now().UnixMilli(), Level: level, Message: msg, JobId: jobID,
	}
	s.logMu.Lock()
	entries := append(s.logStore[jobID], entry)
	if len(entries) > maxLogEntriesPerJob {
		// Keep the most recent window; the oldest lines are the least useful for diagnosis.
		entries = entries[len(entries)-maxLogEntriesPerJob:]
	}
	s.logStore[jobID] = entries
	s.logMu.Unlock()

	s.subMu.Lock()
	for _, ch := range s.logChannels[jobID] {
		select {
		case ch <- entry:
		default:
		}
	}
	s.subMu.Unlock()
}

// --- gRPC Handlers ---

func (s *schedulerServer) WorkerStatus(ctx context.Context, req *pb.WorkerStatusRequest) (*pb.WorkerStatusResponse, error) {
	members := s.disc.Members()
	nodes := make(map[string]string)
	states := make(map[string]*pb.NodeSchedulingState)
	usage := s.gpuTracker.UsageByNode()

	for _, member := range members {
		if len(member.Meta) == 0 {
			continue
		}
		nodes[member.Name] = string(member.Meta)
		// Report why a node is or is not taking work, so an idle cluster with a full queue is
		// diagnosable without reading the master's logs.
		states[member.Name] = &pb.NodeSchedulingState{
			Cordoned:      s.cordons.IsCordoned(member.Name),
			CordonReason:  s.cordons.Reason(member.Name),
			CircuitBroken: s.cb.IsBlocked(member.Name),
			RunningJobs:   int32(len(s.queue.RunningJobsOnNode(member.Name))),
			GpusAllocated: int32(usage[member.Name]),
		}
	}
	return &pb.WorkerStatusResponse{WorkerNodes: nodes, NodeState: states}, nil
}

// Compiled once rather than on every submit.
var (
	celCPURegex = regexp.MustCompile(`cpu_cores\s*(?:>=|==|>)\s*(\d+)`)
	celMemRegex = regexp.MustCompile(`total_memory_mb\s*(?:>=|==|>)\s*(\d+)`)
)

// adoptionGracePeriod is how long a restarted master waits for workers to reconnect and claim
// the jobs they are still running before declaring those jobs lost.
const adoptionGracePeriod = 90 * time.Second

// schedulerStallThreshold is how long the dispatch loop may go without a tick before /health
// reports the master unhealthy. The loop ticks once a second.
const schedulerStallThreshold = 30 * time.Second

// fairshareInterval is how often usage decays and queued jobs are reprioritised.
const fairshareInterval = 60 * time.Second

// Page sizing for list responses. The response used to carry every job in one message, which
// is an unbounded allocation on the master and can exceed gRPC's receive limit outright.
const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// paginate returns one page of jobs and the token for the next, if any.
//
// The token is the offset. That is simple and adequate here: the ordering is stable, and a job
// submitted between two calls shifts the window by one rather than corrupting it. A cursor keyed
// on the last item would be sturdier if the ordering ever becomes user-selectable.
func paginate(jobs []*scheduler.Job, token string, size int) ([]*scheduler.Job, string, error) {
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}

	offset := 0
	if token != "" {
		parsed, err := strconv.Atoi(token)
		if err != nil || parsed < 0 {
			return nil, "", status.Errorf(codes.InvalidArgument, "invalid page_token %q", token)
		}
		offset = parsed
	}
	if offset >= len(jobs) {
		return nil, "", nil
	}

	end := offset + size
	if end >= len(jobs) {
		return jobs[offset:], "", nil
	}
	return jobs[offset:end], strconv.Itoa(end), nil
}

// maxGroupNodes caps a distributed job's rank count. num_nodes was only clamped upward, so a
// single request could create an unbounded number of jobs and log entries.
const maxGroupNodes = 1024

// principalUser returns the accounting identity for a request: the authenticated principal when
// auth is on, otherwise the client-supplied name.
func principalUser(ctx context.Context, requested string) string {
	if p := auth.FromContext(ctx); p != auth.Anonymous {
		return p.Name
	}
	if requested == "" {
		return "anonymous"
	}
	return requested
}

// authorizeJob resolves a job and confirms the caller may act on it.
//
// A caller that is not the owner gets the same "not found" it would get for a job ID that does
// not exist, so the API cannot be used to enumerate other principals' job IDs.
func (s *schedulerServer) authorizeJob(ctx context.Context, jobID string) (*scheduler.Job, error) {
	job, ok := s.queue.GetJob(jobID)
	if !ok {
		return nil, auth.ErrJobForbidden(jobID)
	}
	if !auth.CanAccessJob(auth.FromContext(ctx), job.User) {
		return nil, auth.ErrJobForbidden(jobID)
	}
	return job, nil
}

// resourceRequest determines what a job reserves.
//
// Explicit values win. When a client sends none, the reservation falls back to scraping the
// CEL requirement, which is how it always worked and is kept for older clients — but that
// scrape only recognises a literal `cpu_cores >= N` shape, so `8 <= ad.cpu_cores`,
// `ad.cpu_cores in [8,16]`, or any disjunction reserved nothing at all and let jobs pack onto
// a node without limit. Prefer the explicit fields.
func resourceRequest(celRequirement string, explicitCPUs, explicitMemMB int) (cpus, memMB int) {
	inferredCPUs, inferredMem := parseCELRequirement(celRequirement)
	cpus, memMB = explicitCPUs, explicitMemMB
	if cpus <= 0 {
		cpus = inferredCPUs
	}
	if memMB <= 0 {
		memMB = inferredMem
	}
	return cpus, memMB
}

func (s *schedulerServer) SubmitJob(ctx context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	if s.draining.Load() {
		return nil, status.Error(codes.Unavailable, "master is draining, not accepting new jobs")
	}

	// Validate the requirement now. An expression that fails to compile used to be accepted,
	// then silently failed to match anything on every scheduling cycle forever — the job sat
	// QUEUED with no error, no log line, and no way for the submitter to find out why.
	if err := s.eval.Validate(req.CelRequirement); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid cel_requirement: %v", err)
	}

	jobID := newJobID()
	priority := int(req.Priority)
	if priority == 0 {
		priority = 10
	}
	// Identity comes from the authenticated principal, never from the request. The --user flag
	// was unverified, so a client could evade its fairshare penalty by inventing a new name on
	// every submit, inflate another user's usage, or blow up Prometheus label cardinality with
	// unbounded values. When auth is disabled the principal is anonymous and the flag is still
	// honoured, preserving the previous behaviour.
	user := principalUser(ctx, req.User)
	penalty := s.fairshare.CalculatePenalty(user)
	effectivePriority := priority + penalty

	// Resolve which pool this job belongs to and which account pays for it, before anything
	// else looks at the job. Both are settled once, at submit: a job must not change partition
	// or account while it waits because someone edited the configuration underneath it.
	account, err := s.policy.ResolveAccount(user, req.Account)
	if err != nil {
		return nil, policyError(err)
	}
	partition, err := s.policy.ResolvePartition(req.Partition, user, account)
	if err != nil {
		return nil, policyError(err)
	}
	walltime, effectivePriority, err := policy.ApplyPartitionDefaults(
		partition, int(req.WalltimeSeconds), effectivePriority)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.policy.AdmitQueued(account, s.policy.AccountUsage(s.queue.QueuedJobs())); err != nil {
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}
	partitionName := ""
	if partition != nil {
		partitionName = partition.Name
	}

	// Every dependency must already exist. Catching a typo here rather than leaving the job
	// queued forever is the immediate benefit; the structural one is that a job can only ever
	// depend on something submitted before it, so a cycle cannot be expressed and nothing has to
	// go looking for one.
	for _, depID := range req.DependsOn {
		if !s.queue.KnownJob(depID) {
			return nil, status.Errorf(codes.InvalidArgument,
				"depends_on names job %s, which does not exist; a dependency must be submitted first", depID)
		}
	}
	mode, err := normalizeDependencyMode(req.DependencyMode)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cpus, mem := resourceRequest(req.CelRequirement, int(req.CpusRequired), int(req.MemoryRequiredMb))
	newJob := func(id string) *scheduler.Job {
		return &scheduler.Job{
			ID: id, Requirement: req.CelRequirement, Command: req.Command,
			SubmitTime: time.Now(), Priority: effectivePriority, User: user,
			WalltimeSeconds: walltime, GPUsRequired: int(req.GpusRequired),
			CPUsRequired: cpus, MemoryRequiredMB: mem, BasePriority: priority,
			EnvVars: req.EnvVars, MaxRetries: s.cfg.MaxRetries,
			DependsOn: req.DependsOn, DependencyMode: mode,
			Partition: partitionName, Account: account,
		}
	}

	if req.Array != "" {
		return s.submitArray(req, newJob, user, effectivePriority)
	}

	job := newJob(jobID)
	if err := s.state.Enqueue(job); err != nil {
		var notLeader ha.ErrNotLeader
		if errors.As(err, &notLeader) {
			return nil, leaderRedirect(s, err)
		}
		return nil, status.Errorf(codes.ResourceExhausted, "queue full: %v", err)
	}

	jobsSubmittedTotal.WithLabelValues(user).Inc()
	s.appendLog(jobID, "INFO", fmt.Sprintf("Job queued | Priority: %d | GPUs: %d | Retries: %d | Expr: '%s'", effectivePriority, req.GpusRequired, s.cfg.MaxRetries, req.CelRequirement))
	if len(req.DependsOn) > 0 {
		s.appendLog(jobID, "INFO", fmt.Sprintf("Waiting on %s (%s)", strings.Join(req.DependsOn, ", "), mode))
	}
	logging.Job(jobID).Info("queued", "user", user, "priority", effectivePriority, "gpus", req.GpusRequired)
	return &pb.SubmitJobResponse{JobId: jobID, Status: "QUEUED", JobIds: []string{jobID}}, nil
}

// policyError maps a policy refusal onto the right gRPC code: naming something that does not
// exist is a malformed request, while naming something you may not use is not.
func policyError(err error) error {
	var access policy.AccessDenied
	if errors.As(err, &access) {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

// normalizeDependencyMode validates the requested mode and supplies the default.
func normalizeDependencyMode(mode string) (string, error) {
	switch mode {
	case "":
		return scheduler.DependAfterOK, nil
	case scheduler.DependAfterOK, scheduler.DependAfterAny, scheduler.DependAfterNotOK:
		return mode, nil
	default:
		return "", fmt.Errorf("dependency_mode must be %q, %q or %q (got %q)",
			scheduler.DependAfterOK, scheduler.DependAfterAny, scheduler.DependAfterNotOK, mode)
	}
}

// submitArray expands an array specification into one job per index and enqueues them together.
//
// The enqueue is one operation on purpose. Half an array is worse than none: the user cannot
// ask for "the rest" without first working out which rest, while the tasks that did land are
// already occupying the cluster.
func (s *schedulerServer) submitArray(req *pb.SubmitJobRequest, newJob func(string) *scheduler.Job,
	user string, effectivePriority int) (*pb.SubmitJobResponse, error) {

	spec, err := parseArraySpec(req.Array)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid array: %v", err)
	}

	arrayID := newJobID()
	jobs := make([]*scheduler.Job, 0, len(spec.Indices))
	ids := make([]string, 0, len(spec.Indices))
	for _, index := range spec.Indices {
		job := newJob(newJobID())
		job.ArrayID = arrayID
		job.ArrayIndex = index
		job.ArrayMaxConcurrent = spec.MaxConcurrent
		// Each task needs its own environment map: they share a template, but the index is what
		// distinguishes them, and a shared map would give every task the last one written.
		job.EnvVars = arrayTaskEnv(req.EnvVars, arrayID, index, len(spec.Indices))
		jobs = append(jobs, job)
		ids = append(ids, job.ID)
	}

	if err := s.state.EnqueueBatch(jobs); err != nil {
		var notLeader ha.ErrNotLeader
		if errors.As(err, &notLeader) {
			return nil, leaderRedirect(s, err)
		}
		return nil, status.Errorf(codes.ResourceExhausted, "cannot queue array: %v", err)
	}

	jobsSubmittedTotal.WithLabelValues(user).Add(float64(len(jobs)))
	for _, job := range jobs {
		s.appendLog(job.ID, "INFO", fmt.Sprintf("Array %s task %d queued | Priority: %d",
			arrayID, job.ArrayIndex, effectivePriority))
	}
	slog.Info("array queued", "array_id", arrayID, "tasks", len(jobs),
		"max_concurrent", spec.MaxConcurrent, "user", user)

	return &pb.SubmitJobResponse{
		JobId: ids[0], Status: "QUEUED", JobIds: ids, ArrayId: arrayID,
	}, nil
}

// arrayTaskEnv builds one task's environment: the submitter's variables plus the index, which
// is the only thing that distinguishes the tasks from one another.
func arrayTaskEnv(base map[string]string, arrayID string, index, count int) map[string]string {
	env := make(map[string]string, len(base)+3)
	for k, v := range base {
		env[k] = v
	}
	env["TASCH_ARRAY_ID"] = arrayID
	env["TASCH_ARRAY_TASK_ID"] = strconv.Itoa(index)
	env["TASCH_ARRAY_TASK_COUNT"] = strconv.Itoa(count)
	return env
}

func (s *schedulerServer) SubmitDistributedJob(ctx context.Context, req *pb.SubmitDistributedJobRequest) (*pb.SubmitDistributedJobResponse, error) {
	if s.draining.Load() {
		return nil, status.Error(codes.Unavailable, "master is draining")
	}

	if err := s.eval.Validate(req.CelRequirement); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid cel_requirement: %v", err)
	}

	groupID := "dj-" + newJobID()
	masterPort := int(req.MasterPort)
	if masterPort == 0 {
		masterPort = 29500
	}
	priority := int(req.Priority)
	if priority == 0 {
		priority = 10
	}
	user := principalUser(ctx, req.User)
	// Distributed jobs bypassed fairshare entirely: the penalty was applied only on the single
	// job path, so a penalised user could submit through this RPC and get full priority.
	penalty := s.fairshare.CalculatePenalty(user)
	priority += penalty
	numNodes := int(req.NumNodes)
	if numNodes < 1 {
		numNodes = 1
	}
	if numNodes > maxGroupNodes {
		return nil, status.Errorf(codes.InvalidArgument, "num_nodes %d exceeds the maximum of %d", numNodes, maxGroupNodes)
	}
	gpusPerNode := int(req.GpusPerNode)

	var jobIDs []string
	for rank := 0; rank < numNodes; rank++ {
		jobID := fmt.Sprintf("%s-r%d", groupID, rank)
		envVars := make(map[string]string)
		for k, v := range req.EnvVars {
			envVars[k] = v
		}
		envVars["RANK"] = strconv.Itoa(rank)
		envVars["WORLD_SIZE"] = strconv.Itoa(numNodes)
		envVars["MASTER_PORT"] = strconv.Itoa(masterPort)
		envVars["LOCAL_RANK"] = "0"
		envVars["NPROC_PER_NODE"] = strconv.Itoa(gpusPerNode)

		cpus, mem := resourceRequest(req.CelRequirement, 0, 0)
		job := &scheduler.Job{
			ID: jobID, GroupID: groupID, Requirement: req.CelRequirement,
			Command: req.Command, SubmitTime: time.Now(), Priority: priority,
			User: user, WalltimeSeconds: int(req.WalltimeSeconds),
			GPUsRequired: gpusPerNode, CPUsRequired: cpus, MemoryRequiredMB: mem,
			BasePriority: priority - penalty,
			EnvVars:      envVars, MaxRetries: 0, // No retry for distributed
		}
		if err := s.state.Enqueue(job); err != nil {
			return nil, status.Errorf(codes.ResourceExhausted, "queue full: %v", err)
		}
		jobIDs = append(jobIDs, jobID)
		s.appendLog(jobID, "INFO", fmt.Sprintf("Distributed job rank %d/%d queued in group %s", rank, numNodes, groupID))
	}

	if err := s.state.RegisterGroup(&scheduler.JobGroup{
		GroupID: groupID, JobIDs: jobIDs, NumNodes: numNodes,
		GPUsPerNode: gpusPerNode, MasterPort: masterPort, State: "PENDING",
		CreatedAt: time.Now(),
	}); err != nil {
		// The ranks are queued but their group is not. Without the group nothing co-schedules
		// them, so report the failure rather than leaving orphaned ranks behind.
		return nil, leaderRedirect(s, err)
	}
	jobsSubmittedTotal.WithLabelValues(user).Add(float64(numNodes))
	slog.Info("distributed job queued",
		"group_id", groupID, "nodes", numNodes, "gpus_per_node", gpusPerNode, "user", user)
	return &pb.SubmitDistributedJobResponse{GroupId: groupID, JobIds: jobIDs, Status: "QUEUED"}, nil
}

func (s *schedulerServer) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error) {
	if _, err := s.authorizeJob(ctx, req.JobId); err != nil {
		return nil, err
	}
	job, ok := s.state.Cancel(req.JobId)
	if !ok {
		if job != nil {
			return &pb.CancelJobResponse{JobId: req.JobId, Status: job.State, Message: fmt.Sprintf("Already %s", job.State)}, nil
		}
		return &pb.CancelJobResponse{JobId: req.JobId, Status: "NOT_FOUND", Message: "Job not found"}, nil
	}
	// Release resource allocation
	if job.WorkerNode != "" {
		s.gpuTracker.Release(job.ID)
	}
	if job.WorkerNode != "" {
		if err := s.bus.Send(job.WorkerNode, &pb.DispatchMessage{
			JobId: job.ID, Action: "cancel", Attempt: job.Attempt,
		}); err != nil {
			// The cancel had no acknowledgement and no retry before either: a dropped one left
			// the job running on the worker while the master had already released its
			// resources. At least make the divergence visible.
			logging.Job(job.ID).Error("could not deliver cancel", "node", job.WorkerNode, "error", err)
			s.appendLog(job.ID, "WARN", fmt.Sprintf("Cancel could not be delivered to %s: %v", job.WorkerNode, err))
		}
	}
	s.appendLog(req.JobId, "INFO", "Job cancelled by user")
	return &pb.CancelJobResponse{JobId: req.JobId, Status: "CANCELLED", Message: "Cancelled"}, nil
}

func (s *schedulerServer) GetJobStatus(ctx context.Context, req *pb.GetJobStatusRequest) (*pb.GetJobStatusResponse, error) {
	caller := auth.FromContext(ctx)

	job, ok := s.queue.GetJob(req.JobId)
	if ok && !auth.CanAccessJob(caller, job.User) {
		return nil, auth.ErrJobForbidden(req.JobId)
	}
	if !ok {
		if s.store != nil {
			// Fallback to active/completed jobs in store (e.g. after restart)
			if j, err := s.store.GetJob(req.JobId); err == nil {
				if !auth.CanAccessJob(caller, j.User) {
					return nil, auth.ErrJobForbidden(req.JobId)
				}
				resp := &pb.GetJobStatusResponse{
					JobId: j.ID, State: j.State, WorkerNode: j.WorkerNode,
					Command: j.Command, Output: j.Output, Error: j.Error,
					SubmitTime: j.SubmitTime.Unix(), GroupId: j.GroupID,
				}
				if !j.StartTime.IsZero() {
					resp.StartTime = j.StartTime.Unix()
				}
				if !j.EndTime.IsZero() {
					resp.EndTime = j.EndTime.Unix()
				}
				return resp, nil
			}
			// Fallback to dead letters
			if j, err := s.store.GetDeadLetter(req.JobId); err == nil {
				if !auth.CanAccessJob(caller, j.User) {
					return nil, auth.ErrJobForbidden(req.JobId)
				}
				resp := &pb.GetJobStatusResponse{
					JobId: j.ID, State: j.State, WorkerNode: j.WorkerNode,
					Command: j.Command, Output: j.Output, Error: j.Error,
					SubmitTime: j.SubmitTime.Unix(), GroupId: j.GroupID,
				}
				if !j.StartTime.IsZero() {
					resp.StartTime = j.StartTime.Unix()
				}
				if !j.EndTime.IsZero() {
					resp.EndTime = j.EndTime.Unix()
				}
				return resp, nil
			}
		}
		return &pb.GetJobStatusResponse{JobId: req.JobId, State: "NOT_FOUND"}, nil
	}
	resp := &pb.GetJobStatusResponse{
		JobId: job.ID, State: job.State, WorkerNode: job.WorkerNode,
		Command: job.Command, Output: job.Output, Error: job.Error,
		SubmitTime: job.SubmitTime.Unix(), GroupId: job.GroupID,
		DependsOn: job.DependsOn, ArrayId: job.ArrayID, ArrayIndex: int32(job.ArrayIndex),
		Partition: job.Partition, Account: job.Account,
	}
	if !job.StartTime.IsZero() {
		resp.StartTime = job.StartTime.Unix()
	}
	if !job.EndTime.IsZero() {
		resp.EndTime = job.EndTime.Unix()
	}
	// "QUEUED" on its own does not distinguish a job the cluster is too busy for from one that
	// is waiting on something specific. Saying which turns a support question into an answer.
	if job.State == scheduler.StateQueued {
		if eligible, reason := s.queue.Eligibility(job.ID); eligible != scheduler.EligibleNow {
			resp.BlockedReason = reason
		} else if ok, reason := s.policy.AdmitDispatch(job,
			s.policy.AccountUsage(s.queue.RunningJobs()),
			policy.PartitionUsage(s.queue.RunningJobs())); !ok {
			resp.BlockedReason = reason
		}
	}
	return resp, nil
}

// jobInfo converts a job for the list API.
//
// One converter for both the live queue and the dead-letter bucket, so a job does not describe
// itself differently depending on which bucket it was read from.
func jobInfo(j *scheduler.Job) *pb.JobInfo {
	info := &pb.JobInfo{
		JobId: j.ID, State: j.State, Command: j.Command, Requirement: j.Requirement,
		WorkerNode: j.WorkerNode, Priority: int32(j.Priority), User: j.User,
		SubmitTime: j.SubmitTime.Unix(), GroupId: j.GroupID,
		Partition: j.Partition, Account: j.Account,
		ArrayId: j.ArrayID, ArrayIndex: int32(j.ArrayIndex),
		GpusRequired: int32(j.GPUsRequired), CpusRequired: int32(j.CPUsRequired),
		MemoryRequiredMb: int32(j.MemoryRequiredMB), RetryCount: int32(j.RetryCount),
	}
	// A zero-valued time is "not set", and sending its Unix epoch would render as 1970 in any
	// client that does not know to special-case it.
	if !j.StartTime.IsZero() {
		info.StartTime = j.StartTime.Unix()
	}
	if !j.EndTime.IsZero() {
		info.EndTime = j.EndTime.Unix()
	}
	return info
}

func (s *schedulerServer) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	// Listing is scoped to what the caller owns. An unscoped list handed out every job ID,
	// user, and command in the cluster, which is both a disclosure and the enumeration step
	// that made forging results and cancelling other people's work practical.
	caller := auth.FromContext(ctx)
	stateFilter := strings.ToUpper(req.StateFilter)
	var infos []*pb.JobInfo

	if stateFilter == "DEAD_LETTER" {
		if s.store != nil {
			deadLetters, err := s.store.LoadDeadLetters()
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to load dead letters: %v", err)
			}
			visible := deadLetters[:0]
			for _, j := range deadLetters {
				if auth.CanAccessJob(caller, j.User) {
					visible = append(visible, j)
				}
			}
			sort.Slice(visible, func(a, b int) bool {
				if !visible[a].SubmitTime.Equal(visible[b].SubmitTime) {
					return visible[a].SubmitTime.After(visible[b].SubmitTime)
				}
				return visible[a].ID < visible[b].ID
			})

			page, nextToken, err := paginate(visible, req.PageToken, int(req.PageSize))
			if err != nil {
				return nil, err
			}
			for _, j := range page {
				infos = append(infos, jobInfo(j))
			}
			return &pb.ListJobsResponse{
				Jobs: infos, NextPageToken: nextToken, TotalMatching: int32(len(visible)),
			}, nil
		}
		return &pb.ListJobsResponse{Jobs: infos}, nil
	}

	jobs := s.queue.ListJobs(stateFilter)

	visible := jobs[:0]
	for _, j := range jobs {
		if !auth.CanAccessJob(caller, j.User) {
			continue
		}
		// The user filter narrows what the caller may already see; it never widens it, so an
		// admin can focus on one person's work without it becoming a way around the check above.
		if req.UserFilter != "" && j.User != req.UserFilter {
			continue
		}
		visible = append(visible, j)
	}

	// Jobs live in a map, so impose a deterministic order before paging. Newest first, with the
	// ID breaking ties, so a token stays meaningful between calls.
	sort.Slice(visible, func(a, b int) bool {
		if !visible[a].SubmitTime.Equal(visible[b].SubmitTime) {
			return visible[a].SubmitTime.After(visible[b].SubmitTime)
		}
		return visible[a].ID < visible[b].ID
	})

	page, nextToken, err := paginate(visible, req.PageToken, int(req.PageSize))
	if err != nil {
		return nil, err
	}
	for _, j := range page {
		infos = append(infos, jobInfo(j))
	}
	return &pb.ListJobsResponse{
		Jobs: infos, NextPageToken: nextToken, TotalMatching: int32(len(visible)),
	}, nil
}

func (s *schedulerServer) StreamLogs(req *pb.LogStreamRequest, stream pb.SchedulerService_StreamLogsServer) error {
	// Logs carry job output, which routinely contains tokens and presigned URLs, so they follow
	// the same ownership rule as the job itself.
	if _, err := s.authorizeJob(stream.Context(), req.JobId); err != nil {
		return err
	}

	jobID := req.JobId
	s.logMu.Lock()
	for _, entry := range s.logStore[jobID] {
		if err := stream.Send(entry); err != nil {
			s.logMu.Unlock()
			return err
		}
	}
	s.logMu.Unlock()

	ch := make(chan *pb.LogMessage, 64)
	s.subMu.Lock()
	s.logChannels[jobID] = append(s.logChannels[jobID], ch)
	s.subMu.Unlock()

	defer func() {
		s.subMu.Lock()
		channels := s.logChannels[jobID]
		for i, c := range channels {
			if c == ch {
				s.logChannels[jobID] = append(channels[:i], channels[i+1:]...)
				break
			}
		}
		s.subMu.Unlock()
		close(ch)
	}()

	for {
		select {
		case entry := <-ch:
			if err := stream.Send(entry); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

// isUserError reports whether an error came from the job itself rather than the node running
// it. These must not count against a node's circuit breaker.
func isUserError(errMsg string) bool {
	return strings.HasPrefix(errMsg, "exit status") ||
		errMsg == "cancelled" ||
		strings.Contains(errMsg, "walltime exceeded")
}

// isRetryable reports whether a failed job should be re-run.
//
// Deliberate stops are never retried: a cancelled job must stay cancelled, and a job killed
// for exceeding its walltime will simply exceed it again.
func isRetryable(job *scheduler.Job, errMsg string) bool {
	if job.State == scheduler.StateCancelled {
		return false
	}
	if errMsg == "cancelled" || strings.Contains(errMsg, "walltime exceeded") {
		return false
	}
	return true
}

// adoptReportedJobs reconciles the master against what a worker says it is executing.
//
// Each claimed job is restored to RUNNING on that node and its resources re-booked, so the
// scheduler stops treating the node as idle. A job the master has since cancelled is not
// adopted; the worker is told to stop it instead.
func (s *schedulerServer) adoptReportedJobs(nodeName string, running []*pb.RunningJob) {
	if len(running) == 0 {
		return
	}

	adopted, rejected := 0, 0
	for _, claim := range running {
		if claim.GetJobId() == "" {
			continue
		}
		var startTime time.Time
		if claim.GetStartTime() > 0 {
			startTime = time.Unix(claim.GetStartTime(), 0)
		}

		if !s.state.AdoptRunning(claim.JobId, nodeName, claim.Attempt, startTime) {
			// The job is gone or already finished as far as the master is concerned. Tell the
			// worker to stop, rather than leaving an orphan consuming the node indefinitely.
			rejected++
			logging.Job(claim.JobId).Warn("worker reported a job the master no longer owns; cancelling",
				"node", nodeName)
			if err := s.bus.Send(nodeName, &pb.DispatchMessage{
				JobId: claim.JobId, Action: "cancel", Attempt: claim.Attempt,
			}); err != nil {
				logging.Job(claim.JobId).Error("could not cancel an unrecognised job", "node", nodeName, "error", err)
			}
			continue
		}

		job, ok := s.queue.GetJob(claim.JobId)
		if !ok {
			continue
		}
		_, totalGPUs := nodeGPUInfo(s, nodeName)
		if _, booked := s.gpuTracker.Allocate(nodeName, job.ID, job.GPUsRequired, job.CPUsRequired, job.MemoryRequiredMB, totalGPUs); !booked {
			logging.Job(job.ID).Warn("could not re-book resources for an adopted job", "node", nodeName)
		}
		s.adoptedMu.Lock()
		s.adopted[job.ID] = true
		s.adoptedMu.Unlock()

		s.appendLog(job.ID, "INFO", fmt.Sprintf("Reclaimed by master after reconnect on %s", nodeName))
		adopted++
	}

	if adopted > 0 || rejected > 0 {
		slog.Info("reconciled worker jobs", "node", nodeName, "adopted", adopted, "rejected", rejected)
	}
}

// reapUnclaimedJobs fails jobs left RUNNING by a restart that no worker has claimed.
//
// Workers reconnect within seconds, so anything still unclaimed after the grace period really is
// gone — its worker died with the master, or was decommissioned while the master was down.
func reapUnclaimedJobs(srv *schedulerServer, orphaned []string) {
	if len(orphaned) == 0 {
		return
	}

	timer := time.NewTimer(adoptionGracePeriod)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-srv.ctx.Done():
		return
	}

	reaped := 0
	for _, jobID := range orphaned {
		job, ok := srv.queue.GetJob(jobID)
		if !ok || job.State != scheduler.StateRunning {
			continue
		}

		// Adoption, not connectivity, is the test. A worker that reconnected and did not claim
		// the job is telling us it is not running it; checking only whether the node was
		// reachable left such jobs RUNNING forever, holding resources and never reporting.
		srv.adoptedMu.Lock()
		claimed := srv.adopted[jobID]
		srv.adoptedMu.Unlock()
		if claimed {
			continue
		}

		srv.state.Complete(jobID, false, "", "lost when the master restarted; no worker claimed it")
		srv.gpuTracker.Release(jobID)
		srv.appendLog(jobID, "ERROR", "No worker claimed this job after the master restarted")
		reaped++
	}
	if reaped > 0 {
		slog.Warn("failed jobs whose workers never reconnected", "count", reaped)
	}
}

// ClusterStatus reports this master's role, so a client can find the one accepting writes.
func (s *schedulerServer) ClusterStatus(ctx context.Context, req *pb.ClusterStatusRequest) (*pb.ClusterStatusResponse, error) {
	resp := &pb.ClusterStatusResponse{
		IsLeader:  s.state.IsLeader(),
		HaEnabled: s.raftNode != nil,
		NodeId:    s.cfg.NodeName,
	}
	if s.raftNode == nil {
		// A single master is always the leader; there is nobody to defer to.
		resp.LeaderId = s.cfg.NodeName
		return resp, nil
	}

	resp.NodeId = s.cfg.HA.NodeID
	resp.LeaderId = s.raftNode.LeaderID()
	resp.LeaderAddress = s.raftNode.LeaderAddress()

	peers, err := s.raftNode.Peers()
	if err != nil {
		return resp, nil
	}
	for _, peer := range peers {
		resp.Members = append(resp.Members, &pb.ClusterMember{
			NodeId:   string(peer.ID),
			Address:  string(peer.Address),
			Leader:   string(peer.ID) == resp.LeaderId,
			Suffrage: peer.Suffrage.String(),
		})
	}
	return resp, nil
}

// CordonNode takes a node out of scheduling rotation, or returns it to service.
//
// Cordoning stops new dispatches while letting the jobs already running finish. Draining
// additionally cancels them, which is what an operator wants before rebooting a machine.
func (s *schedulerServer) CordonNode(ctx context.Context, req *pb.CordonNodeRequest) (*pb.CordonNodeResponse, error) {
	if req.NodeName == "" {
		return nil, status.Error(codes.InvalidArgument, "node_name is required")
	}
	// Taking a node out of service affects everyone's work, not just the caller's.
	if auth.FromContext(ctx).Role != auth.RoleAdmin {
		return nil, status.Error(codes.PermissionDenied, "cordoning a node requires an admin principal")
	}

	if !req.Cordon {
		was, err := s.state.Uncordon(req.NodeName)
		if err != nil {
			return nil, leaderRedirect(s, err)
		}
		// The replicated log is the durable record under HA, but a single master still needs
		// its own copy on disk, or a cordon would not survive a restart.
		s.persistCordons()
		msg := "node returned to service"
		if !was {
			msg = "node was not cordoned"
		}
		slog.Info("node uncordoned", "node", req.NodeName)
		return &pb.CordonNodeResponse{NodeName: req.NodeName, Cordoned: false, Message: msg}, nil
	}

	reason := req.Reason
	if reason == "" {
		reason = "cordoned by operator"
	}
	if err := s.state.Cordon(req.NodeName, reason, time.Now()); err != nil {
		return nil, leaderRedirect(s, err)
	}
	s.persistCordons()
	slog.Warn("node cordoned", "node", req.NodeName, "reason", reason, "drain", req.Drain)

	var cancelled int32
	if req.Drain {
		for _, job := range s.queue.RunningJobsOnNode(req.NodeName) {
			if _, ok := s.state.Cancel(job.ID); ok {
				cancelled++
				s.gpuTracker.Release(job.ID)
				if err := s.bus.Send(req.NodeName, &pb.DispatchMessage{
					JobId: job.ID, Action: "cancel", Attempt: job.Attempt,
				}); err != nil {
					logging.Job(job.ID).Error("could not deliver drain cancel",
						"node", req.NodeName, "error", err)
				}
				s.appendLog(job.ID, "WARN", fmt.Sprintf("Cancelled: node %s drained (%s)", req.NodeName, reason))
			}
		}
		slog.Warn("node drained", "node", req.NodeName, "jobs_cancelled", cancelled)
	}

	message := "node cordoned; running jobs will finish"
	if req.Drain {
		message = fmt.Sprintf("node drained; %d running job(s) cancelled", cancelled)
	}
	return &pb.CordonNodeResponse{
		NodeName: req.NodeName, Cordoned: true, JobsCancelled: cancelled, Message: message,
	}, nil
}

// persistReservations writes the reservation set so it survives a restart.
//
// Reservations describe planned work — a maintenance window agreed with the people who own the
// machines. A master restart forgetting them would silently let jobs back onto nodes that are
// about to be taken away.
func (s *schedulerServer) persistReservations() {
	if s.store == nil {
		return
	}
	snapshot := s.reservations.Snapshot()
	encoded := make(map[string][]byte, len(snapshot))
	for id, r := range snapshot {
		data, err := json.Marshal(r)
		if err != nil {
			slog.Error("could not encode reservation", "id", id, "error", err)
			continue
		}
		encoded[id] = data
	}
	if err := s.store.SaveReservations(encoded); err != nil {
		slog.Error("could not persist reservations", "error", err)
	}
}

// expireReservations removes windows that have closed.
//
// Left behind they are only clutter, but the clutter is the kind that gets acted on: an
// operator reading a months-old reservation has no way to tell it is spent.
func (s *schedulerServer) expireReservations() {
	expired := s.reservations.Expired(time.Now())
	if len(expired) == 0 {
		return
	}
	for _, id := range expired {
		if ok, err := s.state.RemoveReservation(id); err == nil && ok {
			slog.Info("reservation window closed", "reservation_id", id)
		}
	}
	s.persistReservations()
}

// persistCordons writes the cordon set so it survives a restart.
//
// With HA on the replicated log is already durable, but a single master still needs this.
func (s *schedulerServer) persistCordons() {
	if s.store == nil {
		return
	}
	encoded := make(map[string][]byte)
	for node, entry := range s.cordons.Snapshot() {
		data, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		encoded[node] = data
	}
	if err := s.store.SaveCordons(encoded); err != nil {
		slog.Error("could not persist cordons", "error", err)
	}
}

// leaderRedirect turns a replication failure into a gRPC error a client can act on.
//
// A write that reaches a follower must not be quietly dropped: the client is told which master
// to retry against, so a failover looks like a brief retry rather than a lost submission.
func leaderRedirect(s *schedulerServer, err error) error {
	var notLeader ha.ErrNotLeader
	if errors.As(err, &notLeader) {
		return status.Errorf(codes.FailedPrecondition,
			"this master is not the leader; retry against %s", notLeader.LeaderHint())
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// AcknowledgeStart records that a worker has begun a job, disarming the dispatch-timeout
// re-queue for it.
//
// This is authenticated and authorized like every other RPC, and the worker no longer has to
// know the master's metrics port to reach it.
func (s *schedulerServer) AcknowledgeStart(ctx context.Context, req *pb.AcknowledgeStartRequest) (*pb.AcknowledgeStartResponse, error) {
	if req.JobId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	s.dispatchPendingMu.Lock()
	_, pending := s.dispatchPending[req.JobId]
	if pending {
		delete(s.dispatchPending, req.JobId)
	}
	s.dispatchPendingMu.Unlock()

	if pending {
		logging.Job(req.JobId).Debug("start acknowledged", "worker", req.WorkerNode, "attempt", req.Attempt)
	}
	// Report success either way: a job that already reported its result, or was cancelled, has
	// no pending entry left, and that is not the worker's problem to retry.
	return &pb.AcknowledgeStartResponse{Acknowledged: true}, nil
}

// WatchDispatch streams the dispatches addressed to one node.
//
// Each worker gets only its own work. The bus this replaces published every job to every
// subscriber and relied on the worker to discard what was not for it, so the dispatch socket
// handed every job's command and environment variables — API tokens included — to anything
// that could connect to it.
func (s *schedulerServer) WatchDispatch(req *pb.WatchDispatchRequest, stream pb.SchedulerService_WatchDispatchServer) error {
	nodeName := req.NodeName
	if nodeName == "" {
		return status.Error(codes.InvalidArgument, "node_name is required")
	}

	// Adopt whatever this worker says it is running before sending it anything new. After a
	// master restart this is the only way to learn that a job survived, and it must happen
	// before the scheduler can hand the node more work on top of it.
	s.adoptReportedJobs(nodeName, req.RunningJobs)

	queue, unsubscribe := s.bus.Subscribe(nodeName)
	defer unsubscribe()

	slog.Info("worker dispatch stream connected", "node", nodeName)
	defer slog.Info("worker dispatch stream disconnected", "node", nodeName)

	for {
		select {
		case msg, ok := <-queue:
			if !ok {
				return nil // the master is shutting down
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-s.ctx.Done():
			return nil
		}
	}
}

func (s *schedulerServer) ReportResult(ctx context.Context, req *pb.ReportResultRequest) (*pb.ReportResultResponse, error) {
	// Look the job up before recording anything: a result from a superseded dispatch must not
	// change state at all.
	job, ok := s.queue.GetJob(req.JobId)
	if !ok {
		return &pb.ReportResultResponse{Acknowledged: true}, nil
	}

	// Fencing check. A worker that lost its acknowledgement keeps running the job while the
	// master re-dispatches it elsewhere; when the first worker eventually reports, its result
	// is for an attempt that no longer owns the job. Acting on it released the *current*
	// node's allocation and overwrote the live result.
	//
	// Attempt 0 means a worker predating the field, so it is accepted for compatibility.
	if req.Attempt != 0 && job.Attempt != 0 && req.Attempt < job.Attempt {
		staleResultsTotal.Inc()
		logging.Job(req.JobId).Warn("ignoring stale result from a superseded dispatch",
			"reported_attempt", req.Attempt, "current_attempt", job.Attempt)
		return &pb.ReportResultResponse{Acknowledged: true}, nil
	}

	// A result for a job that is no longer RUNNING belongs to a dispatch that has already been
	// superseded — by a requeue after preemption or a lost worker, or by a cancel. The attempt
	// fence above does not catch it: a requeue leaves the attempt unchanged, so the numbers
	// still match. Applying it would overwrite the requeue and turn a job that was only
	// delayed into a failed one, which is exactly what preemption promised not to do.
	if job.State != scheduler.StateRunning {
		staleResultsTotal.Inc()
		logging.Job(req.JobId).Info("ignoring a result for a job that is no longer running",
			"state", job.State, "worker", req.WorkerNode)
		return &pb.ReportResultResponse{Acknowledged: true}, nil
	}

	// The job reported in, so it is no longer awaiting a start handshake. Without this the
	// entry leaked for any job that finished before its acknowledgement was processed.
	s.dispatchPendingMu.Lock()
	delete(s.dispatchPending, req.JobId)
	s.dispatchPendingMu.Unlock()

	s.state.Complete(req.JobId, req.Success, req.Output, req.Error)

	// Re-read so the state below reflects the completion just recorded.
	if updated, stillThere := s.queue.GetJob(req.JobId); stillThere {
		job = updated
	}

	// Release resource allocation. Release is keyed by job ID and idempotent, so the several
	// paths that can end a job cannot compound.
	s.gpuTracker.Release(job.ID)

	// Circuit breaker tracking. A failure the job itself caused says nothing about the node's
	// health, so it must not count toward blocking that node.
	if req.Success {
		s.cb.RecordSuccess(req.WorkerNode)
	} else if !isUserError(req.Error) {
		s.cb.RecordFailure(req.WorkerNode, req.JobId)
	}

	// Fairshare usage recording
	dur := job.EndTime.Sub(job.StartTime).Seconds()
	if dur > 0 {
		// Bill by what the job held, not just how long it ran.
		s.fairshare.RecordUsage(job.User, dur, job.CPUsRequired, job.GPUsRequired, job.MemoryRequiredMB)
	}

	// Job retry logic.
	//
	// A cancellation or a walltime kill is a deliberate stop, not a transient failure: retrying
	// it re-runs a job the user explicitly killed, and in the walltime case burns the whole
	// limit again on each attempt. Only the circuit breaker used to consult this distinction.
	if !req.Success && job.GroupID == "" && job.RetryCount < job.MaxRetries && isRetryable(job, req.Error) {
		nextRetryCount := job.RetryCount + 1
		backoff := time.Duration(nextRetryCount*nextRetryCount*10) * time.Second
		s.appendLog(req.JobId, "WARN", fmt.Sprintf("Retry %d/%d in %s", nextRetryCount, job.MaxRetries, backoff))
		retriesTotal.Inc()
		logging.Job(req.JobId).Warn("scheduling retry",
			"attempt", nextRetryCount, "max_retries", job.MaxRetries, "backoff", backoff, "error", req.Error)
		go func(jobID string) {
			select {
			case <-time.After(backoff):
				_, err := s.state.Requeue(jobID, true)
				if err != nil {
					logging.Job(jobID).Error("failed to requeue for retry", "error", err)
				}
			case <-s.ctx.Done():
				// Master is shutting down
			}
		}(req.JobId)
		return &pb.ReportResultResponse{Acknowledged: true}, nil
	}

	// Dead letter queue for exhausted retries
	if !req.Success && job.RetryCount >= job.MaxRetries && job.MaxRetries > 0 && s.store != nil {
		deadLettersTotal.Inc()
		if err := s.store.SaveDeadLetter(job); err != nil {
			logging.Job(job.ID).Error("could not record dead letter", "error", err)
		}
		s.appendLog(req.JobId, "ERROR", fmt.Sprintf("All %d retries exhausted, moved to dead letter queue", job.MaxRetries))
	}

	// Group completion
	if job.GroupID != "" {
		s.handleGroupCompletion(job.GroupID, req.JobId, req.Success)
	}

	resultStatus := "COMPLETED"
	if !req.Success {
		resultStatus = "FAILED"
	}
	jobsCompletedTotal.WithLabelValues(job.User, resultStatus).Inc()
	if dur > 0 {
		jobDuration.WithLabelValues(job.User, resultStatus).Observe(dur)
	}

	s.appendLog(req.JobId, "INFO", fmt.Sprintf("Job %s on worker %s | Output: %s", resultStatus, req.WorkerNode, truncate(req.Output, 200)))
	if req.Error != "" {
		s.appendLog(req.JobId, "ERROR", req.Error)
	}
	logging.Job(req.JobId).Info("result recorded", "status", resultStatus, "worker", req.WorkerNode, "duration_seconds", dur)
	return &pb.ReportResultResponse{Acknowledged: true}, nil
}

func (s *schedulerServer) handleGroupCompletion(groupID, completedJobID string, success bool) {
	group, ok := s.queue.GetGroup(groupID)
	if !ok || group.State == "COMPLETED" || group.State == "FAILED" {
		return
	}
	if !success {
		slog.Warn("gang rank failed, cancelling siblings", "group_id", groupID, "job_id", completedJobID)
		for _, jid := range group.JobIDs {
			if jid == completedJobID {
				continue
			}
			job, jOk := s.state.Cancel(jid)
			if jOk && job != nil {
				if job.WorkerNode != "" {
					s.gpuTracker.Release(job.ID)
				}
				if job.WorkerNode != "" {
					if err := s.bus.Send(job.WorkerNode, &pb.DispatchMessage{
						JobId: job.ID, Action: "cancel", Attempt: job.Attempt,
					}); err != nil {
						logging.Job(job.ID).Error("could not deliver cancel", "node", job.WorkerNode, "error", err)
					}
				}
			}
		}
		if err := s.state.SetGroupState(groupID, "FAILED"); err != nil {
			slog.Error("could not replicate group state change", "error", err)
		}
		return
	}
	allDone := true
	for _, jid := range group.JobIDs {
		j, jOk := s.queue.GetJob(jid)
		if !jOk {
			continue
		}
		if j.State == scheduler.StateQueued || j.State == scheduler.StateRunning {
			allDone = false
			break
		}
	}
	if allDone {
		if err := s.state.SetGroupState(groupID, "COMPLETED"); err != nil {
			slog.Error("could not replicate group state change", "error", err)
		}
		slog.Info("all gang ranks completed", "group_id", groupID)
	}
}

// --- Scheduling ---

const gangTimeout = 5 * time.Minute

func parseCELRequirement(celStr string) (cpus int, mem int) {
	// Look for cpu_cores >= X or cpu_cores == X or cpu_cores > X
	if matches := celCPURegex.FindStringSubmatch(celStr); len(matches) > 1 {
		cpus, _ = strconv.Atoi(matches[1])
	}
	// Look for total_memory_mb >= X or total_memory_mb == X or total_memory_mb > X
	if matches := celMemRegex.FindStringSubmatch(celStr); len(matches) > 1 {
		mem, _ = strconv.Atoi(matches[1])
	}
	return
}

func dispatchLoop(ctx context.Context, srv *schedulerServer) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			schedulingTick(srv)
		case <-ctx.Done():
			return
		}
	}
}

// schedulingTick runs one scheduling cycle: gang groups, then the highest-priority single
// job, then backfill.
//
// Each phase is its own function on purpose. The phases previously shared one switch arm, and
// the early `continue`s meant to skip only the direct-match phase skipped backfill along with
// it — so a gang rank sitting at the head of the heap stopped the entire cluster from
// dispatching, permanently if the rank was orphaned by a restart.
func schedulingTick(srv *schedulerServer) {
	tickStart := time.Now()
	defer func() { schedulingTickDuration.Observe(time.Since(tickStart).Seconds()) }()

	srv.lastTick.Store(time.Now().UnixNano())

	// Followers must not schedule. Two masters dispatching from the same queue would each send
	// the same job to a different node, and both would run it.
	if !srv.state.IsLeader() {
		return
	}
	updateSchedulerGauges(srv)
	failDoomedDependents(srv)
	dispatchGangGroups(srv)

	members := srv.disc.Members()

	// Quota usage is counted once per tick, not once per candidate node. It depends only on
	// what is running, which does not change while a single tick decides where one job goes.
	adm := newAdmission(srv)

	// One dispatch per tick: if the top job went out, leave backfill for the next cycle so a
	// full queue cannot starve the head.
	if dispatchTopJob(srv, members, adm) {
		return
	}

	// Nothing could be placed at the head. If the cluster is full of work this job outranks,
	// make room — otherwise priority stops meaning anything at exactly the moment it matters.
	// The eviction only frees resources; the next tick does the placing, so dispatch stays the
	// responsibility of one path rather than two.
	if srv.cfg.Preemption.Enabled {
		if head := srv.queue.PeekRunnable(); head != nil {
			if ok, _ := srv.policy.AdmitDispatch(head, adm.accounts, adm.partitions); ok {
				if preemptFor(srv, head, members) {
					return
				}
			}
		}
	}

	backfillOntoIdleNodes(srv, members, adm)
}

// admission is the quota state one scheduling tick decides against.
type admission struct {
	accounts   map[string]policy.Usage
	partitions map[string]int
}

func newAdmission(srv *schedulerServer) *admission {
	running := srv.queue.RunningJobs()
	return &admission{
		accounts:   srv.policy.AccountUsage(running),
		partitions: policy.PartitionUsage(running),
	}
}

// failDoomedDependents fails queued jobs that can never become eligible.
//
// A job whose dependency failed under "afterok" is not waiting for anything: no future event
// can release it. Left alone it sits QUEUED forever, holding a queue slot and telling its
// submitter nothing. Failing it with the reason is the only outcome that is true.
func failDoomedDependents(srv *schedulerServer) {
	doomed := srv.queue.DoomedQueued()
	if len(doomed) == 0 {
		return
	}
	for jobID, reason := range doomed {
		full := "dependency not satisfiable: " + reason
		job, ok := srv.state.FailQueued(jobID, full)
		if !ok || job == nil {
			continue
		}
		srv.appendLog(jobID, "ERROR", full)
		logging.Job(jobID).Warn("failed before starting", "reason", reason, "user", job.User)
		jobsCompletedTotal.WithLabelValues(job.User, "FAILED").Inc()
	}
}

func updateSchedulerGauges(srv *schedulerServer) {
	queueDepth.Set(float64(srv.queue.QueueLen()))
	runningJobs.Set(float64(len(srv.queue.RunningJobs())))
	clusterNodes.Set(float64(len(srv.disc.Members())))
	groupsPending.Set(float64(len(srv.queue.PendingGroups())))

	// Per-state counts, so a backlog of failures is visible without querying the API.
	counts := map[string]int{
		scheduler.StateQueued:    0,
		scheduler.StateRunning:   0,
		scheduler.StateCompleted: 0,
		scheduler.StateFailed:    0,
		scheduler.StateCancelled: 0,
	}
	for _, job := range srv.queue.ListJobs("") {
		counts[job.State]++
	}
	for state, n := range counts {
		jobsByState.WithLabelValues(state).Set(float64(n))
	}

	// Export what the tracker actually holds, so "the cluster is full" can be distinguished
	// from "the scheduler stopped placing work".
	for node, used := range srv.gpuTracker.UsageByNode() {
		gpusAllocated.WithLabelValues(node).Set(float64(used))
	}
}

// dispatchGangGroups attempts to co-schedule every pending group, failing those past the
// gang timeout.
func dispatchGangGroups(srv *schedulerServer) {
	for _, group := range srv.queue.PendingGroups() {
		if !group.CreatedAt.IsZero() && time.Since(group.CreatedAt) > gangTimeout {
			slog.Warn("gang group timed out, failing all ranks",
				"group_id", group.GroupID, "num_nodes", group.NumNodes, "timeout", gangTimeout)
			for _, jid := range group.JobIDs {
				srv.state.Cancel(jid)
				srv.appendLog(jid, "ERROR", fmt.Sprintf("Gang group timed out waiting for %d nodes", group.NumNodes))
			}
			if err := srv.state.SetGroupState(group.GroupID, "FAILED"); err != nil {
				slog.Error("could not replicate group state change", "error", err)
			}
			continue
		}
		tryDispatchGroup(srv, group)
	}
}

// dispatchTopJob tries to place the highest-priority single job, reporting whether it
// dispatched one.
//
// A gang rank at the head is not dispatchable here — it belongs to dispatchGangGroups — so
// this reports false and lets backfill proceed rather than stalling the cycle.
func dispatchTopJob(srv *schedulerServer, members []*memberlist.Node, adm *admission) bool {
	// Choose on the leader, then replicate the choice.
	//
	// Matching involves CEL evaluation against live cluster membership, none of which is
	// deterministic across replicas or expressible in a log entry. Deciding "this job goes to
	// that node" here and replicating it as a fact keeps every replica's state identical without
	// any of that.
	// PeekRunnable, not Peek. A job at the head waiting on a dependency, or held back by its
	// array's concurrency cap, can never be placed — offering it every tick would starve
	// everything behind it, which is exactly how a gang rank at the head used to wedge the
	// whole cluster.
	topJob := srv.queue.PeekRunnable()
	if topJob == nil || topJob.GroupID != "" {
		return false
	}

	// Quota is a property of the job and its account, not of any node, so it is settled before
	// the node search rather than inside it. Returning false lets backfill look past this job
	// at one whose account still has room; without that, a single over-quota job at the head
	// would idle the whole cluster.
	if ok, reason := srv.policy.AdmitDispatch(topJob, adm.accounts, adm.partitions); !ok {
		logging.Job(topJob.ID).Debug("held at the head of the queue", "reason", reason)
		return false
	}

	selectedNode := ""
	for _, member := range members {
		if len(member.Meta) == 0 || srv.cb.IsBlocked(member.Name) || srv.cordons.IsCordoned(member.Name) {
			continue
		}
		if !canDispatchResources(srv, member, topJob) {
			continue
		}
		if match, evalErr := srv.eval.Match(topJob.Requirement, string(member.Meta)); evalErr == nil && match {
			selectedNode = member.Name
			break
		}
	}
	if selectedNode == "" {
		return false
	}

	// Dispatch removes the job and marks it running as one replicated step, so a concurrent
	// submit or cancel cannot substitute a different job for the one just matched.
	job, attempt, ok := srv.state.Dispatch(topJob.ID, selectedNode)
	if !ok || job == nil {
		return false
	}

	dispatchJob(srv, job, selectedNode, attempt)
	return true
}

// backfillOntoIdleNodes places a lower-priority job on the first node that can take one.
func backfillOntoIdleNodes(srv *schedulerServer, members []*memberlist.Node, adm *admission) {
	for _, member := range members {
		if len(member.Meta) == 0 || srv.cb.IsBlocked(member.Name) || srv.cordons.IsCordoned(member.Name) {
			continue
		}
		memberMeta := string(member.Meta)
		memberName := member.Name
		// Pick a candidate read-only, then replicate the dispatch of that specific job.
		candidate := srv.queue.FindQueued(func(j *scheduler.Job) bool {
			if j.GroupID != "" {
				return false
			}
			if ok, _ := srv.policy.AdmitDispatch(j, adm.accounts, adm.partitions); !ok {
				return false
			}
			if !canDispatchResources(srv, member, j) {
				return false
			}
			match, err := srv.eval.Match(j.Requirement, memberMeta)
			return err == nil && match
		})
		if candidate == nil {
			continue
		}

		job, attempt, ok := srv.state.Dispatch(candidate.ID, memberName)
		if !ok || job == nil {
			continue
		}
		logging.Job(job.ID).Info("backfilled", "node", memberName)
		srv.appendLog(job.ID, "INFO", fmt.Sprintf("Backfilled onto %s", memberName))
		dispatchJob(srv, job, memberName, attempt)
		return
	}
}

// canDispatchResources checks GPUs, CPUs, and Memory capacities on the node.
func canDispatchResources(srv *schedulerServer, member *memberlist.Node, job *scheduler.Job) bool {
	// A partition is a promise about which nodes a job runs on. Checking it before the resource
	// arithmetic also means a job never lands somewhere its operator excluded just because that
	// node happened to have room.
	if !srv.policy.MatchesPartition(job.Partition, string(member.Meta)) {
		return false
	}

	// Reservations, which also cover the window *before* one opens: a job that could still be
	// running when a maintenance window starts must not be placed on that node now. That is what
	// drains the node in time without anyone having to cordon it early and waste the interval.
	if ok, _ := srv.reservations.Admits(member.Name, job.User, job.Account,
		job.WalltimeSeconds, time.Now()); !ok {
		return false
	}

	var ad map[string]interface{}
	if len(member.Meta) > 0 {
		if err := json.Unmarshal(member.Meta, &ad); err != nil {
			// A node advertising unparseable metadata must not be treated as a node with
			// unlimited free resources, which is what an all-zero ad amounted to.
			slog.Warn("ignoring node with unparseable class ad", "node", member.Name, "error", err)
			return false
		}
	}

	// 1. Check GPUs
	if job.GPUsRequired > 0 {
		total, _ := ad["gpu_count"].(float64)
		if int(total) < job.GPUsRequired {
			return false
		}
		if srv.gpuTracker.AvailableGPUs(member.Name, int(total)) < job.GPUsRequired {
			return false
		}
	}

	// 2. Check CPUs
	if job.CPUsRequired > 0 {
		total, _ := ad["cpu_cores"].(float64)
		if int(total) < job.CPUsRequired {
			return false
		}
		if srv.gpuTracker.AvailableCPUs(member.Name, int(total)) < job.CPUsRequired {
			return false
		}
	}

	// 3. Check Memory
	if job.MemoryRequiredMB > 0 {
		total, _ := ad["total_memory_mb"].(float64)
		if int(total) < job.MemoryRequiredMB {
			return false
		}
		if srv.gpuTracker.AvailableMemory(member.Name, int(total)) < job.MemoryRequiredMB {
			return false
		}
	}

	return true
}

func tryDispatchGroup(srv *schedulerServer, group *scheduler.JobGroup) {
	var queuedJobs []*scheduler.Job
	for _, jid := range group.JobIDs {
		job, ok := srv.queue.GetJob(jid)
		if ok && job.State == scheduler.StateQueued {
			queuedJobs = append(queuedJobs, job)
		}
	}
	if len(queuedJobs) != group.NumNodes {
		return
	}

	members := srv.disc.Members()
	var matchedMembers []*memberlist.Node
	usedNodes := make(map[string]bool)

	for _, job := range queuedJobs {
		matched := false
		for _, member := range members {
			if usedNodes[member.Name] || len(member.Meta) == 0 || srv.cb.IsBlocked(member.Name) {
				continue
			}
			if !canDispatchResources(srv, member, job) {
				continue
			}
			match, err := srv.eval.Match(job.Requirement, string(member.Meta))
			if err == nil && match {
				matchedMembers = append(matchedMembers, member)
				usedNodes[member.Name] = true
				matched = true
				break
			}
		}
		if !matched {
			return
		}
	}

	rank0Addr := matchedMembers[0].Addr.String()
	slog.Info("dispatching gang group", "group_id", group.GroupID, "nodes", group.NumNodes, "rank0_addr", rank0Addr)

	for i, job := range queuedJobs {
		job.EnvVars["MASTER_ADDR"] = rank0Addr
		dispatched, attempt, ok := srv.state.Dispatch(job.ID, matchedMembers[i].Name)
		if !ok || dispatched == nil {
			logging.Job(job.ID).Warn("gang rank could not be dispatched", "node", matchedMembers[i].Name)
			continue
		}
		// The replicated copy has no MASTER_ADDR, which is decided here at dispatch time.
		dispatched.EnvVars = job.EnvVars
		dispatchJob(srv, dispatched, matchedMembers[i].Name, attempt)
		srv.appendLog(job.ID, "INFO", fmt.Sprintf("Gang-scheduled: rank %d → %s (master=%s)", i, matchedMembers[i].Name, rank0Addr))
	}
	if err := srv.state.SetGroupState(group.GroupID, "RUNNING"); err != nil {
		slog.Error("could not replicate group state change", "error", err)
	}
}

// maintenanceLoop periodically reconciles resource accounting and releases memory that would
// otherwise grow for the lifetime of the process.
func maintenanceLoop(ctx context.Context, srv *schedulerServer) {
	const (
		interval       = 60 * time.Second
		terminalMaxAge = 30 * time.Minute
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Maintenance mutates replicated state, so only the leader performs it.
			if !srv.state.IsLeader() {
				continue
			}
			srv.expireReservations()

			running := srv.queue.RunningJobs()
			dropped, rebooked := srv.gpuTracker.Reconcile(running, func(node string) int {
				_, total := nodeGPUInfo(srv, node)
				return total
			})
			if dropped > 0 || rebooked > 0 {
				reconcileCorrectionsTotal.WithLabelValues("dropped").Add(float64(dropped))
				reconcileCorrectionsTotal.WithLabelValues("rebooked").Add(float64(rebooked))
				slog.Info("resource accounting corrected", "dropped", dropped, "rebooked", rebooked)
			}

			if pruned, err := srv.state.PruneTerminal(terminalMaxAge); err == nil && pruned > 0 {
				srv.pruneLogs()
				slog.Info("released finished jobs from memory", "count", pruned)
			}
		case <-ctx.Done():
			return
		}
	}
}

// pruneLogs drops log buffers for jobs the scheduler no longer holds in memory.
func (s *schedulerServer) pruneLogs() {
	resident := make(map[string]bool)
	for _, job := range s.queue.ListJobs("") {
		resident[job.ID] = true
	}

	s.logMu.Lock()
	for jobID := range s.logStore {
		if !resident[jobID] {
			delete(s.logStore, jobID)
		}
	}
	s.logMu.Unlock()
}

// nodeGPUInfo reads a node's advertised GPU vendor and physical GPU count from its ClassAd.
func nodeGPUInfo(srv *schedulerServer, nodeName string) (vendor string, totalGPUs int) {
	for _, member := range srv.disc.Members() {
		if member.Name != nodeName || len(member.Meta) == 0 {
			continue
		}
		var ad map[string]interface{}
		if json.Unmarshal(member.Meta, &ad) == nil {
			if v, ok := ad["gpu_vendor"].(string); ok {
				vendor = v
			}
			if c, ok := ad["gpu_count"].(float64); ok {
				totalGPUs = int(c)
			}
		}
		return vendor, totalGPUs
	}
	return "", 0
}

// setGPUVisibility pins the job to the physical GPU indices it was allocated, using the
// selector variable its vendor understands.
//
// The indices come from the tracker, not from a 0..N-1 range: two single-GPU jobs on the same
// node must see different devices, or they land on GPU 0 together and OOM each other while the
// rest of the node sits idle.
//
// A value the submitter set explicitly always wins, so an operator can still override pinning.
func setGPUVisibility(envVars map[string]string, vendor string, devices []int) {
	if len(devices) == 0 {
		return
	}

	ids := make([]string, len(devices))
	for i, d := range devices {
		ids[i] = strconv.Itoa(d)
	}
	list := strings.Join(ids, ",")

	setIfAbsent := func(key, value string) {
		if _, exists := envVars[key]; !exists {
			envVars[key] = value
		}
	}

	switch vendor {
	case "intel":
		selector := "level_zero:" + list
		setIfAbsent("ONEAPI_DEVICE_SELECTOR", selector)
		setIfAbsent("SYCL_DEVICE_FILTER", selector)
	case "apple":
		setIfAbsent("METAL_DEVICE_INDEX", list)
	case "amd":
		setIfAbsent("HIP_VISIBLE_DEVICES", list)
	default:
		setIfAbsent("CUDA_VISIBLE_DEVICES", list)
	}
}

func dispatchJob(srv *schedulerServer, job *scheduler.Job, nodeName string, attempt int64) {
	dispatchStart := time.Now()
	runnable := attempt > 0
	if !runnable {
		// Cancelled between leaving the queue and being dispatched. Sending it now would run a
		// job the user was already told was cancelled.
		logging.Job(job.ID).Info("no longer runnable, skipping dispatch")
		return
	}

	srv.dispatchPendingMu.Lock()
	srv.dispatchPending[job.ID] = time.Now()
	srv.dispatchPendingMu.Unlock()

	vendor, totalGPUs := nodeGPUInfo(srv, nodeName)

	// Track resource allocation and learn which physical GPUs this job may use.
	devices, ok := srv.gpuTracker.Allocate(nodeName, job.ID, job.GPUsRequired, job.CPUsRequired, job.MemoryRequiredMB, totalGPUs)
	if !ok {
		// The node filled up between matching and dispatch. Put the job back rather than
		// sending it to a node that cannot run it.
		dispatchFailuresTotal.WithLabelValues("no_free_gpus").Inc()
		logging.Job(job.ID).Warn("node has no free GPUs at dispatch, requeueing", "node", nodeName)
		srv.appendLog(job.ID, "WARN", fmt.Sprintf("Node %s had no free GPUs at dispatch; requeued", nodeName))
		srv.dispatchPendingMu.Lock()
		delete(srv.dispatchPending, job.ID)
		srv.dispatchPendingMu.Unlock()
		srv.state.RequeueRunning(job.ID)
		return
	}

	envVars := make(map[string]string)
	for k, v := range job.EnvVars {
		envVars[k] = v
	}

	if job.GPUsRequired > 0 {
		setGPUVisibility(envVars, vendor, devices)
	}

	if err := srv.bus.Send(nodeName, &pb.DispatchMessage{
		JobId: job.ID, Command: job.Command, WalltimeSeconds: int32(job.WalltimeSeconds),
		Action: "execute", EnvVars: envVars, Attempt: attempt,
		CpusRequired: int32(job.CPUsRequired), MemoryRequiredMb: int32(job.MemoryRequiredMB),
	}); err != nil {
		// The worker is gone or wedged. Undo the placement now rather than waiting for the
		// acknowledgement timeout, so the job goes back to a node that can actually take it.
		dispatchFailuresTotal.WithLabelValues("undeliverable").Inc()
		logging.Job(job.ID).Error("could not deliver dispatch", "node", nodeName, "error", err)
		srv.appendLog(job.ID, "WARN", fmt.Sprintf("Dispatch to %s failed: %v; requeued", nodeName, err))
		srv.gpuTracker.Release(job.ID)
		srv.dispatchPendingMu.Lock()
		delete(srv.dispatchPending, job.ID)
		srv.dispatchPendingMu.Unlock()
		srv.state.RequeueRunning(job.ID)
		return
	}

	dispatchDuration.Observe(time.Since(dispatchStart).Seconds())
	if attempt == 1 && !job.SubmitTime.IsZero() {
		// Only the first attempt measures true queue wait; a retry's wait is not the same thing.
		queueWaitDuration.Observe(time.Since(job.SubmitTime).Seconds())
	}
	srv.appendLog(job.ID, "INFO", fmt.Sprintf("Dispatched to node %s", nodeName))
	logging.Job(job.ID).Info("dispatched", "node", nodeName, "attempt", attempt)
}

func walltimeEnforcer(ctx context.Context, srv *schedulerServer) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			for _, job := range srv.queue.RunningJobs() {
				if job.WalltimeSeconds <= 0 {
					continue
				}
				if time.Now().After(job.StartTime.Add(time.Duration(job.WalltimeSeconds) * time.Second)) {
					logging.Job(job.ID).Warn("walltime exceeded, killing", "walltime_seconds", job.WalltimeSeconds)
					walltimeKillsTotal.Inc()
					srv.state.Cancel(job.ID)
					if job.WorkerNode != "" {
						srv.gpuTracker.Release(job.ID)
					}
					if err := srv.bus.Send(job.WorkerNode, &pb.DispatchMessage{
						JobId: job.ID, Action: "cancel", Attempt: job.Attempt,
					}); err != nil {
						logging.Job(job.ID).Error("could not deliver walltime cancel", "node", job.WorkerNode, "error", err)
					}
					srv.appendLog(job.ID, "WARN", fmt.Sprintf("Walltime exceeded (%ds)", job.WalltimeSeconds))
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func dispatchTimeoutEnforcer(ctx context.Context, srv *schedulerServer) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			var timedOutJobs []string
			srv.dispatchPendingMu.Lock()
			for jobID, dispatchedAt := range srv.dispatchPending {
				if now.Sub(dispatchedAt) > 10*time.Second {
					timedOutJobs = append(timedOutJobs, jobID)
				}
			}
			for _, jobID := range timedOutJobs {
				delete(srv.dispatchPending, jobID)
			}
			srv.dispatchPendingMu.Unlock()

			for _, jobID := range timedOutJobs {
				job, ok := srv.queue.GetJob(jobID)
				if !ok || job.State != scheduler.StateRunning {
					continue
				}
				logging.Job(jobID).Warn("dispatch was never acknowledged, re-queueing")
				srv.appendLog(jobID, "WARN", "Dispatch handshake timed out, re-queueing")
				if _, requeued := srv.state.RequeueRunning(jobID); requeued {
					if job.WorkerNode != "" {
						srv.gpuTracker.Release(job.ID)
					}
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// --- Health Endpoints ---

func startHealthAndMetrics(srv *schedulerServer, cfg *config.Config) *http.Server {
	port := cfg.Ports.Metrics
	initMetrics()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		// Liveness reflects the scheduling loop, not just the HTTP server. A constant 200 stayed
		// green after the dispatch goroutine died, so nothing ever restarted a wedged master.
		last := srv.lastTick.Load()
		if last > 0 && time.Since(time.Unix(0, last)) > schedulerStallThreshold {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "unhealthy", "reason": "scheduling loop stalled",
				"last_tick_seconds_ago": int(time.Since(time.Unix(0, last)).Seconds()),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		members := srv.disc.Members()
		if len(members) == 0 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"status":"not_ready","reason":"no cluster members"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ready", "members": len(members),
			"queue_depth": srv.queue.QueueLen(), "draining": srv.draining.Load(),
		})
	})
	bind := cfg.MetricsBind
	if bind == "" {
		bind = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", bind, port)

	// Explicit timeouts: the zero-value http.Server has none, so a handful of connections
	// dribbling headers could exhaust the master's file descriptors.
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("health and metrics listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health/metrics server error", "error", err)
		}
	}()
	return server
}

// --- Startup ---

// StartMaster initializes and runs the master scheduler.
func StartMaster(cfg *config.Config) (*MasterHandle, error) {
	draining := &atomic.Bool{}

	// Open BoltDB store
	db, err := store.Open(config.StorePath())
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	queue := scheduler.NewGlobalScheduler()
	queue.MaxQueueSize = cfg.MaxQueueSize
	fairshare := scheduler.NewFairshareCalculator()
	if cfg.Fairshare.Enabled {
		fairshare.Weights = scheduler.FairshareWeights{
			PerCPUSecond:    cfg.Fairshare.CPUSecondWeight,
			PerGPUSecond:    cfg.Fairshare.GPUSecondWeight,
			PerGBHourMemory: cfg.Fairshare.GBHourMemWeight,
		}
		fairshare.MaxPenalty = cfg.Fairshare.MaxPenalty
	} else {
		// Zero weights mean nothing accrues, so no job is ever penalised.
		fairshare.Weights = scheduler.FairshareWeights{}
		fairshare.MaxPenalty = 0
	}

	type dbWriteOp struct {
		job   *scheduler.Job
		group *scheduler.JobGroup
	}
	dbWriteChan := make(chan dbWriteOp, 1000)
	var dbWg sync.WaitGroup

	dbWg.Add(1)
	go func() {
		defer dbWg.Done()
		for op := range dbWriteChan {
			dbWriteQueueDepth.Set(float64(len(dbWriteChan)))
			if op.job != nil {
				if err := db.SaveJob(op.job); err != nil {
					dbWriteErrorsTotal.Inc()
					logging.Job(op.job.ID).Error("failed to persist job", "error", err)
				}
			}
			if op.group != nil {
				if err := db.SaveGroup(op.group); err != nil {
					dbWriteErrorsTotal.Inc()
					slog.Error("failed to persist group", "group_id", op.group.GroupID, "error", err)
				}
			}
		}
	}()

	cleanDB := func() {
		close(dbWriteChan)
		dbWg.Wait()
		func() { _ = db.Close() }()
	}

	// Wire persistence hooks
	queue.OnJobChange = func(job *scheduler.Job) {
		dbWriteChan <- dbWriteOp{job: job}
	}
	queue.OnGroupChange = func(group *scheduler.JobGroup) {
		dbWriteChan <- dbWriteOp{group: group}
	}

	// Restore state from BoltDB
	var orphaned []string
	if jobs, err := db.LoadJobs(); err == nil {
		restored := 0
		for _, job := range jobs {
			switch job.State {
			case scheduler.StateQueued:
				if err := queue.Enqueue(job); err != nil {
					logging.Job(job.ID).Error("could not re-queue on restore", "error", err)
				}
				restored++
			case scheduler.StateRunning:
				// Do not fail it. The worker is very likely still executing this job: failing it
				// here left the process running while the new master's empty resource accounting
				// believed the node was idle, immediately oversubscribing it, and discarded the
				// real result when it eventually arrived.
				//
				// Keep the job RUNNING and let its worker claim it when it reconnects.
				// reapUnclaimedJobs fails whatever nobody claims.
				queue.RestoreRunning(job)
				orphaned = append(orphaned, job.ID)
			}
		}
		if restored > 0 {
			slog.Info("restored queued jobs from disk", "count", restored)
		}
		if len(orphaned) > 0 {
			slog.Warn("jobs were running when the master stopped; waiting for their workers to reconnect and claim them",
				"count", len(orphaned), "grace_period", adoptionGracePeriod)
		}
	}
	if groups, err := db.LoadGroups(); err == nil {
		for _, g := range groups {
			if g.State == "PENDING" || g.State == "RUNNING" {
				g.State = "FAILED" // Can't resume mid-flight groups
				if err := db.SaveGroup(g); err != nil {
					slog.Error("could not persist group on restore", "group_id", g.GroupID, "error", err)
				}
			}
			queue.RegisterGroup(g)
		}
	}
	if usage, err := db.LoadFairshare(); err == nil && len(usage) > 0 {
		fairshare.Restore(usage)
		slog.Info("restored fairshare data", "users", len(usage))
	}

	cb := newCircuitBreaker()

	cordons := ha.NewCordons()
	if saved, err := db.LoadCordons(); err == nil && len(saved) > 0 {
		restored := make(map[string]ha.CordonEntry, len(saved))
		for node, data := range saved {
			var entry ha.CordonEntry
			if err := json.Unmarshal(data, &entry); err != nil {
				slog.Warn("could not read persisted cordon", "node", node, "error", err)
				continue
			}
			restored[node] = entry
		}
		cordons.Restore(restored)
		slog.Info("restored cordoned nodes", "count", len(restored))
	}

	reservations := ha.NewReservations()
	if saved, err := db.LoadReservations(); err == nil && len(saved) > 0 {
		restored := make(map[string]ha.Reservation, len(saved))
		for id, data := range saved {
			var r ha.Reservation
			if err := json.Unmarshal(data, &r); err != nil {
				slog.Warn("could not read persisted reservation", "id", id, "error", err)
				continue
			}
			restored[id] = r
		}
		reservations.Restore(restored)
		slog.Info("restored reservations", "count", len(restored))
	}

	// Either apply state changes locally, or replicate them across masters.
	//
	// With HA off this is exactly the previous single-master behaviour and costs nothing. With it
	// on, every mutation goes through the replicated log so a surviving master can take over
	// holding the same queue.
	var stateStore ha.Store = ha.NewDirect(queue, fairshare, cordons, reservations)
	var raftNode *ha.Node
	if cfg.HA.Enabled {
		fsm := ha.NewFSM(queue, fairshare, cordons, reservations)
		raftNode, err = ha.Start(ha.Config{
			NodeID:    cfg.HA.NodeID,
			BindAddr:  cfg.HA.BindAddr,
			DataDir:   cfg.HA.DataDir,
			Peers:     cfg.HA.Peers,
			Bootstrap: cfg.HA.Bootstrap,
		}, fsm)
		if err != nil {
			cleanDB()
			return nil, fmt.Errorf("high availability: %w", err)
		}
		stateStore = ha.NewReplicated(raftNode)
		slog.Info("high availability enabled", "node_id", cfg.HA.NodeID, "peers", len(cfg.HA.Peers))
	}

	gt := newGPUTracker()

	hooks := &discovery.EventHooks{
		OnLeave: func(nodeName string) {
			jobs := queue.RunningJobsOnNode(nodeName)
			if len(jobs) == 0 {
				return
			}
			workerLostTotal.Inc()
			slog.Warn("worker left the cluster, failing its jobs", "node", nodeName, "jobs", len(jobs))
			for _, job := range jobs {
				gt.Release(job.ID)
				queue.MarkCompleted(job.ID, false, "", "worker node lost")
				if job.GroupID != "" {
					if group, ok := queue.GetGroup(job.GroupID); ok && group.State == "RUNNING" {
						queue.SetGroupState(job.GroupID, "FAILED")
					}
				}
			}
		},
	}

	gossipKey, err := cfg.GossipKey()
	if err != nil {
		return nil, err
	}
	// The master's gossip name follows node_name. It was hardcoded to "master-node", so two
	// masters — or two both-role hosts — collided under one memberlist identity.
	// The master's gossip identity derives from node_name rather than a hardcoded "master-node",
	// which made two masters — or two both-role hosts — collide under one memberlist identity.
	// The "-master" suffix keeps it distinct from the worker running alongside it in "both"
	// mode, which registers under node_name itself.
	masterGossipName := "master-node"
	if cfg.NodeName != "" {
		masterGossipName = cfg.NodeName + "-master"
	}
	disc, err := discovery.NewNodeDiscovery(masterGossipName, cfg.Ports.Gossip, nil, "", 0, hooks,
		&discovery.Options{EncryptionKey: gossipKey, Profile: cfg.Gossip.Profile})
	if err != nil {
		cleanDB()
		return nil, fmt.Errorf("discovery: %w", err)
	}

	eval, err := matchmaker.NewEvaluator()
	if err != nil {
		_ = disc.Shutdown()
		cleanDB()
		return nil, fmt.Errorf("CEL evaluator: %w", err)
	}

	// Partitions and accounts are compiled here rather than at config load, because a node
	// selector needs the same CEL compiler job requirements use. An invalid one fails the start
	// instead of silently matching no node on every cycle forever.
	pol, err := policy.New(cfg, eval)
	if err != nil {
		_ = disc.Shutdown()
		cleanDB()
		return nil, err
	}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	// Dispatch rides the authenticated gRPC connection. There is no separate broadcast socket
	// any more, so cfg.Ports.ZMQ is unused and nothing listens on it.
	bus := newDispatchBus()

	srv := &schedulerServer{
		disc: disc, queue: queue, eval: eval, bus: bus, fairshare: fairshare,
		store: db, cfg: cfg, draining: draining, cb: cb, gpuTracker: gt,
		cordons: cordons, state: stateStore, raftNode: raftNode, policy: pol, reservations: reservations,
		dispatchPending: make(map[string]time.Time),
		logStore:        make(map[string][]*pb.LogMessage), logChannels: make(map[string][]chan *pb.LogMessage),
		adopted: make(map[string]bool),
		ctx:     shutdownCtx,
	}

	if len(cfg.Partitions) > 0 || len(cfg.Accounts) > 0 {
		slog.Info("scheduling policy active",
			"partitions", len(cfg.Partitions), "accounts", len(cfg.Accounts))
	}
	// Preemption enabled with no preemptible partition acts on nothing, which looks from the
	// outside exactly like preemption being broken. Say which of the two it is.
	if cfg.Preemption.Enabled && !preemptionConfigured(cfg) {
		slog.Warn("preemption is enabled but no partition is marked preemptible, so nothing " +
			"can ever be evicted; set preemptible: true on a partition")
	} else if cfg.Preemption.Enabled {
		slog.Info("preemption active", "priority_margin", cfg.Preemption.PriorityMargin,
			"min_runtime_seconds", cfg.Preemption.MinRuntimeSeconds,
			"max_victims_per_job", cfg.Preemption.MaxVictimsPerJob)
	}

	httpServer := startHealthAndMetrics(srv, cfg)

	go dispatchLoop(shutdownCtx, srv)
	go maintenanceLoop(shutdownCtx, srv)
	go reapUnclaimedJobs(srv, orphaned)
	go walltimeEnforcer(shutdownCtx, srv)
	go dispatchTimeoutEnforcer(shutdownCtx, srv)
	halfLife := time.Duration(cfg.Fairshare.HalfLifeHours * float64(time.Hour))
	go func() {
		if !cfg.Fairshare.Enabled {
			return
		}
		ticker := time.NewTicker(fairshareInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !srv.state.IsLeader() {
					continue
				}
				if err := srv.state.DecayUsage(scheduler.DecayFactorFor(fairshareInterval, halfLife)); err != nil {
					slog.Debug("fairshare decay not replicated", "error", err)
					continue
				}

				// Recompute the penalty on jobs already waiting. It used to be frozen in at
				// submission, so a user who filled the queue and only then became the heaviest
				// consumer kept their whole backlog at its original priority — precisely the
				// case fairshare exists to handle.
				// Resolve the penalties here so every replica applies identical numbers;
				// recomputing them inside each replica would depend on apply-time state.
				penalties := make(map[string]int)
				for user := range fairshare.Snapshot() {
					penalties[user] = fairshare.CalculatePenalty(user)
				}
				if changed, err := srv.state.Reprioritize(penalties); err == nil && changed > 0 {
					slog.Debug("fairshare reprioritised queued jobs", "count", changed)
				}

				if err := db.SaveFairshare(fairshare.Snapshot()); err != nil {
					slog.Error("fairshare persist failed", "error", err)
				}
			case <-shutdownCtx.Done():
				return
			}
		}
	}()

	// gRPC server with optional TLS
	authenticator, err := auth.New(cfg)
	if err != nil {
		shutdownCancel()
		_ = disc.Shutdown()
		bus.Close()
		cleanDB()
		return nil, fmt.Errorf("auth: %w", err)
	}
	authenticator.OnFailure = func(reason string) { authFailuresTotal.WithLabelValues(reason).Inc() }

	// The HTTP API serves the same service to browsers and to anything preferring JSON. It is
	// started after the authenticator exists, because it enforces exactly the same tokens.
	apiServer := startHTTPAPI(srv, cfg, authenticator)
	if !authenticator.Enabled() {
		slog.Warn("authentication is disabled: any host that can reach the gRPC port can run "+
			"arbitrary commands on every worker; set auth.enabled in the config",
			"grpc_port", cfg.Ports.GRPC)
	}
	serverOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(authenticator.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(authenticator.StreamInterceptor()),
	}

	var grpcServer *grpc.Server
	if cfg.TLS.Enabled {
		// Refuse to start rather than silently serving plaintext. A missing cert or key used to
		// fall through to an unencrypted listener while the startup banner still announced TLS,
		// so an operator had no way to notice the downgrade.
		if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
			shutdownCancel()
			_ = disc.Shutdown()
			bus.Close()
			cleanDB()
			return nil, fmt.Errorf("tls.enabled is true but tls.cert_file and tls.key_file must both be set")
		}
		creds, err := credentials.NewServerTLSFromFile(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			shutdownCancel()
			_ = disc.Shutdown()
			bus.Close()
			cleanDB()
			return nil, fmt.Errorf("TLS: %w", err)
		}
		// Require and verify client certificates when a CA is configured. Without this the
		// connection is encrypted but every client is still anonymous, which is what the docs
		// called "mTLS" while the master never asked for a certificate at all.
		if cfg.TLS.CAFile != "" {
			pool := x509.NewCertPool()
			caPEM, readErr := os.ReadFile(cfg.TLS.CAFile)
			if readErr != nil {
				shutdownCancel()
				_ = disc.Shutdown()
				bus.Close()
				cleanDB()
				return nil, fmt.Errorf("TLS: cannot read ca_file %s: %w", cfg.TLS.CAFile, readErr)
			}
			if !pool.AppendCertsFromPEM(caPEM) {
				shutdownCancel()
				_ = disc.Shutdown()
				bus.Close()
				cleanDB()
				return nil, fmt.Errorf("TLS: ca_file %s contains no usable certificates", cfg.TLS.CAFile)
			}
			cert, certErr := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if certErr != nil {
				shutdownCancel()
				_ = disc.Shutdown()
				bus.Close()
				cleanDB()
				return nil, fmt.Errorf("TLS: %w", certErr)
			}
			creds = credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{cert},
				ClientCAs:    pool,
				ClientAuth:   tls.RequireAndVerifyClientCert,
				MinVersion:   tls.VersionTLS12,
			})
			slog.Info("mutual TLS enabled: client certificates required and verified", "ca_file", cfg.TLS.CAFile)
		}
		serverOpts = append(serverOpts, grpc.Creds(creds))
		grpcServer = grpc.NewServer(serverOpts...)
	} else {
		grpcServer = grpc.NewServer(serverOpts...)
	}
	pb.RegisterSchedulerServiceServer(grpcServer, srv)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Ports.GRPC))
	if err != nil {
		shutdownCancel()
		_ = disc.Shutdown()
		bus.Close()
		cleanDB()
		return nil, fmt.Errorf("gRPC listen: %w", err)
	}

	go func() {
		// Join the other masters' gossip so every master sees the same workers. Without this each
		// master forms its own membership view, and a leader can only dispatch to the workers that
		// happened to join it.
		if len(cfg.Gossip.Join) > 0 {
			go func() {
				// Retry: peers may still be starting.
				for attempt := 0; attempt < 10; attempt++ {
					if err := disc.Join(cfg.Gossip.Join); err == nil {
						return
					} else if attempt == 9 {
						slog.Warn("could not join the gossip cluster; this master may not see all workers",
							"seeds", cfg.Gossip.Join, "error", err)
					}
					select {
					case <-time.After(2 * time.Second):
					case <-shutdownCtx.Done():
						return
					}
				}
			}()
		}

		slog.Info("master listening",
			"grpc_port", cfg.Ports.GRPC, "tls", cfg.TLS.Enabled,
			"gossip_port", cfg.Ports.Gossip, "metrics_port", cfg.Ports.Metrics)
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("gRPC server error", "error", err)
		}
	}()

	return &MasterHandle{
		Cancel: func() {
			shutdownCancel()
			grpcServer.GracefulStop()
			// The health/metrics server was never shut down: its goroutine and listener outlived
			// the master, the port stayed bound so an in-process restart could not rebind, and
			// /acknowledge_start kept mutating scheduler state after the scheduler was gone.
			shutdownHTTP, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
			if err := httpServer.Shutdown(shutdownHTTP); err != nil {
				slog.Error("health/metrics server shutdown", "error", err)
			}
			cancelHTTP()
			if apiServer != nil {
				shutdownAPI, cancelAPI := context.WithTimeout(context.Background(), 5*time.Second)
				if err := apiServer.Shutdown(shutdownAPI); err != nil {
					slog.Error("http api server shutdown", "error", err)
				}
				cancelAPI()
			}
			bus.Close()
			_ = disc.Shutdown()
			cleanDB()
		},
		Draining: draining,
		Queue:    queue,
	}, nil
}
