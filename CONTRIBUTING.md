# Contributing to Tasch

## Getting Started

```bash
git clone https://github.com/deziss/tasch.git
cd tasch
make build    # single binary → bin/tasch
make test     # unit tests
make run-test # integration tests (11 scenarios)
```

## Development Workflow

1. Create a feature branch
2. Make changes
3. `make test` + `make run-test`
4. Submit PR

## Where to Add Things

| What | Where |
|------|-------|
| New CLI command | `internal/cli/` |
| New gRPC RPC | `api/v1/scheduler.proto` → `make proto` → `internal/daemon/master.go` |
| New ClassAd field | `pkg/profiler/profiler.go` |
| New GPU vendor | `pkg/profiler/profiler.go` → add `detect<Vendor>GPUs()` |
| New scheduling policy | `internal/daemon/master.go` → `dispatchLoop()` |
| Config changes | `internal/config/config.go` + `internal/setup/setup.go` |

## Commit Style

Imperative mood:
- `add AMD GPU detection via rocm-smi`
- `rename tasch submit to tasch jobs submit`
- `fix gang scheduler deadlock on single-node groups`
