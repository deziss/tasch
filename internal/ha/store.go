package ha

import (
	"time"

	"github.com/deziss/tasch/pkg/scheduler"
)

// Store is how the master mutates scheduler state.
//
// Two implementations exist. Direct applies changes straight to the local scheduler, which is
// what a single master does and is byte-for-byte the previous behaviour. Replicated proposes
// each change through raft so every master converges on the same state.
//
// The interface exists so the scheduling code is written once. Reads are deliberately absent:
// they go to the local scheduler in both modes, since a follower's copy is already consistent
// and forcing every status query through the leader would give up the main benefit of having
// replicas at all.
type Store interface {
	Enqueue(job *scheduler.Job) error
	EnqueueBatch(jobs []*scheduler.Job) error
	FailQueued(jobID, reason string) (*scheduler.Job, bool)
	Dispatch(jobID, node string) (*scheduler.Job, int64, bool)
	Complete(jobID string, success bool, output, errMsg string)
	Cancel(jobID string) (*scheduler.Job, bool)
	Requeue(jobID string, incrementRetry bool) (*scheduler.Job, error)
	RequeueRunning(jobID string) (*scheduler.Job, bool)
	AdoptRunning(jobID, node string, attempt int64, startTime time.Time) bool
	RegisterGroup(g *scheduler.JobGroup) error
	SetGroupState(groupID, state string) error
	RecordUsage(user string, seconds float64, cpus, gpus, memMB int) error
	DecayUsage(factor float64) error
	Reprioritize(penalties map[string]int) (int, error)
	PruneTerminal(maxAge time.Duration) (int, error)
	Cordon(node, reason string, at time.Time) error
	Uncordon(node string) (bool, error)

	// IsLeader reports whether this master may accept writes and run the scheduling loop.
	IsLeader() bool
	// LeaderHint describes the current leader for a redirect, empty when this node is leader.
	LeaderHint() string
}

// Direct applies changes to the local scheduler with no replication.
type Direct struct {
	queue     *scheduler.GlobalScheduler
	fairshare *scheduler.FairshareCalculator
	cordons   *Cordons
}

// NewDirect builds the single-master store.
func NewDirect(queue *scheduler.GlobalScheduler, fairshare *scheduler.FairshareCalculator, cordons *Cordons) *Direct {
	return &Direct{queue: queue, fairshare: fairshare, cordons: cordons}
}

func (d *Direct) Enqueue(job *scheduler.Job) error { return d.queue.Enqueue(job) }

func (d *Direct) EnqueueBatch(jobs []*scheduler.Job) error { return d.queue.EnqueueBatch(jobs) }

func (d *Direct) FailQueued(jobID, reason string) (*scheduler.Job, bool) {
	return d.queue.FailQueued(jobID, reason)
}

func (d *Direct) Dispatch(jobID, node string) (*scheduler.Job, int64, bool) {
	job := d.queue.RemoveByID(jobID)
	if job == nil {
		return nil, 0, false
	}
	attempt, ok := d.queue.MarkRunning(jobID, node)
	return job, attempt, ok
}

func (d *Direct) Complete(jobID string, success bool, output, errMsg string) {
	d.queue.MarkCompleted(jobID, success, output, errMsg)
}

func (d *Direct) Cancel(jobID string) (*scheduler.Job, bool) { return d.queue.Cancel(jobID) }

func (d *Direct) Requeue(jobID string, incrementRetry bool) (*scheduler.Job, error) {
	return d.queue.Requeue(jobID, incrementRetry)
}

func (d *Direct) RequeueRunning(jobID string) (*scheduler.Job, bool) {
	return d.queue.RequeueRunningJob(jobID)
}

func (d *Direct) AdoptRunning(jobID, node string, attempt int64, startTime time.Time) bool {
	return d.queue.AdoptRunning(jobID, node, attempt, startTime)
}

func (d *Direct) RegisterGroup(g *scheduler.JobGroup) error {
	d.queue.RegisterGroup(g)
	return nil
}

func (d *Direct) SetGroupState(groupID, state string) error {
	d.queue.SetGroupState(groupID, state)
	return nil
}

func (d *Direct) RecordUsage(user string, seconds float64, cpus, gpus, memMB int) error {
	d.fairshare.RecordUsage(user, seconds, cpus, gpus, memMB)
	return nil
}

func (d *Direct) DecayUsage(factor float64) error {
	d.fairshare.DecayUsage(factor)
	return nil
}

func (d *Direct) Reprioritize(penalties map[string]int) (int, error) {
	return d.queue.ReprioritizeQueued(func(job *scheduler.Job) int {
		return penalties[job.User]
	}), nil
}

func (d *Direct) PruneTerminal(maxAge time.Duration) (int, error) {
	return d.queue.PruneTerminal(maxAge), nil
}

func (d *Direct) Cordon(node, reason string, at time.Time) error {
	d.cordons.Set(node, reason, at)
	return nil
}

func (d *Direct) Uncordon(node string) (bool, error) { return d.cordons.Clear(node), nil }

// A single master is always the leader: there is nobody to defer to.
func (d *Direct) IsLeader() bool     { return true }
func (d *Direct) LeaderHint() string { return "" }

// Replicated proposes every change through raft.
type Replicated struct {
	node *Node
}

// NewReplicated builds the multi-master store.
func NewReplicated(node *Node) *Replicated { return &Replicated{node: node} }

func (r *Replicated) Enqueue(job *scheduler.Job) error {
	_, err := r.node.Apply(&Command{Type: CmdEnqueue, Job: job})
	return err
}

func (r *Replicated) EnqueueBatch(jobs []*scheduler.Job) error {
	_, err := r.node.Apply(&Command{Type: CmdEnqueueBatch, Jobs: jobs})
	return err
}

func (r *Replicated) FailQueued(jobID, reason string) (*scheduler.Job, bool) {
	res, err := r.node.Apply(&Command{Type: CmdFailQueued, JobID: jobID, Error: reason})
	if err != nil {
		return nil, false
	}
	result, ok := res.(DispatchResult)
	if !ok {
		return nil, false
	}
	return result.Job, result.OK
}

func (r *Replicated) Dispatch(jobID, node string) (*scheduler.Job, int64, bool) {
	res, err := r.node.Apply(&Command{Type: CmdDispatch, JobID: jobID, Node: node})
	if err != nil {
		return nil, 0, false
	}
	result, ok := res.(DispatchResult)
	if !ok {
		return nil, 0, false
	}
	return result.Job, result.Attempt, result.OK
}

func (r *Replicated) Complete(jobID string, success bool, output, errMsg string) {
	_, _ = r.node.Apply(&Command{
		Type: CmdComplete, JobID: jobID, Success: success, Output: output, Error: errMsg,
	})
}

func (r *Replicated) Cancel(jobID string) (*scheduler.Job, bool) {
	res, err := r.node.Apply(&Command{Type: CmdCancel, JobID: jobID})
	if err != nil {
		return nil, false
	}
	result, ok := res.(DispatchResult)
	if !ok {
		return nil, false
	}
	return result.Job, result.OK
}

func (r *Replicated) Requeue(jobID string, incrementRetry bool) (*scheduler.Job, error) {
	res, err := r.node.Apply(&Command{Type: CmdRequeue, JobID: jobID, IncrementRetry: incrementRetry})
	if err != nil {
		return nil, err
	}
	result, _ := res.(DispatchResult)
	return result.Job, nil
}

func (r *Replicated) RequeueRunning(jobID string) (*scheduler.Job, bool) {
	res, err := r.node.Apply(&Command{Type: CmdRequeueRunning, JobID: jobID})
	if err != nil {
		return nil, false
	}
	result, ok := res.(DispatchResult)
	if !ok {
		return nil, false
	}
	return result.Job, result.OK
}

func (r *Replicated) AdoptRunning(jobID, node string, attempt int64, startTime time.Time) bool {
	res, err := r.node.Apply(&Command{
		Type: CmdAdoptRunning, JobID: jobID, Node: node, Attempt: attempt, At: startTime,
	})
	if err != nil {
		return false
	}
	result, ok := res.(DispatchResult)
	return ok && result.OK
}

func (r *Replicated) RegisterGroup(g *scheduler.JobGroup) error {
	_, err := r.node.Apply(&Command{Type: CmdRegisterGroup, Group: g})
	return err
}

func (r *Replicated) SetGroupState(groupID, state string) error {
	_, err := r.node.Apply(&Command{Type: CmdSetGroupState, GroupID: groupID, GroupState: state})
	return err
}

func (r *Replicated) RecordUsage(user string, seconds float64, cpus, gpus, memMB int) error {
	_, err := r.node.Apply(&Command{
		Type: CmdRecordUsage, User: user, Seconds: seconds, CPUs: cpus, GPUs: gpus, MemMB: memMB,
	})
	return err
}

func (r *Replicated) DecayUsage(factor float64) error {
	_, err := r.node.Apply(&Command{Type: CmdDecayUsage, Factor: factor})
	return err
}

func (r *Replicated) Reprioritize(penalties map[string]int) (int, error) {
	res, err := r.node.Apply(&Command{Type: CmdReprioritize, Penalties: penalties})
	if err != nil {
		return 0, err
	}
	changed, _ := res.(int)
	return changed, nil
}

func (r *Replicated) PruneTerminal(maxAge time.Duration) (int, error) {
	res, err := r.node.Apply(&Command{
		Type: CmdPruneTerminal, MaxAgeSeconds: maxAge.Seconds(),
	})
	if err != nil {
		return 0, err
	}
	pruned, _ := res.(int)
	return pruned, nil
}

func (r *Replicated) Cordon(node, reason string, at time.Time) error {
	_, err := r.node.Apply(&Command{Type: CmdCordon, Node: node, Reason: reason, At: at})
	return err
}

func (r *Replicated) Uncordon(node string) (bool, error) {
	res, err := r.node.Apply(&Command{Type: CmdUncordon, Node: node})
	if err != nil {
		return false, err
	}
	was, _ := res.(bool)
	return was, nil
}

func (r *Replicated) IsLeader() bool { return r.node.IsLeader() }

func (r *Replicated) LeaderHint() string {
	if r.node.IsLeader() {
		return ""
	}
	return r.node.LeaderID()
}
