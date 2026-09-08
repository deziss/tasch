package daemon

import (
	"fmt"
	"sync"
	"testing"

	"github.com/deziss/tasch/pkg/scheduler"
)

// TestAllocateAssignsDistinctDevices is the regression test for the defect where every job was
// pinned to GPU 0: device indices were computed as 0..GPUsRequired-1 without consulting what
// was already in use, so concurrent jobs on one node collided on the same physical card while
// the rest of the node idled.
func TestAllocateAssignsDistinctDevices(t *testing.T) {
	gt := newGPUTracker()

	a, ok := gt.Allocate("node1", "job-a", 1, 0, 0, 4)
	if !ok {
		t.Fatal("first allocation failed")
	}
	b, ok := gt.Allocate("node1", "job-b", 1, 0, 0, 4)
	if !ok {
		t.Fatal("second allocation failed")
	}

	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected one device each, got %v and %v", a, b)
	}
	if a[0] == b[0] {
		t.Fatalf("both jobs pinned to GPU %d — they will collide", a[0])
	}
	if a[0] != 0 || b[0] != 1 {
		t.Errorf("expected devices 0 and 1, got %d and %d", a[0], b[0])
	}
}

// TestAllocateFillsNodeThenRefuses confirms a node hands out each device exactly once.
func TestAllocateFillsNodeThenRefuses(t *testing.T) {
	gt := newGPUTracker()
	const total = 4

	seen := map[int]string{}
	for i := 0; i < total; i++ {
		jobID := fmt.Sprintf("job-%d", i)
		devices, ok := gt.Allocate("node1", jobID, 1, 0, 0, total)
		if !ok {
			t.Fatalf("allocation %d failed while the node still had free GPUs", i)
		}
		if prior, dup := seen[devices[0]]; dup {
			t.Fatalf("GPU %d handed to both %s and %s", devices[0], prior, jobID)
		}
		seen[devices[0]] = jobID
	}

	if _, ok := gt.Allocate("node1", "job-overflow", 1, 0, 0, total); ok {
		t.Fatal("allocated a 5th GPU on a 4-GPU node")
	}
	if got := gt.AvailableGPUs("node1", total); got != 0 {
		t.Errorf("AvailableGPUs = %d, want 0", got)
	}
}

// TestAllocateMultiGPUJobGetsContiguousFreeDevices covers a job asking for several GPUs on a
// partially-used node.
func TestAllocateMultiGPUJobGetsContiguousFreeDevices(t *testing.T) {
	gt := newGPUTracker()

	if _, ok := gt.Allocate("node1", "small", 1, 0, 0, 8); !ok {
		t.Fatal("small allocation failed")
	}
	devices, ok := gt.Allocate("node1", "big", 4, 0, 0, 8)
	if !ok {
		t.Fatal("big allocation failed")
	}

	want := []int{1, 2, 3, 4}
	if len(devices) != len(want) {
		t.Fatalf("got %v, want %v", devices, want)
	}
	for i := range want {
		if devices[i] != want[i] {
			t.Fatalf("got %v, want %v", devices, want)
		}
	}
}

// TestReleaseIsIdempotent is the regression test for the double-release defect: several paths
// can end the same job — completion, cancel, walltime kill, gang-sibling cancel, dispatch
// timeout, worker loss — and each released unconditionally. Because the old tracker clamped
// underflow by deleting the node's counter, a second release zeroed usage while other jobs
// were still running, permanently oversubscribing the node.
func TestReleaseIsIdempotent(t *testing.T) {
	gt := newGPUTracker()
	const total = 8

	if _, ok := gt.Allocate("node1", "job-a", 4, 8, 16000, total); !ok {
		t.Fatal("allocate job-a failed")
	}
	if _, ok := gt.Allocate("node1", "job-b", 4, 8, 16000, total); !ok {
		t.Fatal("allocate job-b failed")
	}

	// job-a ends, and every end-of-job path fires for it.
	gt.Release("job-a")
	gt.Release("job-a")
	gt.Release("job-a")

	if got := gt.AvailableGPUs("node1", total); got != 4 {
		t.Errorf("AvailableGPUs = %d, want 4 — job-b still holds 4", got)
	}
	if got := gt.AvailableCPUs("node1", 16); got != 8 {
		t.Errorf("AvailableCPUs = %d, want 8", got)
	}
	if got := gt.AvailableMemory("node1", 32000); got != 16000 {
		t.Errorf("AvailableMemory = %d, want 16000", got)
	}

	// Releasing a job that never allocated must not disturb anything either.
	gt.Release("job-never-existed")
	if got := gt.AvailableGPUs("node1", total); got != 4 {
		t.Errorf("AvailableGPUs = %d after releasing an unknown job, want 4", got)
	}
}

// TestReleaseFreesDevicesForReuse confirms freed indices go back into circulation.
func TestReleaseFreesDevicesForReuse(t *testing.T) {
	gt := newGPUTracker()

	first, _ := gt.Allocate("node1", "job-a", 1, 0, 0, 2)
	gt.Allocate("node1", "job-b", 1, 0, 0, 2)
	gt.Release("job-a")

	reused, ok := gt.Allocate("node1", "job-c", 1, 0, 0, 2)
	if !ok {
		t.Fatal("could not reuse the released device")
	}
	if reused[0] != first[0] {
		t.Errorf("reused device %d, want %d (the one job-a freed)", reused[0], first[0])
	}
}

// TestAllocateIsIdempotentPerJob confirms a re-dispatch of the same job cannot double-book.
func TestAllocateIsIdempotentPerJob(t *testing.T) {
	gt := newGPUTracker()

	first, _ := gt.Allocate("node1", "job-a", 2, 4, 1000, 8)
	second, ok := gt.Allocate("node1", "job-a", 2, 4, 1000, 8)
	if !ok {
		t.Fatal("re-allocating an existing job failed")
	}

	if len(first) != len(second) || first[0] != second[0] {
		t.Errorf("re-allocation returned %v, want the original %v", second, first)
	}
	if got := gt.AvailableGPUs("node1", 8); got != 6 {
		t.Errorf("AvailableGPUs = %d, want 6 — the job was booked twice", got)
	}
	if got := gt.AvailableCPUs("node1", 16); got != 12 {
		t.Errorf("AvailableCPUs = %d, want 12 — CPUs were booked twice", got)
	}
}

// TestTrackerIsolatesNodes confirms allocations on one node do not consume another's devices.
func TestTrackerIsolatesNodes(t *testing.T) {
	gt := newGPUTracker()

	gt.Allocate("node1", "job-a", 2, 0, 0, 2)
	if got := gt.AvailableGPUs("node2", 2); got != 2 {
		t.Errorf("node2 AvailableGPUs = %d, want 2", got)
	}
	if _, ok := gt.Allocate("node2", "job-b", 2, 0, 0, 2); !ok {
		t.Error("node2 refused an allocation because of node1's usage")
	}
}

// TestTrackerConcurrentAllocateRelease exercises the tracker under -race and asserts no device
// is ever handed to two jobs at once.
func TestTrackerConcurrentAllocateRelease(t *testing.T) {
	gt := newGPUTracker()
	const total = 8

	var mu sync.Mutex
	held := map[int]string{}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			jobID := fmt.Sprintf("job-%d", i)
			devices, ok := gt.Allocate("node1", jobID, 1, 0, 0, total)
			if !ok {
				return
			}
			mu.Lock()
			for _, d := range devices {
				if prior, dup := held[d]; dup {
					mu.Unlock()
					t.Errorf("GPU %d held by both %s and %s", d, prior, jobID)
					return
				}
				held[d] = jobID
			}
			mu.Unlock()

			mu.Lock()
			for _, d := range devices {
				delete(held, d)
			}
			mu.Unlock()
			gt.Release(jobID)
		}(i)
	}
	wg.Wait()

	if got := gt.AvailableGPUs("node1", total); got != total {
		t.Errorf("AvailableGPUs = %d after all jobs released, want %d", got, total)
	}
}

// TestReconcileRebuildsAfterRestart is the regression test for the case where a master restart
// wiped the tracker while workers kept running their jobs: the new master saw every node as
// idle and oversubscribed it, with nothing to ever correct the drift.
func TestReconcileRebuildsAfterRestart(t *testing.T) {
	gt := newGPUTracker() // fresh, as after a restart
	total := func(string) int { return 8 }

	running := []*scheduler.Job{
		{ID: "job-a", WorkerNode: "node1", GPUsRequired: 2, CPUsRequired: 4, MemoryRequiredMB: 8000, State: scheduler.StateRunning},
		{ID: "job-b", WorkerNode: "node1", GPUsRequired: 2, State: scheduler.StateRunning},
	}

	dropped, rebooked := gt.Reconcile(running, total)
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if rebooked != 2 {
		t.Fatalf("rebooked = %d, want 2", rebooked)
	}
	if got := gt.AvailableGPUs("node1", 8); got != 4 {
		t.Errorf("AvailableGPUs = %d, want 4 — the running jobs were not re-booked", got)
	}
	if got := gt.AvailableCPUs("node1", 16); got != 12 {
		t.Errorf("AvailableCPUs = %d, want 12", got)
	}

	// Re-running must be stable, not compound.
	dropped, rebooked = gt.Reconcile(running, total)
	if dropped != 0 || rebooked != 0 {
		t.Errorf("second reconcile changed state: dropped=%d rebooked=%d, want 0/0", dropped, rebooked)
	}
	if got := gt.AvailableGPUs("node1", 8); got != 4 {
		t.Errorf("AvailableGPUs = %d after a second reconcile, want 4", got)
	}
}

// TestReconcileDropsLeakedAllocations confirms a missed release is eventually corrected rather
// than permanently shrinking the node's capacity.
func TestReconcileDropsLeakedAllocations(t *testing.T) {
	gt := newGPUTracker()
	total := func(string) int { return 4 }

	gt.Allocate("node1", "job-gone", 2, 0, 0, 4)
	gt.Allocate("node1", "job-live", 1, 0, 0, 4)

	running := []*scheduler.Job{
		{ID: "job-live", WorkerNode: "node1", GPUsRequired: 1, State: scheduler.StateRunning},
	}

	dropped, rebooked := gt.Reconcile(running, total)
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if rebooked != 0 {
		t.Errorf("rebooked = %d, want 0", rebooked)
	}
	if got := gt.AvailableGPUs("node1", 4); got != 3 {
		t.Errorf("AvailableGPUs = %d, want 3 — the leaked allocation was not reclaimed", got)
	}
}

// TestReconcileIgnoresJobsWithoutANode confirms a running job that has not been placed yet is
// not booked against an empty node name.
func TestReconcileIgnoresJobsWithoutANode(t *testing.T) {
	gt := newGPUTracker()
	running := []*scheduler.Job{
		{ID: "job-unplaced", WorkerNode: "", GPUsRequired: 1, State: scheduler.StateRunning},
	}
	if _, rebooked := gt.Reconcile(running, func(string) int { return 4 }); rebooked != 0 {
		t.Errorf("rebooked = %d, want 0", rebooked)
	}
}
