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
	Gossip       GossipConfig    `yaml:"gossip"`

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
