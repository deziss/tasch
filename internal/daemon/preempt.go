package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/logging"
	"github.com/deziss/tasch/pkg/scheduler"
	"github.com/hashicorp/memberlist"
)

// Preemption.
//
// Priority decides the order jobs start in. That is enough until the cluster is full, at which
// point it decides nothing: a job submitted at the highest priority still waits behind whatever
// bulk work happens to be running, which can be hours. "Urgent" stops meaning anything exactly
// when it matters most.
//
// Preemption makes priority bind on a full cluster by evicting lower-priority work to make
// room. It is off by default because the cost is real — the evicted job's progress is thrown
// away — and it is hedged with three guards that exist to stop it becoming a way to make no
// progress at all: a priority margin, a minimum runtime, and a cap on victims per placement.
//
// Evicted jobs are requeued, not failed. They keep their retry budget and go back at their own
// priority, so a preempted job is delayed rather than lost.

// victim is a running job that could be evicted, with the resources doing so would free.
type victim struct {
	job  *scheduler.Job
	node string
}

// preemptFor tries to free room for job on some node by evicting lower-priority work.
//
// It reports whether anything was evicted. Nothing is dispatched here: the next scheduling tick
// places the job normally once the resources come back, which keeps one path responsible for
// dispatch rather than two.
func preemptFor(srv *schedulerServer, job *scheduler.Job, members []*memberlist.Node) bool {
	cfg := srv.cfg.Preemption
	if !cfg.Enabled || job.GroupID != "" {
		// Gang jobs are excluded: freeing room for one rank is pointless unless room appears
		// for every rank at once, and evicting work to achieve a placement that then fails is
		// strictly worse than waiting.
		return false
	}

	for _, member := range members {
		if len(member.Meta) == 0 || srv.cb.IsBlocked(member.Name) || srv.cordons.IsCordoned(member.Name) {
			continue
		}
		if !srv.policy.MatchesPartition(job.Partition, string(member.Meta)) {
			continue
		}
		if ok, _ := srv.reservations.Admits(member.Name, job.User, job.Account,
			job.WalltimeSeconds, time.Now()); !ok {
			continue
		}
		if match, err := srv.eval.Match(job.Requirement, string(member.Meta)); err != nil || !match {
			continue
		}

		chosen := chooseVictims(srv, job, member)
		if len(chosen) == 0 {
			continue
		}
		evict(srv, job, chosen)
		return true
	}
	return false
}

// chooseVictims picks the cheapest set of running jobs on a node whose eviction would let job
// fit, or nothing if no admissible set does.
func chooseVictims(srv *schedulerServer, job *scheduler.Job, member *memberlist.Node) []victim {
	cfg := srv.cfg.Preemption
	candidates := eligibleVictims(srv, job, member.Name, time.Now())
	if len(candidates) == 0 {
		return nil
	}

	// Cheapest first: lowest priority, and among equals the one that has run least, so the
	// least work is discarded.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].job.Priority != candidates[j].job.Priority {
			return candidates[i].job.Priority > candidates[j].job.Priority
		}
		return candidates[i].job.StartTime.After(candidates[j].job.StartTime)
	})

	total := nodeCapacity(member)
	free := freeOn(srv, member.Name, total)

	limit := cfg.MaxVictimsPerJob
	if limit <= 0 {
		limit = len(candidates)
	}

	var chosen []victim
	for _, c := range candidates {
		if fits(job, free) {
			break
		}
		if len(chosen) >= limit {
			break
		}
		free.gpus += c.job.GPUsRequired
		free.cpus += c.job.CPUsRequired
		free.memMB += c.job.MemoryRequiredMB
		chosen = append(chosen, c)
	}

	if !fits(job, free) {
		// Evicting this set still would not place the job, so evicting it would destroy work
		// and achieve nothing.
		return nil
	}
	return chosen
}

// eligibleVictims are the running jobs on a node that the guards permit evicting for job.
func eligibleVictims(srv *schedulerServer, job *scheduler.Job, node string, now time.Time) []victim {
	cfg := srv.cfg.Preemption
	var out []victim
	for _, running := range srv.queue.RunningJobsOnNode(node) {
		if running.GroupID != "" {
			// A gang rank cannot be evicted alone without failing every other rank with it.
			continue
		}
		if !partitionPreemptible(srv, running.Partition) {
			continue
		}
		// Lower priority sorts as a higher number, so a victim must be numerically greater by
		// at least the margin.
		if running.Priority < job.Priority+cfg.PriorityMargin {
			continue
		}
		if cfg.MinRuntimeSeconds > 0 && !running.StartTime.IsZero() {
			if now.Sub(running.StartTime) < time.Duration(cfg.MinRuntimeSeconds)*time.Second {
				continue
			}
		}
		out = append(out, victim{job: running, node: node})
	}
	return out
}

// partitionPreemptible reports whether jobs in a partition may be evicted.
//
// A job with no partition is never preemptible: with no partition there is no operator decision
// to point at, and evicting on a default would surprise every cluster that upgrades.
func partitionPreemptible(srv *schedulerServer, partition string) bool {
	if partition == "" {
		return false
	}
	part, ok := srv.policy.PartitionByName(partition)
	return ok && part.Preemptible
}

// capacity is a node's totals or its free remainder.
type capacity struct {
	gpus  int
	cpus  int
	memMB int
}

func nodeCapacity(member *memberlist.Node) capacity {
	ad := parseNodeAd(member)
	return capacity{
		gpus:  intFromAd(ad, "gpu_count"),
		cpus:  intFromAd(ad, "cpu_cores"),
		memMB: intFromAd(ad, "total_memory_mb"),
	}
}

func freeOn(srv *schedulerServer, node string, total capacity) capacity {
	return capacity{
		gpus:  srv.gpuTracker.AvailableGPUs(node, total.gpus),
		cpus:  srv.gpuTracker.AvailableCPUs(node, total.cpus),
		memMB: srv.gpuTracker.AvailableMemory(node, total.memMB),
	}
}

func fits(job *scheduler.Job, free capacity) bool {
	return free.gpus >= job.GPUsRequired &&
		free.cpus >= job.CPUsRequired &&
		free.memMB >= job.MemoryRequiredMB
}

// evict requeues the chosen jobs and tells their workers to stop.
func evict(srv *schedulerServer, incoming *scheduler.Job, victims []victim) {
	for _, v := range victims {
		// Requeue rather than cancel: the job goes back at its own priority with its retry
		// budget intact, so preemption delays work instead of destroying it.
		requeued, ok := srv.state.RequeueRunning(v.job.ID)
		if !ok || requeued == nil {
			continue
		}
		srv.gpuTracker.Release(v.job.ID)

		if err := srv.bus.Send(v.node, &pb.DispatchMessage{
			JobId: v.job.ID, Action: "cancel", Attempt: v.job.Attempt,
		}); err != nil {
			logging.Job(v.job.ID).Error("could not deliver preemption stop",
				"node", v.node, "error", err)
		}

		preemptionsTotal.Inc()
		reason := fmt.Sprintf("Preempted on %s to make room for %s (priority %d vs %d)",
			v.node, incoming.ID, incoming.Priority, v.job.Priority)
		srv.appendLog(v.job.ID, "WARN", reason)
		srv.appendLog(incoming.ID, "INFO", fmt.Sprintf("Preempted %s on %s", v.job.ID, v.node))
		logging.Job(v.job.ID).Warn("preempted", "node", v.node, "for", incoming.ID,
			"victim_priority", v.job.Priority, "incoming_priority", incoming.Priority)
	}
}

// preemptionConfigured reports whether preemption could ever act, for the startup banner.
func preemptionConfigured(cfg *config.Config) bool {
	if !cfg.Preemption.Enabled {
		return false
	}
	for _, p := range cfg.Partitions {
		if p.Preemptible {
			return true
		}
	}
	return false
}

// parseNodeAd decodes a member's class ad, returning nil when it cannot be read.
//
// A nil ad yields zero capacity everywhere it is used, which is the safe direction: an
// unreadable node looks full rather than infinitely free.
func parseNodeAd(member *memberlist.Node) map[string]interface{} {
	if len(member.Meta) == 0 {
		return nil
	}
	var ad map[string]interface{}
	if err := json.Unmarshal(member.Meta, &ad); err != nil {
		return nil
	}
	return ad
}

// intFromAd reads a numeric class-ad field, which JSON decoding gives back as a float64.
func intFromAd(ad map[string]interface{}, key string) int {
	v, _ := ad[key].(float64)
	return int(v)
}
