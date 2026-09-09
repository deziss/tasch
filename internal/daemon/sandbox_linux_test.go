//go:build linux

package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/deziss/tasch/internal/config"
)

// The sandbox works by re-executing the tasch binary, which a test binary is not. These tests
// point the re-exec at a helper inside the test binary instead, so the real namespace setup —
// clone flags, mounts, pivot_root, the init supervisor — is exercised rather than mocked.
const sandboxHelperEnv = "TASCH_TEST_SANDBOX_HELPER"

// TestSandboxInitHelper is the re-exec target. It is not a test: `go test` runs it, finds the
// marker environment variable, and hands control to the real sandbox entry point.
func TestSandboxInitHelper(t *testing.T) {
	if os.Getenv(sandboxHelperEnv) != "1" {
		t.Skip("helper process, not a test")
	}
	// SandboxInit does not return: it becomes the job's supervisor and exits with its status.
	SandboxInit()
}

// useTestHelper redirects the sandbox re-exec into this test binary for the duration of a test.
func useTestHelper(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	prev := sandboxReexec
	sandboxReexec = func() (string, []string, error) {
		return self, []string{"sandbox-helper", "-test.run=TestSandboxInitHelper"}, nil
	}
	t.Cleanup(func() { sandboxReexec = prev })
}

// requireNamespaces skips when this kernel will not let the test create user namespaces, which
// is the case in some CI sandboxes and on hosts where an administrator disabled them.
func requireNamespaces(t *testing.T) {
	t.Helper()
	if err := sandboxSupportErr(); err != nil {
		t.Skipf("namespaces unavailable here: %v", err)
	}
	probe := exec.Command("/bin/true")
	probe.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWUTS | syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
	}
	if err := probe.Run(); err != nil {
		t.Skipf("cannot create namespaces here: %v", err)
	}
}

// asExitError unwraps the error from a finished command into an *exec.ExitError.
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

// runSandboxed executes a command under the given sandbox config and returns its combined
// output and the error from Run.
func runSandboxed(t *testing.T, cfg *config.Config, command string, env map[string]string) (string, error) {
	t.Helper()
	useTestHelper(t)

	sb, err := newSandbox(cfg, "test-"+strings.ToLower(t.Name()), env)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	jobEnv := append(os.Environ(), sandboxHelperEnv+"=1")
	for k, v := range env {
		jobEnv = append(jobEnv, k+"="+v)
	}

	cmd, err := prepareCommand(context.Background(), command, jobEnv, nil, sb)
	if err != nil {
		t.Fatalf("prepareCommand: %v", err)
	}
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

func sandboxCfg(t *testing.T, mode string) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Sandbox.Mode = mode
	cfg.Sandbox.ScratchDir = filepath.Join(t.TempDir(), "scratch")
	cfg.Sandbox.TmpfsSizeMB = 16
	return cfg
}

// A job in private mode must not be able to read the paths the worker keeps its state and
// credentials in — that is the point of masking them.
func TestPrivateSandboxMasksState(t *testing.T) {
	requireNamespaces(t)

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "token")
	if err := os.WriteFile(secret, []byte("super-secret-token"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := sandboxCfg(t, config.SandboxPrivate)
	cfg.Sandbox.MaskedPaths = []string{secretDir}

	out, err := runSandboxed(t, cfg, "cat "+secret+" 2>&1; echo exit=$?", nil)
	if err != nil {
		t.Fatalf("job failed to run: %v (output %q)", err, out)
	}
	if strings.Contains(out, "super-secret-token") {
		t.Fatalf("masked file was readable inside the sandbox: %q", out)
	}
	if !strings.Contains(out, "exit=1") {
		t.Fatalf("expected the read to fail, got %q", out)
	}
}

// The job's /tmp must be its own. Two jobs sharing /tmp can read each other's intermediate
// files, and a job can plant a file another will pick up.
func TestPrivateSandboxHasPrivateTmp(t *testing.T) {
	requireNamespaces(t)

	marker := "/tmp/tasch-sandbox-marker"
	t.Cleanup(func() { _ = os.Remove(marker) })

	cfg := sandboxCfg(t, config.SandboxPrivate)
	out, err := runSandboxed(t, cfg, "echo written > "+marker+" && ls /tmp | wc -l", nil)
	if err != nil {
		t.Fatalf("job failed: %v (output %q)", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a file the job wrote to /tmp appeared on the host's /tmp")
	}
}

// A PID namespace is what stops one job signalling another's processes. The clearest evidence
// is that the job's own shell is PID 1's child in a namespace holding almost nothing.
func TestPrivateSandboxHasOwnProcessTable(t *testing.T) {
	requireNamespaces(t)

	cfg := sandboxCfg(t, config.SandboxPrivate)
	out, err := runSandboxed(t, cfg, "ls /proc | grep -c '^[0-9]*$'", nil)
	if err != nil {
		t.Fatalf("job failed: %v (output %q)", err, out)
	}
	// init, the job's shell, and the ls/grep pipeline. A host process table has hundreds.
	if got := strings.TrimSpace(out); got == "" || len(got) > 2 {
		t.Fatalf("expected a nearly empty process table, saw %q entries", got)
	}
}

// Strict mode's promise is that nothing outside the configured binds exists. A file in the
// worker's own temp directory is exactly the kind of thing that must be gone.
func TestStrictSandboxHidesHostFilesystem(t *testing.T) {
	requireNamespaces(t)

	hostDir := t.TempDir()
	hostFile := filepath.Join(hostDir, "data.txt")
	if err := os.WriteFile(hostFile, []byte("host data"), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := sandboxCfg(t, config.SandboxStrict)
	out, err := runSandboxed(t, cfg, "cat "+hostFile+" 2>&1; echo exit=$?", nil)
	if err != nil {
		t.Fatalf("job failed to run: %v (output %q)", err, out)
	}
	if strings.Contains(out, "host data") {
		t.Fatalf("host file was readable inside a strict sandbox: %q", out)
	}
	if !strings.Contains(out, "exit=1") {
		t.Fatalf("expected the read to fail, got %q", out)
	}
}

// The system directories must still be there, or nothing runs.
func TestStrictSandboxKeepsSystemPathsReadOnly(t *testing.T) {
	requireNamespaces(t)

	cfg := sandboxCfg(t, config.SandboxStrict)
	out, err := runSandboxed(t, cfg,
		"test -x /bin/sh && echo shell-ok; touch /usr/tasch-probe 2>&1 || echo usr-readonly", nil)
	if err != nil {
		t.Fatalf("job failed: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "shell-ok") {
		t.Fatalf("no usable shell inside the sandbox: %q", out)
	}
	if !strings.Contains(out, "usr-readonly") {
		t.Fatalf("/usr was writable inside a strict sandbox: %q", out)
	}
}

// A strict job gets one writable place, and what it writes there is on the host — that is where
// results land.
func TestStrictSandboxWorkdirIsWritableAndMapped(t *testing.T) {
	requireNamespaces(t)

	cfg := sandboxCfg(t, config.SandboxStrict)
	useTestHelper(t)

	sb, err := newSandbox(cfg, "workdir-test", nil)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer func() { _ = sb.Close() }()

	if sb.Workdir() != sandboxWorkdir {
		t.Fatalf("strict workdir = %q, want %q", sb.Workdir(), sandboxWorkdir)
	}

	env := append(os.Environ(), sandboxHelperEnv+"=1")
	cmd, err := prepareCommand(context.Background(), "pwd; echo produced > result.txt", env, nil, sb)
	if err != nil {
		t.Fatalf("prepareCommand: %v", err)
	}
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("job failed: %v (output %q)", runErr, out)
	}
	if !strings.Contains(string(out), sandboxWorkdir) {
		t.Fatalf("job did not start in %s: %q", sandboxWorkdir, out)
	}

	onHost := filepath.Join(sb.HostScratch(), "result.txt")
	data, err := os.ReadFile(onHost)
	if err != nil {
		t.Fatalf("what the job wrote did not reach the host scratch: %v", err)
	}
	if strings.TrimSpace(string(data)) != "produced" {
		t.Fatalf("host scratch holds %q", data)
	}
}

// The worker reads a job's exit status to decide success, retry and dead-lettering. Running
// through an init supervisor must not change it.
func TestSandboxPropagatesExitCode(t *testing.T) {
	requireNamespaces(t)

	cfg := sandboxCfg(t, config.SandboxPrivate)
	_, err := runSandboxed(t, cfg, "exit 42", nil)
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("expected an exit error, got %v", err)
	}
	if exitErr.ExitCode() != 42 {
		t.Fatalf("exit code = %d, want 42", exitErr.ExitCode())
	}
}

// Cancelling a job must still kill it. Inside a PID namespace this is not automatic: PID 1
// ignores signals it has no handler for, so the init supervisor has to forward them.
func TestSandboxCancelKillsJob(t *testing.T) {
	requireNamespaces(t)

	cfg := sandboxCfg(t, config.SandboxPrivate)
	useTestHelper(t)

	sb, err := newSandbox(cfg, "cancel-test", nil)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer func() { _ = sb.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	env := append(os.Environ(), sandboxHelperEnv+"=1")
	cmd, err := prepareCommand(ctx, "sleep 300", env, nil, sb)
	if err != nil {
		t.Fatalf("prepareCommand: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	cancel()

	select {
	case <-done:
	case <-time.After(processGroupGrace + 10*time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("a cancelled job inside a sandbox did not die")
	}
}

// Isolation must never fail open. If it is configured and cannot be provided, the job has to be
// rejected rather than run with the account's whole filesystem in reach.
func TestSandboxOffReturnsNoSandbox(t *testing.T) {
	cfg := sandboxCfg(t, config.SandboxNone)
	sb, err := newSandbox(cfg, "off", nil)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	if sb != nil {
		t.Fatal("sandbox.mode none must not create a sandbox")
	}
}

// A job that can write cgroupfs can raise its own memory.max and undo every limit the
// scheduler set for it, so the sandbox has to keep it out of reach in both modes.
func TestSandboxHidesCgroupfs(t *testing.T) {
	requireNamespaces(t)

	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("no cgroup v2 on this host")
	}

	for _, mode := range []string{config.SandboxPrivate, config.SandboxStrict} {
		t.Run(mode, func(t *testing.T) {
			cfg := sandboxCfg(t, mode)
			out, err := runSandboxed(t, cfg, "ls /sys/fs/cgroup | wc -l", nil)
			if err != nil {
				t.Fatalf("job failed: %v (output %q)", err, out)
			}
			if got := strings.TrimSpace(lastLine(out)); got != "0" {
				t.Fatalf("cgroupfs was visible inside the sandbox (%s entries); full output %q", got, out)
			}
		})
	}
}

// The two confinements have to compose: a job must start already inside both its cgroup and
// its namespaces, from the same clone. Applying one at a time would leave a window.
func TestSandboxWorksWithCgroup(t *testing.T) {
	requireNamespaces(t)
	if !cgroupsSupported() {
		t.Skipf("cgroups unavailable here: %s", cgroupUnavailableReason())
	}
	useTestHelper(t)

	cg, err := newJobCgroup("sandbox-cgroup-test", 1, 64, 64)
	if err != nil {
		t.Skipf("cannot create a job cgroup here: %v", err)
	}
	defer func() { _ = cg.Close() }()

	cfg := sandboxCfg(t, config.SandboxPrivate)
	sb, err := newSandbox(cfg, "sandbox-cgroup-test", nil)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer func() { _ = sb.Close() }()

	env := append(os.Environ(), sandboxHelperEnv+"=1")
	cmd, err := prepareCommand(context.Background(), "echo confined", env, cg, sb)
	if err != nil {
		t.Fatalf("prepareCommand: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a job confined by both a cgroup and namespaces failed: %v (output %q)", err, out)
	}
	if !strings.Contains(string(out), "confined") {
		t.Fatalf("job output %q", out)
	}
}

// GPU device nodes are exposed only when the master pinned devices to the job, which it signals
// through the environment. A CPU job must not find the accelerators.
func TestStrictSandboxExposesGPUsOnlyWhenPinned(t *testing.T) {
	requireNamespaces(t)

	cfg := sandboxCfg(t, config.SandboxStrict)
	useTestHelper(t)

	plain, err := newSandbox(cfg, "gpu-none", nil)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer func() { _ = plain.Close() }()
	if plain.spec.GPUDevices {
		t.Fatal("a job with no device pinning asked for GPU device nodes")
	}

	pinned, err := newSandbox(cfg, "gpu-pinned", map[string]string{"CUDA_VISIBLE_DEVICES": "1"})
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer func() { _ = pinned.Close() }()
	if !pinned.spec.GPUDevices {
		t.Fatal("a job pinned to a GPU was not given the device nodes to use it")
	}
}

// lastLine returns the final non-empty line of output, so a warning printed by the helper does
// not shift what a test is reading.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}
