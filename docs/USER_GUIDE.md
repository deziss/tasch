# User Guide

## Getting Started

```bash
tasch setup    # configure this node
tasch start    # start the scheduler
tasch nodes    # verify cluster — shows OS, arch, GPUs
```

## Commands

### `tasch nodes` — Cluster Status

Shows all workers with OS, architecture, CPU/memory specs, GPU vendor, model, and VRAM.

```
--- Cluster Nodes ---
Node: linux-gpu
  OS: linux | Arch: amd64 | Cores: 32 | Memory: 128000MB | GPUs: 2
    GPU 0: NVIDIA A100-SXM4-40GB (40960MB)
    GPU 1: NVIDIA A100-SXM4-40GB (40960MB)
    CUDA: 12.2

Node: mac-m2
  OS: darwin | Arch: arm64 | Cores: 24 | Memory: 96000MB | GPUs: 1
    GPU 0: Apple M2 Ultra (76800MB)

Node: win-intel
  OS: windows | Arch: amd64 | Cores: 16 | Memory: 32000MB | GPUs: 1
    GPU 0: Intel Arc A770 (16384MB)
```

### `tasch jobs` — List Jobs

```bash
tasch jobs                     # all jobs
tasch jobs --state=RUNNING     # filter by state
tasch jobs --state=QUEUED
tasch jobs --state=FAILED
```

States: `QUEUED`, `RUNNING`, `COMPLETED`, `FAILED`, `CANCELLED`

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
# Any node with at least 1 CPU
tasch jobs submit "ad.cpu_cores >= 1" "echo hello"

# NVIDIA GPU job (Linux only)
tasch jobs submit --gpus=2 --walltime=3600 \
  "ad.gpu_count >= 2 && ad.gpu_vendor == 'nvidia' && ad.os == 'linux'" \
  "python train.py"

# AMD GPU job
tasch jobs submit "ad.gpu_vendor == 'amd'" "python train.py"

# Intel GPU job (Windows)
tasch jobs submit "ad.gpu_vendor == 'intel' && ad.os == 'windows'" \
  "my_oneapi_app.exe"

# Apple Metal job (macOS arm64)
tasch jobs submit "ad.gpu_vendor == 'apple' && ad.architecture == 'arm64'" \
  "./my_metal_app"

# Multi-resource constraint: 8 cores + 16 GB RAM + NVIDIA
tasch jobs submit \
  "ad.cpu_cores >= 8 && ad.total_memory_mb >= 16000 && ad.gpu_vendor == 'nvidia'" \
  --gpus=1 "python train.py"

# Pass env vars
tasch jobs submit --env="LR=0.001" --env="EPOCHS=50" \
  "ad.gpu_count >= 1" "python train.py"
```

**Auto-retry:** Failed jobs are automatically retried up to 3 times (configurable via `max_retries`) with exponential backoff (10s, 40s, 90s). After all retries, jobs go to the dead letter queue.

**GPU env binding:** The scheduler automatically injects the correct GPU visibility env var at dispatch time based on the worker's GPU vendor — no manual configuration required.

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
| `--priority` | 0 | Job priority |
| `--user` | | Username |
| `--walltime` | 0 | Max seconds |
| `--env` | | KEY=VALUE (repeatable) |

Auto-injected env vars: `$RANK`, `$WORLD_SIZE`, `$MASTER_ADDR`, `$MASTER_PORT`, `$LOCAL_RANK`, `$NPROC_PER_NODE`.

```bash
tasch jobs train --nodes=4 --gpus-per-node=2 --walltime=7200 \
  --requirement="ad.gpu_count >= 2 && ad.gpu_vendor == 'nvidia'" \
  "torchrun \
    --nproc_per_node=\$NPROC_PER_NODE \
    --nnodes=\$WORLD_SIZE \
    --node_rank=\$RANK \
    --master_addr=\$MASTER_ADDR \
    --master_port=\$MASTER_PORT \
    train.py --data=/mnt/data/dataset"
```

Gang scheduling: waits for ALL N nodes before dispatching. If one rank fails, all siblings are cancelled. 5-minute timeout if nodes can't be found.

### `tasch jobs status <id>` — Job Details

Shows state, command, worker, timing, output, errors, and group ID for distributed jobs.

### `tasch jobs cancel <id>` — Cancel

Cancels QUEUED (removed from queue) or RUNNING (kill signal sent to worker).

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
| `ad.gpu_vendor` | string | `nvidia`, `amd`, `intel`, `apple`, or empty |
| `ad.gpu_memory_mb` | list | Per-GPU VRAM in MB |
| `ad.cuda_version` | string | CUDA version (NVIDIA only) |
| `ad.rocm_version` | string | ROCm version (AMD only) |
| `ad.host_type` | string | `container` or `vm_or_baremetal` |

Operators: `==`, `!=`, `>=`, `<=`, `>`, `<`, `&&`, `||`, `!`

Missing fields → `false` (non-match), not error.

### Common CEL Patterns

```bash
# Linux NVIDIA nodes only
"ad.gpu_vendor == 'nvidia' && ad.os == 'linux'"

# High-memory nodes (64 GB+)
"ad.total_memory_mb >= 65536"

# GPU with at least 40 GB VRAM
"ad.gpu_count >= 1 && ad.gpu_memory_mb[0] >= 40000"

# Any ARM64 node
"ad.architecture == 'arm64'"

# CPU-only nodes (no GPU required)
"ad.cpu_cores >= 4"

# Windows nodes with Intel GPU
"ad.os == 'windows' && ad.gpu_vendor == 'intel'"
```

---

## Reliability Features

### Job Retry
Failed jobs auto-retry up to `max_retries` (default 3) with exponential backoff. After all retries, moved to dead letter queue. Distributed training jobs do NOT retry — if one rank fails, all siblings are cancelled.

### Dispatch Handshake
After receiving a job via ZMQ, workers send an HTTP acknowledgement to the master's `/acknowledge_start` endpoint. If the master doesn't receive acknowledgement within 10 seconds, the job is automatically re-queued. This prevents silent job loss due to network issues.

### Persistence
All jobs persisted to `tasch.db`.
- **User-local**: `~/.tasch/tasch.db`
- **System-wide**: `/var/lib/tasch/tasch.db`

Master restart restores queued jobs. Running jobs at crash time are marked FAILED.

---

## System-wide Installation (Service)

If installed via `.deb` or `.rpm`, Tasch can be managed as a systemd service.

### Configuration
The system configuration is located at `/etc/tasch/config.yaml`. The service runs as the `tasch` user.

### Service Management
```bash
sudo systemctl enable tasch
sudo systemctl start tasch
sudo systemctl status tasch
sudo journalctl -u tasch -f  # Follow logs
```

### Data and Logs
- **Config**: `/etc/tasch/config.yaml`
- **State/Database**: `/var/lib/tasch/tasch.db`
- **Binary**: `/usr/bin/tasch`

---

## Reliability Reference

### Circuit Breaker
If a worker fails 3 consecutive jobs, it's blocked from receiving new jobs for 5 minutes. Resets on first success.

### GPU Tracking
Master tracks allocated GPUs, CPU cores, and memory per node. Prevents dispatching more resources than available, even during concurrent dispatch cycles.

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
