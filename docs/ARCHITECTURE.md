# Architecture

## Overview

Single `tasch` binary operates as master, worker, or both based on config. Runs natively on Linux, Windows, and macOS (amd64 and arm64).

```
┌───────────────────────────────────────────────────────────┐
│                   CLI Commands                             │
│     tasch nodes · tasch jobs submit · tasch jobs train     │
└──────────────────────────┬────────────────────────────────┘
                           │ gRPC (optional TLS)
┌──────────────────────────▼────────────────────────────────┐
│                    Master Scheduler                        │
│                                                            │
│  gRPC Server (8 RPCs)    ·  BoltDB Persistence (async)    │
│  Min-Heap Queue          ·  Fairshare Calculator           │
│  CEL Matchmaker          ·  Circuit Breaker                │
│  Gang Scheduler          ·  Multi-Resource Tracker         │
│  Walltime Enforcer       ·  Job Retry + Dead Letters       │
│  Dispatch Handshake      ·  Prometheus Metrics             │
│  ZMQ Publisher           ·  Health: /health /ready        │
│  Gossip Discovery + Worker Loss Detection                  │
└──────────────────────────┬────────────────────────────────┘
                           │ ZMQ PUB/SUB (auto-reconnect) + Gossip
              ┌────────────┼────────────┬───────────────┐
              ▼            ▼            ▼               ▼
         ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────────┐
         │ Linux   │ │ Linux   │ │ macOS   │ │ Windows     │
         │ amd64   │ │ arm64   │ │ arm64   │ │ amd64/arm64 │
         │ NVIDIA  │ │ Tegra   │ │ Metal   │ │ Intel/AMD   │
         │ AMD     │ │ AMD     │ │         │ │ Qualcomm    │
         └─────────┘ └─────────┘ └─────────┘ └─────────────┘
```

## Protocol Layers

| Protocol | Default Port | Purpose |
|----------|-------------|---------|
| Memberlist (SWIM) | 7946 | Node discovery + ClassAd broadcast + failure detection |
| ZeroMQ PUB/SUB | 5555 | Job dispatch + cancel signals (auto-reconnect) |
| gRPC (HTTP/2) | 50051 | CLI commands + worker result reporting (keepalive + TLS) |
| HTTP | 9090 | Health checks + Prometheus metrics + dispatch handshake |

## Dispatch Loop (1s cycle)

```
Phase 0: Gang-schedule PENDING distributed groups (5 min timeout)
Phase 1: Match top-priority single job (circuit breaker + multi-resource check)
Phase 2: Backfill lower-priority jobs on idle nodes
```

Each cycle updates Prometheus gauges: queue depth, running jobs, cluster nodes, pending groups.

## Dispatch Handshake

After a job is dispatched via ZMQ, the master registers it in `dispatchPending`. When the worker receives the job and begins execution, it sends an HTTP POST to `/acknowledge_start`. If no acknowledgement arrives within **10 seconds**, the `dispatchTimeoutEnforcer` loop re-queues the job as `QUEUED`.

This prevents silent job loss when a worker receives a dispatch message but fails to start execution (e.g., crash, network partition, resource exhaustion).

## GPU Detection (Cross-Platform)

| OS | GPU Vendors | Detection Method |
|----|-------------|-----------------|
| Linux amd64/arm64 | NVIDIA, AMD, Jetson Tegra | `nvidia-smi`, `rocm-smi`, `/sys/...` |
| Windows amd64/arm64 | NVIDIA, AMD, Intel, Qualcomm | WMI `Win32_VideoController`, `nvidia-smi.exe` |
| macOS amd64/arm64 | Apple Metal, AMD eGPU | `system_profiler SPDisplaysDataType`, `sysctl hw.memsize` |

Platform-specific logic is isolated using Go build tags:
```
profiler_linux.go        //go:build linux
profiler_windows.go      //go:build windows
profiler_darwin.go       //go:build darwin
profiler_fallback.go     //go:build !linux && !windows && !darwin
```

## OS-Aware GPU Env Binding

At dispatch time, the master injects the correct GPU visibility env var into the job's environment based on the **target worker's** GPU vendor (from its ClassAd):

| Vendor | Env Var Injected |
|--------|-----------------|
| NVIDIA | `CUDA_VISIBLE_DEVICES=0,1,...` |
| AMD | `HIP_VISIBLE_DEVICES=0,1,...` |
| Intel | `ONEAPI_DEVICE_SELECTOR=gpu:0,gpu:1,...` + `SYCL_DEVICE_FILTER` |
| Apple | `METAL_DEVICE_INDEX=0` |

## Platform-Agnostic Worker Execution

Worker subprocess creation is handled by build-tag-selected helpers:
- `exec_unix.go` (`//go:build !windows`): `sh -c <command>`
- `exec_windows.go` (`//go:build windows`): `cmd.exe /d /c <command>` with `HideWindow: true` (no popup console)

## Multi-Resource Tracking

The `gpuTracker` struct tracks 3 resource dimensions per worker node:

```
Allocate(node, gpus, cpus, memoryMB)
Release(node, gpus, cpus, memoryMB)
Available(node, totalGPUs) → bool
AvailableCPUs(node, totalCores) → int
AvailableMemory(node, totalMB) → int
```

Resources are parsed from the CEL expression at submit time via `parseCELRequirement()`:
- `cpu_cores >= N` → sets `CPUsRequired`
- `total_memory_mb >= N` → sets `MemoryRequiredMB`
- `--gpus` flag → sets `GPUsRequired`

## Persistence (BoltDB)

`~/.tasch/tasch.db` — 4 buckets:

| Bucket | Contents |
|--------|----------|
| `jobs` | All jobs (any state) |
| `groups` | Distributed job groups |
| `fairshare` | Per-user usage counters |
| `dead_letters` | Jobs that exhausted all retries |

Write path: scheduler events → `OnJobChange`/`OnGroupChange` hooks → buffered `dbWriteChan` → dedicated goroutine → BoltDB. Hot path never blocks on disk.

On restart: QUEUED jobs re-enqueued, RUNNING jobs marked FAILED, groups marked FAILED, fairshare restored.

## Job Retry

```
Job fails → RetryCount < MaxRetries?
  Yes → sleep(retryCount² × 10s) → re-enqueue as QUEUED
  No  → save to dead_letters bucket → mark FAILED
```

Distributed training jobs (GroupID set) never retry — one rank failure cancels all siblings.

## Circuit Breaker

Per-worker consecutive failure tracking:
- 3+ consecutive failures → worker blocked for 5 minutes
- Success resets counter
- Dispatch loop skips blocked workers

**Exclusions** (do not count as failures):
- Jobs cancelled by user
- Jobs killed by walltime enforcer
- Same job ID failing consecutively on the same node (poison-pill protection)

## Graceful Shutdown

```
SIGTERM received
  → draining = true (reject new submissions)
  → wait up to drain_timeout for running jobs
  → cancel worker
  → cancel master (gRPC graceful stop + close ZMQ + drain+close BoltDB)
  → remove PID file

tasch stop:
  → SIGTERM → wait 15s → SIGKILL if stuck
```

## Package Layout

```
cmd/tasch/main.go              # Entry point + Cobra root
internal/
  config/                      # Config YAML + TLS + env overrides
  setup/                       # Interactive wizard
  store/store.go               # BoltDB persistence (4 buckets)
  daemon/
    master.go                  # All scheduler logic + gRPC handlers
    worker.go                  # Executor + ZMQ reconnect + gRPC keepalive + handshake
    exec_unix.go               # sh -c command builder (linux/darwin/etc.)
    exec_windows.go            # cmd.exe /d /c builder (hidden window)
    metrics.go                 # Prometheus metric definitions
    stop.go                    # PID-based stop with SIGKILL fallback
  cli/
    jobs.go                    # tasch jobs * commands + dead letters
    nodes.go                   # tasch nodes
    connect.go                 # gRPC client from config
pkg/
  profiler/
    profiler.go                # Shared Host struct + profiling entry point
    profiler_linux.go          # NVIDIA + AMD + Jetson detection
    profiler_windows.go        # WMI/PowerShell detection
    profiler_darwin.go         # Apple Metal + Unified Memory detection
    profiler_fallback.go       # Stub for unsupported OS
  scheduler/                   # Min-heap, Job/JobGroup, hooks, fairshare
  matchmaker/                  # Google CEL evaluator
  discovery/                   # Memberlist + EventHooks
  messaging/                   # ZeroMQ PUB/SUB
```
