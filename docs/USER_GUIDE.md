# User Guide

## Getting Started

```bash
tasch setup    # configure this node
tasch start    # start the scheduler
tasch nodes    # verify cluster
```

## Commands

### `tasch nodes` — Cluster Status

Shows all workers with hardware + GPU info.

### `tasch jobs` — List Jobs

```bash
tasch jobs                     # all jobs
tasch jobs --state=RUNNING     # filter by state
```

### `tasch jobs submit` — Submit a Job

```bash
tasch jobs submit <cel_expression> <command> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--priority` | `-p` | 10 | Lower = runs first |
| `--user` | `-u` | anonymous | Fairshare tracking |
| `--walltime` | `-w` | 0 | Max seconds (0 = unlimited) |
| `--gpus` | | 0 | GPUs required |
| `--env` | `-e` | | KEY=VALUE (repeatable) |

Examples:
```bash
tasch jobs submit "ad.cpu_cores >= 1" "echo hello"
tasch jobs submit --gpus=2 --walltime=3600 "ad.gpu_count >= 2" "python train.py"
tasch jobs submit --env="LR=0.001" --gpus=1 "ad.gpu_count >= 1" "python train.py"
tasch jobs submit "ad.gpu_vendor == 'amd'" "python train.py"  # AMD only
```

**Auto-retry:** Failed jobs are automatically retried up to 3 times (configurable via `max_retries` in config) with exponential backoff (10s, 40s, 90s). After all retries, jobs go to the dead letter queue.

### `tasch jobs train` — Distributed Training

```bash
tasch jobs train <command> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--nodes` | 2 | Number of nodes |
| `--gpus-per-node` | 1 | GPUs per node |
| `--requirement` | `ad.gpu_count >= 1` | CEL expression |
| `--master-port` | 29500 | DDP coordination port |

Auto-injected env vars: `$RANK`, `$WORLD_SIZE`, `$MASTER_ADDR`, `$MASTER_PORT`, `$LOCAL_RANK`, `$NPROC_PER_NODE`.

```bash
tasch jobs train --nodes=2 --gpus-per-node=2 --walltime=7200 \
  "torchrun --nproc_per_node=\$NPROC_PER_NODE --nnodes=\$WORLD_SIZE \
   --node_rank=\$RANK --master_addr=\$MASTER_ADDR --master_port=\$MASTER_PORT \
   train.py --data=s3://s3.merai.app/dataset"
```

Gang scheduling: waits for ALL N nodes before dispatching. If one rank fails, all siblings are cancelled. 5-minute timeout if nodes can't be found.

### `tasch jobs status <id>` — Job Details

Shows state, command, worker, timing, output, errors, group, retry count.

### `tasch jobs cancel <id>` — Cancel

Cancels QUEUED (removed from queue) or RUNNING (kill signal to worker).

### `tasch jobs logs <id>` — Stream Logs

```bash
tasch jobs logs <id>           # existing logs
tasch jobs logs <id> --follow  # live stream
```

### `tasch jobs failed` — Dead Letter Queue

Shows jobs that exhausted all retry attempts.

```bash
$ tasch jobs failed
--- Dead Letter Queue (2 jobs) ---
JOB_ID     USER       TRIES  ERROR
------------------------------------------------------------
a3f2b1c0   alice      3      exit status 1
b7e4d2f1   bob        3      walltime exceeded (300s)
```

---

## CEL Expression Reference

| Variable | Type | Description |
|----------|------|-------------|
| `ad.os` | string | `linux`, `darwin`, `windows` |
| `ad.architecture` | string | `amd64`, `arm64` |
| `ad.cpu_cores` | int | Logical CPU count |
| `ad.total_memory_mb` | int | Total RAM in MB |
| `ad.gpu_count` | int | Number of GPUs (0 if none) |
| `ad.gpu_vendor` | string | `nvidia`, `amd`, or empty |
| `ad.gpu_memory_mb` | list | Per-GPU VRAM in MB |
| `ad.cuda_version` | string | CUDA version (NVIDIA) |
| `ad.rocm_version` | string | ROCm version (AMD) |
| `ad.host_type` | string | `container` or `vm_or_baremetal` |

Operators: `==`, `!=`, `>=`, `<=`, `>`, `<`, `&&`, `||`, `!`

Missing fields → `false` (non-match), not error.

---

## Reliability Features

### Job Retry
Failed jobs auto-retry up to `max_retries` (default 3) with exponential backoff. After all retries, moved to dead letter queue. Distributed training jobs (group jobs) do NOT retry — if one rank fails, all are cancelled.

### Persistence
All jobs persisted to `~/.tasch/tasch.db`. Master restart restores queued jobs. Running jobs at crash time are marked FAILED.

### Circuit Breaker
If a worker fails 3 consecutive jobs, it's blocked from receiving new jobs for 5 minutes. Resets on success.

### GPU Tracking
Master tracks how many GPUs are allocated on each node. Prevents dispatching more GPUs than available, even with concurrent dispatch cycles.

### Queue Limits
Max `max_queue_size` jobs in queue (default 10,000). New submissions rejected with error when full.

### Graceful Drain
`tasch stop` → master stops accepting new jobs → waits up to `drain_timeout` seconds for running jobs to finish → then shuts down.

### Health Checks
- `/health` — liveness (always 200)
- `/ready` — readiness (member count, queue depth, drain status)
- `/metrics` — Prometheus metrics

### Prometheus Metrics
| Metric | Type |
|--------|------|
| `tasch_jobs_submitted_total` | Counter (by user) |
| `tasch_jobs_completed_total` | Counter (by user, status) |
| `tasch_queue_depth` | Gauge |
| `tasch_running_jobs` | Gauge |
| `tasch_cluster_nodes` | Gauge |
| `tasch_dispatch_duration_seconds` | Histogram |
| `tasch_job_duration_seconds` | Histogram (by user, status) |
| `tasch_groups_pending` | Gauge |
| `tasch_walltime_kills_total` | Counter |
| `tasch_worker_lost_total` | Counter |
