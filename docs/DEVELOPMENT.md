# Development Guide

## Build

```bash
make build       # → bin/tasch (native platform)
make deb         # → build .deb package
make rpm         # → build .rpm package
make package     # → build both
make clean       # remove binary and dist/
make proto       # regenerate gRPC stubs
make test        # unit tests (13 tests)
make run-test    # build + integration test (13 scenarios)
```

### Cross-Platform Build

```bash
chmod +x build.sh && ./build.sh
```

Outputs to `dist/`:

| Binary | OS | Arch |
|--------|----|------|
| `tasch-linux-amd64` | Linux | x86-64 |
| `tasch-linux-arm64` | Linux | ARM64 |
| `tasch-windows-amd64.exe` | Windows | x86-64 |
| `tasch-windows-arm64.exe` | Windows | ARM64 |
| `tasch-darwin-amd64` | macOS | Intel |
| `tasch-darwin-arm64` | macOS | Apple Silicon |

Builds use `-ldflags="-s -w"` to strip debug symbols. 32-bit targets are not supported.

## Project Structure

```
cmd/tasch/main.go              # Single binary, Cobra root, drain logic
internal/
  config/config.go             # Config YAML + TLS + env overrides
  setup/setup.go               # Interactive wizard
  store/store.go               # BoltDB persistence (jobs, groups, fairshare, dead letters)
  daemon/
    master.go                  # Scheduler, gRPC handlers, circuit breaker, multi-resource tracker,
                               # retry, async persistence, dispatch handshake, walltime enforcer
    worker.go                  # Executor, ZMQ auto-reconnect, gRPC keepalive, start acknowledgement
    exec_unix.go               # Unix sh -c command builder  (//go:build !windows)
    exec_windows.go            # Windows cmd.exe command builder (//go:build windows)
    metrics.go                 # 10 Prometheus metrics
    stop.go                    # PID stop + SIGKILL fallback
  cli/
    jobs.go                    # tasch jobs * subcommands + dead letter queue
    nodes.go                   # tasch nodes
    connect.go                 # gRPC client from config
pkg/
  profiler/
    profiler.go                # Shared Host struct + top-level profiling
    profiler_linux.go          # NVIDIA + AMD + Jetson (//go:build linux)
    profiler_windows.go        # WMI/PowerShell + NVIDIA fallback (//go:build windows)
    profiler_darwin.go         # Apple Metal + Unified Memory (//go:build darwin)
    profiler_fallback.go       # Empty stub for other OS (//go:build !linux && !windows && !darwin)
  scheduler/queue.go           # Min-heap, Job/JobGroup, OnJobChange/OnGroupChange hooks, fairshare
  matchmaker/evaluator.go      # CEL evaluation
  discovery/discovery.go       # Memberlist + EventHooks (OnJoin, OnLeave)
  messaging/                   # ZMQ PUB/SUB + DispatchPayload
```

## Tests

**Unit tests** (13): `make test`
- `pkg/matchmaker`: 5 tests (CEL expressions)
- `pkg/scheduler`: 8 tests (queue, FIFO, state, cancel, backfill, fairshare, queue limit, persistence hooks)

**Integration** (13 scenarios): `make run-test`

| # | Scenario |
|---|----------|
| 1 | Cluster topology — both nodes visible |
| 2 | Basic job submit + COMPLETED assertion |
| 3 | Priority + walltime job |
| 4 | List all jobs |
| 5 | Job detail (`jobs status`) |
| 6 | Submit + Cancel → CANCELLED assertion |
| 7 | Stream logs from completed job |
| 8 | Distributed training (`jobs train`) + group listing |
| 9 | Env var injection + output verification |
| 10 | `tasch nodes` from worker config |
| 11 | `tasch jobs` from worker config |
| 12 | Multi-resource constraint (CPU + Memory) → COMPLETED |
| 13 | Over-subscription → stays QUEUED → cancel |

## Code Patterns

### Adding a new GPU vendor

1. Detect in the appropriate platform profiler file (`profiler_linux.go`, `profiler_windows.go`, `profiler_darwin.go`)
2. Set `gpu_vendor` string in the `Host` struct (e.g. `"intel"`)
3. Add env var injection branch in `dispatchJob()` in `master.go`
4. Add CEL example to `docs/USER_GUIDE.md`

### Adding a new ClassAd field

1. Add detection to `pkg/profiler/profiler.go` (or the appropriate OS file)
2. Add field to the JSON ClassAd map in `profiler.go`
3. Add to the CEL variable table in `docs/USER_GUIDE.md` and `README.md`

### Adding persistence for a new data type

1. Add bucket + methods to `internal/store/store.go`
2. Wire hook in `StartMaster()` in `internal/daemon/master.go`

### Adding a Prometheus metric

1. Define in `internal/daemon/metrics.go`
2. Register in `initMetrics()`
3. Increment/observe in master.go handlers

### Adding a circuit breaker rule

Modify the `shouldCountAsFailure()` function in `master.go`. Currently excludes: cancelled jobs, walltime kills, and duplicate poison-pill job IDs.

### Adding a new platform build target

1. Update `build.sh` with the new `GOOS`/`GOARCH` combination
2. Add a platform profiler file if needed with the appropriate `//go:build` tag
3. Add `exec_<platform>.go` if command execution differs

## Dependencies

| Package | Purpose |
|---------|---------|
| `go.etcd.io/bbolt` | BoltDB embedded database |
| `prometheus/client_golang` | Metrics |
| `hashicorp/memberlist` | SWIM gossip |
| `go-zeromq/zmq4` | Pure-Go ZMQ |
| `google/cel-go` | CEL expressions |
| `shirou/gopsutil/v3` | CPU/memory profiling |
| `spf13/cobra` | CLI framework |
| `gopkg.in/yaml.v3` | Config file |

All pure Go — no CGo. Produces static binaries suitable for containerization and bare-metal deployment.
