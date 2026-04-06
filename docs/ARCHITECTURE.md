# Architecture

## Overview

Single `tasch` binary operates in three modes based on config:

```
tasch (role: master)  → Scheduler + gRPC server + ZMQ publisher + gossip
tasch (role: worker)  → GPU profiler + gossip + ZMQ subscriber + executor
tasch (role: both)    → All of the above in one process
```

```
┌────────────────────────────────────────────────────┐
│                   CLI Commands                      │
│         tasch nodes / tasch jobs submit ...         │
└───────────────────────┬────────────────────────────┘
                        │ gRPC (reads master_addr from config)
┌───────────────────────▼────────────────────────────┐
│              Master Scheduler                       │
│  8 gRPC RPCs · Min-Heap Queue · CEL Matchmaker     │
│  Gang Scheduler · Backfill · Fairshare · Walltime  │
│  ZMQ PUB · Gossip                                  │
└───────────────────────┬────────────────────────────┘
                        │ ZMQ PUB/SUB + Gossip
              ┌─────────┼─────────┐
              ▼         ▼         ▼
         ┌────────┐ ┌────────┐ ┌────────┐
         │Worker 1│ │Worker 2│ │Worker N│
         │NVIDIA  │ │AMD     │ │CPU-only│
         └────────┘ └────────┘ └────────┘
```

## Three Protocol Layers

| Protocol | Purpose | Port |
|----------|---------|------|
| **Memberlist (SWIM gossip)** | Node discovery + ClassAd broadcast | 7946 |
| **ZeroMQ PUB/SUB** | Job dispatch + cancel signals | 5555 |
| **gRPC (HTTP/2)** | CLI commands + worker result reporting | 50051 |

## Dispatch Loop (master, every 1s)

```
Phase 0: Gang-schedule PENDING distributed job groups
         → Find N distinct matching nodes → dispatch all ranks simultaneously
Phase 1: Match top-priority single job → dispatch to first matching node
Phase 2: Backfill → lower-priority jobs fill idle nodes
```

## GPU Profiling

Workers detect GPUs at startup:
1. Try `nvidia-smi --query-gpu=name,memory.total` → NVIDIA
2. If not found, try `rocm-smi --showproductname --showmeminfo vram` → AMD
3. If neither found → gpu_count: 0

Auto-sets `CUDA_VISIBLE_DEVICES` (NVIDIA) or `HIP_VISIBLE_DEVICES` (AMD) at dispatch time.

## Gang Scheduling (Distributed Jobs)

1. `SubmitDistributedJob` creates N rank jobs with DDP env vars
2. Gang scheduler waits until N distinct matching nodes available
3. Resolves rank-0 node IP → injects `MASTER_ADDR` into all ranks
4. Dispatches all ranks simultaneously
5. If one rank fails → cancels all siblings

## Config-Based Architecture

```yaml
# ~/.tasch/config.yaml
role: both
node_name: gpu-38
master_addr: 10.0.1.38
ports:
  gossip: 7946
  grpc: 50051
  zmq: 5555
```

- `tasch start` reads config to determine what to start
- `tasch jobs/nodes` reads config to find master gRPC address
- Works from any machine with a config file pointing to the master

## Package Layout

```
cmd/tasch/main.go              # Entry point, Cobra root command
internal/
  config/config.go             # Config YAML struct + load/save
  setup/setup.go               # Interactive wizard
  daemon/master.go             # Master scheduler (all handlers + scheduling)
  daemon/worker.go             # Worker agent (execution + reporting)
  daemon/stop.go               # PID-based stop
  cli/jobs.go                  # tasch jobs * commands
  cli/nodes.go                 # tasch nodes command
  cli/connect.go               # Shared gRPC client from config
pkg/
  profiler/                    # Hardware + GPU detection (NVIDIA + AMD)
  scheduler/                   # Min-heap, Job/JobGroup, fairshare
  matchmaker/                  # Google CEL evaluator
  discovery/                   # Memberlist gossip
  messaging/                   # ZeroMQ PUB/SUB
```
