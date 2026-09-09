package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/auth"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/logging"
	"github.com/deziss/tasch/pkg/discovery"
	"github.com/deziss/tasch/pkg/profiler"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// watchDispatchWithReconnect streams dispatches for this node, reconnecting on failure.
//
// This replaces a ZeroMQ SUB socket that subscribed to everything and filtered by target node
// on arrival. Besides handing every worker every job's command and environment variables, that
// design dropped any dispatch published while a subscriber was mid-reconnect, because PUB has
// no delivery guarantee and the master discarded its send errors.
func watchDispatchWithReconnect(ctx context.Context, client pb.SchedulerServiceClient, nodeName string, running func() []*pb.RunningJob) <-chan *pb.DispatchMessage {
	ch := make(chan *pb.DispatchMessage, 100)
	go func() {
		defer close(ch)
		backoff := 1 * time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			// Report what is executing on every connect. After a master restart this is the only
			// way the master learns these jobs survived; without it they are declared failed
			// while the processes keep running and holding the node's resources.
			stream, err := client.WatchDispatch(ctx, &pb.WatchDispatchRequest{
				NodeName: nodeName, RunningJobs: running(),
			})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("cannot open dispatch stream, retrying", "backoff", backoff, "error", err)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}

			backoff = 1 * time.Second
			for {
				msg, err := stream.Recv()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Warn("dispatch stream closed, reconnecting", "error", err)
					break
				}
				select {
				case ch <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch
}

// reportWithRetry reports job result to master with exponential backoff.
func reportWithRetry(ctx context.Context, client pb.SchedulerServiceClient, req *pb.ReportResultRequest) {
	backoff := 1 * time.Second
	for {
		reportCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := client.ReportResult(reportCtx, req)
		cancel()
		if err == nil {
			return // Success
		}

		if ctx.Err() != nil {
			logging.Job(req.JobId).Warn("worker shutting down, discarding result report", "error", err)
			return
		}

		logging.Job(req.JobId).Warn("result report failed, retrying", "backoff", backoff, "error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			logging.Job(req.JobId).Warn("worker shut down during backoff, discarding result report")
			return
		}

		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// StartWorker initializes and runs a worker agent.
func StartWorker(cfg *config.Config) (cancel func(), err error) {
	nodeName := cfg.NodeName
	masterHost := cfg.MasterAddr

	ad, err := profiler.GenerateClassAd()
	if err != nil {
		return nil, fmt.Errorf("hardware profiling: %w", err)
	}

	advertiseAddr := os.Getenv("TASCH_ADVERTISE_ADDR")
	if advertiseAddr == "" && masterHost != "127.0.0.1" {
		advertiseAddr = discovery.GetLocalIP()
	}

	gossipKey, err := cfg.GossipKey()
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewNodeDiscovery(nodeName, 0, []byte(ad), advertiseAddr, 0, nil,
		&discovery.Options{EncryptionKey: gossipKey, Profile: cfg.Gossip.Profile})
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	joinAddr := fmt.Sprintf("%s:%d", masterHost, cfg.Ports.Gossip)
	if err := disc.Join([]string{joinAddr}); err != nil {
		_ = disc.Shutdown()
		return nil, fmt.Errorf("cluster join at %s: %w", joinAddr, err)
	}

	grpcAddr := os.Getenv("TASCH_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = fmt.Sprintf("%s:%d", masterHost, cfg.Ports.GRPC)
	}

	// gRPC with keepalive for resilience
	dialOpts := []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	if cfg.TLS.Enabled && cfg.TLS.CAFile != "" {
		var creds credentials.TransportCredentials
		if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			// Present a client certificate when one is configured, so a master requiring mutual
			// TLS accepts this worker.
			pool := x509.NewCertPool()
			caPEM, err := os.ReadFile(cfg.TLS.CAFile)
			if err != nil {
				_ = disc.Shutdown()
				return nil, fmt.Errorf("TLS: cannot read ca_file: %w", err)
			}
			if !pool.AppendCertsFromPEM(caPEM) {
				_ = disc.Shutdown()
				return nil, fmt.Errorf("TLS: ca_file %s contains no usable certificates", cfg.TLS.CAFile)
			}
			cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if err != nil {
				_ = disc.Shutdown()
				return nil, fmt.Errorf("TLS: %w", err)
			}
			creds = credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
				MinVersion:   tls.VersionTLS12,
			})
		} else {
			var err error
			creds, err = credentials.NewClientTLSFromFile(cfg.TLS.CAFile, "")
			if err != nil {
				_ = disc.Shutdown()
				return nil, fmt.Errorf("TLS: %w", err)
			}
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	if cfg.ClientToken != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(auth.TokenCredentials{
			Token:    cfg.ClientToken,
			Insecure: !cfg.TLS.Enabled,
		}))
	}

	grpcConn, err := grpc.NewClient(grpcAddr, dialOpts...)
	if err != nil {
		_ = disc.Shutdown()
		return nil, fmt.Errorf("gRPC connect: %w", err)
	}
	masterClient := pb.NewSchedulerServiceClient(grpcConn)

	var cancelMu sync.Mutex
	cancelFuncs := make(map[string]context.CancelFunc)
	// Attempt currently running for each job, so a re-delivered dispatch — a stream reconnect,
	// or the master re-dispatching after a lost acknowledgement — does not start a second copy
	// of a job this worker is already running.
	runningAttempts := make(map[string]int64)
	runningStarted := make(map[string]time.Time)

	// snapshotRunning describes this worker's in-flight jobs for the master to reconcile against.
	snapshotRunning := func() []*pb.RunningJob {
		cancelMu.Lock()
		defer cancelMu.Unlock()
		out := make([]*pb.RunningJob, 0, len(runningAttempts))
		for jobID, attempt := range runningAttempts {
			entry := &pb.RunningJob{JobId: jobID, Attempt: attempt}
			if started, ok := runningStarted[jobID]; ok {
				entry.StartTime = started.Unix()
			}
			out = append(out, entry)
		}
		return out
	}

	subCtx, cancelSub := context.WithCancel(context.Background())

	// Dispatch arrives over the same authenticated gRPC connection as everything else.
	msgCh := watchDispatchWithReconnect(subCtx, masterClient, nodeName, snapshotRunning)

	slog.Info("worker joined cluster", "node", nodeName, "master", masterHost)

	if cgroupsSupported() {
		slog.Info("job resource limits are enforced via cgroup v2")
	} else {
		slog.Warn("job resource limits cannot be enforced; jobs may use the whole machine",
			"reason", cgroupUnavailableReason())
	}

	// Jobs currently executing on this node, reported by /ready.
	var runningJobCount atomic.Int64

	// A worker-only node exported nothing: no /metrics, no /health, no /ready. The machines
	// actually running the workloads were invisible to Prometheus and to any load balancer.
	// In "both" mode the master already owns this port, so skip it there.
	var workerHTTP *http.Server
	if cfg.Role == "worker" {
		workerHTTP = startWorkerHealth(cfg, nodeName, &runningJobCount)
	}

	// Bound concurrent jobs. Every dispatch used to spawn an unbounded goroutine, so a burst of
	// submissions could fork a worker to death with nothing to stop it. A nil channel means
	// unlimited, preserving the previous behaviour when max_concurrent_jobs is unset.
	var jobSlots chan struct{}
	if cfg.MaxConcurrentJobs > 0 {
		jobSlots = make(chan struct{}, cfg.MaxConcurrentJobs)
	}

	go func() {
		for msg := range msgCh {
			// The stream only carries this node's work, so there is nothing to filter.
			payload := msg

			switch payload.Action {
			case "cancel":
				cancelMu.Lock()
				if cf, ok := cancelFuncs[payload.JobId]; ok {
					logging.Job(payload.JobId).Info("cancelling")
					cf()
				}
				cancelMu.Unlock()

			case "execute", "":
				cancelMu.Lock()
				if running, busy := runningAttempts[payload.JobId]; busy && running >= payload.Attempt {
					cancelMu.Unlock()
					logging.Job(payload.JobId).Info("already running, ignoring duplicate dispatch",
						"running_attempt", running, "offered_attempt", payload.Attempt)
					continue
				}
				runningAttempts[payload.JobId] = payload.Attempt
				runningStarted[payload.JobId] = time.Now()
				cancelMu.Unlock()

				go func(p *pb.DispatchMessage) {
					acknowledgeStart(subCtx, masterClient, nodeName, p)
					var ctx context.Context
					var cf context.CancelFunc
					if p.WalltimeSeconds > 0 {
						ctx, cf = context.WithTimeout(context.Background(), time.Duration(p.WalltimeSeconds)*time.Second)
					} else {
						ctx, cf = context.WithCancel(context.Background())
					}

					cancelMu.Lock()
					cancelFuncs[p.JobId] = cf
					cancelMu.Unlock()

					defer func() {
						cf()
						cancelMu.Lock()
						delete(cancelFuncs, p.JobId)
						if runningAttempts[p.JobId] == p.Attempt {
							delete(runningAttempts, p.JobId)
							delete(runningStarted, p.JobId)
						}
						cancelMu.Unlock()
					}()

					if jobSlots != nil {
						jobSlots <- struct{}{}
						defer func() { <-jobSlots }()
					}

					runningJobCount.Add(1)
					defer runningJobCount.Add(-1)

					logging.Job(p.JobId).Info("executing", "command", p.Command, "attempt", p.Attempt)
					startTime := time.Now()

					// Cap captured output. It was buffered without limit — a chatty job could OOM
					// the worker and every job sharing it — and then, if the job survived, the
					// oversized result was rejected by gRPC's 4 MiB receive limit, leaving
					// reportWithRetry looping forever on a permanent error.
					outputLimit := cfg.MaxOutputBytes
					if outputLimit <= 0 {
						outputLimit = defaultMaxOutputBytes
					}
					stdout := newCappedBuffer(outputLimit)
					stderr := newCappedBuffer(outputLimit)

					// Confine the job to what it reserved. Failure here is not fatal: many hosts
					// cannot delegate a cgroup subtree, and a worker that cannot enforce limits
					// must still be able to run work.
					cg, cgErr := newJobCgroup(p.JobId, int(p.CpusRequired), int(p.MemoryRequiredMb), cfg.MaxPIDsPerJob)
					if cgErr != nil {
						logging.Job(p.JobId).Warn("running without resource limits", "error", cgErr)
					}
					defer func() { _ = cg.Close() }()

					cmd := prepareCommand(ctx, p.Command, cg)
					cmd.Stdout = stdout
					cmd.Stderr = stderr

					if len(p.EnvVars) > 0 {
						env := os.Environ()
						for k, v := range p.EnvVars {
							env = append(env, fmt.Sprintf("%s=%s", k, v))
						}
						cmd.Env = env
					}

					execErr := cmd.Run()
					endTime := time.Now()

					if stdout.Len() > 0 {
						fmt.Print(stdout.String())
					}
					if stderr.Len() > 0 {
						fmt.Print(stderr.String())
					}

					success := execErr == nil
					errMsg := ""
					if execErr != nil {
						errMsg = execErr.Error()
						switch {
						case ctx.Err() == context.DeadlineExceeded:
							errMsg = fmt.Sprintf("walltime exceeded (%ds)", p.WalltimeSeconds)
						case ctx.Err() == context.Canceled:
							errMsg = "cancelled"
						case cg.WasOOMKilled():
							// Turn an opaque "signal: killed" into the actual reason.
							errMsg = fmt.Sprintf("out of memory: exceeded the %d MB reservation", p.MemoryRequiredMb)
						}
						logging.Job(p.JobId).Warn("job failed", "error", errMsg)
					} else {
						logging.Job(p.JobId).Info("completed",
							"duration", endTime.Sub(startTime), "peak_memory_bytes", cg.PeakMemoryBytes())
					}

					reportWithRetry(subCtx, masterClient, &pb.ReportResultRequest{
						JobId: p.JobId, WorkerNode: nodeName, Success: success,
						Output: stdout.String(), Error: errMsg,
						StartTime: startTime.Unix(), EndTime: endTime.Unix(),
						Attempt: p.Attempt,
					})
				}(payload)
			}
		}
	}()

	return func() {
		if workerHTTP != nil {
			shutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
			if err := workerHTTP.Shutdown(shutdownCtx); err != nil {
				slog.Error("worker health server shutdown", "error", err)
			}
			cancelHTTP()
		}
		cancelSub()
		func() { _ = grpcConn.Close() }()
		_ = disc.Shutdown()
	}, nil
}

// acknowledgeStart tells the master this worker has begun a job.
//
// This used to be an unauthenticated HTTP POST to the master's metrics port, with the port
// taken from the worker's own config — so the handshake silently failed whenever the master's
// metrics port differed, and the master then re-dispatched a job that was already running.
func acknowledgeStart(ctx context.Context, client pb.SchedulerServiceClient, nodeName string, p *pb.DispatchMessage) {
	for attempt := 0; attempt < 3; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := client.AcknowledgeStart(callCtx, &pb.AcknowledgeStartRequest{
			JobId: p.JobId, WorkerNode: nodeName, Attempt: p.Attempt,
		})
		cancel()
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
	logging.Job(p.JobId).Warn("could not acknowledge start; the master may re-dispatch this job")
}

// startWorkerHealth serves liveness, readiness, and metrics for a worker-only node.
func startWorkerHealth(cfg *config.Config, nodeName string, running *atomic.Int64) *http.Server {
	initMetrics()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ready", "node": nodeName, "running_jobs": running.Load(),
		})
	})

	bind := cfg.MetricsBind
	if bind == "" {
		bind = "0.0.0.0"
	}
	addr := fmt.Sprintf("%s:%d", bind, cfg.Ports.Metrics)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		slog.Info("worker health and metrics listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("worker health server error", "error", err)
		}
	}()
	return server
}
