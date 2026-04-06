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
	return &cobra.Command{
		Use:   "nodes",
		Short: "Show cluster nodes and hardware",
		Run: func(cmd *cobra.Command, args []string) {
			cfg := cfgLoader()
			client, conn := GetClient(cfg)
			defer conn.Close()

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

				fmt.Printf("Node: %s\n", nodeID)
				fmt.Printf("  OS: %v | Arch: %v | Cores: %v | Memory: %vMB | GPUs: %d\n",
					p["os"], p["architecture"], p["cpu_cores"], p["total_memory_mb"], gpuCount)

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
