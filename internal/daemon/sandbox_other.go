//go:build !linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/deziss/tasch/internal/config"
)

// Job isolation is built on Linux namespaces. On other platforms there is no equivalent Tasch
// can rely on, so a worker configured for isolation refuses jobs rather than running them
// unconfined while reporting that they were confined.

type sandbox struct{}

func newSandbox(cfg *config.Config, jobID string, env map[string]string) (*sandbox, error) {
	if cfg.Sandbox.Mode == "" || cfg.Sandbox.Mode == config.SandboxNone {
		return nil, nil
	}
	return nil, fmt.Errorf("sandbox.mode is %q but job isolation requires Linux namespaces",
		cfg.Sandbox.Mode)
}

func (s *sandbox) applyTo(cmd *exec.Cmd, command string) error { return nil }

func (s *sandbox) Workdir() string { return "" }

func (s *sandbox) HostScratch() string { return "" }

func (s *sandbox) Close() error { return nil }

// SandboxInit exists so the CLI compiles everywhere; nothing can invoke it usefully here.
func SandboxInit() {
	fmt.Fprintln(os.Stderr, "tasch sandbox: job isolation requires Linux namespaces")
	os.Exit(126)
}

func sandboxSupportErrForLog() error {
	return fmt.Errorf("job isolation requires Linux namespaces")
}
