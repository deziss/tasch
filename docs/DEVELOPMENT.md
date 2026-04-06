# Development Guide

## Build

```bash
make build     # → bin/tasch
make clean     # remove binary
make proto     # regenerate gRPC stubs
make test      # unit tests (13 tests)
make run-test  # build + integration test (11 scenarios)
```

## Project Structure

```
cmd/tasch/main.go              # Single binary, Cobra root, drain logic
internal/
  config/config.go             # Config YAML + TLS + queue limits + retry + drain timeout
  setup/setup.go               # Interactive wizard
  store/store.go               # BoltDB persistence (jobs, groups, fairshare, dead letters)
  daemon/
    master.go                  # Scheduler, gRPC handlers, circuit breaker, GPU tracker, retry, persistence
    worker.go                  # Executor, ZMQ auto-reconnect, gRPC keepalive, report retry
    metrics.go                 # 9 Prometheus metrics
    stop.go                    # PID stop + SIGKILL fallback
  cli/
    jobs.go                    # tasch jobs * subcommands + dead letter queue
    nodes.go                   # tasch nodes
    connect.go                 # gRPC client from config
pkg/
  profiler/profiler.go         # NVIDIA + AMD GPU detection
  scheduler/queue.go           # Min-heap, Job/JobGroup, OnJobChange/OnGroupChange hooks, fairshare
  matchmaker/evaluator.go      # CEL evaluation
  discovery/discovery.go       # Memberlist + EventHooks (OnJoin, OnLeave)
  messaging/                   # ZMQ PUB/SUB + DispatchPayload
```

## Tests

**Unit tests** (13): `make test`
- `pkg/matchmaker`: 5 tests (CEL)
- `pkg/scheduler`: 8 tests (queue, FIFO, state, cancel, backfill, fairshare, queue limit, persistence hooks)

**Integration** (11 scenarios): `make run-test`

## Code Patterns

### Adding persistence for a new data type
1. Add bucket + methods to `internal/store/store.go`
2. Wire hook in `StartMaster()` in `internal/daemon/master.go`

### Adding a Prometheus metric
1. Define in `internal/daemon/metrics.go`
2. Register in `initMetrics()`
3. Increment/observe in master.go handlers

### Adding a circuit breaker rule
Modify `circuitBreaker` struct in `master.go`. Currently: 3 failures = 5 min block.

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

All pure Go — no CGo. Static binary.
