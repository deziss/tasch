package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	pb "github.com/deziss/tasch/api/v1"
	"github.com/deziss/tasch/internal/config"
	"github.com/spf13/cobra"
)

// NodesCmd returns the `tasch nodes` command.
func NodesCmd(cfgLoader func() *config.Config) *cobra.Command {
	cmd := nodesListCmd(cfgLoader)
	cmd.AddCommand(cordonCmd(cfgLoader, true))
	cmd.AddCommand(cordonCmd(cfgLoader, false))
	cmd.AddCommand(drainCmd(cfgLoader))
	return cmd
}

func nodesListCmd(cfgLoader func() *config.Config) *cobra.Command {
	return &cobra.Command{
		Use:   "nodes",
		Short: "Show cluster nodes and hardware",
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer func() { _ = conn.Close() }()

			resp, err := client.WorkerStatus(context.Background(), &pb.WorkerStatusRequest{})
			if err != nil {
				log.Fatalf("Failed to get cluster status: %v", err)
			}

			fmt.Println("--- Cluster Nodes ---")
			if len(resp.WorkerNodes) == 0 {
				fmt.Println("No active workers found.")
				return
			}

			for nodeID, ad := range resp.WorkerNodes {
				var p map[string]interface{}
				if err := json.Unmarshal([]byte(ad), &p); err != nil {
					fmt.Printf("Node: %s\n  ClassAd: %s\n", nodeID, ad)
					continue
				}

				gpuCount := 0
				if gc, ok := p["gpu_count"].(float64); ok {
					gpuCount = int(gc)
				}

				// Say plainly whether the node is taking work. An idle cluster with a full queue
				// was previously only diagnosable by reading the master's logs.
				state := resp.NodeState[nodeID]
				status := "READY"
				switch {
				case state.GetCordoned():
					status = "CORDONED"
				case state.GetCircuitBroken():
					status = "CIRCUIT-BROKEN"
				}

				fmt.Printf("Node: %s  [%s]\n", nodeID, status)
				fmt.Printf("  OS: %v | Arch: %v | Cores: %v | Memory: %vMB | GPUs: %d\n",
					p["os"], p["architecture"], p["cpu_cores"], p["total_memory_mb"], gpuCount)
				fmt.Printf("  Running: %d job(s) | GPUs in use: %d/%d\n",
					state.GetRunningJobs(), state.GetGpusAllocated(), gpuCount)
				if state.GetCordoned() {
					fmt.Printf("  Cordoned: %s\n", state.GetCordonReason())
				}
				if state.GetCircuitBroken() {
					fmt.Printf("  Circuit breaker tripped after repeated failures; will retry automatically\n")
				}

				if gpuCount > 0 {
					vendor := ""
					if v, ok := p["gpu_vendor"].(string); ok {
						vendor = v
					}
					if models, ok := p["gpu_models"].([]interface{}); ok {
						for i, m := range models {
							memStr := ""
							if mems, ok := p["gpu_memory_mb"].([]interface{}); ok && i < len(mems) {
								memStr = fmt.Sprintf(" (%vMB)", mems[i])
							}
							fmt.Printf("    GPU %d: %v%s\n", i, m, memStr)
						}
					}
					versionLabel := "CUDA"
					versionField := "cuda_version"
					if vendor == "amd" {
						versionLabel = "ROCm"
						versionField = "rocm_version"
					}
					if ver, ok := p[versionField].(string); ok && ver != "" {
						fmt.Printf("    %s: %s\n", versionLabel, ver)
					}
				}
			}
		},
	}
}

// cordonCmd builds `tasch nodes cordon` and `tasch nodes uncordon`.
func cordonCmd(cfgLoader func() *config.Config, cordon bool) *cobra.Command {
	use, short := "uncordon <node>", "Return a node to scheduling rotation"
	if cordon {
		use, short = "cordon <node>", "Stop scheduling new jobs onto a node"
	}

	var reason string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + ".\n\n" +
			"Cordoning stops new dispatches but lets the jobs already running finish. Use\n" +
			"`tasch nodes drain` to cancel them as well. Cordons survive a master restart.",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer func() { _ = conn.Close() }()

			resp, err := client.CordonNode(context.Background(), &pb.CordonNodeRequest{
				NodeName: args[0], Cordon: cordon, Reason: reason,
			})
			if err != nil {
				log.Fatalf("Failed: %v", err)
			}
			fmt.Printf("%s: %s\n", resp.NodeName, resp.Message)
		},
	}
	if cordon {
		cmd.Flags().StringVar(&reason, "reason", "", "Why the node is being taken out of service")
	}
	return cmd
}

// drainCmd builds `tasch nodes drain`.
func drainCmd(cfgLoader func() *config.Config) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "drain <node>",
		Short: "Cordon a node and cancel the jobs running on it",
		Long: "Cordon a node and cancel the jobs running on it.\n\n" +
			"Use before rebooting or decommissioning a machine. Cancelled jobs are not retried,\n" +
			"so drain deliberately loses their work; cordon alone if you can wait for them.",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer func() { _ = conn.Close() }()

			resp, err := client.CordonNode(context.Background(), &pb.CordonNodeRequest{
				NodeName: args[0], Cordon: true, Drain: true, Reason: reason,
			})
			if err != nil {
				log.Fatalf("Failed: %v", err)
			}
			fmt.Printf("%s: %s\n", resp.NodeName, resp.Message)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the node is being drained")
	return cmd
}
