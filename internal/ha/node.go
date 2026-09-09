package ha

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	boltstore "github.com/hashicorp/raft-boltdb/v2"
)

const (
	// applyTimeout bounds how long a write waits for the log entry to commit. A leader that has
	// lost quorum blocks rather than failing fast, so the caller needs a deadline.
	applyTimeout = 10 * time.Second

	// retainedSnapshots is how many snapshots to keep on disk.
	retainedSnapshots = 3

	// maxLogPool is the number of connections held open to each peer.
	maxLogPool = 3
)

// Config describes this master's participation in the replicated cluster.
type Config struct {
	// NodeID uniquely identifies this master. It must be stable across restarts, or the cluster
	// accumulates dead voters.
	NodeID string

	// BindAddr is the address peers use to reach this node's raft transport.
	BindAddr string

	// DataDir holds the replicated log and snapshots. It must not be shared between nodes.
	DataDir string

	// Peers lists the other masters, as "id=host:port". Only used when bootstrapping.
	Peers []string

	// Bootstrap forms a new cluster from Peers. Exactly one node should do this, once; every
	// other node joins an existing cluster instead.
	Bootstrap bool
}

// Node wraps a raft instance and the state machine it drives.
type Node struct {
	raft *raft.Raft
	fsm  *FSM
	cfg  Config

	transport *raft.NetworkTransport
	logStore  *boltstore.BoltStore
	snapshots raft.SnapshotStore

	// leadershipCh receives true when this node becomes leader and false when it loses it.
	leadershipCh chan bool
}

// Start brings up the raft node.
func Start(cfg Config, fsm *FSM) (*Node, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("ha.node_id is required so a restarting master keeps its identity")
	}
	if err := os.MkdirAll(cfg.DataDir, 0750); err != nil {
		return nil, fmt.Errorf("create raft data dir: %w", err)
	}

	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(cfg.NodeID)
	// Route raft's own logging through slog so it lands in the same structured stream.
	rc.LogLevel = "WARN"

	addr, err := net.ResolveTCPAddr("tcp", cfg.BindAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve ha.bind_addr %q: %w", cfg.BindAddr, err)
	}
	transport, err := raft.NewTCPTransport(cfg.BindAddr, addr, maxLogPool, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft transport on %s: %w", cfg.BindAddr, err)
	}

	snapshots, err := raft.NewFileSnapshotStore(cfg.DataDir, retainedSnapshots, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft snapshot store: %w", err)
	}

	// One BoltDB file holds both the log and the stable store; raft-boltdb keeps them in
	// separate buckets.
	store, err := boltstore.NewBoltStore(filepath.Join(cfg.DataDir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("raft log store: %w", err)
	}

	r, err := raft.NewRaft(rc, fsm, store, store, snapshots, transport)
	if err != nil {
		_ = store.Close()
		_ = transport.Close()
		return nil, fmt.Errorf("start raft: %w", err)
	}

	node := &Node{
		raft:         r,
		fsm:          fsm,
		cfg:          cfg,
		transport:    transport,
		logStore:     store,
		snapshots:    snapshots,
		leadershipCh: make(chan bool, 8),
	}

	if cfg.Bootstrap {
		if err := node.bootstrap(); err != nil {
			return nil, err
		}
	}

	go node.watchLeadership()
	return node, nil
}

// bootstrap forms a new cluster.
//
// Bootstrapping an existing cluster would install a competing configuration, so it is skipped
// whenever this node already has state — which makes restarting a bootstrapped node safe.
func (n *Node) bootstrap() error {
	hasState, err := raft.HasExistingState(n.logStore, n.logStore, n.snapshots)
	if err != nil {
		return fmt.Errorf("check for existing raft state: %w", err)
	}
	if hasState {
		slog.Info("raft already has state; skipping bootstrap")
		return nil
	}

	servers := []raft.Server{{
		ID:      raft.ServerID(n.cfg.NodeID),
		Address: n.transport.LocalAddr(),
	}}
	for _, peer := range n.cfg.Peers {
		id, addr, err := parsePeer(peer)
		if err != nil {
			return err
		}
		if id == n.cfg.NodeID {
			continue
		}
		servers = append(servers, raft.Server{ID: raft.ServerID(id), Address: raft.ServerAddress(addr)})
	}

	slog.Info("bootstrapping replicated cluster", "voters", len(servers))
	if err := n.raft.BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
		return fmt.Errorf("bootstrap cluster: %w", err)
	}
	return nil
}

// parsePeer splits an "id=host:port" peer specification.
func parsePeer(spec string) (id, addr string, err error) {
	for i := 0; i < len(spec); i++ {
		if spec[i] == '=' {
			id, addr = spec[:i], spec[i+1:]
			if id == "" || addr == "" {
				return "", "", fmt.Errorf("peer %q must be in the form id=host:port", spec)
			}
			return id, addr, nil
		}
	}
	return "", "", fmt.Errorf("peer %q must be in the form id=host:port", spec)
}

// watchLeadership republishes raft's leadership transitions.
func (n *Node) watchLeadership() {
	for isLeader := range n.raft.LeaderCh() {
		if isLeader {
			slog.Info("this master became the leader", "node_id", n.cfg.NodeID)
		} else {
			slog.Warn("this master lost leadership", "node_id", n.cfg.NodeID)
		}
		select {
		case n.leadershipCh <- isLeader:
		default:
			// A slow consumer must not stall raft's own goroutine.
		}
	}
}

// LeadershipChanges returns a channel signalling leadership transitions.
func (n *Node) LeadershipChanges() <-chan bool { return n.leadershipCh }

// IsLeader reports whether this node may accept writes.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// LeaderAddress returns the current leader's raft address, empty if unknown.
func (n *Node) LeaderAddress() string {
	addr, _ := n.raft.LeaderWithID()
	return string(addr)
}

// LeaderID returns the current leader's node ID, empty if unknown.
func (n *Node) LeaderID() string {
	_, id := n.raft.LeaderWithID()
	return string(id)
}

// Apply proposes a command and waits for it to commit.
//
// It returns ErrNotLeader when this node cannot accept writes, so callers can redirect rather
// than silently accepting something that will never replicate.
func (n *Node) Apply(cmd *Command) (interface{}, error) {
	if !n.IsLeader() {
		return nil, ErrNotLeader{LeaderID: n.LeaderID(), LeaderAddress: n.LeaderAddress()}
	}
	data, err := cmd.Encode()
	if err != nil {
		return nil, err
	}

	future := n.raft.Apply(data, applyTimeout)
	if err := future.Error(); err != nil {
		if err == raft.ErrLeadershipLost || err == raft.ErrNotLeader {
			return nil, ErrNotLeader{LeaderID: n.LeaderID(), LeaderAddress: n.LeaderAddress()}
		}
		return nil, fmt.Errorf("replicate %s: %w", cmd.Type, err)
	}

	// A command whose Apply returned an error surfaces it here rather than being lost.
	if err, isErr := future.Response().(error); isErr {
		return nil, err
	}
	return future.Response(), nil
}

// AddVoter adds a master to the cluster. Only the leader may do this.
func (n *Node) AddVoter(id, addr string) error {
	if !n.IsLeader() {
		return ErrNotLeader{LeaderID: n.LeaderID(), LeaderAddress: n.LeaderAddress()}
	}
	return n.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(addr), 0, 10*time.Second).Error()
}

// RemoveServer removes a master from the cluster.
func (n *Node) RemoveServer(id string) error {
	if !n.IsLeader() {
		return ErrNotLeader{LeaderID: n.LeaderID(), LeaderAddress: n.LeaderAddress()}
	}
	return n.raft.RemoveServer(raft.ServerID(id), 0, 10*time.Second).Error()
}

// Peers returns the current cluster configuration.
func (n *Node) Peers() ([]raft.Server, error) {
	future := n.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return nil, err
	}
	return future.Configuration().Servers, nil
}

// Stats exposes raft's own counters for the readiness endpoint.
func (n *Node) Stats() map[string]string { return n.raft.Stats() }

// Barrier waits until every command committed before now has been applied locally.
//
// Used before a read that must not observe stale state — after a leadership change, a new leader
// has the entries but may not yet have applied them.
func (n *Node) Barrier() error {
	return n.raft.Barrier(applyTimeout).Error()
}

// Shutdown stops the node and releases its files.
func (n *Node) Shutdown() error {
	if err := n.raft.Shutdown().Error(); err != nil {
		return err
	}
	if err := n.transport.Close(); err != nil {
		slog.Warn("closing raft transport", "error", err)
	}
	return n.logStore.Close()
}

// ErrNotLeader is returned when a write reaches a node that cannot accept it.
type ErrNotLeader struct {
	LeaderID      string
	LeaderAddress string
}

// LeaderHint describes where a client should retry.
func (e ErrNotLeader) LeaderHint() string {
	if e.LeaderID == "" {
		return "the leader is currently unknown; retry shortly"
	}
	return fmt.Sprintf("%s (%s)", e.LeaderID, e.LeaderAddress)
}

func (e ErrNotLeader) Error() string {
	if e.LeaderID == "" {
		return "this master is not the leader and no leader is currently known"
	}
	return fmt.Sprintf("this master is not the leader; the leader is %s at %s", e.LeaderID, e.LeaderAddress)
}
