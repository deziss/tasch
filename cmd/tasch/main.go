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
	"github.com/deziss/tasch/internal/logging"
	"github.com/deziss/tasch/internal/setup"
	"github.com/deziss/tasch/internal/version"
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
		Version: version.String(),
	}
	// Print the full build description rather than just the number, so a deployed node can be
	// matched to a commit.
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	rootCmd.PersistentFlags().StringVar(&configPath, "config", config.DefaultPath(), "Config file path")

	rootCmd.AddCommand(setupCmd())
	rootCmd.AddCommand(startCmd())
	rootCmd.AddCommand(stopCmd())
	rootCmd.AddCommand(cli.NodesCmd(loadConfig))
	rootCmd.AddCommand(cli.JobsCmd(loadConfig))
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(configCmd())

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
	if err := cfg.ApplyEnvOverrides(); err != nil {
		log.Fatalf("Invalid environment override: %v", err)
	}
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

			// Double-start prevention. Check that the PID is actually alive rather than trusting
			// the file's existence — a stale file from a crash would otherwise block every
			// subsequent start, and systemd would restart-loop into a failed unit.
			if pid, running := daemon.IsRunning(); running {
				fmt.Printf("Tasch is already running (PID %d). Stop with: tasch stop\n", pid)
				os.Exit(1)
			} else if pid != 0 {
				fmt.Printf("Removing stale PID file for dead process %d.\n", pid)
				daemon.RemovePID()
			}

			if err := cfg.Validate(); err != nil {
				log.Fatalf("Invalid configuration: %v", err)
			}

			// Install the structured logger before anything starts, so every daemon event —
			// including memberlist's and gRPC's, which use the standard log package — lands in
			// one parseable stream.
			logging.Setup(cfg.LogFormat, cfg.LogLevel)

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

			if err := daemon.WritePID(); err != nil {
				// Without a PID file `tasch stop` cannot find this process.
				log.Printf("Warning: could not write PID file: %v", err)
			}

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
			cfg := loadConfig()
			// Outlast the daemon's own drain, plus a margin for the final shutdown steps.
			if err := daemon.StopDaemon(cfg.DrainTimeout + 15); err != nil {
				log.Fatalf("%v", err)
			}
		},
	}
}

// --- version ---

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version, commit, and toolchain",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version.String())
		},
	}
}

// --- config ---

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and check the configuration",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Check the config for errors without starting anything",
		Run: func(cmd *cobra.Command, args []string) {
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				fmt.Printf("Config error: %v\n", err)
				os.Exit(1)
			}
			if err := cfg.ApplyEnvOverrides(); err != nil {
				fmt.Printf("Environment override error: %v\n", err)
				os.Exit(1)
			}
			if err := cfg.Validate(); err != nil {
				fmt.Printf("Invalid configuration: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Config %s is valid.\n", configPath)
			fmt.Printf("  role:    %s\n", cfg.Role)
			fmt.Printf("  node:    %s\n", cfg.NodeName)
			fmt.Printf("  master:  %s\n", cfg.GRPCAddr())

			// Call out the settings that silently leave a cluster wide open.
			if !cfg.Auth.Enabled {
				fmt.Println("  WARNING: auth.enabled is false — any host that can reach the gRPC port")
				fmt.Println("           can run arbitrary commands on every worker.")
			}
			if cfg.Gossip.EncryptionKey == "" && cfg.Gossip.KeyFile == "" {
				fmt.Println("  WARNING: gossip.encryption_key is unset — any host can join the cluster")
				fmt.Println("           and advertise fabricated resources to attract jobs.")
			}
			if !cfg.TLS.Enabled {
				fmt.Println("  WARNING: tls.enabled is false — job commands and environment variables")
				fmt.Println("           travel in cleartext.")
			}
			if cfg.MaxConcurrentJobs == 0 {
				fmt.Println("  NOTE:    max_concurrent_jobs is unlimited; a burst of submissions can")
				fmt.Println("           fork a worker to death.")
			}
		},
	})
	return cmd
}
