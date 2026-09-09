// Package ha provides leader election and state replication across several masters.
//
// Tasch's master was a single point of failure: one process held the queue, one BoltDB file held
// the state, and workers dialled one address. Losing that host meant no dispatch, no submission,
// no status, and no results, until someone restarted it — and whatever had not yet reached disk
// was gone.
//
// The design follows the usual pattern for a replicated scheduler: the leader makes scheduling
// decisions locally, then replicates the *decision* rather than the procedure that produced it.
// Matching a job against a node involves CEL evaluation and closures over live cluster state,
// none of which is deterministic across replicas or serializable into a log. Deciding "job X
// goes to node Y" on the leader and replicating that as a fact keeps every replica's state
// machine identical without any of that complexity.
package ha

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/deziss/tasch/pkg/scheduler"
)

// CommandType identifies a replicated state change.
type CommandType string

const (
	CmdEnqueue        CommandType = "enqueue"
	CmdDispatch       CommandType = "dispatch"
	CmdComplete       CommandType = "complete"
	CmdCancel         CommandType = "cancel"
	CmdRequeue        CommandType = "requeue"
	CmdRequeueRunning CommandType = "requeue_running"
	CmdAdoptRunning   CommandType = "adopt_running"
	CmdRegisterGroup  CommandType = "register_group"
	CmdSetGroupState  CommandType = "set_group_state"
	CmdRecordUsage    CommandType = "record_usage"
	CmdDecayUsage     CommandType = "decay_usage"
	CmdReprioritize   CommandType = "reprioritize"
	CmdPruneTerminal  CommandType = "prune_terminal"
	CmdEnqueueBatch   CommandType = "enqueue_batch"
	CmdFailQueued     CommandType = "fail_queued"
	CmdCordon         CommandType = "cordon"
	CmdUncordon       CommandType = "uncordon"
)

// Command is one entry in the replicated log.
//
// Fields are a flat union rather than a nested payload per type: the set is small, the encoding
// stays readable in a log dump, and adding a field cannot break decoding of older entries.
type Command struct {
	Type CommandType `json:"type"`

	// Job carries the whole record for an enqueue. Jobs carries a whole array, which is
	// submitted as one entry so a replica can never hold a partial array.
	Job  *scheduler.Job   `json:"job,omitempty"`
	Jobs []*scheduler.Job `json:"jobs,omitempty"`

	JobID   string `json:"job_id,omitempty"`
	Node    string `json:"node,omitempty"`
	Attempt int64  `json:"attempt,omitempty"`

	Success bool   `json:"success,omitempty"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`

	IncrementRetry bool `json:"increment_retry,omitempty"`

	Group      *scheduler.JobGroup `json:"group,omitempty"`
	GroupID    string              `json:"group_id,omitempty"`
	GroupState string              `json:"group_state,omitempty"`

	User    string  `json:"user,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	CPUs    int     `json:"cpus,omitempty"`
	GPUs    int     `json:"gpus,omitempty"`
	MemMB   int     `json:"mem_mb,omitempty"`
	Factor  float64 `json:"factor,omitempty"`

	// Penalties is the fairshare penalty per user, computed on the leader.
	//
	// The penalty depends on a snapshot of every user's usage, so recomputing it inside each
	// replica's Apply would be sensitive to ordering. Sending the resolved numbers keeps replicas
	// byte-identical.
	Penalties map[string]int `json:"penalties,omitempty"`

	MaxAgeSeconds float64 `json:"max_age_seconds,omitempty"`

	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at,omitempty"`
}

// Encode serializes a command for the replicated log.
func (c *Command) Encode() ([]byte, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode %s command: %w", c.Type, err)
	}
	return data, nil
}

// DecodeCommand parses a log entry.
func DecodeCommand(data []byte) (*Command, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return nil, fmt.Errorf("decode command: %w", err)
	}
	if cmd.Type == "" {
		return nil, fmt.Errorf("command has no type")
	}
	return &cmd, nil
}

// MaxAge returns the prune cutoff as a duration.
func (c *Command) MaxAge() time.Duration {
	return time.Duration(c.MaxAgeSeconds * float64(time.Second))
}
