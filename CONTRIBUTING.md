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
| ClassAd field | `pkg/profiler/profiler.go` |
| GPU vendor | `pkg/profiler/profiler.go` → `detect<Vendor>GPUs()` |
| Scheduler policy | `internal/daemon/master.go` → `dispatchLoop()` |
| Persistence bucket | `internal/store/store.go` |
| Prometheus metric | `internal/daemon/metrics.go` |
| Config field | `internal/config/config.go` + `internal/setup/setup.go` |
| Packaging | `nfpm.yaml` + `packaging/` |

## Testing

- Unit: `make test` (13 tests)
- Integration: `make run-test` (11 scenarios)
- All tests must pass before merging

## Commit Style

```
feat: add job priority boost for GPU jobs
fix: circuit breaker not resetting on success
docs: update API reference with retry fields
```
