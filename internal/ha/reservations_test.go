package ha

import (
	"strings"
	"testing"
	"time"
)

func window(start, end time.Time, nodes []string, users ...string) Reservation {
	return Reservation{ID: "r1", Nodes: nodes, Start: start, End: end, Users: users, Reason: "firmware"}
}

// While a maintenance window is open, nothing runs on the node — that is what distinguishes it
// from a reservation held *for* somebody.
func TestActiveMaintenanceWindowExcludesEveryone(t *testing.T) {
	rs := NewReservations()
	now := time.Now()
	rs.Add(window(now.Add(-time.Minute), now.Add(time.Hour), []string{"gpu-01"}))

	ok, reason := rs.Admits("gpu-01", "alice", "research", 60, now)
	if ok {
		t.Fatal("a job was admitted to a node inside its maintenance window")
	}
	if !strings.Contains(reason, "maintenance") || !strings.Contains(reason, "firmware") {
		t.Fatalf("reason %q should say it is maintenance and why", reason)
	}

	// Other nodes are unaffected.
	if ok, _ := rs.Admits("gpu-02", "alice", "research", 60, now); !ok {
		t.Fatal("a reservation on one node blocked another")
	}
}

// A reservation held for a team admits that team and nobody else.
func TestActiveReservationAdmitsItsOwner(t *testing.T) {
	rs := NewReservations()
	now := time.Now()
	rs.Add(window(now.Add(-time.Minute), now.Add(time.Hour), []string{"gpu-01"}, "alice"))

	if ok, reason := rs.Admits("gpu-01", "alice", "", 60, now); !ok {
		t.Fatalf("the reservation's owner was refused: %s", reason)
	}
	ok, reason := rs.Admits("gpu-01", "bob", "", 60, now)
	if ok {
		t.Fatal("a job from outside the reservation was admitted during the window")
	}
	if strings.Contains(reason, "maintenance") {
		t.Fatalf("reason %q should not call a held reservation maintenance", reason)
	}
}

// The part a cordon cannot do: before the window, a job that would still be running when it
// opens is held back, so the node drains itself in time and nothing has to be killed.
func TestUpcomingWindowDrainsTheNode(t *testing.T) {
	rs := NewReservations()
	now := time.Now()
	start := now.Add(30 * time.Minute)
	rs.Add(window(start, start.Add(time.Hour), []string{"gpu-01"}))

	// Ten minutes of work finishes well before the window: let it run.
	if ok, reason := rs.Admits("gpu-01", "alice", "", 600, now); !ok {
		t.Fatalf("a job that finishes before the window was refused: %s", reason)
	}

	// An hour of work would still be running when the window opens.
	ok, reason := rs.Admits("gpu-01", "alice", "", 3600, now)
	if ok {
		t.Fatal("a job that would run into the window was admitted")
	}
	if !strings.Contains(reason, "could still be running") {
		t.Fatalf("reason %q should say the job would run into the window", reason)
	}
}

// A job with no walltime cannot be promised to finish, so it cannot start ahead of a window.
// Without this rule one unbounded job defeats the whole mechanism.
func TestJobWithoutWalltimeCannotPrecedeAWindow(t *testing.T) {
	rs := NewReservations()
	now := time.Now()
	rs.Add(window(now.Add(time.Hour), now.Add(2*time.Hour), []string{"gpu-01"}))

	ok, reason := rs.Admits("gpu-01", "alice", "", 0, now)
	if ok {
		t.Fatal("an unbounded job was admitted ahead of a reservation")
	}
	if !strings.Contains(reason, "no walltime") {
		t.Fatalf("reason %q should point at the missing walltime", reason)
	}
}

// The owner of an upcoming reservation may run into it: the nodes are being held for them.
func TestOwnerMayRunIntoTheirOwnWindow(t *testing.T) {
	rs := NewReservations()
	now := time.Now()
	start := now.Add(10 * time.Minute)
	rs.Add(Reservation{ID: "r1", Nodes: []string{"gpu-01"}, Start: start,
		End: start.Add(time.Hour), Accounts: []string{"research"}})

	if ok, reason := rs.Admits("gpu-01", "alice", "research", 7200, now); !ok {
		t.Fatalf("the account holding the reservation was refused: %s", reason)
	}
	if ok, _ := rs.Admits("gpu-01", "bob", "other", 7200, now); ok {
		t.Fatal("someone else was allowed to run into a reservation they do not hold")
	}
}

// A closed window must stop constraining anything, and be collectable.
func TestExpiredReservationsAreInert(t *testing.T) {
	rs := NewReservations()
	now := time.Now()
	rs.Add(window(now.Add(-2*time.Hour), now.Add(-time.Hour), []string{"gpu-01"}))

	if ok, reason := rs.Admits("gpu-01", "alice", "", 0, now); !ok {
		t.Fatalf("a closed window still blocked a job: %s", reason)
	}
	expired := rs.Expired(now)
	if len(expired) != 1 || expired[0] != "r1" {
		t.Fatalf("Expired() = %v, want the closed reservation", expired)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	rs := NewReservations()
	now := time.Now().Truncate(time.Second)
	rs.Add(window(now, now.Add(time.Hour), []string{"gpu-01", "gpu-02"}, "alice"))

	restored := NewReservations()
	restored.Restore(rs.Snapshot())

	got, ok := restored.Get("r1")
	if !ok {
		t.Fatal("the reservation did not survive the round trip")
	}
	if len(got.Nodes) != 2 || got.Users[0] != "alice" || !got.Start.Equal(now) {
		t.Fatalf("restored reservation = %+v", got)
	}
	if !restored.Remove("r1") || len(restored.List()) != 0 {
		t.Fatal("Remove did not take effect on the restored set")
	}
}
