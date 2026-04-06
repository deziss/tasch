# Architecture

## Overview

Single `tasch` binary operates as master, worker, or both based on config.

```
┌───────────────────────────────────────────────────────────┐
│                   CLI Commands                             │
│     tasch nodes · tasch jobs submit · tasch jobs train     │
└──────────────────────────┬────────────────────────────────┘
                           │ gRPC (optional TLS)
┌──────────────────────────▼────────────────────────────────┐
│                    Master Scheduler                        │
│                                                            │
│  gRPC Server (8 RPCs)  ·  BoltDB Persistence               │
│  Min-Heap Queue        ·  Fairshare Calculator              │
│  CEL Matchmaker        ·  Circuit Breaker                   │
│  Gang Scheduler        ·  GPU Resource Tracker              │
│  Walltime Enforcer     ·  Job Retry + Dead Letters          │
│  ZMQ Publisher         ·  Prometheus Metrics                │
│  Health: /health /ready /metrics                           │
│  Gossip Discovery + Worker Loss Detection                  │
└──────────────────────────┬────────────────────────────────┘
                           │ ZMQ PUB/SUB (auto-reconnect) + Gossip
              ┌────────────┼────────────┐
              ▼            ▼            ▼
         ┌────────┐  ┌────────┐  ┌────────┐
         │Worker 1│  │Worker 2│  │Worker N│
         │NVIDIA  │  │AMD     │  │CPU-only│
         │gRPC KA │  │gRPC KA │  │gRPC KA │
         └────────┘  └────────┘  └────────┘
```

## Protocol Layers

| Protocol | Port | Purpose |
|----------|------|---------|
| Memberlist (SWIM) | 7946 | Node discovery + ClassAd broadcast + failure detection |
| ZeroMQ PUB/SUB | 5555 | Job dispatch + cancel signals (auto-reconnect) |
| gRPC (HTTP/2) | 50051 | CLI commands + worker result reporting (keepalive + TLS) |
| HTTP | 9090 | Health checks + Prometheus metrics |

## Dispatch Loop (1s cycle)

```
Phase 0: Gang-schedule PENDING distributed groups (5 min timeout)
Phase 1: Match top-priority single job (circuit breaker + GPU tracking check)
Phase 2: Backfill lower-priority jobs on idle nodes
```

Each cycle updates Prometheus gauges: queue depth, running jobs, cluster nodes, pending groups.

## Persistence (BoltDB)

`~/.tasch/tasch.db` — 4 buckets:

| Bucket | Contents |
|--------|----------|
| `jobs` | All jobs (any state) |
| `groups` | Distributed job groups |
| `fairshare` | Per-user usage counters |
| `dead_letters` | Jobs that exhausted all retries |

Write-through: `OnJobChange` / `OnGroupChange` hooks on GlobalScheduler fire on every state transition.

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

## GPU Resource Tracking

```
dispatch → gpuTracker.Allocate(node, count)
complete/cancel/worker-lost → gpuTracker.Release(node, count)
dispatch check → gpuTracker.Available(node, totalFromClassAd) >= required
```

Prevents multiple concurrent dispatches from oversubscribing GPUs on the same node.

## Worker Resilience

- **ZMQ auto-reconnect**: exponential backoff 1s → 2s → 4s → ... → 30s max
- **gRPC keepalive**: 30s interval, 10s timeout, permits without stream
- **ReportResult**: 10s timeout + one retry on failure
- **TLS**: optional CA verification when connecting to master

## Graceful Shutdown

```
SIGTERM received
  → draining = true (reject new submissions)
  → wait up to drain_timeout for running jobs
  → cancel worker
  → cancel master (gRPC graceful stop + close ZMQ + close BoltDB)
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
    worker.go                  # Executor + ZMQ reconnect + gRPC keepalive
    metrics.go                 # Prometheus metric definitions
    stop.go                    # PID-based stop with SIGKILL fallback
  cli/
    jobs.go                    # tasch jobs * commands + dead letters
    nodes.go                   # tasch nodes
    connect.go                 # gRPC client from config
pkg/
  profiler/                    # NVIDIA + AMD GPU detection
  scheduler/                   # Min-heap, Job/JobGroup, hooks, fairshare
  matchmaker/                  # Google CEL evaluator
  discovery/                   # Memberlist + EventHooks
  messaging/                   # ZeroMQ PUB/SUB
```
