package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateRejectsUnknownRole is the regression test for the silent no-op start: an
// unrecognised role matched neither the master nor the worker branch, so the process printed
// "Tasch is running", wrote a PID file, and then did nothing at all.
func TestValidateRejectsUnknownRole(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Role = "Master" // capitalised; neither branch matched

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an unrecognised role")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error = %q, want it to name the role field", err)
	}

	for _, role := range []string{"master", "worker", "both"} {
		cfg.Role = role
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected valid role %q: %v", role, err)
		}
	}
}

func TestValidateRejectsBadPorts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero port", func(c *Config) { c.Ports.GRPC = 0 }},
		{"out of range", func(c *Config) { c.Ports.GRPC = 99999 }},
		{"negative", func(c *Config) { c.Ports.Gossip = -1 }},
		{"collision", func(c *Config) { c.Ports.GRPC = c.Ports.Metrics }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate accepted an invalid port configuration")
			}
		})
	}
}

// TestValidateRequiresTLSFiles confirms tls.enabled without a cert is caught at startup rather
// than silently falling back to a plaintext listener while the banner announced TLS.
func TestValidateRequiresTLSFiles(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TLS.Enabled = true

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted tls.enabled with no cert or key")
	}

	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	os.WriteFile(cert, []byte("x"), 0600)
	os.WriteFile(key, []byte("x"), 0600)

	cfg.TLS.CertFile = cert
	cfg.TLS.KeyFile = key
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected a complete TLS config: %v", err)
	}

	cfg.TLS.CertFile = filepath.Join(dir, "missing.pem")
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted a TLS cert path that does not exist")
	}
}

func TestValidateAuthPrincipals(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig()
		cfg.Auth.Enabled = true
		return cfg
	}

	t.Run("no principals", func(t *testing.T) {
		if err := base().Validate(); err == nil {
			t.Fatal("auth.enabled with no principals was accepted")
		}
	})

	t.Run("bad role", func(t *testing.T) {
		cfg := base()
		cfg.Auth.Principals = []Principal{{Name: "a", Token: "t", Role: "superuser"}}
		if err := cfg.Validate(); err == nil {
			t.Fatal("an unknown principal role was accepted")
		}
	})

	t.Run("shared token", func(t *testing.T) {
		cfg := base()
		cfg.Auth.Principals = []Principal{
			{Name: "a", Token: "same", Role: "user"},
			{Name: "b", Token: "same", Role: "user"},
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("two principals sharing a token was accepted, making them indistinguishable")
		}
	})

	t.Run("duplicate name", func(t *testing.T) {
		cfg := base()
		cfg.Auth.Principals = []Principal{
			{Name: "a", Token: "t1", Role: "user"},
			{Name: "a", Token: "t2", Role: "user"},
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("a duplicate principal name was accepted")
		}
	})

	t.Run("valid", func(t *testing.T) {
		cfg := base()
		cfg.Auth.Principals = []Principal{
			{Name: "alice", Token: "t1", Role: "user"},
			{Name: "root", Token: "t2", Role: "admin"},
			{Name: "node1", Token: "t3", Role: "worker"},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a valid auth config was rejected: %v", err)
		}
	})
}

func TestGossipKey(t *testing.T) {
	cfg := DefaultConfig()

	key, err := cfg.GossipKey()
	if err != nil || key != nil {
		t.Fatalf("GossipKey = %v, %v; want nil, nil when unconfigured", key, err)
	}

	cfg.Gossip.EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
	key, err = cfg.GossipKey()
	if err != nil {
		t.Fatalf("GossipKey: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key length = %d, want 32", len(key))
	}

	cfg.Gossip.EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 20))
	if _, err := cfg.GossipKey(); err == nil {
		t.Error("a 20-byte key was accepted; AES requires 16, 24, or 32")
	}

	cfg.Gossip.EncryptionKey = "not base64!!"
	if _, err := cfg.GossipKey(); err == nil {
		t.Error("a non-base64 key was accepted")
	}
}

// TestApplyEnvOverridesReportsBadValues is the regression test for silently discarded errors:
// fmt.Sscanf's return was ignored, so TASCH_GRPC_PORT=abc left the port at its previous value
// and the daemon listened somewhere the operator did not expect.
func TestApplyEnvOverridesReportsBadValues(t *testing.T) {
	t.Setenv("TASCH_GRPC_PORT", "notanumber")
	cfg := DefaultConfig()
	if err := cfg.ApplyEnvOverrides(); err == nil {
		t.Fatal("a non-numeric port override was accepted silently")
	}

	t.Setenv("TASCH_GRPC_PORT", "99999")
	if err := cfg.ApplyEnvOverrides(); err == nil {
		t.Fatal("an out-of-range port override was accepted")
	}

	t.Setenv("TASCH_GRPC_PORT", "51000")
	if err := cfg.ApplyEnvOverrides(); err != nil {
		t.Fatalf("a valid override was rejected: %v", err)
	}
	if cfg.Ports.GRPC != 51000 {
		t.Errorf("ports.grpc = %d, want 51000", cfg.Ports.GRPC)
	}
}

func TestSaveConfigIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveConfig(path, DefaultConfig()); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// The config names TLS key paths and can carry auth tokens.
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("config mode = %04o, want no group or world access", perm)
	}
}

func TestPrincipalsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "principals.yaml")
	os.WriteFile(path, []byte("principals:\n  - name: alice\n    token: tok\n    role: user\n"), 0600)

	cfg := DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.PrincipalsFile = path

	got, err := cfg.Principals()
	if err != nil {
		t.Fatalf("Principals: %v", err)
	}
	if len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("Principals = %+v, want one entry for alice", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected a file-based principal list: %v", err)
	}
}

// TestHAQuorumSizing pins the cluster-size rule, which is a correctness constraint rather than
// a style preference: Raft commits nothing without a majority, so quorum is floor(N/2)+1 and the
// failures survived are N-quorum.
func TestHAQuorumSizing(t *testing.T) {
	peers := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("master-%d=10.0.0.%d:8300", i, i+1)
		}
		return out
	}

	base := func(n int) *Config {
		cfg := DefaultConfig()
		cfg.HA = HAConfig{
			Enabled: true, NodeID: "master-0",
			BindAddr: "10.0.0.1:8300", DataDir: "/var/lib/tasch/raft",
			Peers: peers(n),
		}
		return cfg
	}

	cases := []struct {
		size    int
		wantErr bool
		why     string
	}{
		// One master is a valid deployment, but not as an HA cluster: there is nothing to fail
		// over to, so it must be configured with ha.enabled off.
		{size: 1, wantErr: true, why: "a single peer is not a cluster"},
		// Two is the important case. Quorum is 2, so losing either master stops everything —
		// the same zero failures as one master, with twice the hardware that can fail.
		{size: 2, wantErr: true, why: "two masters tolerate no failures and are worse than one"},
		{size: 3, wantErr: false, why: "three tolerate one failure"},
		// Four shares three's quorum of 3, so it survives no more failures while adding a
		// machine that can fail.
		{size: 4, wantErr: true, why: "even sizes are strictly worse than the odd size below"},
		{size: 5, wantErr: false, why: "five tolerate two failures"},
		{size: 6, wantErr: true, why: "even sizes are strictly worse than the odd size below"},
		{size: 7, wantErr: false, why: "seven tolerate three failures"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dmasters", tc.size), func(t *testing.T) {
			err := base(tc.size).Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("%d masters was accepted: %s", tc.size, tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%d masters was rejected (%s): %v", tc.size, tc.why, err)
			}
		})
	}
}

// TestHAErrorsExplainWhy confirms the rejection tells an operator what is wrong rather than just
// that something is. Someone hitting this is mid-deployment and needs the reasoning, not a rule.
func TestHAErrorsExplainWhy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HA = HAConfig{
		Enabled: true, NodeID: "master-0", BindAddr: "10.0.0.1:8300",
		DataDir: "/var/lib/tasch/raft",
		Peers:   []string{"master-0=10.0.0.1:8300", "master-1=10.0.0.2:8300"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("two masters were accepted")
	}
	for _, want := range []string{"tolerates no failures", "single master"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestHARequiresSelfInPeers confirms a master that is not in its own peer list is rejected: it
// would start, replicate nothing, and never be eligible for leadership.
func TestHARequiresSelfInPeers(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HA = HAConfig{
		Enabled: true, NodeID: "master-9", BindAddr: "10.0.0.9:8300",
		DataDir: "/var/lib/tasch/raft",
		Peers: []string{
			"master-0=10.0.0.1:8300", "master-1=10.0.0.2:8300", "master-2=10.0.0.3:8300",
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a master missing from its own peer list was accepted")
	}
}
