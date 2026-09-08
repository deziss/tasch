package daemon

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	jobsSubmittedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tasch_jobs_submitted_total",
		Help: "Total number of jobs submitted.",
	}, []string{"user"})

	jobsCompletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tasch_jobs_completed_total",
		Help: "Total number of jobs completed.",
	}, []string{"user", "status"})

	queueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tasch_queue_depth",
		Help: "Number of jobs currently queued.",
	})

	runningJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tasch_running_jobs",
		Help: "Number of jobs currently running.",
	})

	clusterNodes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tasch_cluster_nodes",
		Help: "Number of nodes in the cluster.",
	})

	dispatchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "tasch_dispatch_duration_seconds",
		Help:    "Time taken to dispatch a job to a worker.",
		Buckets: prometheus.DefBuckets,
	})

	jobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tasch_job_duration_seconds",
		Help:    "Total execution time of completed jobs.",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"user", "status"})

	groupsPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tasch_groups_pending",
		Help: "Number of distributed job groups waiting for node allocation.",
	})

	walltimeKillsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasch_walltime_kills_total",
		Help: "Total number of jobs killed due to walltime enforcement.",
	})

	workerLostTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasch_worker_lost_total",
		Help: "Total number of worker node departures detected.",
	})

	// --- Scheduler SLIs ---
	//
	// The original ten metrics said nothing about whether scheduling was healthy: there was no
	// queue-wait measure (the single most important scheduler SLI), and
	// tasch_dispatch_duration_seconds timed only the publish call, not matchmaking. These fill
	// that gap.

	queueWaitDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "tasch_queue_wait_seconds",
		Help:    "Time from job submission to first dispatch.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800},
	})

	schedulingTickDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "tasch_scheduling_tick_seconds",
		Help:    "Duration of one scheduling cycle, covering matchmaking and dispatch.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	})

	jobsByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tasch_jobs_by_state",
		Help: "Jobs currently held in memory, by state.",
	}, []string{"state"})

	// --- Resource utilization ---
	//
	// The GPU tracker's contents were never exported, so there was no way to see whether the
	// cluster was actually full or the scheduler had simply stopped placing work.

	gpusAllocated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tasch_gpus_allocated",
		Help: "GPUs currently allocated, by node.",
	}, []string{"node"})

	// --- Delivery and durability ---

	dispatchFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tasch_dispatch_failures_total",
		Help: "Dispatches that could not be delivered to a worker.",
	}, []string{"reason"})

	retriesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasch_job_retries_total",
		Help: "Total job retries scheduled.",
	})

	deadLettersTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasch_dead_letters_total",
		Help: "Total jobs moved to the dead letter queue after exhausting retries.",
	})

	staleResultsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasch_stale_results_total",
		Help: "Results discarded because they belonged to a superseded dispatch attempt.",
	})

	// dbWriteQueueDepth surfaces the persistence backlog. The write channel is bounded, and when
	// it fills the scheduler blocks on it — previously with no way to see that happening.
	dbWriteQueueDepth = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tasch_db_write_queue_depth",
		Help: "Pending asynchronous database writes.",
	})

	dbWriteErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasch_db_write_errors_total",
		Help: "Failed asynchronous database writes.",
	})

	reconcileCorrectionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tasch_reconcile_corrections_total",
		Help: "Resource accounting corrections applied by the reconciliation loop.",
	}, []string{"kind"})

	authFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tasch_auth_failures_total",
		Help: "Rejected requests, by reason.",
	}, []string{"reason"})
)

// initMetrics registers the collectors exactly once.
//
// It used to call MustRegister unconditionally on the default registry, so a second StartMaster
// in the same process — which the tests do — panicked on duplicate registration.
var metricsOnce sync.Once

func initMetrics() {
	metricsOnce.Do(registerMetrics)
}

func registerMetrics() {
	prometheus.MustRegister(
		jobsSubmittedTotal,
		jobsCompletedTotal,
		queueDepth,
		runningJobs,
		clusterNodes,
		dispatchDuration,
		jobDuration,
		groupsPending,
		walltimeKillsTotal,
		workerLostTotal,
		queueWaitDuration,
		schedulingTickDuration,
		jobsByState,
		gpusAllocated,
		dispatchFailuresTotal,
		retriesTotal,
		deadLettersTotal,
		staleResultsTotal,
		dbWriteQueueDepth,
		dbWriteErrorsTotal,
		reconcileCorrectionsTotal,
		authFailuresTotal,
	)
}

// startMetricsServer is now replaced by startHealthAndMetrics in master.go
