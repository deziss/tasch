package ha

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/deziss/tasch/pkg/scheduler"
	"github.com/hashicorp/raft"
)

// Cordons is the set of nodes taken out of scheduling rotation, replicated alongside job state.
type Cordons struct {
	mu      sync.RWMutex
	entries map[string]CordonEntry
}

// CordonEntry records why and when a node was cordoned.
type CordonEntry struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

func NewCordons() *Cordons {
	return &Cordons{entries: make(map[string]CordonEntry)}
}

func (c *Cordons) Set(node, reason string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[node] = CordonEntry{Reason: reason, Since: at}
}

func (c *Cordons) Clear(node string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, was := c.entries[node]
	delete(c.entries, node)
	return was
}

func (c *Cordons) IsCordoned(node string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.entries[node]
	return ok
}

func (c *Cordons) Reason(node string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[node].Reason
}

func (c *Cordons) Snapshot() map[string]CordonEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]CordonEntry, len(c.entries))
	for node, entry := range c.entries {
		out[node] = entry
	}
	return out
}

func (c *Cordons) Restore(entries map[string]CordonEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]CordonEntry, len(entries))
	for node, entry := range entries {
		c.entries[node] = entry
	}
}

// FSM is the replicated scheduler state.
//
// Every replica applies the same commands in the same order, so a follower promoted to leader
// already holds the queue, the running jobs, the groups, the fairshare accounting and the
// cordons — no recovery step, and nothing lost that had been acknowledged to a client.
type FSM struct {
	queue     *scheduler.GlobalScheduler
	fairshare *scheduler.FairshareCalculator
	cordons   *Cordons

	// applied counts commands applied, for observability.
	applied uint64
}

// NewFSM builds the state machine over an existing scheduler.
func NewFSM(queue *scheduler.GlobalScheduler, fairshare *scheduler.FairshareCalculator, cordons *Cordons) *FSM {
	return &FSM{queue: queue, fairshare: fairshare, cordons: cordons}
}

// Queue exposes the replicated scheduler for reads.
func (f *FSM) Queue() *scheduler.GlobalScheduler { return f.queue }

// Fairshare exposes the replicated usage accounting for reads.
func (f *FSM) Fairshare() *scheduler.FairshareCalculator { return f.fairshare }

// Cordons exposes the replicated cordon set for reads.
func (f *FSM) Cordons() *Cordons { return f.cordons }

// Applied reports how many commands this replica has applied.
func (f *FSM) Applied() uint64 { return f.applied }

// DispatchResult is returned by an applied dispatch command.
type DispatchResult struct {
	Job     *scheduler.Job
	Attempt int64
	OK      bool
}

// Apply executes one committed log entry.
//
// It must be deterministic: every replica applies the same entries in the same order and has to
// reach the same state. Anything non-deterministic — matching, CEL evaluation, penalty
// computation — is resolved on the leader before the command is proposed.
func (f *FSM) Apply(entry *raft.Log) interface{} {
	cmd, err := DecodeCommand(entry.Data)
	if err != nil {
		// A malformed entry cannot be skipped silently: replicas would diverge. Surface it.
		slog.Error("undecodable raft log entry", "index", entry.Index, "error", err)
		return err
	}
	f.applied++

	switch cmd.Type {
	case CmdEnqueue:
		if cmd.Job == nil {
			return fmt.Errorf("enqueue command carries no job")
		}
		return f.queue.Enqueue(cmd.Job)

	case CmdDispatch:
		// The leader already chose the node; applying it is deterministic.
		job := f.queue.RemoveByID(cmd.JobID)
		if job == nil {
			// Already gone: cancelled or dispatched by an entry applied earlier.
			return DispatchResult{OK: false}
		}
		attempt, ok := f.queue.MarkRunning(cmd.JobID, cmd.Node)
		return DispatchResult{Job: job, Attempt: attempt, OK: ok}

	case CmdComplete:
		f.queue.MarkCompleted(cmd.JobID, cmd.Success, cmd.Output, cmd.Error)
		return nil

	case CmdCancel:
		job, ok := f.queue.Cancel(cmd.JobID)
		return DispatchResult{Job: job, OK: ok}

	case CmdRequeue:
		job, err := f.queue.Requeue(cmd.JobID, cmd.IncrementRetry)
		if err != nil {
			return err
		}
		return DispatchResult{Job: job, OK: true}

	case CmdRequeueRunning:
		job, ok := f.queue.RequeueRunningJob(cmd.JobID)
		return DispatchResult{Job: job, OK: ok}

	case CmdAdoptRunning:
		ok := f.queue.AdoptRunning(cmd.JobID, cmd.Node, cmd.Attempt, cmd.At)
		return DispatchResult{OK: ok}

	case CmdRegisterGroup:
		if cmd.Group == nil {
			return fmt.Errorf("register_group command carries no group")
		}
		f.queue.RegisterGroup(cmd.Group)
		return nil

	case CmdSetGroupState:
		f.queue.SetGroupState(cmd.GroupID, cmd.GroupState)
		return nil

	case CmdRecordUsage:
		f.fairshare.RecordUsage(cmd.User, cmd.Seconds, cmd.CPUs, cmd.GPUs, cmd.MemMB)
		return nil

	case CmdDecayUsage:
		f.fairshare.DecayUsage(cmd.Factor)
		return nil

	case CmdReprioritize:
		// Penalties were resolved on the leader, so each replica applies identical numbers.
		penalties := cmd.Penalties
		return f.queue.ReprioritizeQueued(func(job *scheduler.Job) int {
			return penalties[job.User]
		})

	case CmdPruneTerminal:
		return f.queue.PruneTerminal(cmd.MaxAge())

	case CmdCordon:
		f.cordons.Set(cmd.Node, cmd.Reason, cmd.At)
		return nil

	case CmdUncordon:
		return f.cordons.Clear(cmd.Node)

	default:
		return fmt.Errorf("unknown command type %q", cmd.Type)
	}
}

// fsmSnapshot is a point-in-time copy of the replicated state.
type fsmSnapshot struct {
	Jobs      []*scheduler.Job       `json:"jobs"`
	Groups    []*scheduler.JobGroup  `json:"groups"`
	Fairshare map[string]float64     `json:"fairshare"`
	Cordons   map[string]CordonEntry `json:"cordons"`
}

// Snapshot captures the state so the log can be truncated and a new replica can catch up
// without replaying every command since the cluster began.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{
		Jobs:      f.queue.ListJobs(""),
		Groups:    f.queue.ListGroups(),
		Fairshare: f.fairshare.Snapshot(),
		Cordons:   f.cordons.Snapshot(),
	}, nil
}

// Restore replaces the state from a snapshot.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer func() { _ = rc.Close() }()

	var snap fsmSnapshot
	if err := json.NewDecoder(rc).Decode(&snap); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}

	f.queue.Reset(snap.Jobs, snap.Groups)
	f.fairshare.Restore(snap.Fairshare)
	f.cordons.Restore(snap.Cordons)

	slog.Info("restored replicated state from snapshot",
		"jobs", len(snap.Jobs), "groups", len(snap.Groups), "cordoned_nodes", len(snap.Cordons))
	return nil
}

// Persist writes the snapshot out.
func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("persist snapshot: %w", err)
	}
	return sink.Close()
}

// Release is called when the snapshot is no longer needed.
func (s *fsmSnapshot) Release() {}
