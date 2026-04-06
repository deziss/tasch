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
	"google.golang.org/grpc/credentials/insecure"
)

// StartWorker initializes and runs a worker agent.
// It blocks until the returned cancel function is called.
func StartWorker(cfg *config.Config) (cancel func(), err error) {
	nodeName := cfg.NodeName
	masterHost := cfg.MasterAddr

	ad, err := profiler.GenerateClassAd()
	if err != nil {
		return nil, fmt.Errorf("hardware profiling: %w", err)
	}

	// Auto-detect advertise address for remote masters
	advertiseAddr := os.Getenv("TASCH_ADVERTISE_ADDR")
	if advertiseAddr == "" && masterHost != "127.0.0.1" {
		advertiseAddr = discovery.GetLocalIP()
	}

	disc, err := discovery.NewNodeDiscovery(nodeName, 0, []byte(ad), advertiseAddr, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery init: %w", err)
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

	grpcConn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		disc.Shutdown()
		return nil, fmt.Errorf("gRPC connect to %s: %w", grpcAddr, err)
	}
	masterClient := pb.NewSchedulerServiceClient(grpcConn)

	subCtx, cancelSub := context.WithCancel(context.Background())
	subEndpoint := fmt.Sprintf("tcp://%s:%d", masterHost, cfg.Ports.ZMQ)

	sub, err := messaging.NewZMQSubscriber(subCtx, subEndpoint)
	if err != nil {
		cancelSub()
		grpcConn.Close()
		disc.Shutdown()
		return nil, fmt.Errorf("ZMQ subscribe to %s: %w", subEndpoint, err)
	}

	fmt.Printf("Worker '%s' joined cluster (master: %s)\n", nodeName, masterHost)
	fmt.Println("Listening for tasks...")

	var cancelMu sync.Mutex
	cancelFuncs := make(map[string]context.CancelFunc)

	go func() {
		for {
			msg, err := sub.Receive()
			if err != nil {
				if subCtx.Err() != nil {
					return
				}
				continue
			}

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

					fmt.Printf("Received Job %s: Executing '%s'...\n", p.JobID, p.Command)
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
							errMsg = fmt.Sprintf("walltime exceeded (%ds limit)", p.WalltimeSeconds)
						} else if ctx.Err() == context.Canceled {
							errMsg = "cancelled by user"
						}
						fmt.Printf("Job %s exited with error: %s\n", p.JobID, errMsg)
					} else {
						fmt.Printf("Job %s completed successfully.\n", p.JobID)
					}

					_, reportErr := masterClient.ReportResult(context.Background(), &pb.ReportResultRequest{
						JobId: p.JobID, WorkerNode: nodeName, Success: success,
						Output: stdout.String(), Error: errMsg,
						StartTime: startTime.Unix(), EndTime: endTime.Unix(),
					})
					if reportErr != nil {
						log.Printf("Warning: failed to report result for job %s: %v", p.JobID, reportErr)
					}
				}(payload)
			}
		}
	}()

	return func() {
		cancelSub()
		sub.Close()
		grpcConn.Close()
		disc.Shutdown()
	}, nil
}
