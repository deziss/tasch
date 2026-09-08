package scheduler

import (
	"container/heap"
	"fmt"
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

// --- Fairshare ---

// FairshareCalculator assesses a user's priority penalty based on past usage.
type FairshareCalculator struct {
	mu        sync.Mutex
	UserUsage map[string]float64
}

// NewFairshareCalculator creates a new calculator.
func NewFairshareCalculator() *FairshareCalculator {
	return &FairshareCalculator{
		UserUsage: make(map[string]float64),
	}
}

// RecordUsage adds resource usage for a user.
func (fc *FairshareCalculator) RecordUsage(user string, units float64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.UserUsage[user] += units
}

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

// CalculatePenalty assigns a numerical penalty to base priority.
func (fc *FairshareCalculator) CalculatePenalty(user string) int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	usage, exists := fc.UserUsage[user]
	if !exists {
		return 0
	}
	return int(usage / 100)
}

// DecayUsage reduces all user usage by a factor (called periodically).
func (fc *FairshareCalculator) DecayUsage(factor float64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for user := range fc.UserUsage {
		fc.UserUsage[user] *= factor
	}
}
