# Tasch — Distributed Task Scheduler

**Tasch** is a production-grade, distributed task scheduler with GPU support. One binary, one setup command, and your cluster is ready.

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

```bash
tasch setup    # interactive wizard — role, name, master addr, ports
tasch start    # starts master/worker/both based on config
```

Remote worker (server 20):
```bash
tasch setup    # select "Worker", enter server-10 IP
tasch start
```

Use from any machine:
```bash
tasch nodes                    # cluster status + GPUs
tasch jobs submit --gpus=1 "ad.gpu_count >= 1" "python train.py"
tasch jobs train --nodes=2 "torchrun ... train.py"
tasch jobs                     # list all jobs
tasch jobs status <id>         # detail + output
tasch jobs logs <id> --follow  # live log streaming
tasch jobs cancel <id>
tasch jobs failed              # dead letter queue
tasch stop                     # graceful drain + shutdown
```

## Features

| Feature | Description |
|---------|-------------|
| **Single binary** | `tasch setup` → `tasch start` → done |
| **GPU detection** | NVIDIA (`nvidia-smi`) + AMD (`rocm-smi`), auto `CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES` |
| **Distributed training** | `tasch jobs train` — gang scheduling + auto DDP env vars (`RANK`, `WORLD_SIZE`, `MASTER_ADDR`) |
| **BoltDB persistence** | Jobs, groups, fairshare survive master restart (`~/.tasch/tasch.db`) |
| **Job retry** | Auto-retry failed jobs (default 3x) with exponential backoff. Dead letter queue for exhausted retries |
| **Health checks** | `/health` (liveness) + `/ready` (readiness) endpoints on metrics port |
| **Prometheus metrics** | 9 metrics: queue depth, running jobs, dispatch duration, job duration, walltime kills, worker loss |
| **Circuit breaker** | 3 consecutive failures → worker blocked 5 minutes |
| **GPU resource tracking** | Prevents GPU oversubscription across concurrent dispatches |
| **Graceful drain** | `tasch stop` → stop accepting → wait for running jobs → shutdown |
| **ZMQ auto-reconnect** | Worker reconnects to master with exponential backoff (1s-30s) |
| **gRPC keepalive** | 30s heartbeat, 10s timeout, survives transient disconnects |
| **TLS support** | Optional mTLS for gRPC (`tls.enabled`, cert/key/CA in config) |
| **Queue limits** | Max 10,000 jobs (configurable). Rejects when full |
| **CEL matchmaking** | `ad.gpu_count >= 2 && ad.gpu_vendor == "nvidia"` |
| **Backfill scheduling** | Lower-priority jobs fill idle nodes while big jobs wait |
| **Fairshare** | Heavy users get priority penalties (auto-decaying) |
| **Walltime** | `--walltime=3600` kills jobs exceeding the limit |
| **Worker loss detection** | Gossip detects node departure → running jobs marked FAILED |
| **Gang timeout** | Distributed jobs fail after 5 min if not enough nodes |

## CLI Reference

```
tasch setup                          # Interactive setup wizard
tasch start                          # Start based on config
tasch stop                           # Graceful drain + shutdown

tasch nodes                          # Cluster nodes + GPU info

tasch jobs                           # List all jobs
tasch jobs submit <expr> <cmd>       # Submit single job
tasch jobs train <cmd>               # Distributed training
tasch jobs cancel <id>               # Cancel job
tasch jobs status <id>               # Job detail + output
tasch jobs logs <id> [--follow]      # Stream logs
tasch jobs failed                    # Dead letter queue
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
| `ad.gpu_vendor` | string | `"nvidia"` or `"amd"` |
| `ad.gpu_memory_mb` | list | `ad.gpu_memory_mb[0] >= 40000` |
| `ad.cuda_version` | string | |
| `ad.rocm_version` | string | |
| `ad.cpu_cores` | int | `ad.cpu_cores >= 8` |
| `ad.total_memory_mb` | int | |
| `ad.os` | string | `ad.os == "linux"` |
| `ad.architecture` | string | |
| `ad.host_type` | string | `vm_or_baremetal` or `container` |

## Config File

`~/.tasch/config.yaml`:
```yaml
role: both
node_name: gpu-server-10
master_addr: 10.0.1.10
max_queue_size: 10000
max_retries: 3
drain_timeout: 60
ports:
  gossip: 7946
  grpc: 50051
  zmq: 5555
  metrics: 9090
tls:
  enabled: false
  cert_file: ""
  key_file: ""
  ca_file: ""
```

Override with `--config <path>` or `TASCH_*` env vars.

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/health` | 9090 | Liveness probe (always 200) |
| `/ready` | 9090 | Readiness (members, queue depth, drain status) |
| `/metrics` | 9090 | Prometheus metrics |

## Project Structure

```
tasch/
├── cmd/tasch/main.go           # Single binary entry point
├── internal/
│   ├── config/                 # Config YAML + TLS + validation
│   ├── setup/                  # Interactive setup wizard
│   ├── daemon/
│   │   ├── master.go           # Scheduler, dispatch, gang-sched, retry, circuit breaker, GPU tracking
│   │   ├── worker.go           # Executor, ZMQ reconnect, gRPC keepalive
│   │   ├── metrics.go          # Prometheus metrics (9 metrics)
│   │   └── stop.go             # PID-based stop with SIGKILL fallback
│   ├── store/store.go          # BoltDB persistence (jobs, groups, fairshare, dead letters)
│   └── cli/                    # CLI commands (jobs, nodes, failed)
├── pkg/
│   ├── profiler/               # NVIDIA + AMD GPU detection
│   ├── scheduler/              # Min-heap queue, Job/JobGroup, fairshare, hooks
│   ├── matchmaker/             # Google CEL evaluator
│   ├── discovery/              # Memberlist gossip + EventHooks
│   └── messaging/              # ZeroMQ PUB/SUB
├── api/v1/scheduler.proto      # 8 gRPC RPCs
├── Makefile
└── test.sh                     # 11-scenario integration test
```

## Documentation

| Document | Description |
|----------|-------------|
| [Setup Guide](docs/SETUP.md) | Installation, deployment, TLS, systemd |
| [User Guide](docs/USER_GUIDE.md) | All commands, CEL syntax, training workflows |
| [API Reference](docs/API.md) | 8 gRPC RPCs, Protobuf messages, ZMQ protocol |
| [Architecture](docs/ARCHITECTURE.md) | System design, persistence, circuit breaker, GPU tracking |
| [Development](docs/DEVELOPMENT.md) | Building, testing, code patterns |
| [Changelog](CHANGELOG.md) | Version history |

## License

MIT — see [LICENSE](LICENSE).
