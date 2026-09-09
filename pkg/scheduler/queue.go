package scheduler

import (
	"container/heap"
	"fmt"
	"math"
	"sync"
	"time"
)

// Job states
const (
	StateQueued    = "QUEUED"
	StateRunning   = "RUNNING"
	StateCompleted = "COMPLETED"
	StateFailed    = "FAILED"
	StateCancelled = "CANCELLED"
)

// Job represents a single unit of work in the scheduler queue.
type Job struct {
	ID              string    `json:"id"`
	Requirement     string    `json:"requirement"`
	Command         string    `json:"command"`
	SubmitTime      time.Time `json:"submit_time"`
	Priority        int       `json:"priority"` // Lower integer = higher priority
	User            string    `json:"user"`
	WalltimeSeconds int       `json:"walltime_seconds"` // Max execution time; 0 = no limit

	// GPU and resource fields
	GPUsRequired     int               `json:"gpus_required"`
	CPUsRequired     int               `json:"cpus_required"`
	MemoryRequiredMB int               `json:"memory_required_mb"`
	EnvVars          map[string]string `json:"env_vars,omitempty"`

	// Retry
	MaxRetries int `json:"max_retries"`
	RetryCount int `json:"retry_count"`

	// Distributed job group
	GroupID string `json:"group_id,omitempty"`

	// Runtime state
	State      string    `json:"state"`
	WorkerNode string    `json:"worker_node"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	Output     string    `json:"output"`
	Error      string    `json:"error"`

	// Internal tracking for heap
	index int

	// BasePriority is the priority the submitter asked for, before any fairshare penalty. It is
	// kept so the penalty can be recomputed while a job waits without compounding on itself.
	BasePriority int `json:"base_priority,omitempty"`

	// Attempt increments on every dispatch. It is the fencing token: a result carrying an
	// older attempt belongs to a superseded dispatch and must be ignored, or a late report
	// from a previously targeted worker releases the allocation of whichever node is running
	// the job now.
	Attempt int64 `json:"attempt,omitempty"`
}

// JobGroup represents a distributed training job spanning multiple nodes.
type JobGroup struct {
	GroupID     string    `json:"group_id"`
	JobIDs      []string  `json:"job_ids"`
	NumNodes    int       `json:"num_nodes"`
	GPUsPerNode int       `json:"gpus_per_node"`
	MasterPort  int       `json:"master_port"`
	State       string    `json:"state"` // PENDING, RUNNING, COMPLETED, FAILED
	CreatedAt   time.Time `json:"created_at"`
}

// Copy returns a deep copy of the Job.
func (j *Job) Copy() *Job {
	if j == nil {
		return nil
	}
	copied := *j
	if j.EnvVars != nil {
		copied.EnvVars = make(map[string]string, len(j.EnvVars))
		for k, v := range j.EnvVars {
			copied.EnvVars[k] = v
		}
	}
	return &copied
}

// Copy returns a deep copy of the JobGroup.
func (g *JobGroup) Copy() *JobGroup {
	if g == nil {
		return nil
	}
	copied := *g
	if g.JobIDs != nil {
		copied.JobIDs = make([]string, len(g.JobIDs))
		copy(copied.JobIDs, g.JobIDs)
	}
	return &copied
}

// JobQueue implements heap.Interface and holds Jobs.
type JobQueue []*Job

func (jq JobQueue) Len() int { return len(jq) }

func (jq JobQueue) Less(i, j int) bool {
	if jq[i].Priority == jq[j].Priority {
		return jq[i].SubmitTime.Before(jq[j].SubmitTime)
	}
	return jq[i].Priority < jq[j].Priority
}

func (jq JobQueue) Swap(i, j int) {
	jq[i], jq[j] = jq[j], jq[i]
	jq[i].index = i
	jq[j].index = j
}

func (jq *JobQueue) Push(x interface{}) {
	n := len(*jq)
	item := x.(*Job)
	item.index = n
	*jq = append(*jq, item)
}

func (jq *JobQueue) Pop() interface{} {
	old := *jq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*jq = old[0 : n-1]
	return item
}

// GlobalScheduler manages the job queue, job state, and job groups.
type GlobalScheduler struct {
	mu           sync.Mutex
	queue        JobQueue
	jobs         map[string]*Job
	groups       map[string]*JobGroup
	MaxQueueSize int // 0 = unlimited

	// Persistence hooks (called outside lock via deferred calls)
	OnJobChange   func(job *Job)
	OnGroupChange func(group *JobGroup)
}

// NewGlobalScheduler initializes a new Global Scheduler.
func NewGlobalScheduler() *GlobalScheduler {
	gs := &GlobalScheduler{
		queue:  make(JobQueue, 0),
		jobs:   make(map[string]*Job),
		groups: make(map[string]*JobGroup),
	}
	heap.Init(&gs.queue)
	return gs
}

// Enqueue adds a job to the priority queue with QUEUED state.
// Returns error if queue is full.
func (gs *GlobalScheduler) Enqueue(job *Job) error {
	gs.mu.Lock()
	if gs.MaxQueueSize > 0 && gs.queue.Len() >= gs.MaxQueueSize {
		gs.mu.Unlock()
		return fmt.Errorf("queue full (%d jobs)", gs.MaxQueueSize)
	}
	// Reject a duplicate ID rather than overwriting. Both gs.jobs and the BoltDB bucket were
	// last-write-wins, so a collision silently cross-linked two users' jobs: results, logs, and
	// resource releases landed on the wrong one.
	if _, exists := gs.jobs[job.ID]; exists {
		gs.mu.Unlock()
		return fmt.Errorf("job %s already exists", job.ID)
	}
	job.State = StateQueued
	gs.jobs[job.ID] = job
	heap.Push(&gs.queue, job)
	snapshot := job.Copy()
	gs.mu.Unlock()
	gs.notifyJobChange(snapshot)
	return nil
}

// Dequeue removes and returns the highest priority job.
func (gs *GlobalScheduler) Dequeue() *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.queue.Len() == 0 {
		return nil
	}
	return heap.Pop(&gs.queue).(*Job).Copy()
}

// DequeueIf pops the highest-priority job, but only if match reports true for it.
//
// The test and the pop happen under one lock. Callers previously did this as a Peek, then some
// matching work, then a Dequeue — three separate lock acquisitions — so a job submitted or
// cancelled in between changed the head, and the Dequeue returned a job that had never been
// matched against the node it was about to be sent to.
//
// match runs while the queue lock is held; keep it as short as the matching allows.
func (gs *GlobalScheduler) DequeueIf(match func(job *Job) bool) *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.queue.Len() == 0 {
		return nil
	}
	if !match(gs.queue[0].Copy()) {
		return nil
	}
	return heap.Pop(&gs.queue).(*Job).Copy()
}

// Peek returns the highest priority job without removing it.
func (gs *GlobalScheduler) Peek() *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.queue.Len() == 0 {
		return nil
	}
	return gs.queue[0].Copy()
}

// QueueLen returns the number of jobs currently in the queue.
func (gs *GlobalScheduler) QueueLen() int {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return gs.queue.Len()
}

// Backfill finds the first queued job that satisfies matchFunc,
// removes it from the queue, and returns it.
func (gs *GlobalScheduler) Backfill(matchFunc func(job *Job) bool) *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	for i, job := range gs.queue {
		if matchFunc(job.Copy()) {
			heap.Remove(&gs.queue, i)
			return job.Copy()
		}
	}
	return nil
}

// FindQueued returns a copy of the first queued job satisfying match, without removing it.
//
// Selection and removal are separate because the removal has to be replicated as a decision
// about a specific job: the predicate closes over live cluster state and cannot travel in a log
// entry.
func (gs *GlobalScheduler) FindQueued(match func(job *Job) bool) *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	for _, job := range gs.queue {
		if match(job.Copy()) {
			return job.Copy()
		}
	}
	return nil
}

// RemoveByID removes a QUEUED job from the queue by ID and returns it.
func (gs *GlobalScheduler) RemoveByID(jobID string) *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	job, ok := gs.jobs[jobID]
	if !ok || job.State != StateQueued {
		return nil
	}
	if job.index >= 0 && job.index < gs.queue.Len() {
		heap.Remove(&gs.queue, job.index)
	}
	return job.Copy()
}

// Cancel removes a job from the queue if QUEUED, or marks it CANCELLED if RUNNING.
func (gs *GlobalScheduler) Cancel(jobID string) (*Job, bool) {
	gs.mu.Lock()

	job, exists := gs.jobs[jobID]
	if !exists {
		gs.mu.Unlock()
		return nil, false
	}

	switch job.State {
	case StateQueued:
		if job.index >= 0 && job.index < gs.queue.Len() {
			heap.Remove(&gs.queue, job.index)
		}
		job.State = StateCancelled
		job.EndTime = time.Now()
		snapshot := job.Copy()
		gs.mu.Unlock()
		gs.notifyJobChange(snapshot)
		return snapshot.Copy(), true
	case StateRunning:
		job.State = StateCancelled
		job.EndTime = time.Now()
		snapshot := job.Copy()
		gs.mu.Unlock()
		gs.notifyJobChange(snapshot)
		return snapshot.Copy(), true
	default:
		gs.mu.Unlock()
		return job.Copy(), false
	}
}

// Requeue resets the job's runtime fields and enqueues it again.
func (gs *GlobalScheduler) Requeue(jobID string, incrementRetry bool) (*Job, error) {
	gs.mu.Lock()
	job, ok := gs.jobs[jobID]
	if !ok {
		gs.mu.Unlock()
		return nil, fmt.Errorf("job not found")
	}
	// A job already sitting in the heap must not be pushed onto it a second time. Two heap
	// entries share one *Job, so job.index names only one of them: a later heap.Remove using
	// that index evicts an unrelated job, which is then silently lost while still marked
	// QUEUED. Duplicate result reports made this reachable in practice.
	if job.State == StateQueued {
		gs.mu.Unlock()
		return nil, fmt.Errorf("job %s is already queued", jobID)
	}
	if job.State == StateCancelled {
		gs.mu.Unlock()
		return nil, fmt.Errorf("job %s was cancelled", jobID)
	}
	if incrementRetry {
		job.RetryCount++
	}
	job.State = StateQueued
	job.WorkerNode = ""
	job.StartTime = time.Time{}
	job.EndTime = time.Time{}
	job.Output = ""
	job.Error = ""
	job.index = -1 // Reset heap index

	heap.Push(&gs.queue, job)
	snapshot := job.Copy()
	gs.mu.Unlock()

	gs.notifyJobChange(snapshot)
	return snapshot.Copy(), nil
}

// notifyJobChange runs the persistence hook. It must be called with a snapshot, never with
// the live *Job from gs.jobs: the hook hands the value to another goroutine that marshals it
// while the scheduler keeps mutating the original.
func (gs *GlobalScheduler) notifyJobChange(job *Job) {
	if gs.OnJobChange != nil {
		gs.OnJobChange(job)
	}
}

// notifyGroupChange runs the persistence hook. As with notifyJobChange, the argument must be
// a snapshot rather than the live *JobGroup.
func (gs *GlobalScheduler) notifyGroupChange(group *JobGroup) {
	if gs.OnGroupChange != nil {
		gs.OnGroupChange(group)
	}
}

// RestoreRunning re-inserts a job that was RUNNING when the master stopped, without touching
// its state.
//
// The job is not queued: it is presumed to still be executing on its worker, and stays RUNNING
// until either that worker reconnects and claims it or the grace period expires.
func (gs *GlobalScheduler) RestoreRunning(job *Job) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	job.index = -1
	gs.jobs[job.ID] = job
}

// AdoptRunning restores a job to RUNNING on a node, as reported by a worker after a master
// restart.
//
// It is deliberately not MarkRunning: no new attempt is allocated, because the dispatch that
// started this job already happened and its fencing token must be preserved — the worker will
// report its result carrying that token, and a fresh one would make the result look stale and
// get it discarded.
//
// Returns false if the job is unknown or has already reached a terminal state, so a worker
// reporting stale work cannot resurrect a job a user has since cancelled.
func (gs *GlobalScheduler) AdoptRunning(jobID, workerNode string, attempt int64, startTime time.Time) bool {
	gs.mu.Lock()

	job, ok := gs.jobs[jobID]
	if !ok {
		gs.mu.Unlock()
		return false
	}
	if job.State == StateCancelled || job.State == StateCompleted {
		gs.mu.Unlock()
		return false
	}
	// A job the worker claims must not still be sitting in the queue.
	if job.State == StateQueued && job.index >= 0 && job.index < gs.queue.Len() {
		heap.Remove(&gs.queue, job.index)
	}

	job.State = StateRunning
	job.WorkerNode = workerNode
	job.Error = ""
	job.EndTime = time.Time{}
	if attempt > job.Attempt {
		job.Attempt = attempt
	}
	if !startTime.IsZero() {
		job.StartTime = startTime
	} else if job.StartTime.IsZero() {
		job.StartTime = time.Now()
	}
	snapshot := job.Copy()
	gs.mu.Unlock()

	gs.notifyJobChange(snapshot)
	return true
}

// MarkRunning transitions a job to RUNNING state, returning the dispatch attempt number.
//
// ok is false for an unknown job and for one that was cancelled after being dequeued; callers
// must not dispatch in that case. The attempt increments on every successful transition and
// must be carried through the dispatch and echoed back in the result.
func (gs *GlobalScheduler) MarkRunning(jobID, workerNode string) (attempt int64, ok bool) {
	gs.mu.Lock()
	job, ok := gs.jobs[jobID]
	// Dequeue pops a job without changing its state, so a cancel can land between the pop and
	// this call. Overwriting CANCELLED with RUNNING loses the cancellation entirely: the user
	// is told the job was cancelled, no cancel is ever published to a worker, and the job runs
	// to completion anyway.
	if ok && job.State == StateCancelled {
		ok = false
	}
	var snapshot *Job
	if ok {
		job.State = StateRunning
		job.WorkerNode = workerNode
		job.StartTime = time.Now()
		job.Attempt++
		attempt = job.Attempt
		snapshot = job.Copy()
	}
	gs.mu.Unlock()
	if ok {
		gs.notifyJobChange(snapshot)
	}
	return attempt, ok
}

// RequeueRunningJob transitions a RUNNING job back to QUEUED state and pushes it back onto the heap.
func (gs *GlobalScheduler) RequeueRunningJob(jobID string) (*Job, bool) {
	gs.mu.Lock()
	job, ok := gs.jobs[jobID]
	if !ok || job.State != StateRunning {
		gs.mu.Unlock()
		return nil, false
	}
	job.State = StateQueued
	job.WorkerNode = ""
	job.StartTime = time.Time{}
	heap.Push(&gs.queue, job)
	snapshot := job.Copy()
	gs.mu.Unlock()
	gs.notifyJobChange(snapshot)
	return snapshot.Copy(), true
}

// MarkCompleted transitions a job to COMPLETED or FAILED state.
func (gs *GlobalScheduler) MarkCompleted(jobID string, success bool, output, errMsg string) {
	gs.mu.Lock()
	job, ok := gs.jobs[jobID]
	if !ok {
		gs.mu.Unlock()
		return
	}
	if job.State == StateCancelled {
		gs.mu.Unlock()
		return
	}
	if success {
		job.State = StateCompleted
	} else {
		job.State = StateFailed
	}
	job.Output = output
	job.Error = errMsg
	job.EndTime = time.Now()
	snapshot := job.Copy()
	gs.mu.Unlock()
	gs.notifyJobChange(snapshot)
}

// GetJob returns a job by ID.
func (gs *GlobalScheduler) GetJob(jobID string) (*Job, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	job, ok := gs.jobs[jobID]
	if !ok {
		return nil, false
	}
	return job.Copy(), true
}

// ListJobs returns all tracked jobs, optionally filtered by state.
func (gs *GlobalScheduler) ListJobs(stateFilter string) []*Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var result []*Job
	for _, job := range gs.jobs {
		if stateFilter == "" || job.State == stateFilter {
			result = append(result, job.Copy())
		}
	}
	return result
}

// RunningJobs returns all jobs currently in RUNNING state.
func (gs *GlobalScheduler) RunningJobs() []*Job {
	return gs.ListJobs(StateRunning)
}

// RunningJobsOnNode returns all RUNNING jobs assigned to a specific worker node.
func (gs *GlobalScheduler) RunningJobsOnNode(nodeName string) []*Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var result []*Job
	for _, job := range gs.jobs {
		if job.State == StateRunning && job.WorkerNode == nodeName {
			result = append(result, job.Copy())
		}
	}
	return result
}

// IsTerminal reports whether a state is final.
func IsTerminal(state string) bool {
	return state == StateCompleted || state == StateFailed || state == StateCancelled
}

// PruneTerminal drops finished jobs that ended more than maxAge ago, returning how many went.
//
// Nothing ever left gs.jobs, so the map grew for the master's lifetime with every job ever
// submitted — each retaining its full captured output. That is both an unbounded memory leak
// and a latency problem: RunningJobs and ListJobs walk this map under the global lock, and the
// dispatch loop calls RunningJobs once a second.
//
// Pruning only affects the in-memory view. The database keeps the durable history, and
// GetJobStatus already falls back to it for a job that is no longer resident.
func (gs *GlobalScheduler) PruneTerminal(maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)

	gs.mu.Lock()
	defer gs.mu.Unlock()

	pruned := 0
	for id, job := range gs.jobs {
		if !IsTerminal(job.State) {
			continue
		}
		// A terminal job with no end time is malformed; treat it as prunable.
		if !job.EndTime.IsZero() && job.EndTime.After(cutoff) {
			continue
		}
		delete(gs.jobs, id)
		pruned++
	}
	return pruned
}

// --- Job Group Management ---

// RegisterGroup registers a new job group for distributed training.
func (gs *GlobalScheduler) RegisterGroup(g *JobGroup) {
	gs.mu.Lock()
	gs.groups[g.GroupID] = g
	snapshot := g.Copy()
	gs.mu.Unlock()
	gs.notifyGroupChange(snapshot)
}

// GetGroup returns a job group by ID.
func (gs *GlobalScheduler) GetGroup(groupID string) (*JobGroup, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	g, ok := gs.groups[groupID]
	if !ok {
		return nil, false
	}
	return g.Copy(), true
}

// PendingGroups returns all groups in PENDING state.
func (gs *GlobalScheduler) PendingGroups() []*JobGroup {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var result []*JobGroup
	for _, g := range gs.groups {
		if g.State == "PENDING" {
			result = append(result, g.Copy())
		}
	}
	return result
}

// SetGroupState updates a group's state.
func (gs *GlobalScheduler) SetGroupState(groupID, state string) {
	gs.mu.Lock()
	g, ok := gs.groups[groupID]
	var snapshot *JobGroup
	if ok {
		g.State = state
		snapshot = g.Copy()
	}
	gs.mu.Unlock()
	if ok {
		gs.notifyGroupChange(snapshot)
	}
}

// ReprioritizeQueued recomputes the priority of every queued job and restores heap order.
//
// The fairshare penalty was applied once, at submission, and frozen into the job. A user who
// filled the queue and only then became the heaviest consumer kept their whole backlog at the
// priority it was submitted with, so fairshare had no effect on exactly the case it exists for.
//
// penaltyFor receives a job and returns its new penalty; base priority is preserved separately
// on the job so penalties do not compound across calls.
func (gs *GlobalScheduler) ReprioritizeQueued(penaltyFor func(job *Job) int) int {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	changed := 0
	for _, job := range gs.queue {
		want := job.BasePriority + penaltyFor(job)
		if want != job.Priority {
			job.Priority = want
			changed++
		}
	}
	if changed > 0 {
		// Priorities moved arbitrarily, so rebuild rather than sifting individual entries.
		heap.Init(&gs.queue)
	}
	return changed
}

// ListGroups returns a copy of every registered job group.
func (gs *GlobalScheduler) ListGroups() []*JobGroup {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	out := make([]*JobGroup, 0, len(gs.groups))
	for _, g := range gs.groups {
		out = append(out, g.Copy())
	}
	return out
}

// Reset replaces all scheduler state, rebuilding the queue from the supplied jobs.
//
// Used when a replica loads a snapshot: the heap is derived from the job set rather than being
// part of the snapshot, since its internal array order is an implementation detail and would
// otherwise have to stay byte-identical across replicas.
func (gs *GlobalScheduler) Reset(jobs []*Job, groups []*JobGroup) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	gs.jobs = make(map[string]*Job, len(jobs))
	gs.groups = make(map[string]*JobGroup, len(groups))
	gs.queue = make(JobQueue, 0, len(jobs))

	for _, job := range jobs {
		restored := job.Copy()
		restored.index = -1
		gs.jobs[restored.ID] = restored
		if restored.State == StateQueued {
			gs.queue = append(gs.queue, restored)
		}
	}
	heap.Init(&gs.queue)

	for _, g := range groups {
		gs.groups[g.GroupID] = g.Copy()
	}
}

// --- Fairshare ---

// FairshareWeights convert a job's resources into billable usage.
//
// Usage was previously raw wall-clock seconds, so a job holding 64 GPUs accrued exactly as much
// as one holding a single CPU core. A user could saturate the cluster's accelerators and be
// charged the same as someone running `sleep`. These weights are the equivalent of Slurm's TRES
// billing: a GPU-second costs far more than a CPU-second because the GPU is what is scarce.
type FairshareWeights struct {
	PerCPUSecond    float64
	PerGPUSecond    float64
	PerGBHourMemory float64
}

// DefaultFairshareWeights charges a GPU-second like 32 CPU-seconds, reflecting how much scarcer
// accelerators are on the clusters Tasch targets.
func DefaultFairshareWeights() FairshareWeights {
	return FairshareWeights{PerCPUSecond: 1, PerGPUSecond: 32, PerGBHourMemory: 0.25}
}

// FairshareCalculator assesses a user's priority penalty based on past usage.
//
// The penalty is derived from a user's *share* of recent cluster usage rather than an absolute
// number of seconds. A share is scale-invariant: it means the same thing on a two-node cluster
// and a two-hundred-node one, whereas the previous `seconds/100` produced no useful penalty at
// all on a small cluster and an overwhelming one on a large busy cluster.
type FairshareCalculator struct {
	mu        sync.Mutex
	UserUsage map[string]float64

	// Weights and MaxPenalty are set once at construction.
	Weights    FairshareWeights
	MaxPenalty int
}

// NewFairshareCalculator creates a new calculator.
func NewFairshareCalculator() *FairshareCalculator {
	return &FairshareCalculator{
		UserUsage:  make(map[string]float64),
		Weights:    DefaultFairshareWeights(),
		MaxPenalty: DefaultMaxFairsharePenalty,
	}
}

// RecordUsage adds a completed job's billable usage to a user's account.
//
// seconds is the job's wall-clock duration; the remaining arguments are what it held for that
// duration.
func (fc *FairshareCalculator) RecordUsage(user string, seconds float64, cpus, gpus, memMB int) {
	if seconds <= 0 {
		return
	}
	w := fc.Weights

	// A job that reserved nothing explicitly still consumed a machine, so bill it as one core
	// rather than as free.
	billableCPUs := float64(cpus)
	if billableCPUs <= 0 {
		billableCPUs = 1
	}

	units := seconds * billableCPUs * w.PerCPUSecond
	units += seconds * float64(gpus) * w.PerGPUSecond
	units += (seconds / 3600) * (float64(memMB) / 1024) * w.PerGBHourMemory

	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.UserUsage[user] += units
}

// DefaultMaxFairsharePenalty bounds how much fairshare can add to a job's priority number.
// Priority is "lower is better", so this is how far a heavy user's jobs can be pushed back.
const DefaultMaxFairsharePenalty = 50

// fairshareForgetThreshold is the usage below which an account is dropped entirely.
const fairshareForgetThreshold = 0.001

// Snapshot returns a copy of the usage map, safe to marshal from another goroutine.
//
// UserUsage is exported and was previously handed to the persistence layer directly. Because
// RecordUsage writes it under fc.mu while the 60s persistence tick marshaled it without the
// lock, any job completing during that tick crashed the master with an unrecoverable
// "concurrent map read and map write" fatal error.
func (fc *FairshareCalculator) Snapshot() map[string]float64 {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	out := make(map[string]float64, len(fc.UserUsage))
	for user, usage := range fc.UserUsage {
		out[user] = usage
	}
	return out
}

// Restore replaces the usage map, e.g. from persisted state at startup.
func (fc *FairshareCalculator) Restore(usage map[string]float64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.UserUsage = make(map[string]float64, len(usage))
	for user, u := range usage {
		fc.UserUsage[user] = u
	}
}

// CalculatePenalty returns the priority penalty for a user, from 0 to MaxPenalty.
//
// The penalty scales with the user's share of total recent usage, so it reflects how much of
// the cluster they have been taking relative to everyone else. When one user is alone on the
// cluster their share is 1 and everyone gets the same penalty — which is correct, since
// fairshare only orders users against each other.
func (fc *FairshareCalculator) CalculatePenalty(user string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	usage, exists := fc.UserUsage[user]
	if !exists || usage <= 0 {
		return 0
	}

	var total float64
	for _, u := range fc.UserUsage {
		total += u
	}
	if total <= 0 {
		return 0
	}

	maxPenalty := fc.MaxPenalty
	if maxPenalty <= 0 {
		maxPenalty = DefaultMaxFairsharePenalty
	}
	return int((usage / total) * float64(maxPenalty))
}

// DecayUsage reduces all usage by a factor, ageing out old activity.
func (fc *FairshareCalculator) DecayUsage(factor float64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for user, usage := range fc.UserUsage {
		decayed := usage * factor
		// Drop accounts that have decayed to nothing, so a cluster that has seen many one-off
		// users does not accumulate entries forever.
		if decayed < fairshareForgetThreshold {
			delete(fc.UserUsage, user)
			continue
		}
		fc.UserUsage[user] = decayed
	}
}

// DecayFactorFor returns the multiplier that halves usage over halfLife when applied once per
// interval.
//
// Decay was previously a hardcoded 0.95 per minute, a half-life of about thirteen minutes. That
// is far too short to be fairshare: a user could saturate the cluster all morning and carry no
// penalty by lunchtime. A half-life measured in days is the norm.
func DecayFactorFor(interval, halfLife time.Duration) float64 {
	if halfLife <= 0 || interval <= 0 {
		return 1
	}
	return math.Pow(0.5, interval.Seconds()/halfLife.Seconds())
}
