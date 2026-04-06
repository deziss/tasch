# Tasch — Distributed Task Scheduler

**Tasch** is a lightweight, distributed task scheduler with GPU support. One binary, one setup command, and your cluster is ready.

```
┌──────────┐         ┌──────────┐
│ Server 10│ ◄─────► │ Server 20│
│ (master  │  gossip │ (worker) │
│ +worker) │  + ZMQ  │ 4x V100  │
│ 2x A100  │         │          │
└──────────┘         └──────────┘
```

## Install

```bash
git clone https://github.com/deziss/tasch.git
cd tasch && make build
sudo cp bin/tasch /usr/local/bin/
```

## Quick Start

### 1. Setup

```bash
# Interactive wizard — asks role, node name, ports
tasch setup
```

```
Welcome to Tasch Distributed Scheduler!

Select role:
  1) Master  — schedules and dispatches jobs
  2) Worker  — executes jobs on this machine
  3) Both    — master + worker on this machine

Role [3]: 3
Node name [gpu-server-10]:
Master address [10.0.1.10]:
...

Detected hardware:
  CPU:  32 cores (AMD EPYC 7543)
  RAM:  128GB
  GPU 0: NVIDIA A100-SXM4-40GB (40960MB)
  GPU 1: NVIDIA A100-SXM4-40GB (40960MB)
  CUDA: 12.2

Config written to ~/.tasch/config.yaml
Start with: tasch start
```

### 2. Start

```bash
tasch start    # reads config, starts master/worker/both
```

### 3. Add remote workers

On server 20:
```bash
tasch setup    # select "Worker", enter server-10 IP as master address
tasch start
```

### 4. Use it

```bash
tasch nodes                    # see all cluster nodes + GPUs
tasch jobs                     # list jobs
tasch jobs submit "ad.gpu_count >= 1" "python train.py" --gpus=1
tasch jobs train --nodes=2 "torchrun ... train.py"
tasch jobs status <id>         # job details + output
tasch jobs logs <id>           # stream logs
tasch jobs cancel <id>
tasch stop                     # graceful shutdown
```

These commands work from **any machine** with a config pointing to the master.

## Features

| Feature | Description |
|---------|-------------|
| **Single binary** | `tasch setup` → `tasch start` → done |
| **GPU detection** | NVIDIA (`nvidia-smi`) + AMD (`rocm-smi`), auto-detected |
| **Distributed training** | `tasch jobs train` with gang scheduling + DDP env vars |
| **CEL matchmaking** | `ad.gpu_count >= 2 && ad.gpu_vendor == "nvidia"` |
| **Cross-server** | Workers join via gossip — just set master address |
| **Backfill** | Idle nodes run lower-priority jobs while big jobs wait |
| **Fairshare** | Heavy users get priority penalties (auto-decaying) |
| **Walltime** | `--walltime=3600` kills jobs that exceed the limit |
| **Env injection** | `--env BATCH_SIZE=64` + auto `CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES` |
| **Live logs** | `tasch jobs logs <id> --follow` |

## CLI Reference

```
tasch setup                          # Interactive setup wizard
tasch start                          # Start based on config
tasch stop                           # Graceful shutdown

tasch nodes                          # Cluster status + GPU info

tasch jobs                           # List all jobs
tasch jobs submit <expr> <cmd>       # Submit single job
tasch jobs train <cmd>               # Distributed training
tasch jobs cancel <id>               # Cancel job
tasch jobs status <id>               # Job detail + output
tasch jobs logs <id> [--follow]      # Stream logs
```

### Submit Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--priority` | `-p` | 10 | Lower = higher priority |
| `--user` | `-u` | anonymous | Fairshare tracking |
| `--walltime` | `-w` | 0 | Max seconds (0 = unlimited) |
| `--gpus` | | 0 | GPUs required |
| `--env` | `-e` | | KEY=VALUE (repeatable) |

### Train Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--nodes` | 2 | Number of nodes |
| `--gpus-per-node` | 1 | GPUs per node |
| `--requirement` | `ad.gpu_count >= 1` | CEL expression |
| `--master-port` | 29500 | DDP coordination port |

## ClassAd Variables (CEL)

| Variable | Type | Example |
|----------|------|---------|
| `ad.gpu_count` | int | `ad.gpu_count >= 2` |
| `ad.gpu_vendor` | string | `ad.gpu_vendor == "nvidia"` |
| `ad.gpu_models` | list | |
| `ad.gpu_memory_mb` | list | `ad.gpu_memory_mb[0] >= 40000` |
| `ad.cuda_version` | string | |
| `ad.rocm_version` | string | |
| `ad.cpu_cores` | int | `ad.cpu_cores >= 8` |
| `ad.total_memory_mb` | int | |
| `ad.os` | string | `ad.os == "linux"` |
| `ad.architecture` | string | `ad.architecture == "amd64"` |
| `ad.host_type` | string | `vm_or_baremetal` or `container` |

## Config File

`~/.tasch/config.yaml`:
```yaml
role: both              # master | worker | both
node_name: gpu-server-10
master_addr: 10.0.1.10
ports:
  gossip: 7946
  grpc: 50051
  zmq: 5555
```

Override with `--config <path>` or `TASCH_*` env vars.

## Project Structure

```
tasch/
├── cmd/tasch/main.go           # Single binary entry point
├── internal/
│   ├── config/config.go        # Config YAML load/save
│   ├── setup/setup.go          # Interactive setup wizard
│   ├── daemon/master.go        # Master scheduler (8 gRPC RPCs, dispatch, gang-sched)
│   ├── daemon/worker.go        # Worker agent (GPU profiling, exec, env inject)
│   ├── daemon/stop.go          # PID-based stop
│   └── cli/                    # CLI commands (jobs, nodes)
├── pkg/
│   ├── profiler/               # Hardware + GPU detection (NVIDIA + AMD)
│   ├── scheduler/              # Min-heap queue, Job/JobGroup, fairshare
│   ├── matchmaker/             # Google CEL evaluator
│   ├── discovery/              # Memberlist gossip wrapper
│   └── messaging/              # ZeroMQ PUB/SUB
├── api/v1/scheduler.proto      # 8 gRPC RPCs
├── Makefile
└── test.sh                     # 11-scenario integration test
```

## Documentation

| Document | Description |
|----------|-------------|
| [Setup Guide](docs/SETUP.md) | Installation, single/multi-node deployment |
| [User Guide](docs/USER_GUIDE.md) | All commands, CEL syntax, training workflows |
| [API Reference](docs/API.md) | 8 gRPC RPCs, Protobuf messages, ZMQ protocol |
| [Architecture](docs/ARCHITECTURE.md) | System design, gang scheduling, GPU profiling |
| [Development](docs/DEVELOPMENT.md) | Building, testing, code patterns |
| [Changelog](CHANGELOG.md) | Version history |

## License

MIT — see [LICENSE](LICENSE).
