package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds the Tasch node configuration written by `tasch setup`.
type Config struct {
	Role       string     `yaml:"role"`        // master, worker, both
	NodeName   string     `yaml:"node_name"`
	MasterAddr string     `yaml:"master_addr"` // IP/hostname of master node
	Ports      PortConfig `yaml:"ports"`
}

// PortConfig holds all network port settings.
type PortConfig struct {
	Gossip int `yaml:"gossip"`
	GRPC   int `yaml:"grpc"`
	ZMQ    int `yaml:"zmq"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Role:       "both",
		NodeName:   "",
		MasterAddr: "127.0.0.1",
		Ports: PortConfig{
			Gossip: 7946,
			GRPC:   50051,
			ZMQ:    5555,
		},
	}
}

// DefaultPath returns ~/.tasch/config.yaml
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".tasch", "config.yaml")
}

// PidPath returns ~/.tasch/tasch.pid
func PidPath() string {
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
	return os.WriteFile(path, data, 0644)
}

// ApplyEnvOverrides lets TASCH_* env vars override config fields.
func (c *Config) ApplyEnvOverrides() {
	if v := os.Getenv("TASCH_MASTER_ADDR"); v != "" {
		c.MasterAddr = v
	}
	if v := os.Getenv("TASCH_GOSSIP_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &c.Ports.Gossip)
	}
	if v := os.Getenv("TASCH_GRPC_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &c.Ports.GRPC)
	}
	if v := os.Getenv("TASCH_ZMQ_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &c.Ports.ZMQ)
	}
}
