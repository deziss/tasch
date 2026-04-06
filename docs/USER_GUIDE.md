# User Guide

## Getting Started

```bash
tasch setup    # configure this node
tasch start    # start the scheduler
tasch nodes    # verify cluster
```

## Commands

### `tasch nodes` — Cluster Status

```bash
$ tasch nodes
--- Cluster Nodes ---
Node: gpu-38
  OS: linux | Arch: amd64 | Cores: 32 | Memory: 128000MB | GPUs: 2
    GPU 0: NVIDIA A100-SXM4-40GB (40960MB)
    GPU 1: NVIDIA A100-SXM4-40GB (40960MB)
    CUDA: 12.2
Node: gpu-44
  OS: linux | Arch: amd64 | Cores: 16 | Memory: 64000MB | GPUs: 4
    GPU 0: AMD Instinct MI250X (65536MB)
    ...
    ROCm: 5.7.1
```

### `tasch jobs` — List Jobs

```bash
tasch jobs                     # list all
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
| `--walltime` | `-w` | 0 | Max seconds (0 = no limit) |
| `--gpus` | | 0 | GPUs required |
| `--env` | `-e` | | KEY=VALUE (repeatable) |

**Examples:**

```bash
# Simple job
tasch jobs submit "ad.cpu_cores >= 1" "echo hello"

# GPU training with 2 GPUs, 1 hour limit
tasch jobs submit --gpus=2 --walltime=3600 --user=alice \
  "ad.gpu_count >= 2" "python train.py"

# Custom env vars
tasch jobs submit --gpus=1 --env="BATCH_SIZE=64" --env="LR=0.001" \
  "ad.gpu_count >= 1" "python train.py"

# Only AMD GPUs
tasch jobs submit --gpus=2 "ad.gpu_vendor == 'amd' && ad.gpu_count >= 2" \
  "python train.py"
```

### `tasch jobs train` — Distributed Training

Runs the same command on N nodes with auto-injected PyTorch DDP variables.

```bash
tasch jobs train <command> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--nodes` | 2 | Number of nodes |
| `--gpus-per-node` | 1 | GPUs per node |
| `--requirement` | `ad.gpu_count >= 1` | CEL expression |
| `--master-port` | 29500 | DDP coordination port |

**Auto-injected env vars per rank:**

| Variable | Description |
|----------|-------------|
| `$RANK` | Node rank (0, 1, ..., N-1) |
| `$WORLD_SIZE` | Total nodes |
| `$MASTER_ADDR` | Rank-0 node IP (auto-resolved) |
| `$MASTER_PORT` | Coordination port |
| `$LOCAL_RANK` | 0 |
| `$NPROC_PER_NODE` | GPUs per node |

**Example:**

```bash
tasch jobs train --nodes=2 --gpus-per-node=2 --walltime=7200 --user=ml-team \
  "torchrun --nproc_per_node=\$NPROC_PER_NODE --nnodes=\$WORLD_SIZE \
   --node_rank=\$RANK --master_addr=\$MASTER_ADDR --master_port=\$MASTER_PORT \
   train.py --data=s3://s3.merai.app/dataset"
```

**How it works:**
1. Creates N rank jobs in a group
2. Gang-schedules: waits for N matching nodes
3. Resolves rank-0 IP → injects `MASTER_ADDR`
4. Dispatches all ranks simultaneously
5. If one rank fails, all siblings are cancelled

### `tasch jobs status` — Job Details

```bash
$ tasch jobs status dj-abc123-r0
Job dj-abc123-r0
  State:    COMPLETED
  Command:  torchrun ...
  Group:    dj-abc123
  Worker:   gpu-38
  Submit:   2026-04-06T10:00:00+00:00
  Start:    2026-04-06T10:00:02+00:00
  End:      2026-04-06T10:05:30+00:00
  Duration: 5m28s
  Output:
    Training complete. Loss: 0.023
```

### `tasch jobs cancel` — Cancel a Job

```bash
tasch jobs cancel <job_id>
```

### `tasch jobs logs` — Stream Logs

```bash
tasch jobs logs <job_id>           # existing logs
tasch jobs logs <job_id> --follow  # live stream
```

---

## CEL Expression Reference

### Variables

| Variable | Type | Description |
|----------|------|-------------|
| `ad.os` | string | `linux`, `darwin`, `windows` |
| `ad.architecture` | string | `amd64`, `arm64` |
| `ad.cpu_cores` | int | Logical CPU count |
| `ad.total_memory_mb` | int | Total RAM in MB |
| `ad.available_mem_mb` | int | Available RAM in MB |
| `ad.host_type` | string | `container` or `vm_or_baremetal` |
| `ad.gpu_count` | int | Number of GPUs (0 if none) |
| `ad.gpu_vendor` | string | `nvidia`, `amd`, or empty |
| `ad.gpu_models` | list | GPU model names |
| `ad.gpu_memory_mb` | list | Per-GPU VRAM in MB |
| `ad.cuda_version` | string | CUDA version (NVIDIA) |
| `ad.rocm_version` | string | ROCm version (AMD) |

### Operators

`==`, `!=`, `>=`, `<=`, `>`, `<`, `&&`, `||`, `!`

Missing fields evaluate to `false` (non-match), not an error.

### Examples

```python
ad.gpu_count >= 2                                    # 2+ GPUs
ad.gpu_vendor == "nvidia" && ad.gpu_count >= 2       # NVIDIA only
ad.gpu_vendor == "amd" && ad.gpu_memory_mb[0] >= 32000  # AMD with 32GB+
ad.cpu_cores >= 8 && ad.total_memory_mb >= 16384     # 8 cores + 16GB
ad.os == "linux" && ad.host_type == "vm_or_baremetal" # bare metal Linux
```

---

## Concepts

### Job States

```
QUEUED → RUNNING → COMPLETED | FAILED
                → CANCELLED (from QUEUED or RUNNING)
```

### Priority

Lower number = higher urgency. Default 10. Priority 1 runs before 10.

### Fairshare

Heavy users get a priority penalty that decays over time.

### Walltime

Max execution time. Killed automatically if exceeded.

### Backfill

When the top job can't run, lower-priority jobs fill idle nodes.

### Gang Scheduling

Distributed jobs wait for ALL N nodes before dispatching any rank. Prevents NCCL deadlocks.

---

## Workflow Examples

### Batch processing
```bash
for i in $(seq 1 100); do
  tasch jobs submit "ad.cpu_cores >= 2" "python process.py --file=$i" --user=batch --walltime=300
done
```

### Monitor a distributed job
```bash
tasch jobs train --nodes=2 "torchrun ... train.py"
# → Group ID: dj-abc123

tasch jobs | grep dj-abc123         # see all ranks
tasch jobs logs dj-abc123-r0 -f     # stream rank 0
tasch jobs status dj-abc123-r1      # check rank 1
```
