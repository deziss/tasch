package ha

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Reservations hold nodes aside for a window of time.
//
// A cordon is the blunt version of this: it stops a node taking new work immediately and stays
// until someone remembers to lift it. That is wrong for planned work. Cordoning a node an hour
// before a maintenance window wastes an hour of it, and cordoning it at the start of the window
// leaves whatever is still running to be killed.
//
// A reservation says *when*, so the scheduler can drain the node by itself: it stops placing
// jobs that would still be running when the window opens, and lets everything that fits run to
// completion. It also expresses the other half of the same idea — holding nodes for a
// particular team ahead of a deadline — with the same mechanism.
type Reservations struct {
	mu      sync.RWMutex
	entries map[string]Reservation
}

// Reservation is one window on one set of nodes.
type Reservation struct {
	ID    string    `json:"id"`
	Nodes []string  `json:"nodes"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`

	// Users and Accounts may run on the reserved nodes during the window. Both empty means
	// nobody may: that is a maintenance reservation, which is the point of reserving a node
	// away from everyone rather than for someone.
	Users    []string `json:"users,omitempty"`
	Accounts []string `json:"accounts,omitempty"`

	Reason    string    `json:"reason,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Maintenance reports whether the reservation excludes everyone.
func (r Reservation) Maintenance() bool { return len(r.Users) == 0 && len(r.Accounts) == 0 }

// Covers reports whether the reservation applies to a node.
func (r Reservation) Covers(node string) bool {
	for _, n := range r.Nodes {
		if n == node {
			return true
		}
	}
	return false
}

// Active reports whether the window is open at t.
func (r Reservation) Active(t time.Time) bool {
	return !t.Before(r.Start) && t.Before(r.End)
}

// Admits reports whether a job's owner may use the reserved nodes.
func (r Reservation) Admits(user, account string) bool {
	for _, u := range r.Users {
		if u == user {
			return true
		}
	}
	for _, a := range r.Accounts {
		if a == account {
			return true
		}
	}
	return false
}

func NewReservations() *Reservations {
	return &Reservations{entries: make(map[string]Reservation)}
}

func (rs *Reservations) Add(r Reservation) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.entries[r.ID] = r
}

func (rs *Reservations) Remove(id string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	_, was := rs.entries[id]
	delete(rs.entries, id)
	return was
}

// List returns every reservation, soonest first.
func (rs *Reservations) List() []Reservation {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	out := make([]Reservation, 0, len(rs.entries))
	for _, r := range rs.entries {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Get returns one reservation.
func (rs *Reservations) Get(id string) (Reservation, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	r, ok := rs.entries[id]
	return r, ok
}

// Expired returns reservations whose window closed before t, so they can be cleaned up.
func (rs *Reservations) Expired(t time.Time) []string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	var out []string
	for id, r := range rs.entries {
		if r.End.Before(t) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Admits reports whether a job may start on a node now, and why not if it may not.
//
// Two rules, and the second is the one that makes a reservation more useful than a cordon:
//
//  1. While a window is open, only the people it was reserved for may run on those nodes.
//  2. Before a window opens, a job may only start if it will have finished by then. A job with
//     no walltime has no such guarantee, so it cannot start on a node with a reservation ahead
//     of it — which is what drains the node in time, without anyone having to cordon it early
//     and waste the interval.
func (rs *Reservations) Admits(node string, user, account string, walltimeSeconds int,
	now time.Time) (bool, string) {

	rs.mu.RLock()
	defer rs.mu.RUnlock()

	for _, r := range rs.entries {
		if !r.Covers(node) {
			continue
		}

		if r.Active(now) {
			if r.Admits(user, account) {
				continue
			}
			if r.Maintenance() {
				return false, fmt.Sprintf("%s is reserved for maintenance until %s (%s)",
					node, r.End.Format(time.RFC3339), r.reasonOr("no reason given"))
			}
			return false, fmt.Sprintf("%s is reserved for someone else until %s (%s)",
				node, r.End.Format(time.RFC3339), r.reasonOr("no reason given"))
		}

		if r.Start.After(now) {
			if r.Admits(user, account) {
				// The job's owner holds the upcoming reservation, so running into it is fine.
				continue
			}
			if walltimeSeconds <= 0 {
				return false, fmt.Sprintf("%s is reserved from %s and this job has no walltime, "+
					"so it cannot be guaranteed to finish first", node, r.Start.Format(time.RFC3339))
			}
			if now.Add(time.Duration(walltimeSeconds) * time.Second).After(r.Start) {
				return false, fmt.Sprintf("%s is reserved from %s and this job could still be "+
					"running then", node, r.Start.Format(time.RFC3339))
			}
		}
	}
	return true, ""
}

func (r Reservation) reasonOr(fallback string) string {
	if r.Reason == "" {
		return fallback
	}
	return r.Reason
}

// Snapshot copies the set for replication and persistence.
func (rs *Reservations) Snapshot() map[string]Reservation {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	out := make(map[string]Reservation, len(rs.entries))
	for id, r := range rs.entries {
		out[id] = r
	}
	return out
}

// Restore replaces the set, on a snapshot install or a restart.
func (rs *Reservations) Restore(entries map[string]Reservation) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.entries = make(map[string]Reservation, len(entries))
	for id, r := range entries {
		rs.entries[id] = r
	}
}
