package cli

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/store"
	"github.com/spf13/cobra"
)

func parseEnvFlags(envFlags []string) map[string]string {
	result := make(map[string]string)
	for _, e := range envFlags {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

// JobsCmd returns the `tasch jobs` command tree.
func JobsCmd(cfgLoader func() *config.Config) *cobra.Command {
	var listStateFilter string

	jobsCmd := &cobra.Command{
		Use:   "jobs",
		Short: "Manage cluster jobs (list, submit, train, cancel, status, logs)",
		Run: func(cmd *cobra.Command, args []string) {
			// Bare `tasch jobs` = list all
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer conn.Close()
			listJobs(client, listStateFilter)
		},
	}
	jobsCmd.Flags().StringVarP(&listStateFilter, "state", "s", "", "Filter by state (QUEUED, RUNNING, COMPLETED, FAILED, CANCELLED)")

	// --- submit ---
	var submitPriority int32
	var submitUser string
	var submitWalltime int32
	var submitGPUs int32
	var submitEnvVars []string

	submitCmd := &cobra.Command{
		Use:   "submit [cel_expression] [command]",
		Short: "Submit a new job to the cluster",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer conn.Close()

			resp, err := client.SubmitJob(context.Background(), &pb.SubmitJobRequest{
				CelRequirement:  args[0],
				Command:         args[1],
				Priority:        submitPriority,
				User:            submitUser,
				WalltimeSeconds: submitWalltime,
				GpusRequired:    submitGPUs,
				EnvVars:         parseEnvFlags(submitEnvVars),
			})
			if err != nil {
				log.Fatalf("Submit failed: %v", err)
			}
			fmt.Printf("Job submitted!\n  ID:       %s\n  Status:   %s\n", resp.JobId, resp.Status)
			if submitGPUs > 0 {
				fmt.Printf("  GPUs:     %d\n", submitGPUs)
			}
			if submitWalltime > 0 {
				fmt.Printf("  Walltime: %ds\n", submitWalltime)
			}
		},
	}
	submitCmd.Flags().Int32VarP(&submitPriority, "priority", "p", 0, "Job priority (lower = higher, default: 10)")
	submitCmd.Flags().StringVarP(&submitUser, "user", "u", "", "Username for fairshare tracking")
	submitCmd.Flags().Int32VarP(&submitWalltime, "walltime", "w", 0, "Max execution time in seconds (0 = no limit)")
	submitCmd.Flags().Int32Var(&submitGPUs, "gpus", 0, "Number of GPUs required")
	submitCmd.Flags().StringSliceVarP(&submitEnvVars, "env", "e", nil, "Environment variables (KEY=VALUE)")

	// --- train ---
	var trainNodes int32
	var trainGPUsPerNode int32
	var trainRequirement string
	var trainPriority int32
	var trainUser string
	var trainWalltime int32
	var trainMasterPort int32
	var trainEnvVars []string

	trainCmd := &cobra.Command{
		Use:   "train [command]",
		Short: "Submit a distributed training job across multiple nodes",
		Long: `Submit a distributed training job that runs the same command on N nodes.
Auto-injected env vars: $RANK, $WORLD_SIZE, $MASTER_ADDR, $MASTER_PORT, $LOCAL_RANK, $NPROC_PER_NODE`,
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer conn.Close()

			resp, err := client.SubmitDistributedJob(context.Background(), &pb.SubmitDistributedJobRequest{
				CelRequirement:  trainRequirement,
				Command:         args[0],
				NumNodes:        trainNodes,
				GpusPerNode:     trainGPUsPerNode,
				Priority:        trainPriority,
				User:            trainUser,
				WalltimeSeconds: trainWalltime,
				MasterPort:      trainMasterPort,
				EnvVars:         parseEnvFlags(trainEnvVars),
			})
			if err != nil {
				log.Fatalf("Submit distributed job failed: %v", err)
			}

			fmt.Printf("Distributed training job submitted!\n")
			fmt.Printf("  Group ID:       %s\n", resp.GroupId)
			fmt.Printf("  Nodes:          %d\n", trainNodes)
			fmt.Printf("  GPUs/node:      %d\n", trainGPUsPerNode)
			fmt.Printf("  Status:         %s\n", resp.Status)
			fmt.Printf("  Rank jobs:      %s\n", strings.Join(resp.JobIds, ", "))
			fmt.Printf("\nMonitor:\n")
			for i, jid := range resp.JobIds {
				fmt.Printf("  tasch jobs logs %s    # rank %d\n", jid, i)
			}
		},
	}
	trainCmd.Flags().Int32Var(&trainNodes, "nodes", 2, "Number of nodes")
	trainCmd.Flags().Int32Var(&trainGPUsPerNode, "gpus-per-node", 1, "GPUs per node")
	trainCmd.Flags().StringVar(&trainRequirement, "requirement", "ad.gpu_count >= 1", "CEL expression for node matching")
	trainCmd.Flags().Int32VarP(&trainPriority, "priority", "p", 0, "Job priority")
	trainCmd.Flags().StringVarP(&trainUser, "user", "u", "", "Username")
	trainCmd.Flags().Int32VarP(&trainWalltime, "walltime", "w", 0, "Max execution time in seconds")
	trainCmd.Flags().Int32Var(&trainMasterPort, "master-port", 29500, "DDP coordination port")
	trainCmd.Flags().StringSliceVarP(&trainEnvVars, "env", "e", nil, "Environment variables (KEY=VALUE)")

	// --- cancel ---
	cancelCmd := &cobra.Command{
		Use:   "cancel [job_id]",
		Short: "Cancel a queued or running job",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer conn.Close()

			resp, err := client.CancelJob(context.Background(), &pb.CancelJobRequest{JobId: args[0]})
			if err != nil {
				log.Fatalf("Cancel failed: %v", err)
			}
			fmt.Printf("Job %s: %s — %s\n", resp.JobId, resp.Status, resp.Message)
		},
	}

	// --- status (job detail) ---
	statusCmd := &cobra.Command{
		Use:   "status [job_id]",
		Short: "Get the status and result of a specific job",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer conn.Close()

			resp, err := client.GetJobStatus(context.Background(), &pb.GetJobStatusRequest{JobId: args[0]})
			if err != nil {
				log.Fatalf("Failed to get job status: %v", err)
			}
			if resp.State == "NOT_FOUND" {
				fmt.Printf("Job %s not found.\n", args[0])
				return
			}

			fmt.Printf("Job %s\n", resp.JobId)
			fmt.Printf("  State:   %s\n", resp.State)
			fmt.Printf("  Command: %s\n", resp.Command)
			if resp.GroupId != "" {
				fmt.Printf("  Group:   %s\n", resp.GroupId)
			}
			if resp.WorkerNode != "" {
				fmt.Printf("  Worker:  %s\n", resp.WorkerNode)
			}
			if resp.SubmitTime > 0 {
				fmt.Printf("  Submit:  %s\n", time.Unix(resp.SubmitTime, 0).Format(time.RFC3339))
			}
			if resp.StartTime > 0 {
				fmt.Printf("  Start:   %s\n", time.Unix(resp.StartTime, 0).Format(time.RFC3339))
			}
			if resp.EndTime > 0 {
				fmt.Printf("  End:     %s\n", time.Unix(resp.EndTime, 0).Format(time.RFC3339))
				if resp.StartTime > 0 {
					dur := time.Unix(resp.EndTime, 0).Sub(time.Unix(resp.StartTime, 0))
					fmt.Printf("  Duration: %s\n", dur.Round(time.Millisecond))
				}
			}
			if resp.Output != "" {
				fmt.Printf("  Output:\n    %s\n", strings.ReplaceAll(strings.TrimSpace(resp.Output), "\n", "\n    "))
			}
			if resp.Error != "" {
				fmt.Printf("  Error:   %s\n", resp.Error)
			}
		},
	}

	// --- logs ---
	var logsFollow bool

	logsCmd := &cobra.Command{
		Use:   "logs [job_id]",
		Short: "Stream execution logs for a job",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer conn.Close()

			stream, err := client.StreamLogs(context.Background(), &pb.LogStreamRequest{JobId: args[0]})
			if err != nil {
				log.Fatalf("Failed to stream logs: %v", err)
			}
			for {
				entry, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					if !logsFollow {
						break
					}
					log.Fatalf("Stream error: %v", err)
				}
				ts := time.UnixMilli(entry.Timestamp).Format("15:04:05.000")
				fmt.Printf("[%s] [%s] %s\n", ts, entry.Level, entry.Message)
			}
		},
	}
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow log output")

	// --- failed (dead letter queue) ---
	failedCmd := &cobra.Command{
		Use:   "failed",
		Short: "Show jobs that exhausted all retries (dead letter queue)",
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			// Dead letters are in local BoltDB — only works on master machine
			storeImport, err := store.Open(config.StorePath())
			if err != nil {
				// Fallback: list FAILED jobs from gRPC
				client, conn := GetClient(cfg)
				defer conn.Close()
				listJobs(client, "FAILED")
				return
			}
			defer storeImport.Close()

			_ = cfg // used for fallback path
			deadLetters, err := storeImport.LoadDeadLetters()
			if err != nil || len(deadLetters) == 0 {
				fmt.Println("No dead letter jobs found.")
				return
			}

			fmt.Printf("--- Dead Letter Queue (%d jobs) ---\n", len(deadLetters))
			fmt.Printf("%-10s %-10s %-6s %s\n", "JOB_ID", "USER", "TRIES", "ERROR")
			fmt.Println(strings.Repeat("-", 60))
			for _, j := range deadLetters {
				errMsg := j.Error
				if len(errMsg) > 30 {
					errMsg = errMsg[:27] + "..."
				}
				fmt.Printf("%-10s %-10s %-6d %s\n", j.ID, j.User, j.RetryCount, errMsg)
			}
		},
	}

	// Wire up subcommands
	jobsCmd.AddCommand(submitCmd, trainCmd, cancelCmd, statusCmd, logsCmd, failedCmd)

	return jobsCmd
}

func listJobs(client pb.SchedulerServiceClient, stateFilter string) {
	resp, err := client.ListJobs(context.Background(), &pb.ListJobsRequest{StateFilter: stateFilter})
	if err != nil {
		log.Fatalf("Failed to list jobs: %v", err)
	}
	if len(resp.Jobs) == 0 {
		fmt.Println("No jobs found.")
		return
	}

	fmt.Printf("%-14s %-12s %-10s %-15s %-6s %-12s %s\n", "JOB_ID", "STATE", "USER", "WORKER", "PRI", "GROUP", "COMMAND")
	fmt.Println(strings.Repeat("-", 100))
	for _, j := range resp.Jobs {
		command := j.Command
		if len(command) > 25 {
			command = command[:22] + "..."
		}
		group := j.GroupId
		if group == "" {
			group = "-"
		}
		fmt.Printf("%-14s %-12s %-10s %-15s %-6d %-12s %s\n",
			j.JobId, j.State, j.User, j.WorkerNode, j.Priority, group, command)
	}
}
