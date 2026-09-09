package scheduler

import (
	"container/heap"
	"fmt"
	"strings"
	"time"
)

// Job dependencies and array jobs.
//
// Both are answers to the same question: which of the queued jobs is actually allowed to start
// right now? Priority alone cannot express "not until the preprocessing finishes" or "no more
// than five of these thousand at a time", so users express them by submitting in waves from a
// shell script and watching. The scheduler is the only thing that can see the whole queue, so
// it is the only thing that can do this without a human in the loop.
//
// Eligibility is derived, never stored. A job's dependencies are satisfied or not according to
// the state of the jobs it names, and an array's throttle is satisfied or not according to how
// many of its siblings are running. Deriving it means there is no "unblock" event to miss and
// no blocked-state bookkeeping that can drift out of step with reality.

// Eligibility says whether a queued job may be dispatched.
type Eligibility int

const (
	// EligibleNow: nothing is holding the job back.
	EligibleNow Eligibility = iota
	// EligibleWaiting: a dependency has not finished, or the array is at its concurrency limit.
	EligibleWaiting
	// EligibleDoomed: a dependency ended in a way this job can never be released by, so it will
	// never run and should be failed rather than left in the queue forever.
	EligibleDoomed
)

// eligibilityLocked reports whether a queued job may start, and why not if it may not.
//
// Callers must hold gs.mu. The queue's own selection paths call it while iterating, which is
// why it takes no lock of its own.
//
// arrayRunning maps array ID to how many of its tasks are running. Callers that scan the queue
// pass one built by a single pass over the job map; recomputing it per job would make every
// scan quadratic in the number of jobs the scheduler has ever seen.
func (gs *GlobalScheduler) eligibilityLocked(job *Job, arrayRunning map[string]int) (Eligibility, string) {
	for _, depID := range job.DependsOn {
		dep, known := gs.jobs[depID]
		if !known {
			// The dependency has been pruned, which only happens to jobs that reached a terminal
			// state long enough ago that anything waiting on them was already released or failed
			// at the time. Treating it as satisfied is the only option that does not strand the
			// job forever.
			continue
		}
		switch state, mode := dep.State, dependencyMode(job); {
		case state == StateQueued || state == StateRunning:
			return EligibleWaiting, fmt.Sprintf("waiting for %s (%s)", depID, strings.ToLower(state))

		case mode == DependAfterAny:
			// Any terminal outcome releases it.

		case mode == DependAfterNotOK:
			if state == StateCompleted {
				return EligibleDoomed, fmt.Sprintf("%s succeeded, but this job waits for it to fail", depID)
			}

		default: // DependAfterOK
			if state != StateCompleted {
				return EligibleDoomed, fmt.Sprintf("%s ended %s", depID, strings.ToLower(state))
			}
		}
	}

	if job.ArrayID != "" && job.ArrayMaxConcurrent > 0 {
		if arrayRunning == nil {
			arrayRunning = gs.arrayRunningLocked()
		}
		if running := arrayRunning[job.ArrayID]; running >= job.ArrayMaxConcurrent {
			return EligibleWaiting, fmt.Sprintf("array %s is at its limit of %d concurrent tasks",
				job.ArrayID, job.ArrayMaxConcurrent)
		}
	}

	return EligibleNow, ""
}

// dependencyMode returns the job's dependency mode, defaulting to afterok.
//
// afterok is the default because it is the one that is safe to get wrong: a job whose input
// step failed almost never wants to run on whatever partial output was left behind.
func dependencyMode(job *Job) string {
	switch job.DependencyMode {
	case DependAfterAny, DependAfterNotOK:
		return job.DependencyMode
	default:
		return DependAfterOK
	}
}

// arrayRunningLocked counts, per array, how many of its tasks are running.
func (gs *GlobalScheduler) arrayRunningLocked() map[string]int {
	counts := make(map[string]int)
	for _, j := range gs.jobs {
		if j.ArrayID != "" && j.State == StateRunning {
			counts[j.ArrayID]++
		}
	}
	return counts
}

// Eligibility reports whether a job may start, and the reason if not. It is for display: the
// scheduling paths use the locked form while they hold the queue.
func (gs *GlobalScheduler) Eligibility(jobID string) (Eligibility, string) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	job, ok := gs.jobs[jobID]
	if !ok {
		return EligibleNow, ""
	}
	return gs.eligibilityLocked(job, nil)
}

// PeekRunnable returns the highest-priority queued job that is allowed to start.
//
// It is not Peek. The head of the queue may be waiting on a dependency or throttled by its
// array, and returning it would wedge the scheduler exactly the way a gang rank at the head
// once did: the dispatcher would keep offering a job that can never be placed, and everything
// behind it would starve.
func (gs *GlobalScheduler) PeekRunnable() *Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	// A heap only orders its root, so the best *eligible* job needs a scan of the whole slice.
	arrayRunning := gs.arrayRunningLocked()
	var best *Job
	for _, job := range gs.queue {
		if eligible, _ := gs.eligibilityLocked(job, arrayRunning); eligible != EligibleNow {
			continue
		}
		if best == nil || jobBefore(job, best) {
			best = job
		}
	}
	return best.Copy()
}

// jobBefore is the queue's ordering, extracted so the eligible-job scan uses exactly the same
// rule as the heap rather than a second copy of it that could drift.
func jobBefore(a, b *Job) bool {
	if a.Priority == b.Priority {
		return a.SubmitTime.Before(b.SubmitTime)
	}
	return a.Priority < b.Priority
}

// DoomedQueued returns the queued jobs that can never run because of how a dependency ended,
// each with the reason.
//
// The scheduler calls this after a job reaches a terminal state. Without it, a job waiting on
// something that failed sits QUEUED forever: nothing will ever satisfy it, and nothing was
// watching for that.
func (gs *GlobalScheduler) DoomedQueued() map[string]string {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	arrayRunning := gs.arrayRunningLocked()
	var doomed map[string]string
	for _, job := range gs.queue {
		if eligible, reason := gs.eligibilityLocked(job, arrayRunning); eligible == EligibleDoomed {
			if doomed == nil {
				doomed = make(map[string]string)
			}
			doomed[job.ID] = reason
		}
	}
	return doomed
}

// FailQueued removes a queued job and marks it failed, for a job that can never become
// eligible. It reports whether the job was queued to begin with.
func (gs *GlobalScheduler) FailQueued(jobID, reason string) (*Job, bool) {
	gs.mu.Lock()
	job, ok := gs.jobs[jobID]
	if !ok || job.State != StateQueued {
		gs.mu.Unlock()
		return nil, false
	}
	if job.index >= 0 && job.index < gs.queue.Len() {
		heap.Remove(&gs.queue, job.index)
	}
	job.State = StateFailed
	job.Error = reason
	job.EndTime = time.Now()
	snapshot := job.Copy()
	gs.mu.Unlock()

	gs.notifyJobChange(snapshot)
	return snapshot, true
}

// EnqueueBatch adds several jobs as one unit, which is how an array is submitted.
//
// All or nothing matters here: half an array is worse than none. A user who asked for indices
// 1-1000 and got 1-400 because the queue filled has no way to ask for "the rest" without
// working out which rest, and the tasks that did land are already consuming the cluster.
func (gs *GlobalScheduler) EnqueueBatch(jobs []*Job) error {
	if len(jobs) == 0 {
		return nil
	}

	gs.mu.Lock()
	if gs.MaxQueueSize > 0 && gs.queue.Len()+len(jobs) > gs.MaxQueueSize {
		gs.mu.Unlock()
		return fmt.Errorf("queue full: %d jobs would exceed the limit of %d, with %d already queued",
			len(jobs), gs.MaxQueueSize, gs.queue.Len())
	}
	for _, job := range jobs {
		if _, exists := gs.jobs[job.ID]; exists {
			gs.mu.Unlock()
			return fmt.Errorf("job %s already exists", job.ID)
		}
	}

	snapshots := make([]*Job, 0, len(jobs))
	for _, job := range jobs {
		job.State = StateQueued
		gs.jobs[job.ID] = job
		heap.Push(&gs.queue, job)
		snapshots = append(snapshots, job.Copy())
	}
	gs.mu.Unlock()

	for _, snapshot := range snapshots {
		gs.notifyJobChange(snapshot)
	}
	return nil
}

// KnownJob reports whether a job ID exists, which is how a dependency is validated at submit.
func (gs *GlobalScheduler) KnownJob(jobID string) bool {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	_, ok := gs.jobs[jobID]
	return ok
}

// ArrayTasks returns the jobs belonging to one array, for reporting.
func (gs *GlobalScheduler) ArrayTasks(arrayID string) []*Job {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var out []*Job
	for _, j := range gs.jobs {
		if j.ArrayID == arrayID {
			out = append(out, j.Copy())
		}
	}
	return out
}
