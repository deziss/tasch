# Tasch — Distributed Task Scheduler

**Tasch** is a production-grade, distributed task scheduler with multi-GPU and cross-platform support. One binary, one setup command, and your cluster is ready.

```
┌──────────────────┐        ┌──────────────────┐
│  Server 10       │◄──────►│  Server 20       │
│  (master+worker) │ gossip │  (worker)        │
│  Linux/amd64     │ + ZMQ  │  Linux/arm64     │
│  2× NVIDIA A100  │        │  4× AMD RX7900   │
└──────────────────┘        └──────────────────┘

┌──────────────────┐        ┌──────────────────┐
│  Mac Studio      │        │  Windows VM      │
│  Apple M2 Ultra  │        │  Windows/arm64   │
│  Metal GPU       │        │  Intel Arc GPU   │
└──────────────────┘        └──────────────────┘
```

## Install

### From Source
```bash
git clone https://github.com/deziss/tasch.git
cd tasch && make build
sudo cp bin/tasch /usr/local/bin/
```

### Cross-Platform Builds
Use the included build script to compile for all supported platforms:
```bash
chmod +x build.sh && ./build.sh
# Produces: dist/tasch-linux-amd64, dist/tasch-linux-arm64,
#           dist/tasch-windows-amd64.exe, dist/tasch-windows-arm64.exe,
#           dist/tasch-darwin-amd64, dist/tasch-darwin-arm64
```

### From Package (.deb / .rpm)
Download the latest release from GitHub and install via your package manager:
```bash
# Debian/Ubuntu
sudo dpkg -i tasch_0.1.0_amd64.deb

# RHEL/CentOS/Fedora
sudo rpm -i tasch-0.1.0-1.x86_64.rpm
```
After installation, the binary is at `/usr/bin/tasch`, and the configuration is at `/etc/tasch/config.yaml`.

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
tasch nodes                    # cluster status + GPUs + OS + arch
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
| **Cross-platform** | Linux, Windows, macOS · amd64 and arm64 (64-bit only) |
| **Multi-GPU vendor** | NVIDIA · AMD · Intel · Apple Metal · Jetson Tegra |
| **GPU env binding** | Auto-injects `CUDA_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `ONEAPI_DEVICE_SELECTOR`, `METAL_DEVICE_INDEX` per vendor |
| **Distributed training** | `tasch jobs train` — gang scheduling + auto DDP env vars (`RANK`, `WORLD_SIZE`, `MASTER_ADDR`) |
| **BoltDB persistence** | Jobs, groups, fairshare survive master restart (`~/.tasch/tasch.db`) |
| **Job retry** | Auto-retry failed jobs (default 3×) with exponential backoff. Dead letter queue for exhausted retries |
| **Health checks** | `/health` (liveness) + `/ready` (readiness) endpoints on metrics port |
| **Prometheus metrics** | 10 metrics: queue depth, running jobs, dispatch duration, job duration, walltime kills, worker loss |
| **Circuit breaker** | 3 consecutive failures → worker blocked 5 minutes |
| **Multi-resource tracking** | Prevents GPU, CPU, and memory oversubscription across concurrent dispatches |
| **ZMQ dispatch handshake** | Worker sends HTTP acknowledgement on job start; master re-queues unacknowledged jobs after 10s |
| **Graceful drain** | `tasch stop` → stop accepting → wait for running jobs → shutdown |
| **ZMQ auto-reconnect** | Worker reconnects to master with exponential backoff (1s–30s) |
| **gRPC keepalive** | 30s heartbeat, 10s timeout, survives transient disconnects |
| **TLS support** | Optional mTLS for gRPC (`tls.enabled`, cert/key/CA in config) |
| **Queue limits** | Max 10,000 jobs (configurable). Rejects when full |
| **CEL matchmaking** | `ad.gpu_count >= 2 && ad.gpu_vendor == "nvidia" && ad.os == "linux"` |
| **Backfill scheduling** | Lower-priority jobs fill idle nodes while big jobs wait |
| **Fairshare** | Heavy users get priority penalties (auto-decaying) |
| **Walltime** | `--walltime=3600` kills jobs exceeding the limit |
| **Worker loss detection** | Gossip detects node departure → running jobs marked FAILED |
| **Gang timeout** | Distributed jobs fail after 5 min if not enough nodes |
| **Async DB writes** | Decoupled BoltDB writes via buffered channel — scheduling never blocks on disk |

## CLI Reference

```
tasch setup                          # Interactive setup wizard
tasch start                          # Start based on config
tasch stop                           # Graceful drain + shutdown

tasch nodes                          # Cluster nodes + GPU/OS/arch info

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
| `ad.gpu_vendor` | string | `"nvidia"`, `"amd"`, `"intel"`, `"apple"` |
| `ad.gpu_memory_mb` | list | `ad.gpu_memory_mb[0] >= 40000` |
| `ad.cuda_version` | string | NVIDIA CUDA version |
| `ad.rocm_version` | string | AMD ROCm version |
| `ad.cpu_cores` | int | `ad.cpu_cores >= 8` |
| `ad.total_memory_mb` | int | `ad.total_memory_mb >= 16000` |
| `ad.os` | string | `"linux"`, `"windows"`, `"darwin"` |
| `ad.architecture` | string | `"amd64"`, `"arm64"` |
| `ad.host_type` | string | `"vm_or_baremetal"` or `"container"` |

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
| `/acknowledge_start` | 9090 | Internal: worker → master job handshake (POST) |

## Project Structure

```
tasch/
├── cmd/tasch/main.go           # Single binary entry point
├── internal/
│   ├── config/                 # Config YAML + TLS + env overrides
│   ├── setup/                  # Interactive setup wizard
│   ├── daemon/
│   │   ├── master.go           # Scheduler, dispatch, gang-sched, retry, circuit breaker, multi-resource tracking
│   │   ├── worker.go           # Executor, ZMQ reconnect, gRPC keepalive, start acknowledgement
│   │   ├── exec_unix.go        # Unix shell command builder (sh -c)
│   │   ├── exec_windows.go     # Windows cmd.exe command builder (hidden window)
│   │   ├── metrics.go          # Prometheus metrics
│   │   └── stop.go             # PID-based stop with SIGKILL fallback
│   ├── store/store.go          # BoltDB persistence (jobs, groups, fairshare, dead letters)
│   └── cli/                    # CLI commands (jobs, nodes, failed)
├── pkg/
│   ├── profiler/               # Cross-platform GPU detection
│   │   ├── profiler.go         # Shared Host struct and profiling entry point
│   │   ├── profiler_linux.go   # NVIDIA + AMD + Jetson Tegra detection
│   │   ├── profiler_windows.go # WMI/PowerShell detection (NVIDIA, AMD, Intel, Qualcomm)
│   │   ├── profiler_darwin.go  # Apple Metal + Unified Memory detection
│   │   └── profiler_fallback.go# Stub for unsupported OS
│   ├── scheduler/              # Min-heap queue, Job/JobGroup, fairshare, hooks
│   ├── matchmaker/             # Google CEL evaluator
│   ├── discovery/              # Memberlist gossip + EventHooks
│   └── messaging/              # ZeroMQ PUB/SUB
├── api/v1/scheduler.proto      # 8 gRPC RPCs
├── build.sh                    # Cross-platform build script (6 targets)
├── Makefile
└── test.sh                     # 13-scenario integration test
```

## Documentation

| Document | Description |
|----------|-------------|
| [Setup Guide](docs/SETUP.md) | Installation, deployment, TLS, systemd, cross-platform |
| [User Guide](docs/USER_GUIDE.md) | All commands, CEL syntax, training workflows |
| [API Reference](docs/API.md) | 8 gRPC RPCs, Protobuf messages, ZMQ protocol |
| [Architecture](docs/ARCHITECTURE.md) | System design, persistence, circuit breaker, GPU tracking |
| [Development](docs/DEVELOPMENT.md) | Building, testing, cross-compilation, code patterns |
| [Changelog](CHANGELOG.md) | Version history |

## License

MIT — see [LICENSE](LICENSE).
