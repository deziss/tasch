package ha

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/deziss/tasch/pkg/scheduler"
)

// testCluster is a set of raft nodes sharing a bootstrap configuration.
type testCluster struct {
	nodes []*Node
	fsms  []*FSM
}

// newTestCluster starts n masters and waits for one to win the election.
func newTestCluster(t *testing.T, n int) *testCluster {
	t.Helper()

	base := freePortBase(t, n)
	peers := make([]string, n)
	for i := 0; i < n; i++ {
		peers[i] = fmt.Sprintf("node-%d=127.0.0.1:%d", i, base+i)
	}

	cluster := &testCluster{}
	for i := 0; i < n; i++ {
		fsm := newTestFSM()
		node, err := Start(Config{
			NodeID:    fmt.Sprintf("node-%d", i),
			BindAddr:  fmt.Sprintf("127.0.0.1:%d", base+i),
			DataDir:   t.TempDir(),
			Peers:     peers,
			Bootstrap: true, // every node has the same configuration, so this is idempotent
		}, fsm)
		if err != nil {
			t.Fatalf("start node-%d: %v", i, err)
		}
		cluster.nodes = append(cluster.nodes, node)
		cluster.fsms = append(cluster.fsms, fsm)
	}

	t.Cleanup(func() {
		for _, node := range cluster.nodes {
			// A test may have deliberately shut one down to force a failover.
			if node == nil {
				continue
			}
			_ = node.Shutdown()
		}
	})

	cluster.waitForLeader(t, 15*time.Second)
	return cluster
}

// waitForLeader blocks until one node holds leadership steadily.
//
// A single poll is not enough: IsLeader reads local state, so during an election two nodes can
// briefly both report leadership — one of them stale. Requiring the same leader across
// consecutive polls distinguishes a settled cluster from a transition in flight.
func (c *testCluster) waitForLeader(t *testing.T, timeout time.Duration) *Node {
	t.Helper()

	const stableReadings = 3
	var candidate *Node
	stable := 0

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leaders := c.leaders()
		if len(leaders) == 1 && leaders[0] == candidate {
			stable++
			if stable >= stableReadings {
				return candidate
			}
		} else if len(leaders) == 1 {
			candidate, stable = leaders[0], 1
		} else {
			candidate, stable = nil, 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no stable leader within %s", timeout)
	return nil
}

func (c *testCluster) leaders() []*Node {
	var leaders []*Node
	for i, node := range c.nodes {
		if node == nil {
			continue
		}
		if node.IsLeader() {
			leaders = append(leaders, c.nodes[i])
		}
	}
	return leaders
}

// TestClusterElectsExactlyOneLeader is the split-brain check: two masters accepting writes at
// once would each build a different queue, and one set of jobs would silently vanish on the next
// election.
func TestClusterElectsExactlyOneLeader(t *testing.T) {
	cluster := newTestCluster(t, 3)

	leader := cluster.waitForLeader(t, 15*time.Second)

	// Agreement is the property that matters. Two masters accepting writes at once would each
	// build a different queue, and one set of jobs would vanish at the next election.
	for _, node := range cluster.nodes {
		if id := node.LeaderID(); id != leader.cfg.NodeID {
			t.Errorf("node %s thinks the leader is %q, want %q", node.cfg.NodeID, id, leader.cfg.NodeID)
		}
	}
}

// TestWritesReplicateToFollowers confirms a job submitted to the leader is visible on every
// replica — which is the whole point: a follower promoted later must already have it.
func TestWritesReplicateToFollowers(t *testing.T) {
	cluster := newTestCluster(t, 3)
	leader := cluster.waitForLeader(t, 15*time.Second)
	store := NewReplicated(leader)

	if err := store.Enqueue(testJob("j1")); err != nil {
		t.Fatalf("enqueue on leader: %v", err)
	}

	// Followers apply asynchronously; give them a moment.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		replicated := 0
		for _, fsm := range cluster.fsms {
			if _, ok := fsm.Queue().GetJob("j1"); ok {
				replicated++
			}
		}
		if replicated == len(cluster.fsms) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the job did not reach every replica")
}

// TestFollowerRejectsWrites confirms a follower refuses writes and names the leader, so a client
// can be redirected rather than having its submission silently accepted and lost.
func TestFollowerRejectsWrites(t *testing.T) {
	cluster := newTestCluster(t, 3)

	cluster.waitForLeader(t, 15*time.Second)

	var follower *Node
	for _, node := range cluster.nodes {
		if !node.IsLeader() {
			follower = node
			break
		}
	}
	if follower == nil {
		t.Fatal("no follower found")
	}

	err := NewReplicated(follower).Enqueue(testJob("j1"))
	if err == nil {
		t.Fatal("a follower accepted a write")
	}
	notLeader, ok := err.(ErrNotLeader)
	if !ok {
		t.Fatalf("error = %v, want ErrNotLeader so the client can be redirected", err)
	}
	if notLeader.LeaderID == "" {
		t.Error("the rejection did not name the leader")
	}
}

// TestFailoverPreservesState is the reason any of this exists. Kill the leader and the surviving
// masters must elect a new one that already holds every acknowledged job.
func TestFailoverPreservesState(t *testing.T) {
	cluster := newTestCluster(t, 3)
	leader := cluster.waitForLeader(t, 15*time.Second)

	// Commit work through the original leader.
	store := NewReplicated(leader)
	for _, id := range []string{"j1", "j2", "j3"} {
		if err := store.Enqueue(testJob(id)); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	if _, _, ok := store.Dispatch("j1", "node-a"); !ok {
		t.Fatal("dispatch on the original leader failed")
	}

	// Take the leader out, as a host failure would.
	leaderIdx := -1
	for i, node := range cluster.nodes {
		if node == leader {
			leaderIdx = i
			break
		}
	}
	if err := leader.Shutdown(); err != nil {
		t.Fatalf("shutdown leader: %v", err)
	}
	cluster.nodes[leaderIdx] = nil

	// The survivors must elect a new leader.
	newLeader := cluster.waitForLeader(t, 20*time.Second)
	if newLeader == leader {
		t.Fatal("the dead node is still reporting leadership")
	}

	// Reads on the new leader must be up to date before they are trusted.
	if err := newLeader.Barrier(); err != nil {
		t.Fatalf("barrier on the new leader: %v", err)
	}

	fsm := cluster.fsms[indexOf(cluster.nodes, newLeader)]
	for _, id := range []string{"j1", "j2", "j3"} {
		if _, ok := fsm.Queue().GetJob(id); !ok {
			t.Errorf("%s was lost in the failover", id)
		}
	}
	dispatched, _ := fsm.Queue().GetJob("j1")
	if dispatched.State != scheduler.StateRunning || dispatched.WorkerNode != "node-a" {
		t.Errorf("j1 = %s on %q after failover, want RUNNING on node-a",
			dispatched.State, dispatched.WorkerNode)
	}

	// And the new leader must accept new work.
	if err := NewReplicated(newLeader).Enqueue(testJob("j4")); err != nil {
		t.Fatalf("the new leader will not accept writes: %v", err)
	}
}

func indexOf(nodes []*Node, target *Node) int {
	for i, node := range nodes {
		if node == target {
			return i
		}
	}
	return -1
}

// freePortBase reserves a contiguous run of n free localhost ports and returns the first.
//
// The listeners are closed before the run is returned: raft binds them itself. That leaves a
// small race with anything else on the machine, which is why the range is probed as a block
// rather than one port at a time.
func freePortBase(t *testing.T, n int) int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()

	for attempt := 0; attempt < 20; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port: %v", err)
		}
		base := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		ok := true
		block := make([]net.Listener, 0, n)
		for i := 0; i < n; i++ {
			probe, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			block = append(block, probe)
		}
		for _, probe := range block {
			_ = probe.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("could not find a free block of ports")
	return 0
}
