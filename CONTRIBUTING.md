# Contributing to Tasch

## Setup

PREREQUISITES: Go 1.24+, `nfpm` (for packaging), `protoc` (for gRPC).

```bash
git clone https://github.com/deziss/tasch.git
cd tasch
make build && make test && make run-test
```

## Where to Add Things

| What | Where |
|------|-------|
| CLI command | `internal/cli/` |
| gRPC RPC | `api/v1/scheduler.proto` → `make proto` → `internal/daemon/master.go` |
| ClassAd field | `pkg/profiler/profiler.go` (shared) + OS-specific profiler file |
| GPU vendor (Linux) | `pkg/profiler/profiler_linux.go` → `detectNvidiaGPUs()` / `detectAMDGPUs()` pattern |
| GPU vendor (Windows) | `pkg/profiler/profiler_windows.go` → WMI query or binary probe |
| GPU vendor (macOS) | `pkg/profiler/profiler_darwin.go` → `system_profiler` or `sysctl` |
| GPU env binding | `internal/daemon/master.go` → `dispatchJob()` vendor switch |
| Worker command builder | `internal/daemon/exec_unix.go` or `exec_windows.go` |
| Scheduler policy | `internal/daemon/master.go` → `dispatchLoop()` |
| Persistence bucket | `internal/store/store.go` |
| Prometheus metric | `internal/daemon/metrics.go` |
| Config field | `internal/config/config.go` + `internal/setup/setup.go` |
| Platform build target | `build.sh` + new `profiler_<os>.go` if needed |
| Packaging | `nfpm.yaml` + `packaging/` |

## Testing

- Unit: `make test` (13 tests)
- Integration: `make run-test` (13 scenarios)
- All tests must pass before merging

## Cross-Platform Notes

- Use Go build tags (`//go:build linux`, `//go:build windows`, `//go:build darwin`) for OS-specific code
- 32-bit architectures (386, arm) are intentionally not supported — do not add them
- The worker's `prepareCommand()` function in `exec_unix.go` / `exec_windows.go` must remain the single entry point for subprocess creation — do not call `exec.Command` directly in `worker.go`
- Test on at least Linux/amd64 before submitting cross-platform changes

## Commit Style

```
feat: add job priority boost for GPU jobs
fix: circuit breaker not resetting on success
docs: update API reference with retry fields
build: add FreeBSD arm64 build target
```
