package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the Tasch node configuration written by `tasch setup`.
type Config struct {
	Role         string          `yaml:"role"` // master, worker, both
	NodeName     string          `yaml:"node_name"`
	MasterAddr   string          `yaml:"master_addr"` // IP/hostname of master node
	Ports        PortConfig      `yaml:"ports"`
	MaxQueueSize int             `yaml:"max_queue_size"` // 0 = unlimited
	MaxRetries   int             `yaml:"max_retries"`    // default 3
	DrainTimeout int             `yaml:"drain_timeout"`  // seconds, default 60
	TLS          TLSConfig       `yaml:"tls"`
	Auth         AuthConfig      `yaml:"auth"`
	Fairshare    FairshareConfig `yaml:"fairshare"`
	HA           HAConfig        `yaml:"ha"`

	// MasterAddrs lists every master, for clients and workers to fail over between when HA is
	// enabled. Empty falls back to the single MasterAddr.
	MasterAddrs []string     `yaml:"master_addrs"`
	Gossip      GossipConfig `yaml:"gossip"`

	// MetricsBind is the address the health/metrics server listens on. Defaults to all
	// interfaces for backward compatibility; set 127.0.0.1 to keep it off the network.
	MetricsBind string `yaml:"metrics_bind"`

	// MaxConcurrentJobs caps how many jobs a worker runs at once. 0 means unlimited, which is
	// what the worker used to do — every dispatch spawned a goroutine with no ceiling.
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`

	// MaxOutputBytes caps captured job stdout+stderr. Output was buffered without limit and
	// then rejected by gRPC's 4 MiB receive cap, leaving the reporting worker retrying forever.
	MaxOutputBytes int `yaml:"max_output_bytes"`

	// LogFormat is "text" (default) or "json". LogLevel is debug, info, warn, or error.
	LogFormat string `yaml:"log_format"`
	LogLevel  string `yaml:"log_level"`

	// MaxPIDsPerJob caps the processes a single job may create, which is the cheapest guard
	// against a fork bomb taking a worker down. 0 uses the built-in default.
	MaxPIDsPerJob int `yaml:"max_pids_per_job"`

	// ClientToken is the token this node presents when calling the master. Set it via
	// TASCH_AUTH_TOKEN or client_token; it is what the CLI and the worker authenticate with.
	ClientToken string `yaml:"client_token"`

	// Sandbox confines what a job can see and touch on the worker.
	Sandbox SandboxConfig `yaml:"sandbox"`

	// Partitions divide the cluster into named pools of nodes with their own limits and
	// priority. Accounts group users so quotas can be applied to a team rather than a person.
	// Both are empty by default, which is the previous behaviour: one undivided cluster with
	// no per-group ceiling.
	Partitions []PartitionConfig `yaml:"partitions"`
	Accounts   []AccountConfig   `yaml:"accounts"`

	// Preemption lets an urgent job take a busy node by evicting lower-priority work.
	Preemption PreemptionConfig `yaml:"preemption"`
}

// PreemptionConfig controls whether an urgent job may evict running work to get a node.
//
// Without it, priority only decides the order jobs *start* in. Once the cluster is full, a job
// submitted at the highest priority waits behind whatever bulk work happens to be running,
// which can be hours — so "urgent" means nothing precisely when it matters. Preemption makes
// priority mean something at a full cluster, at the cost of throwing away work in progress,
// which is why it is off by default and hedged with the guards below.
type PreemptionConfig struct {
	Enabled bool `yaml:"enabled"`

	// PriorityMargin is how much higher-priority the incoming job must be. Preempting across a
	// difference of one turns ordinary priority jitter into eviction churn; a margin makes
	// preemption a statement about class of work rather than a tie-break.
	PriorityMargin int `yaml:"priority_margin"`

	// MinRuntimeSeconds protects work that has only just started. Without it a cluster under
	// load can spend its time starting and killing the same jobs, making no progress at all.
	MinRuntimeSeconds int `yaml:"min_runtime_seconds"`

	// MaxVictimsPerJob bounds how much is thrown away to place one job. A job needing a whole
	// large node could otherwise evict everything on it at once.
	MaxVictimsPerJob int `yaml:"max_victims_per_job"`
}

// PartitionConfig is a named pool of nodes with its own admission rules.
//
// Without partitions every job competes for every node, so one team's long CPU batch can sit in
// front of another team's GPU work purely because it was submitted first. A partition scopes a
// job to the nodes it belongs on, and gives an operator somewhere to hang the limits that
// differ between those pools — walltime ceilings above all, which are what stop a single job
// occupying a scarce node indefinitely.
type PartitionConfig struct {
	Name string `yaml:"name"`

	// NodeSelector is a CEL expression over the node's class ad, the same language job
	// requirements use. Empty matches every node.
	NodeSelector string `yaml:"node_selector"`

	// Default marks the partition jobs land in when they name none. At most one may be default;
	// with none, a job that names no partition is unrestricted, as it was before partitions
	// existed.
	Default bool `yaml:"default"`

	// PriorityBoost is added to a job's priority on entry. Negative raises it, matching the
	// queue's convention that lower sorts first — so an interactive partition uses a negative
	// boost and a bulk one a positive.
	PriorityBoost int `yaml:"priority_boost"`

	// MaxRunningJobs caps concurrent jobs in this partition. 0 is unlimited.
	MaxRunningJobs int `yaml:"max_running_jobs"`

	// MaxWalltimeSeconds refuses jobs asking for longer, and DefaultWalltimeSeconds is applied
	// to jobs that ask for nothing. A partition with a ceiling but no default still admits jobs
	// that never end, which is usually not what the ceiling was for.
	MaxWalltimeSeconds     int `yaml:"max_walltime_seconds"`
	DefaultWalltimeSeconds int `yaml:"default_walltime_seconds"`

	// Preemptible allows jobs in this partition to be evicted for higher-priority work. It is a
	// property of the partition rather than of the job on purpose: given the choice, every
	// submitter would mark their own job unpreemptible, and the setting would mean nothing.
	Preemptible bool `yaml:"preemptible"`

	// AllowedUsers and AllowedAccounts restrict who may submit here. Empty means everyone.
	AllowedUsers    []string `yaml:"allowed_users"`
	AllowedAccounts []string `yaml:"allowed_accounts"`
}

// AccountConfig is a group of users that quotas apply to, optionally nested.
//
// Nesting is what makes a quota a budget rather than a per-user cap: a department can be given
// 32 GPUs and split them between its teams without any team being able to exceed the
// department's share, because a job counts against every account above it as well as its own.
type AccountConfig struct {
	Name string `yaml:"name"`

	// Parent nests this account inside another. Empty makes it a root.
	Parent string `yaml:"parent"`

	// Users belonging to this account. A user in several accounts submits to the first listed
	// unless the job names one of the others.
	Users []string `yaml:"users"`

	// Ceilings on what this account and everything under it may hold at once. 0 is unlimited.
	MaxRunningJobs int `yaml:"max_running_jobs"`
	MaxGPUs        int `yaml:"max_gpus"`
	MaxCPUs        int `yaml:"max_cpus"`
	MaxMemoryMB    int `yaml:"max_memory_mb"`

	// MaxQueuedJobs bounds the backlog an account may build up. It is checked at submit, so a
	// runaway script is refused at the door rather than after it has filled the queue for
	// everyone.
	MaxQueuedJobs int `yaml:"max_queued_jobs"`
}

// SandboxConfig controls job isolation on the worker.
//
// cgroups already cap how much CPU, memory and how many processes a job may use, but a job
// still ran as the service account with the account's whole filesystem, its process table and
// its IPC namespace in reach. One job could read another's scratch files, signal another's
// processes, or read the daemon's own token and TLS key. This closes that.
//
// Mode is the one setting that matters:
//
//   - "none"    — no namespaces. What Tasch did before this existed, and still the default so
//     an upgrade does not silently change what a running job can reach.
//   - "private" — mount, PID, IPC and UTS namespaces. The job gets its own /tmp, /dev/shm and
//     process table, and the home directory of the account is masked. The rest of the
//     filesystem stays visible and writable, so shared data paths keep working. This is the
//     cheapest setting that stops jobs interfering with each other.
//   - "strict"  — "private" plus a pivot_root into a rootfs assembled from read-only binds.
//     The job sees only ReadOnlyPaths (the system directories), the paths in WritablePaths,
//     and its own scratch directory. Nothing else on the host exists as far as it is
//     concerned. This is the setting that makes an untrusted job safe to run.
type SandboxConfig struct {
	Mode string `yaml:"mode"`

	// Network is "host" (default) or "none". "none" puts the job in an empty network
	// namespace with only loopback, which stops it reaching the cluster's own ports — but also
	// stops it fetching packages or datasets, and breaks multi-node distributed jobs.
	Network string `yaml:"network"`

	// ScratchDir is where per-job working directories are created on the host. Each job gets
	// its own, removed when the job ends.
	ScratchDir string `yaml:"scratch_dir"`

	// ReadOnlyPaths are host directories bind-mounted read-only into a strict sandbox. They are
	// what makes an interpreter and its libraries available. Missing paths are skipped.
	ReadOnlyPaths []string `yaml:"readonly_paths"`

	// WritablePaths are host directories bind-mounted read-write at the same path in both
	// private and strict mode — shared datasets, model caches, network storage.
	WritablePaths []string `yaml:"writable_paths"`

	// MaskedPaths are covered with an empty read-only tmpfs so their contents cannot be read.
	// In private mode this is how the service account's home and Tasch's own state directory
	// are kept away from jobs.
	MaskedPaths []string `yaml:"masked_paths"`

	// TmpfsSizeMB bounds the job's private /tmp and /dev/shm. Without a bound, tmpfs is charged
	// to the cgroup's memory limit, so a job filling /tmp is killed rather than filling the
	// host disk — but an explicit size gives a clearer error.
	TmpfsSizeMB int `yaml:"tmpfs_size_mb"`

	// Hostname the job sees. Jobs that log their hostname otherwise leak the node's name.
	Hostname string `yaml:"hostname"`

	// AllowNewPrivileges leaves setuid binaries and file capabilities working inside the
	// sandbox. Off by default: with no_new_privs set, a job cannot regain privilege through
	// sudo or a setuid helper even if one is reachable.
	AllowNewPrivileges bool `yaml:"allow_new_privileges"`

	// GPUDevices exposes /dev/nvidia*, /dev/dri and friends to jobs that requested a GPU.
	// Strict mode builds its own /dev, so without this a GPU job finds no device to open.
	// Defaults to true; there is no reason to schedule a GPU job that cannot use the GPU.
	GPUDevices *bool `yaml:"gpu_devices"`
}

// SandboxMode* are the values SandboxConfig.Mode accepts.
const (
	SandboxNone    = "none"
	SandboxPrivate = "private"
	SandboxStrict  = "strict"
)

// defaultReadOnlyPaths are the host directories a strict sandbox needs for a shell, an
// interpreter and its shared libraries to load. Anything absent is skipped, so the same list
// works on a merged-/usr distribution and an older split one.
var defaultReadOnlyPaths = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/libx32", "/etc", "/opt",
}

// WantsGPUDevices reports whether GPU device nodes should be exposed, defaulting to true.
func (s SandboxConfig) WantsGPUDevices() bool {
	return s.GPUDevices == nil || *s.GPUDevices
}

// ResolvedReadOnlyPaths returns the configured read-only paths, or the built-in list.
func (s SandboxConfig) ResolvedReadOnlyPaths() []string {
	if len(s.ReadOnlyPaths) > 0 {
		return s.ReadOnlyPaths
	}
	return defaultReadOnlyPaths
}

// ResolvedScratchDir returns where per-job working directories are created.
func (s SandboxConfig) ResolvedScratchDir() string {
	if s.ScratchDir != "" {
		return s.ScratchDir
	}
	if info, err := os.Stat("/var/lib/tasch"); err == nil && info.IsDir() {
		return "/var/lib/tasch/scratch"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "tasch-scratch")
	}
	return filepath.Join(home, ".tasch", "scratch")
}

// HAConfig configures running several masters with automatic failover.
//
// Off by default, and off means exactly the previous behaviour: one master, local state, no
// quorum requirement. Turning it on replicates scheduler state across masters so losing one is
// survivable — at the cost of needing a majority alive, which means an odd number of masters,
// three at minimum.
type HAConfig struct {
	Enabled bool `yaml:"enabled"`

	// NodeID identifies this master in the replicated cluster. It must be stable across
	// restarts: a master that comes back under a new ID leaves its old identity behind as a dead
	// voter, and enough of those cost the cluster its quorum.
	NodeID string `yaml:"node_id"`

	// BindAddr is where peers reach this master's replication transport.
	BindAddr string `yaml:"bind_addr"`

	// DataDir holds this master's replicated log and snapshots. It must not be shared.
	DataDir string `yaml:"data_dir"`

	// Peers lists every master as "id=host:port", including this one.
	Peers []string `yaml:"peers"`

	// Bootstrap forms the cluster from Peers on first start. It is ignored once this node has
	// state, so leaving it set is safe across restarts.
	Bootstrap bool `yaml:"bootstrap"`
}

// FairshareConfig tunes how past usage penalises a user's priority.
type FairshareConfig struct {
	// Enabled turns fairshare on. Off leaves every job at the priority it was submitted with.
	Enabled bool `yaml:"enabled"`

	// HalfLifeHours is how long it takes for recorded usage to decay by half. The old behaviour
	// was a hardcoded factor giving a half-life of roughly thirteen minutes, short enough that a
	// user could saturate the cluster all morning and carry no penalty by lunchtime.
	HalfLifeHours float64 `yaml:"half_life_hours"`

	// MaxPenalty bounds how far a heavy user's jobs can be pushed back.
	MaxPenalty int `yaml:"max_penalty"`

	// Billing weights. A GPU-second is charged far more than a CPU-second because accelerators
	// are the scarce resource; without weighting, a job holding 64 GPUs accrued exactly as much
	// usage as one running sleep.
	CPUSecondWeight float64 `yaml:"cpu_second_weight"`
	GPUSecondWeight float64 `yaml:"gpu_second_weight"`
	GBHourMemWeight float64 `yaml:"gb_hour_memory_weight"`
}

// AuthConfig holds gRPC authentication settings.
type AuthConfig struct {
	// Enabled turns on token authentication for every gRPC call. It defaults to false so
	// existing deployments keep working, but a cluster reachable by anything other than fully
	// trusted hosts must set it: without it, any client that can reach the gRPC port can submit
	// a job, which means running an arbitrary shell command on every worker.
	Enabled bool `yaml:"enabled"`

	// Principals are the identities allowed to call the API.
	Principals []Principal `yaml:"principals"`

	// PrincipalsFile optionally holds the principal list in a separate file, so tokens can be
	// kept out of a world-readable config.
	PrincipalsFile string `yaml:"principals_file"`
}

// Principal is one authenticated identity.
type Principal struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
	// Role is one of: user (submit and manage own jobs), admin (manage any job),
	// worker (report results only).
	Role string `yaml:"role"`
}

// GossipConfig holds memberlist settings.
type GossipConfig struct {
	// EncryptionKey is a base64-encoded 16, 24, or 32 byte key. When set, gossip traffic is
	// encrypted and authenticated, which is what stops an arbitrary host from joining the
	// cluster and advertising fabricated resources to attract every job.
	EncryptionKey string `yaml:"encryption_key"`

	// KeyFile optionally reads the key from a file instead.
	KeyFile string `yaml:"key_file"`

	// Join lists gossip seed addresses ("host:port") to contact on startup.
	//
	// With several masters this is what puts them in one membership view. Without it each forms
	// its own cluster, and whichever master holds leadership can only see the workers that
	// happened to join it — everything else stays queued forever.
	Join []string `yaml:"join"`

	// Profile selects memberlist timing: "lan" (default), "wan", or "local". The old hardcoded
	// "local" profile is tuned for loopback and produces false node-failure detections on any
	// real network — each of which fails every job on the node it wrongly declared dead.
	Profile string `yaml:"profile"`
}

// PortConfig holds all network port settings.
type PortConfig struct {
	Gossip int `yaml:"gossip"`
	GRPC   int `yaml:"grpc"`
	// ZMQ is unused. Job dispatch moved onto the authenticated gRPC connection, so nothing
	// listens on this port any more. The field is kept so existing config files still parse.
	ZMQ     int `yaml:"zmq"`
	Metrics int `yaml:"metrics"`
}

// TLSConfig holds optional TLS/mTLS settings.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Role:         "both",
		NodeName:     "",
		MasterAddr:   "127.0.0.1",
		MaxQueueSize: 10000,
		MaxRetries:   3,
		DrainTimeout: 60,
		MetricsBind:  "0.0.0.0",
		LogFormat:    "text",
		LogLevel:     "info",
		// 0 = unlimited, preserving prior behavior; operators should set this.
		MaxConcurrentJobs: 0,
		// 3 MiB, comfortably under gRPC's 4 MiB default receive limit.
		MaxOutputBytes: 3 << 20,
		Gossip:         GossipConfig{Profile: "lan"},
		Preemption: PreemptionConfig{
			// Off, because it throws away work in progress. The numbers are the defaults that
			// apply once it is switched on.
			Enabled:           false,
			PriorityMargin:    5,
			MinRuntimeSeconds: 60,
			MaxVictimsPerJob:  4,
		},
		Sandbox: SandboxConfig{
			// "none" keeps an upgrade from changing what running jobs can reach. SETUP.md
			// explains why a shared cluster should move to "private" or "strict".
			Mode:        SandboxNone,
			Network:     "host",
			TmpfsSizeMB: 512,
			Hostname:    "tasch-job",
		},
		Fairshare: FairshareConfig{
			Enabled:         true,
			HalfLifeHours:   24,
			MaxPenalty:      50,
			CPUSecondWeight: 1,
			GPUSecondWeight: 32,
			GBHourMemWeight: 0.25,
		},
		Ports: PortConfig{
			Gossip:  7946,
			GRPC:    50051,
			ZMQ:     5555,
			Metrics: 9090,
		},
	}
}

// DefaultPath returns the first existing path from:
// 1. /etc/tasch/config.yaml (system-wide)
// 2. ~/.tasch/config.yaml (user-local)
func DefaultPath() string {
	if _, err := os.Stat("/etc/tasch/config.yaml"); err == nil {
		return "/etc/tasch/config.yaml"
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".tasch", "config.yaml")
}

// StorePath returns the storage path, prioritizing /var/lib/tasch if it exists.
func StorePath() string {
	if info, err := os.Stat("/var/lib/tasch"); err == nil && info.IsDir() {
		return "/var/lib/tasch/tasch.db"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "tasch.db"
	}
	return filepath.Join(home, ".tasch", "tasch.db")
}

// PidPath returns the PID file path, prioritizing /run/tasch or /var/lib/tasch for system services.
func PidPath() string {
	if info, err := os.Stat("/var/lib/tasch"); err == nil && info.IsDir() {
		return "/var/lib/tasch/tasch.pid"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "tasch.pid"
	}
	return filepath.Join(home, ".tasch", "tasch.pid")
}

// GRPCAddr returns the master gRPC address from config.
func (c *Config) GRPCAddr() string {
	return fmt.Sprintf("%s:%d", c.MasterAddr, c.Ports.GRPC)
}

// GRPCAddrs returns every master address a client should try.
//
// With several masters only the leader accepts writes, and which one that is changes on
// failover, so clients need the full set rather than a single address.
func (c *Config) GRPCAddrs() []string {
	if len(c.MasterAddrs) == 0 {
		return []string{c.GRPCAddr()}
	}
	addrs := make([]string, 0, len(c.MasterAddrs))
	for _, host := range c.MasterAddrs {
		// An entry may already carry a port; otherwise apply the configured one.
		if strings.Contains(host, ":") {
			addrs = append(addrs, host)
			continue
		}
		addrs = append(addrs, fmt.Sprintf("%s:%d", host, c.Ports.GRPC))
	}
	return addrs
}

// LoadConfig reads a YAML config from disk.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config %s: %w", path, err)
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// SaveConfig writes the config to disk, creating directories as needed.
func SaveConfig(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	// 0600: the file names TLS key paths and can carry auth tokens.
	return os.WriteFile(path, data, 0600)
}

// Validate checks the config for combinations that would otherwise fail silently at runtime.
func (c *Config) Validate() error {
	switch c.Role {
	case "master", "worker", "both":
	default:
		// An unrecognised role started neither the master nor the worker: the process printed
		// "Tasch is running", wrote a PID file, and then did nothing at all.
		return fmt.Errorf("role must be master, worker, or both (got %q)", c.Role)
	}

	ports := map[string]int{
		"gossip":  c.Ports.Gossip,
		"grpc":    c.Ports.GRPC,
		"metrics": c.Ports.Metrics,
	}
	seen := make(map[int]string, len(ports))
	for name, port := range ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("ports.%s must be between 1 and 65535 (got %d)", name, port)
		}
		if other, dup := seen[port]; dup {
			return fmt.Errorf("ports.%s and ports.%s are both %d", other, name, port)
		}
		seen[port] = name
	}

	if c.TLS.Enabled {
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return fmt.Errorf("tls.enabled requires both tls.cert_file and tls.key_file")
		}
		for _, f := range []string{c.TLS.CertFile, c.TLS.KeyFile} {
			if _, err := os.Stat(f); err != nil {
				return fmt.Errorf("tls file %s is not readable: %w", f, err)
			}
		}
	}

	if c.MaxQueueSize < 0 {
		return fmt.Errorf("max_queue_size cannot be negative (got %d)", c.MaxQueueSize)
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("max_retries cannot be negative (got %d)", c.MaxRetries)
	}
	if c.DrainTimeout < 0 {
		return fmt.Errorf("drain_timeout cannot be negative (got %d)", c.DrainTimeout)
	}
	if c.MaxConcurrentJobs < 0 {
		return fmt.Errorf("max_concurrent_jobs cannot be negative (got %d)", c.MaxConcurrentJobs)
	}
	if c.MaxPIDsPerJob < 0 {
		return fmt.Errorf("max_pids_per_job cannot be negative (got %d)", c.MaxPIDsPerJob)
	}

	if c.Preemption.Enabled {
		for _, f := range []struct {
			name  string
			value int
		}{
			{"priority_margin", c.Preemption.PriorityMargin},
			{"min_runtime_seconds", c.Preemption.MinRuntimeSeconds},
			{"max_victims_per_job", c.Preemption.MaxVictimsPerJob},
		} {
			if f.value < 0 {
				return fmt.Errorf("preemption.%s cannot be negative (got %d)", f.name, f.value)
			}
		}
	}

	if err := c.validatePartitions(); err != nil {
		return err
	}
	if err := c.validateAccounts(); err != nil {
		return err
	}

	switch c.Sandbox.Mode {
	case "", SandboxNone, SandboxPrivate, SandboxStrict:
	default:
		return fmt.Errorf("sandbox.mode must be %q, %q or %q (got %q)",
			SandboxNone, SandboxPrivate, SandboxStrict, c.Sandbox.Mode)
	}
	switch c.Sandbox.Network {
	case "", "host", "none":
	default:
		return fmt.Errorf("sandbox.network must be \"host\" or \"none\" (got %q)", c.Sandbox.Network)
	}
	if c.Sandbox.TmpfsSizeMB < 0 {
		return fmt.Errorf("sandbox.tmpfs_size_mb cannot be negative (got %d)", c.Sandbox.TmpfsSizeMB)
	}
	for _, p := range append(append([]string{}, c.Sandbox.ReadOnlyPaths...),
		append(append([]string{}, c.Sandbox.WritablePaths...), c.Sandbox.MaskedPaths...)...) {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("sandbox paths must be absolute (got %q)", p)
		}
	}

	if c.Fairshare.Enabled {
		if c.Fairshare.HalfLifeHours <= 0 {
			return fmt.Errorf("fairshare.half_life_hours must be positive (got %v)", c.Fairshare.HalfLifeHours)
		}
		if c.Fairshare.MaxPenalty < 0 {
			return fmt.Errorf("fairshare.max_penalty cannot be negative (got %d)", c.Fairshare.MaxPenalty)
		}
		for name, w := range map[string]float64{
			"cpu_second_weight":     c.Fairshare.CPUSecondWeight,
			"gpu_second_weight":     c.Fairshare.GPUSecondWeight,
			"gb_hour_memory_weight": c.Fairshare.GBHourMemWeight,
		} {
			if w < 0 {
				return fmt.Errorf("fairshare.%s cannot be negative (got %v)", name, w)
			}
		}
	}

	if c.HA.Enabled {
		if c.HA.NodeID == "" {
			return fmt.Errorf("ha.node_id is required and must stay the same across restarts")
		}
		if c.HA.BindAddr == "" {
			return fmt.Errorf("ha.bind_addr is required so peers can reach this master")
		}
		if c.HA.DataDir == "" {
			return fmt.Errorf("ha.data_dir is required and must not be shared between masters")
		}
		// Raft commits nothing without a majority, so quorum is floor(N/2)+1 and the number of
		// failures survived is N-quorum. Two masters have a quorum of two: losing either leaves
		// the survivor holding a complete copy of the state but unable to elect a leader or
		// commit anything. That tolerates the same zero failures as a single master while
		// doubling the hardware that can cause an outage, and it is far harder to recover from
		// — a lone survivor needs its Raft configuration rewritten by hand.
		if len(c.HA.Peers) < 3 {
			return fmt.Errorf("ha.peers needs at least 3 masters: %d tolerates no failures at all "+
				"(quorum of %d), so it is worse than running a single master",
				len(c.HA.Peers), len(c.HA.Peers)/2+1)
		}
		// An even size shares the quorum of the odd size below it, so it adds a machine that can
		// fail without surviving any more failures.
		if len(c.HA.Peers)%2 == 0 {
			return fmt.Errorf("ha.peers should be an odd number of masters: %d tolerates %d "+
				"failure(s), the same as %d, while adding another machine that can fail",
				len(c.HA.Peers), len(c.HA.Peers)-(len(c.HA.Peers)/2+1), len(c.HA.Peers)-1)
		}
		seen := make(map[string]bool, len(c.HA.Peers))
		selfListed := false
		for _, peer := range c.HA.Peers {
			id, _, found := strings.Cut(peer, "=")
			if !found || id == "" {
				return fmt.Errorf("ha.peers entry %q must be in the form id=host:port", peer)
			}
			if seen[id] {
				return fmt.Errorf("duplicate master id %q in ha.peers", id)
			}
			seen[id] = true
			if id == c.HA.NodeID {
				selfListed = true
			}
		}
		if !selfListed {
			return fmt.Errorf("ha.peers must include this master (ha.node_id %q)", c.HA.NodeID)
		}
	}

	switch c.Gossip.Profile {
	case "", "lan", "wan", "local":
	default:
		return fmt.Errorf("gossip.profile must be lan, wan, or local (got %q)", c.Gossip.Profile)
	}

	if c.Auth.Enabled {
		principals, err := c.Principals()
		if err != nil {
			return err
		}
		if len(principals) == 0 {
			return fmt.Errorf("auth.enabled requires at least one principal")
		}
		names := make(map[string]bool, len(principals))
		tokens := make(map[string]bool, len(principals))
		for _, p := range principals {
			if p.Name == "" || p.Token == "" {
				return fmt.Errorf("every auth principal needs a name and a token")
			}
			switch p.Role {
			case "user", "admin", "worker":
			default:
				return fmt.Errorf("principal %q has role %q; must be user, admin, or worker", p.Name, p.Role)
			}
			if names[p.Name] {
				return fmt.Errorf("duplicate auth principal name %q", p.Name)
			}
			if tokens[p.Token] {
				return fmt.Errorf("two auth principals share a token")
			}
			names[p.Name] = true
			tokens[p.Token] = true
		}
	}

	return nil
}

// Principals returns the configured principals, reading auth.principals_file when set.
func (c *Config) Principals() ([]Principal, error) {
	if c.Auth.PrincipalsFile == "" {
		return c.Auth.Principals, nil
	}
	data, err := os.ReadFile(c.Auth.PrincipalsFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read auth.principals_file %s: %w", c.Auth.PrincipalsFile, err)
	}
	var file struct {
		Principals []Principal `yaml:"principals"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("invalid auth.principals_file %s: %w", c.Auth.PrincipalsFile, err)
	}
	return append(append([]Principal(nil), c.Auth.Principals...), file.Principals...), nil
}

// GossipKey returns the decoded gossip encryption key, or nil when gossip is unencrypted.
func (c *Config) GossipKey() ([]byte, error) {
	encoded := c.Gossip.EncryptionKey
	if encoded == "" && c.Gossip.KeyFile != "" {
		data, err := os.ReadFile(c.Gossip.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read gossip.key_file %s: %w", c.Gossip.KeyFile, err)
		}
		encoded = strings.TrimSpace(string(data))
	}
	if encoded == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("gossip encryption key is not valid base64: %w", err)
	}
	switch len(key) {
	case 16, 24, 32:
		return key, nil
	default:
		return nil, fmt.Errorf("gossip encryption key must decode to 16, 24, or 32 bytes (got %d)", len(key))
	}
}

// ApplyEnvOverrides lets TASCH_* env vars override config fields.
func (c *Config) ApplyEnvOverrides() error {
	if v := os.Getenv("TASCH_MASTER_ADDR"); v != "" {
		c.MasterAddr = v
	}
	// Report a malformed value rather than discarding it. A typo in TASCH_GRPC_PORT used to be
	// ignored silently, leaving the daemon listening somewhere the operator did not expect.
	overrides := map[string]*int{
		"TASCH_GOSSIP_PORT":  &c.Ports.Gossip,
		"TASCH_GRPC_PORT":    &c.Ports.GRPC,
		"TASCH_ZMQ_PORT":     &c.Ports.ZMQ,
		"TASCH_METRICS_PORT": &c.Ports.Metrics,
	}
	for env, target := range overrides {
		v := os.Getenv(env)
		if v == "" {
			continue
		}
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s=%q is not a number", env, v)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s=%d is out of range 1-65535", env, port)
		}
		*target = port
	}
	if v := os.Getenv("TASCH_AUTH_TOKEN"); v != "" {
		c.ClientToken = v
	}
	return nil
}

// validatePartitions checks partition names, defaults and walltime bounds.
func (c *Config) validatePartitions() error {
	seen := make(map[string]bool, len(c.Partitions))
	defaultName := ""
	for i, p := range c.Partitions {
		if p.Name == "" {
			return fmt.Errorf("partitions[%d] has no name", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("partition %q is defined twice", p.Name)
		}
		seen[p.Name] = true

		if p.Default {
			if defaultName != "" {
				return fmt.Errorf("partitions %q and %q are both marked default; only one can be",
					defaultName, p.Name)
			}
			defaultName = p.Name
		}
		if p.MaxRunningJobs < 0 {
			return fmt.Errorf("partition %q: max_running_jobs cannot be negative", p.Name)
		}
		if p.MaxWalltimeSeconds < 0 || p.DefaultWalltimeSeconds < 0 {
			return fmt.Errorf("partition %q: walltimes cannot be negative", p.Name)
		}
		if p.MaxWalltimeSeconds > 0 && p.DefaultWalltimeSeconds > p.MaxWalltimeSeconds {
			return fmt.Errorf("partition %q: default_walltime_seconds (%d) exceeds its own "+
				"max_walltime_seconds (%d), so every job that relies on the default is rejected",
				p.Name, p.DefaultWalltimeSeconds, p.MaxWalltimeSeconds)
		}
	}
	return nil
}

// validateAccounts checks that the account tree is a tree: unique names, existing parents, and
// no cycles. A cycle would make a quota rollup loop forever on the first job submitted.
func (c *Config) validateAccounts() error {
	byName := make(map[string]AccountConfig, len(c.Accounts))
	for i, a := range c.Accounts {
		if a.Name == "" {
			return fmt.Errorf("accounts[%d] has no name", i)
		}
		if _, dup := byName[a.Name]; dup {
			return fmt.Errorf("account %q is defined twice", a.Name)
		}
		for _, limit := range []struct {
			field string
			value int
		}{
			{"max_running_jobs", a.MaxRunningJobs}, {"max_gpus", a.MaxGPUs},
			{"max_cpus", a.MaxCPUs}, {"max_memory_mb", a.MaxMemoryMB},
			{"max_queued_jobs", a.MaxQueuedJobs},
		} {
			if limit.value < 0 {
				return fmt.Errorf("account %q: %s cannot be negative", a.Name, limit.field)
			}
		}
		byName[a.Name] = a
	}

	for _, a := range c.Accounts {
		if a.Parent == "" {
			continue
		}
		if _, ok := byName[a.Parent]; !ok {
			return fmt.Errorf("account %q names parent %q, which does not exist", a.Name, a.Parent)
		}
		// Walk to a root, refusing to take more steps than there are accounts.
		seen := map[string]bool{a.Name: true}
		for cur := a.Parent; cur != ""; {
			if seen[cur] {
				return fmt.Errorf("accounts form a cycle through %q; the hierarchy must be a tree", cur)
			}
			seen[cur] = true
			cur = byName[cur].Parent
		}
	}
	return nil
}
