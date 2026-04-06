# Development Guide

## Prerequisites

- Go 1.21+ (recommended 1.24+)
- Make
- protoc + protoc-gen-go + protoc-gen-go-grpc (only if modifying API)

## Build

```bash
make build     # → bin/tasch
make clean     # remove binary
make proto     # regenerate gRPC stubs
make test      # unit tests
make run-test  # build + integration test (11 scenarios)
```

## Project Structure

```
cmd/tasch/main.go              # Single binary entry point
internal/
  config/config.go             # Config YAML load/save
  setup/setup.go               # Interactive setup wizard
  daemon/master.go             # Master: handlers + scheduling + gang-sched
  daemon/worker.go             # Worker: execution + GPU env injection
  daemon/stop.go               # PID-based stop
  cli/jobs.go                  # tasch jobs * subcommands
  cli/nodes.go                 # tasch nodes
  cli/connect.go               # gRPC client from config
pkg/
  profiler/profiler.go         # NVIDIA + AMD GPU detection
  scheduler/queue.go           # Min-heap, Job, JobGroup, fairshare
  matchmaker/evaluator.go      # CEL evaluation
  discovery/discovery.go       # Memberlist gossip
  messaging/                   # ZMQ PUB/SUB + DispatchPayload
api/v1/scheduler.proto         # 8 gRPC RPCs
```

## Tests

**Unit tests** (11 tests): `make test`
- `pkg/matchmaker`: 5 tests (CEL)
- `pkg/scheduler`: 6 tests (queue, cancel, backfill, fairshare)

**Integration tests** (11 scenarios): `make run-test`
1. Cluster nodes
2. Basic job submit
3. Priority + walltime
4. Job listing
5. Job detail
6. Submit + cancel
7. Log streaming
8. Distributed training (gang scheduling)
9. Env var passing
10. `tasch nodes` from worker config
11. `tasch jobs` from worker config

## Code Patterns

### Adding a CLI command

Add to `internal/cli/jobs.go` or create a new file. Register in `cmd/tasch/main.go`.

### Adding a gRPC RPC

1. Edit `api/v1/scheduler.proto`
2. `make proto`
3. Implement handler on `schedulerServer` in `internal/daemon/master.go`

### Adding a ClassAd field

Add to `ClassAd` struct in `pkg/profiler/profiler.go`. Instantly available in CEL.

### Adding GPU vendor support

Add a `detect<Vendor>GPUs()` function in `pkg/profiler/profiler.go`. Call it from `DetectGPUs()`.

## Dependencies

| Package | Purpose |
|---------|---------|
| `hashicorp/memberlist` | SWIM gossip |
| `go-zeromq/zmq4` | Pure-Go ZMQ |
| `google/cel-go` | CEL expressions |
| `shirou/gopsutil/v3` | CPU/memory profiling |
| `spf13/cobra` | CLI framework |
| `gopkg.in/yaml.v3` | Config file |

All pure Go — no CGo. Static binary.
