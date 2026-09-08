package daemon

import (
	"testing"

	pb "github.com/deziss/tasch/api/v1"
)

// TestBusDeliversOnlyToTheTargetNode is the regression test for the credential-broadcast defect:
// the old ZeroMQ PUB socket sent every dispatch to every subscriber, with the "is this mine?"
// check performed on the worker. Anything that could connect to the bus port received every
// job's command and every job's environment variables, tokens included.
func TestBusDeliversOnlyToTheTargetNode(t *testing.T) {
	b := newDispatchBus()

	node1, stop1 := b.Subscribe("node1")
	defer stop1()
	node2, stop2 := b.Subscribe("node2")
	defer stop2()

	secret := &pb.DispatchMessage{
		JobId:   "job-a",
		Command: "train.py",
		Action:  "execute",
		EnvVars: map[string]string{"HF_TOKEN": "super-secret"},
	}
	if err := b.Send("node1", secret); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case got := <-node1:
		if got.JobId != "job-a" {
			t.Errorf("node1 received %s, want job-a", got.JobId)
		}
	default:
		t.Fatal("node1 did not receive the dispatch addressed to it")
	}

	select {
	case leaked := <-node2:
		t.Fatalf("node2 received a dispatch for node1, leaking env %v", leaked.EnvVars)
	default:
		// Correct: node2 sees nothing.
	}
}

// TestBusReportsUndeliverable confirms a dispatch to a node with no live stream is an error the
// caller can act on. The publish it replaces returned nil unconditionally and its error was
// discarded at every call site, so a lost dispatch or a lost cancel was silent.
func TestBusReportsUndeliverable(t *testing.T) {
	b := newDispatchBus()
	err := b.Send("ghost-node", &pb.DispatchMessage{JobId: "job-a", Action: "execute"})
	if err == nil {
		t.Fatal("Send to a node with no stream reported success")
	}
}

// TestBusReportsFullQueue confirms a worker that stops reading does not block the scheduler and
// is reported rather than silently dropped.
func TestBusReportsFullQueue(t *testing.T) {
	b := newDispatchBus()
	_, stop := b.Subscribe("slow-node")
	defer stop()

	// Fill the queue; the subscriber never reads.
	for i := 0; i < dispatchQueueDepth; i++ {
		if err := b.Send("slow-node", &pb.DispatchMessage{JobId: "filler", Action: "execute"}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if err := b.Send("slow-node", &pb.DispatchMessage{JobId: "overflow", Action: "execute"}); err == nil {
		t.Fatal("Send onto a full queue reported success")
	}
}

// TestBusUnsubscribeStopsDelivery confirms a disconnected worker stops receiving work.
func TestBusUnsubscribeStopsDelivery(t *testing.T) {
	b := newDispatchBus()
	_, stop := b.Subscribe("node1")

	stop()
	stop() // must be safe to call twice; the stream defer and shutdown can race

	if err := b.Send("node1", &pb.DispatchMessage{JobId: "job-a", Action: "execute"}); err == nil {
		t.Fatal("a dispatch was accepted for an unsubscribed node")
	}
	if got := len(b.ConnectedNodes()); got != 0 {
		t.Errorf("ConnectedNodes = %d, want 0", got)
	}
}

// TestBusHandlesReconnectOverlap confirms a worker reconnecting before its old stream is torn
// down still receives its work; the worker's own attempt tracking discards the duplicate.
func TestBusHandlesReconnectOverlap(t *testing.T) {
	b := newDispatchBus()

	oldStream, stopOld := b.Subscribe("node1")
	newStream, stopNew := b.Subscribe("node1")
	defer stopNew()

	if err := b.Send("node1", &pb.DispatchMessage{JobId: "job-a", Action: "execute"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(oldStream) != 1 || len(newStream) != 1 {
		t.Errorf("old=%d new=%d, want both to receive the dispatch", len(oldStream), len(newStream))
	}

	stopOld()
	if err := b.Send("node1", &pb.DispatchMessage{JobId: "job-b", Action: "execute"}); err != nil {
		t.Fatalf("Send after the old stream closed: %v", err)
	}
}

// TestBusCloseReleasesSubscribers confirms shutdown unblocks every stream.
func TestBusCloseReleasesSubscribers(t *testing.T) {
	b := newDispatchBus()
	stream, stop := b.Subscribe("node1")

	b.Close()

	if _, open := <-stream; open {
		t.Error("the stream is still open after Close")
	}
	// The stream's own deferred unsubscribe must not panic on an already-closed channel.
	stop()
}
