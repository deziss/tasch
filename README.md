# Tasch — Distributed Task Scheduler

**Tasch** is a distributed task scheduler with multi-GPU and cross-platform support. One binary, one setup command, and your cluster is ready.

> **Status: alpha.** Authentication and gossip encryption exist but are **off by default**, and
> jobs are resource-limited rather than isolated — they share the service account's filesystem
> and network. Set `auth.enabled` and `gossip.encryption_key`, and read
> [SECURITY.md](SECURITY.md), before running this anywhere but a trusted network.

```
┌──────────────────┐        ┌──────────────────┐
│  Server 10       │◄──────►│  Server 20       │
│  (master+worker) │ gossip │  (worker)        │
│  Linux/amd64     │ + gRPC │  Linux/arm64     │
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
# Produces: dist/bin/tasch-linux-amd64, dist/bin/tasch-linux-arm64,
#           dist/bin/tasch-windows-amd64.exe, dist/bin/tasch-windows-arm64.exe,
#           dist/bin/tasch-darwin-amd64, dist/bin/tasch-darwin-arm64
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
| **Cordon / drain** | Take a node out of rotation for maintenance. Cordons survive a master restart, and `tasch nodes` shows why a node is not taking work |
| **Multi-resource tracking** | Prevents GPU, CPU, and memory oversubscription across concurrent dispatches |
| **Enforced limits** | On Linux, `--cpus` and `--memory` become real cgroup v2 limits, not just bookkeeping. Every job gets a process cap, so a fork bomb cannot take the worker down. Requires the systemd unit's `Delegate=` |
| **Dispatch handshake** | Worker acknowledges job start; master re-queues unacknowledged jobs after 10s |
| **Fencing tokens** | Every dispatch carries an attempt number; results from a superseded dispatch are discarded, so a job cannot be double-counted or release another node's resources |
| **Graceful drain** | `tasch stop` → stop accepting → wait for running jobs → shutdown |
| **Dispatch auto-reconnect** | Worker re-establishes its dispatch stream with exponential backoff (1s–30s) |
| **gRPC keepalive** | 30s heartbeat, 10s timeout, survives transient disconnects |
| **Authentication** | Token-based principals with `user` / `admin` / `worker` roles (`auth.enabled`). Job ownership is enforced on cancel, status, and logs |
| **TLS / mTLS** | Server TLS for gRPC; setting `tls.ca_file` on the master additionally requires and verifies client certificates |
| **Encrypted gossip** | `gossip.encryption_key` authenticates and encrypts membership traffic, so arbitrary hosts cannot join |
| **Per-node dispatch** | Each worker receives only its own jobs, over the authenticated gRPC connection — commands and env vars are never broadcast |
| **Queue limits** | Max 10,000 jobs (configurable). Rejects when full |
| **CEL matchmaking** | `ad.gpu_count >= 2 && ad.gpu_vendor == "nvidia" && ad.os == "linux"` |
| **Backfill scheduling** | Lower-priority jobs fill idle nodes while big jobs wait |
| **Fairshare** | Heavy users get priority penalties (auto-decaying) |
| **Walltime** | `--walltime=3600` kills jobs exceeding the limit |
| **Worker loss detection** | Gossip detects node departure → running jobs marked FAILED |
| **Resource reconciliation** | GPU/CPU/memory accounting is rebuilt from the live job set every 60s, so a missed release cannot permanently shrink a node |
| **GPU device pinning** | Concurrent jobs on one node receive distinct physical device indices |
| **Gang timeout** | Distributed jobs fail after 5 min if not enough nodes |
| **Async DB writes** | Decoupled BoltDB writes via buffered channel — scheduling never blocks on disk |

## CLI Reference

```
tasch setup                          # Interactive setup wizard
tasch start                          # Start based on config
tasch stop                           # Graceful drain + shutdown

tasch nodes                          # Cluster nodes + GPU/OS/arch + scheduling state
tasch nodes cordon <node>            # Stop scheduling new jobs onto a node
tasch nodes uncordon <node>          # Return a node to service
tasch nodes drain <node>             # Cordon and cancel the jobs running on it

tasch jobs                           # List all jobs
tasch jobs submit <expr> <cmd>       # Submit single job
tasch jobs train <cmd>               # Distributed training
tasch jobs cancel <id>               # Cancel job
tasch jobs status <id>               # Job detail + output
tasch jobs logs <id> [--follow]      # Stream logs
tasch jobs failed                    # Dead letter queue

tasch config validate                # Check config, warn on insecure settings
tasch version                        # Build version, commit, toolchain
```

### Submit Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--priority` | `-p` | 10 | Lower = higher priority |
| `--user` | `-u` | anonymous | Fairshare tracking |
| `--walltime` | `-w` | 0 | Max seconds (0 = unlimited) |
| `--gpus` | | 0 | GPUs required |
| `--cpus` | | 0 | CPU cores to reserve (0 = infer from the CEL expression) |
| `--memory` | | 0 | Memory to reserve in MB (0 = infer) |
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
  metrics: 9090
metrics_bind: 0.0.0.0        # set 127.0.0.1 to keep metrics off the network
max_concurrent_jobs: 0       # per worker; 0 = unlimited
max_output_bytes: 3145728    # captured stdout+stderr cap per job
max_pids_per_job: 4096       # process cap per job (fork-bomb guard)
tls:
  enabled: false
  cert_file: ""
  key_file: ""
  ca_file: ""                # on the master, also enables mutual TLS
auth:
  enabled: false             # set true for anything but a fully trusted network
  principals:
    - name: alice
      token: "..."           # generate with: openssl rand -hex 32
      role: user             # user | admin | worker
gossip:
  encryption_key: ""         # base64, 16/24/32 bytes: openssl rand -base64 32
  profile: lan               # lan | wan | local
client_token: ""             # this node's token; prefer TASCH_AUTH_TOKEN
```

Check it before starting:
```bash
tasch config validate
```

Override with `--config <path>` or `TASCH_*` env vars.

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/health` | 9090 | Liveness — 503 if the scheduling loop has stalled |
| `/ready` | 9090 | Readiness (members, queue depth, drain status) |
| `/metrics` | 9090 | Prometheus metrics |

## Project Structure

```
tasch/
├── cmd/tasch/main.go           # Single binary entry point
├── internal/
│   ├── config/                 # Config YAML + TLS + env overrides
│   ├── setup/                  # Interactive setup wizard
│   ├── daemon/
│   │   ├── master.go           # Scheduler, dispatch, gang-sched, retry, circuit breaker, multi-resource tracking
│   │   ├── worker.go           # Executor, dispatch stream reconnect, gRPC keepalive, start acknowledgement
│   │   ├── dispatch_bus.go     # Per-node dispatch routing
│   │   ├── capped_buffer.go    # Bounded job output capture
│   │   ├── exec_unix.go        # Unix shell command builder (sh -c)
│   │   ├── exec_windows.go     # Windows cmd.exe command builder (hidden window)
│   │   ├── metrics.go          # Prometheus metrics
│   │   └── stop.go             # PID-based stop with SIGKILL fallback
│   ├── store/store.go          # BoltDB persistence (jobs, groups, fairshare, dead letters)
│   ├── auth/                   # gRPC token authentication and job ownership
│   ├── version/                # Build version stamped via -ldflags
│   └── cli/                    # CLI commands (jobs, nodes, failed)
├── pkg/
│   ├── profiler/               # Cross-platform GPU detection
│   │   ├── profiler.go         # Shared ClassAd struct, profiling, gossip size fitting
│   │   ├── profiler_linux.go   # NVIDIA + AMD + Jetson Tegra detection
│   │   ├── profiler_windows.go # WMI/PowerShell detection (NVIDIA, AMD, Intel, Qualcomm)
│   │   ├── profiler_darwin.go  # Apple Metal + Unified Memory detection
│   │   └── profiler_fallback.go# Stub for unsupported OS
│   ├── scheduler/              # Min-heap queue, Job/JobGroup, fairshare, hooks
│   ├── matchmaker/             # Google CEL evaluator
│   └── discovery/              # Memberlist gossip + EventHooks (optional encryption)
├── api/v1/scheduler.proto      # 9 gRPC RPCs
├── build.sh                    # Cross-platform build script (6 targets)
├── Makefile
└── test.sh                     # 13-scenario integration test
```

## Documentation

| Document | Description |
|----------|-------------|
| [Setup Guide](docs/SETUP.md) | Installation, deployment, TLS, systemd, cross-platform |
| [User Guide](docs/USER_GUIDE.md) | All commands, CEL syntax, training workflows |
| [API Reference](docs/API.md) | gRPC RPCs and Protobuf messages |
| [Security](SECURITY.md) | Threat model, what is and is not protected, safe deployment |
| [Architecture](docs/ARCHITECTURE.md) | System design, persistence, circuit breaker, GPU tracking |
| [Development](docs/DEVELOPMENT.md) | Building, testing, cross-compilation, code patterns |
| [Changelog](CHANGELOG.md) | Version history |

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).

Tasch is network-facing server software. Under the AGPL, if you run a modified version and let
others interact with it over a network, you must offer those users the corresponding source of
your modified version.
