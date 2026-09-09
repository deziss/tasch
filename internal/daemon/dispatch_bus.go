package daemon

import (
	"fmt"
	"sync"

	pb "github.com/deziss/tasch/api/v1"
)

// dispatchQueueDepth is how many pending messages a worker's stream may buffer before the
// master gives up on delivering more. Dispatches are re-driven by the acknowledgement timeout,
// so a full queue costs a redelivery rather than a lost job.
const dispatchQueueDepth = 64

// dispatchBus routes dispatch messages to the worker they are addressed to.
//
// It replaces the ZeroMQ PUB socket, which broadcast every job's command and environment
// variables to every subscriber and left the "is this mine?" check to the receiving worker.
// That made the bus port a cluster-wide credential feed for anyone who could connect to it.
// Here each worker holds its own stream, and a message is only ever written to the queue of the
// node it names.
// subscription is one worker stream's queue.
//
// The close is guarded by a sync.Once shared between the stream's own teardown and a
// master-wide shutdown, since both can reach the same subscription: the stream's deferred
// unsubscribe would otherwise close a channel Close had already closed, panicking during
// shutdown.
type subscription struct {
	ch   chan *pb.DispatchMessage
	once sync.Once
}

func (s *subscription) close() {
	s.once.Do(func() { close(s.ch) })
}

type dispatchBus struct {
	mu    sync.RWMutex
	nodes map[string]map[*subscription]struct{}
}

func newDispatchBus() *dispatchBus {
	return &dispatchBus{nodes: make(map[string]map[*subscription]struct{})}
}

// Subscribe registers a stream for a node and returns its queue plus an unsubscribe function.
//
// A node may hold more than one subscription at once — a reconnecting worker briefly overlaps
// with its previous stream — so every current subscriber for the node receives the message and
// the worker's own attempt tracking discards the duplicate. Unsubscribe is safe to call more
// than once.
func (b *dispatchBus) Subscribe(nodeName string) (<-chan *pb.DispatchMessage, func()) {
	sub := &subscription{ch: make(chan *pb.DispatchMessage, dispatchQueueDepth)}

	b.mu.Lock()
	subs, ok := b.nodes[nodeName]
	if !ok {
		subs = make(map[*subscription]struct{})
		b.nodes[nodeName] = subs
	}
	subs[sub] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		if subs, ok := b.nodes[nodeName]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(b.nodes, nodeName)
			}
		}
		b.mu.Unlock()
		sub.close()
	}
	return sub.ch, unsubscribe
}

// Send delivers a message to the node it targets, reporting whether any stream accepted it.
//
// Unlike the fire-and-forget publish it replaces — whose error was discarded at every call site,
// and which silently dropped anything published while a subscriber was reconnecting — this
// tells the caller when a dispatch went nowhere.
func (b *dispatchBus) Send(nodeName string, msg *pb.DispatchMessage) error {
	b.mu.RLock()
	subs := make([]*subscription, 0, len(b.nodes[nodeName]))
	for sub := range b.nodes[nodeName] {
		subs = append(subs, sub)
	}
	b.mu.RUnlock()

	if len(subs) == 0 {
		return fmt.Errorf("no worker stream connected for node %s", nodeName)
	}

	delivered := 0
	for _, sub := range subs {
		select {
		case sub.ch <- msg:
			delivered++
		default:
			// This worker is not keeping up. Skip it rather than blocking the scheduler.
		}
	}
	if delivered == 0 {
		return fmt.Errorf("worker %s is not keeping up; dispatch queue is full", nodeName)
	}
	return nil
}

// IsConnected reports whether a node currently holds a dispatch stream.
func (b *dispatchBus) IsConnected(nodeName string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.nodes[nodeName]) > 0
}

// ConnectedNodes returns the node names with at least one live stream.
func (b *dispatchBus) ConnectedNodes() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.nodes))
	for name := range b.nodes {
		names = append(names, name)
	}
	return names
}

// Close releases every subscription, unblocking all streams.
func (b *dispatchBus) Close() {
	b.mu.Lock()
	nodes := b.nodes
	b.nodes = make(map[string]map[*subscription]struct{})
	b.mu.Unlock()

	for _, subs := range nodes {
		for sub := range subs {
			sub.close()
		}
	}
}
