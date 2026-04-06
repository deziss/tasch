package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/pkg/discovery"
	"github.com/deziss/tasch/pkg/messaging"
	"github.com/deziss/tasch/pkg/profiler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// subscribeWithReconnect creates a ZMQ subscriber with automatic reconnection.
func subscribeWithReconnect(ctx context.Context, endpoint string) <-chan string {
	ch := make(chan string, 100)
	go func() {
		defer close(ch)
		backoff := 1 * time.Second
		for {
			sub, err := messaging.NewZMQSubscriber(ctx, endpoint)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[zmq] Reconnecting to %s in %s: %v", endpoint, backoff, err)
				time.Sleep(backoff)
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			backoff = 1 * time.Second
			for {
				msg, err := sub.Receive()
				if err != nil {
					if ctx.Err() != nil {
						sub.Close()
						return
					}
					log.Printf("[zmq] Receive error, reconnecting: %v", err)
					sub.Close()
					break
				}
				ch <- msg
			}
		}
	}()
	return ch
}

// reportWithRetry reports job result to master with timeout and one retry.
func reportWithRetry(client pb.SchedulerServiceClient, req *pb.ReportResultRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.ReportResult(ctx, req)
	if err != nil {
		log.Printf("[report] First attempt failed for job %s: %v, retrying...", req.JobId, err)
		time.Sleep(2 * time.Second)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		_, err = client.ReportResult(ctx2, req)
		if err != nil {
			log.Printf("[report] Retry failed for job %s: %v", req.JobId, err)
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

	disc, err := discovery.NewNodeDiscovery(nodeName, 0, []byte(ad), advertiseAddr, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	joinAddr := fmt.Sprintf("%s:%d", masterHost, cfg.Ports.Gossip)
	if err := disc.Join([]string{joinAddr}); err != nil {
		disc.Shutdown()
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
		creds, err := credentials.NewClientTLSFromFile(cfg.TLS.CAFile, "")
		if err != nil {
			disc.Shutdown()
			return nil, fmt.Errorf("TLS: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	grpcConn, err := grpc.NewClient(grpcAddr, dialOpts...)
	if err != nil {
		disc.Shutdown()
		return nil, fmt.Errorf("gRPC connect: %w", err)
	}
	masterClient := pb.NewSchedulerServiceClient(grpcConn)

	subCtx, cancelSub := context.WithCancel(context.Background())
	subEndpoint := fmt.Sprintf("tcp://%s:%d", masterHost, cfg.Ports.ZMQ)

	// ZMQ with auto-reconnection
	msgCh := subscribeWithReconnect(subCtx, subEndpoint)

	fmt.Printf("Worker '%s' joined cluster (master: %s)\n", nodeName, masterHost)
	fmt.Println("Listening for tasks...")

	var cancelMu sync.Mutex
	cancelFuncs := make(map[string]context.CancelFunc)

	go func() {
		for msg := range msgCh {
			var payload messaging.DispatchPayload
			if err := json.Unmarshal([]byte(msg), &payload); err != nil {
				continue
			}
			if payload.TargetNode != nodeName {
				continue
			}

			switch payload.Action {
			case "cancel":
				cancelMu.Lock()
				if cf, ok := cancelFuncs[payload.JobID]; ok {
					fmt.Printf("Cancelling job %s...\n", payload.JobID)
					cf()
				}
				cancelMu.Unlock()

			case "execute", "":
				go func(p messaging.DispatchPayload) {
					var ctx context.Context
					var cf context.CancelFunc
					if p.WalltimeSeconds > 0 {
						ctx, cf = context.WithTimeout(context.Background(), time.Duration(p.WalltimeSeconds)*time.Second)
					} else {
						ctx, cf = context.WithCancel(context.Background())
					}

					cancelMu.Lock()
					cancelFuncs[p.JobID] = cf
					cancelMu.Unlock()

					defer func() {
						cf()
						cancelMu.Lock()
						delete(cancelFuncs, p.JobID)
						cancelMu.Unlock()
					}()

					fmt.Printf("Job %s: executing '%s'...\n", p.JobID, p.Command)
					startTime := time.Now()

					var stdout, stderr bytes.Buffer
					cmd := exec.CommandContext(ctx, "sh", "-c", p.Command)
					cmd.Stdout = &stdout
					cmd.Stderr = &stderr

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
						if ctx.Err() == context.DeadlineExceeded {
							errMsg = fmt.Sprintf("walltime exceeded (%ds)", p.WalltimeSeconds)
						} else if ctx.Err() == context.Canceled {
							errMsg = "cancelled"
						}
						fmt.Printf("Job %s error: %s\n", p.JobID, errMsg)
					} else {
						fmt.Printf("Job %s completed.\n", p.JobID)
					}

					reportWithRetry(masterClient, &pb.ReportResultRequest{
						JobId: p.JobID, WorkerNode: nodeName, Success: success,
						Output: stdout.String(), Error: errMsg,
						StartTime: startTime.Unix(), EndTime: endTime.Unix(),
					})
				}(payload)
			}
		}
	}()

	return func() {
		cancelSub()
		grpcConn.Close()
		disc.Shutdown()
	}, nil
}
