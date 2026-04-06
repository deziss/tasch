package scheduler

import (
	"container/heap"
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
	GPUsRequired int               `json:"gpus_required"`
	EnvVars      map[string]string `json:"env_vars,omitempty"`

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
}

// JobGroup represents a distributed training job spanning multiple nodes.
type JobGroup struct {
	GroupID     string   `json:"group_id"`
	JobIDs      []string `json:"job_ids"`
	NumNodes    int      `json:"num_nodes"`
	GPUsPerNode int      `json:"gpus_per_node"`
	MasterPort  int      `json:"master_port"`
	State       string   `json:"state"` // PENDING, RUNNING, COMPLETED, FAILED
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
	mu     sync.Mutex
	queue  JobQueue
	jobs   map[string]*Job
	groups map[string]*JobGroup
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
func (gs *GlobalScheduler) Enqueue(job *Job) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	job.State = StateQueued
	gs.jobs[job.ID] = job
	heap.Push(&gs.queue, job)
}

// Dequeue removes and returns the highest priority job.
func (gs *GlobalScheduler) Dequeue() *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.queue.Len() == 0 {
		return nil
	}
	return heap.Pop(&gs.queue).(*Job)
}

// Peek returns the highest priority job without removing it.
func (gs *GlobalScheduler) Peek() *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.queue.Len() == 0 {
		return nil
	}
	return gs.queue[0]
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
		if matchFunc(job) {
			heap.Remove(&gs.queue, i)
			return job
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
	return job
}

// Cancel removes a job from the queue if QUEUED, or marks it CANCELLED if RUNNING.
func (gs *GlobalScheduler) Cancel(jobID string) (*Job, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	job, exists := gs.jobs[jobID]
	if !exists {
		return nil, false
	}

	switch job.State {
	case StateQueued:
		if job.index >= 0 && job.index < gs.queue.Len() {
			heap.Remove(&gs.queue, job.index)
		}
		job.State = StateCancelled
		job.EndTime = time.Now()
		return job, true
	case StateRunning:
		job.State = StateCancelled
		job.EndTime = time.Now()
		return job, true
	default:
		return job, false
	}
}

// MarkRunning transitions a job to RUNNING state.
func (gs *GlobalScheduler) MarkRunning(jobID, workerNode string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if job, ok := gs.jobs[jobID]; ok {
		job.State = StateRunning
		job.WorkerNode = workerNode
		job.StartTime = time.Now()
	}
}

// MarkCompleted transitions a job to COMPLETED or FAILED state.
func (gs *GlobalScheduler) MarkCompleted(jobID string, success bool, output, errMsg string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	job, ok := gs.jobs[jobID]
	if !ok {
		return
	}
	if job.State == StateCancelled {
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
}

// GetJob returns a job by ID.
func (gs *GlobalScheduler) GetJob(jobID string) (*Job, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	job, ok := gs.jobs[jobID]
	return job, ok
}

// ListJobs returns all tracked jobs, optionally filtered by state.
func (gs *GlobalScheduler) ListJobs(stateFilter string) []*Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var result []*Job
	for _, job := range gs.jobs {
		if stateFilter == "" || job.State == stateFilter {
			result = append(result, job)
		}
	}
	return result
}

// RunningJobs returns all jobs currently in RUNNING state.
func (gs *GlobalScheduler) RunningJobs() []*Job {
	return gs.ListJobs(StateRunning)
}

// --- Job Group Management ---

// RegisterGroup registers a new job group for distributed training.
func (gs *GlobalScheduler) RegisterGroup(g *JobGroup) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	gs.groups[g.GroupID] = g
}

// GetGroup returns a job group by ID.
func (gs *GlobalScheduler) GetGroup(groupID string) (*JobGroup, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	g, ok := gs.groups[groupID]
	return g, ok
}

// PendingGroups returns all groups in PENDING state.
func (gs *GlobalScheduler) PendingGroups() []*JobGroup {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var result []*JobGroup
	for _, g := range gs.groups {
		if g.State == "PENDING" {
			result = append(result, g)
		}
	}
	return result
}

// SetGroupState updates a group's state.
func (gs *GlobalScheduler) SetGroupState(groupID, state string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if g, ok := gs.groups[groupID]; ok {
		g.State = state
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
