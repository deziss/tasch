package daemon

import (
	"testing"
)

// TestCordonBlocksScheduling covers the basic contract: a cordoned node reports as such, and an
// uncordoned one does not.
func TestCordonBlocksScheduling(t *testing.T) {
	c := newCordonRegistry()

	if c.IsCordoned("node1") {
		t.Fatal("a fresh registry reports node1 as cordoned")
	}

	c.Cordon("node1", "kernel upgrade")
	if !c.IsCordoned("node1") {
		t.Error("node1 is not cordoned after Cordon")
	}
	if got := c.Reason("node1"); got != "kernel upgrade" {
		t.Errorf("reason = %q, want %q", got, "kernel upgrade")
	}
	if c.IsCordoned("node2") {
		t.Error("cordoning node1 also cordoned node2")
	}

	if !c.Uncordon("node1") {
		t.Error("Uncordon reported node1 was not cordoned")
	}
	if c.IsCordoned("node1") {
		t.Error("node1 is still cordoned after Uncordon")
	}
	if c.Uncordon("node1") {
		t.Error("Uncordon reported success for a node that was not cordoned")
	}
}

// TestCordonUpdatesReason confirms re-cordoning replaces the reason rather than being ignored.
func TestCordonUpdatesReason(t *testing.T) {
	c := newCordonRegistry()
	c.Cordon("node1", "first")
	c.Cordon("node1", "second")

	if got := c.Reason("node1"); got != "second" {
		t.Errorf("reason = %q, want %q", got, "second")
	}
}

// TestCordonSurvivesRestart is the reason cordons are persisted at all: a node taken out of
// service for maintenance that silently returns to rotation because the master restarted is
// worse than never having cordoned it.
func TestCordonSurvivesRestart(t *testing.T) {
	original := newCordonRegistry()
	original.Cordon("node1", "disk replacement")
	original.Cordon("node2", "kernel upgrade")

	snapshot := original.Snapshot()

	// A fresh registry, as after a restart.
	restored := newCordonRegistry()
	restored.Restore(snapshot)

	for _, node := range []string{"node1", "node2"} {
		if !restored.IsCordoned(node) {
			t.Errorf("%s lost its cordon across a restart", node)
		}
	}
	if got := restored.Reason("node1"); got != "disk replacement" {
		t.Errorf("reason = %q, want %q", got, "disk replacement")
	}
}

// TestSnapshotIsDetached confirms the snapshot does not alias live state.
func TestSnapshotIsDetached(t *testing.T) {
	c := newCordonRegistry()
	c.Cordon("node1", "maintenance")

	snapshot := c.Snapshot()
	c.Uncordon("node1")

	if _, ok := snapshot["node1"]; !ok {
		t.Error("the snapshot changed when the registry did")
	}
}

// TestRestoreIsDetached confirms Restore copies rather than adopting the caller's map.
func TestRestoreIsDetached(t *testing.T) {
	entries := map[string]cordonEntry{"node1": {Reason: "maintenance"}}

	c := newCordonRegistry()
	c.Restore(entries)
	delete(entries, "node1")

	if !c.IsCordoned("node1") {
		t.Error("Restore adopted the caller's map instead of copying it")
	}
}

// TestCordonConcurrentAccess exercises the registry under -race: scheduling reads it every tick
// while an operator may be writing to it.
func TestCordonConcurrentAccess(t *testing.T) {
	c := newCordonRegistry()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			c.Cordon("node1", "churn")
			c.Uncordon("node1")
		}
	}()
	for i := 0; i < 500; i++ {
		c.IsCordoned("node1")
		c.Reason("node1")
		c.Snapshot()
	}
	<-done
}
