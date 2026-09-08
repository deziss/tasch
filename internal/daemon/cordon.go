package daemon

import (
	"sync"
	"time"
)

// cordonRegistry tracks nodes an operator has taken out of scheduling rotation.
//
// There was previously no way to stop a node receiving work for maintenance. The only options
// were to kill the worker — which fails every job running on it — or to wait for the queue to
// drain on its own. This is the equivalent of `scontrol update state=DRAIN` or
// `kubectl cordon`.
//
// Cordoning is distinct from the circuit breaker: the breaker is automatic, temporary, and
// reacts to failures, whereas a cordon is deliberate and lasts until an operator lifts it.
type cordonRegistry struct {
	mu      sync.RWMutex
	entries map[string]cordonEntry
}

type cordonEntry struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

func newCordonRegistry() *cordonRegistry {
	return &cordonRegistry{entries: make(map[string]cordonEntry)}
}

// Cordon takes a node out of rotation. Cordoning an already-cordoned node updates its reason.
func (c *cordonRegistry) Cordon(node, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[node] = cordonEntry{Reason: reason, Since: time.Now()}
}

// Uncordon returns a node to rotation, reporting whether it had been cordoned.
func (c *cordonRegistry) Uncordon(node string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, was := c.entries[node]
	delete(c.entries, node)
	return was
}

// IsCordoned reports whether a node should be skipped when placing work.
func (c *cordonRegistry) IsCordoned(node string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.entries[node]
	return ok
}

// Reason returns why a node was cordoned, or "" if it is not.
func (c *cordonRegistry) Reason(node string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[node].Reason
}

// Snapshot returns a copy of the registry, for persistence.
func (c *cordonRegistry) Snapshot() map[string]cordonEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]cordonEntry, len(c.entries))
	for node, entry := range c.entries {
		out[node] = entry
	}
	return out
}

// Restore replaces the registry from persisted state.
//
// Cordons must survive a master restart: a node taken out of service for maintenance that
// silently returns to rotation because the master was restarted is worse than no cordon at all.
func (c *cordonRegistry) Restore(entries map[string]cordonEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cordonEntry, len(entries))
	for node, entry := range entries {
		c.entries[node] = entry
	}
}
