package daemon

import (
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
)

func initMetrics() {
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
	)
}

// startMetricsServer is now replaced by startHealthAndMetrics in master.go
