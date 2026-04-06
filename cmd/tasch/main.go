package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/deziss/tasch/internal/cli"
	"github.com/deziss/tasch/internal/config"
	"github.com/deziss/tasch/internal/daemon"
	"github.com/deziss/tasch/internal/setup"
	"github.com/spf13/cobra"
)

var configPath string

func main() {
	rootCmd := &cobra.Command{
		Use:   "tasch",
		Short: "Tasch — Distributed Task Scheduler",
		Long: `Tasch is a lightweight, distributed task scheduler with GPU support.

Get started:
  tasch setup     Interactive setup wizard
  tasch start     Start the scheduler
  tasch nodes     View cluster nodes
  tasch jobs      Manage jobs`,
	}

	rootCmd.PersistentFlags().StringVar(&configPath, "config", config.DefaultPath(), "Config file path")

	rootCmd.AddCommand(setupCmd())
	rootCmd.AddCommand(startCmd())
	rootCmd.AddCommand(stopCmd())
	rootCmd.AddCommand(cli.NodesCmd(loadConfig))
	rootCmd.AddCommand(cli.JobsCmd(loadConfig))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func loadConfig() *config.Config {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		// If no config exists, use defaults (allows `tasch jobs --config` override)
		cfg = config.DefaultConfig()
	}
	cfg.ApplyEnvOverrides()
	return cfg
}

// --- setup ---

func setupCmd() *cobra.Command {
	var nonInteractive bool
	var role, nodeName, masterAddr string
	var gossipPort, grpcPort, zmqPort int

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure this node (interactive wizard)",
		Run: func(cmd *cobra.Command, args []string) {
			var cfg *config.Config
			var err error

			if nonInteractive {
				cfg, err = setup.RunNonInteractive(role, nodeName, masterAddr, gossipPort, grpcPort, zmqPort)
			} else {
				cfg, err = setup.RunInteractive()
			}
			if err != nil {
				log.Fatalf("Setup failed: %v", err)
			}

			if err := config.SaveConfig(configPath, cfg); err != nil {
				log.Fatalf("Failed to write config: %v", err)
			}

			fmt.Printf("\nConfig written to %s\n", configPath)
			fmt.Println("Start with: tasch start")
		},
	}

	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Skip interactive prompts")
	cmd.Flags().StringVar(&role, "role", "", "Node role: master, worker, both")
	cmd.Flags().StringVar(&nodeName, "node-name", "", "Node name")
	cmd.Flags().StringVar(&masterAddr, "master-addr", "", "Master address")
	cmd.Flags().IntVar(&gossipPort, "gossip-port", 0, "Gossip port")
	cmd.Flags().IntVar(&grpcPort, "grpc-port", 0, "gRPC port")
	cmd.Flags().IntVar(&zmqPort, "zmq-port", 0, "ZMQ port")

	return cmd
}

// --- start ---

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start tasch (master/worker/both based on config)",
		Run: func(cmd *cobra.Command, args []string) {
			cfg := loadConfig()

			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				fmt.Println("No config found. Run 'tasch setup' first.")
				os.Exit(1)
			}

			// Double-start prevention
			if pidData, err := os.ReadFile(config.PidPath()); err == nil {
				fmt.Printf("Tasch may already be running (PID file exists: %s). Stop with: tasch stop\n", string(pidData))
				os.Exit(1)
			}

			fmt.Printf("Starting tasch (%s mode)...\n", cfg.Role)

			var masterHandle *daemon.MasterHandle
			var workerCancel func()

			if cfg.Role == "master" || cfg.Role == "both" {
				var err error
				masterHandle, err = daemon.StartMaster(cfg)
				if err != nil {
					log.Fatalf("Master failed to start: %v", err)
				}
			}

			if cfg.Role == "worker" || cfg.Role == "both" {
				var err error
				workerCancel, err = daemon.StartWorker(cfg)
				if err != nil {
					log.Fatalf("Worker failed to start: %v", err)
				}
			}

			daemon.WritePID()

			fmt.Println("Tasch is running. Stop with: tasch stop")

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			<-sigChan

			fmt.Println("\nShutting down...")

			// Graceful drain: stop accepting new jobs, wait for running to finish
			if masterHandle != nil {
				masterHandle.Draining.Store(true)
				fmt.Printf("Draining (waiting up to %ds for running jobs)...\n", cfg.DrainTimeout)
				deadline := time.Now().Add(time.Duration(cfg.DrainTimeout) * time.Second)
				for time.Now().Before(deadline) {
					running := masterHandle.Queue.RunningJobs()
					if len(running) == 0 {
						break
					}
					fmt.Printf("  %d jobs still running...\n", len(running))
					time.Sleep(2 * time.Second)
				}
			}

			if workerCancel != nil {
				workerCancel()
			}
			if masterHandle != nil {
				masterHandle.Cancel()
			}
			daemon.RemovePID()
			fmt.Println("Tasch stopped.")
		},
	}
}

// --- stop ---

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running tasch instance",
		Run: func(cmd *cobra.Command, args []string) {
			if err := daemon.StopDaemon(); err != nil {
				log.Fatalf("%v", err)
			}
		},
	}
}
