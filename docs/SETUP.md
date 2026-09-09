# Setup & Installation Guide

## Requirements

- **OS:** Linux, macOS, or Windows (native — no WSL required)
- **Arch:** amd64 or arm64 (32-bit not supported)
- **Go:** 1.25+ (only for building from source)
- **GPU (optional):**
  - NVIDIA — `nvidia-smi` in PATH
  - AMD — `rocm-smi` in PATH
  - Intel — detected via WMI on Windows only (there is no Linux Intel probe)
  - Apple — detected automatically on macOS (Metal)
  - Jetson/Tegra — detected via sysfs on Linux/arm64

## Install

### From Source (any platform)
```bash
git clone https://github.com/deziss/tasch.git
cd tasch && make build
sudo cp bin/tasch /usr/local/bin/
```

### Cross-Platform Build
```bash
chmod +x build.sh && ./build.sh
# Outputs: dist/bin/tasch-linux-amd64, dist/bin/tasch-linux-arm64,
#          dist/bin/tasch-windows-amd64.exe, dist/bin/tasch-windows-arm64.exe,
#          dist/bin/tasch-darwin-amd64, dist/bin/tasch-darwin-arm64
```

### From Package (.deb / .rpm)
```bash
# Debian/Ubuntu
sudo dpkg -i tasch_0.9.0_amd64.deb

# RHEL/CentOS/Fedora
sudo rpm -i tasch-0.9.0-1.x86_64.rpm
```
Binary at `/usr/bin/tasch`, config at `/etc/tasch/config.yaml`.

## Setup

```bash
tasch setup    # interactive wizard
```

Asks: role (master/worker/both), node name, master address, ports. Detects and displays hardware including GPU vendor and model.

Non-interactive:
```bash
tasch setup --non-interactive --role=worker --node-name=gpu-20 --master-addr=10.0.1.10
```

## Config File

`~/.tasch/config.yaml`:
```yaml
role: both
node_name: gpu-server-10
master_addr: 10.0.1.10
max_queue_size: 10000       # 0 = unlimited
max_retries: 3              # auto-retry failed jobs
drain_timeout: 60           # seconds to wait during graceful shutdown
ports:
  gossip: 7946
  grpc: 50051
  metrics: 9090
tls:
  enabled: false
  cert_file: /path/to/cert.pem
  key_file: /path/to/key.pem
  ca_file: /path/to/ca.pem
```

## Start & Stop

```bash
tasch start    # starts master/worker/both based on config
tasch stop     # graceful drain → SIGTERM → waits drain_timeout+15s → SIGKILL if stuck
```

## Deployment Examples

### Single machine
```bash
tasch setup         # select "Both"
tasch start
tasch nodes
```

### Two servers
**Server 10 (master + worker):**
```bash
tasch setup         # select "Both"
tasch start
```

**Server 20 (worker):**
```bash
tasch setup         # select "Worker", enter server-10 IP
tasch start
```

**From either machine:**
```bash
tasch nodes                                          # shows both servers + GPUs + OS + arch
tasch jobs submit --gpus=1 "ad.gpu_count >= 1" "python train.py"
tasch jobs train --nodes=2 "torchrun ... train.py"
```

### Mixed-OS cluster
Tasch workers can run on different operating systems simultaneously:
```bash
# Linux node (amd64) — NVIDIA GPU
tasch setup --non-interactive --role=worker --node-name=linux-gpu --master-addr=10.0.1.10

# macOS node (arm64) — Apple Metal GPU
tasch setup --non-interactive --role=worker --node-name=mac-m2 --master-addr=10.0.1.10

# Windows node (amd64) — Intel GPU
tasch setup --non-interactive --role=worker --node-name=win-intel --master-addr=10.0.1.10
```

Submit OS-targeted jobs:
```bash
tasch jobs submit "ad.os == 'linux' && ad.gpu_vendor == 'nvidia'" "python train.py"
tasch jobs submit "ad.os == 'darwin' && ad.gpu_vendor == 'apple'" "./my_metal_app"
tasch jobs submit "ad.os == 'windows' && ad.gpu_vendor == 'intel'" "my_oneapi_app.exe"
```

### Systemd service

Use the unit shipped in the `.deb`/`.rpm`, or copy `packaging/tasch.service` from the repository.
Do not hand-write a minimal one: the packaged unit carries the sandboxing settings and, more
importantly, `Delegate=cpu memory pids`, without which per-job resource limits silently do not
apply.

```bash
sudo cp packaging/tasch.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tasch
```

## Network Ports

| Port | Protocol | Service |
|------|----------|---------|
| 7946 | UDP + TCP | Gossip (cluster discovery) |
| 50051 | TCP | gRPC (CLI + result reporting) |
| 9090 | TCP | Health checks + Prometheus metrics |
| 8300 | TCP | Raft replication between masters (HA only, configurable) |

## GPU Detection

| Platform | GPU Vendors Detected | Detection Method |
|----------|---------------------|-----------------|
| Linux (amd64/arm64) | NVIDIA, AMD, Jetson Tegra | `nvidia-smi`, `rocm-smi`, sysfs |
| Windows (amd64/arm64) | NVIDIA, AMD, Intel, Qualcomm Adreno | WMI `Win32_VideoController`, `nvidia-smi.exe` |
| macOS (amd64/arm64) | Apple Metal, AMD eGPU | `system_profiler SPDisplaysDataType`, `sysctl` |

GPU vendor env vars auto-injected at dispatch:

| Vendor | Env Var |
|--------|---------|
| NVIDIA | `CUDA_VISIBLE_DEVICES` |
| AMD | `HIP_VISIBLE_DEVICES` |
| Intel | `ONEAPI_DEVICE_SELECTOR`, `SYCL_DEVICE_FILTER` |
| Apple | `METAL_DEVICE_INDEX` |

## Authentication

Off by default. Turn it on for any cluster that is not fully trusted — without it, anyone who
can reach the gRPC port can run arbitrary commands on every worker.

```yaml
auth:
  enabled: true
  principals:
    - name: alice
      token: "<openssl rand -hex 32>"
      role: user       # submit jobs; may only act on its own jobs
    - name: root
      token: "<openssl rand -hex 32>"
      role: admin      # may act on any job
    - name: gpu-node-1
      token: "<openssl rand -hex 32>"
      role: worker     # may only report results and receive dispatches
```

Each node and CLI user presents its own token via `client_token` in the config or, preferably,
`TASCH_AUTH_TOKEN` in the environment. To keep tokens out of the config file entirely, put the
principal list in its own 0600 file and point `auth.principals_file` at it.

Job ownership follows the authenticated principal: `--user` is a label only and is ignored when
auth is enabled.

## Gossip Encryption

Also off by default. Without a key, any host that can reach port 7946 can join the cluster and
advertise fabricated resources to attract jobs.

```yaml
gossip:
  encryption_key: "<openssl rand -base64 32>"   # identical on every node
  profile: lan                                   # lan | wan | local
```

Use `wan` for nodes across a high-latency link. The old `local` profile is tuned for loopback
and will produce false node-failure detections on a real network — each of which fails every
job on the node it wrongly declared dead.

## TLS Configuration

Enables TLS for gRPC, which carries job submissions, results, and dispatches.

```yaml
tls:
  enabled: true
  cert_file: /etc/tasch/server.pem     # Master: server cert. Worker: ignored.
  key_file: /etc/tasch/server-key.pem  # Master: server key.
  ca_file: /etc/tasch/ca.pem           # Worker: CA cert to verify master.
```

Setting `ca_file` **on the master** additionally turns on mutual TLS: client certificates are
required and verified. Workers and the CLI then present their own `cert_file`/`key_file`.

> **Not covered by TLS:** the metrics/health HTTP server on 9090. Bind it to localhost with
> `metrics_bind: 127.0.0.1` if it should not be reachable from the network. Gossip is protected
> separately by `gossip.encryption_key`.

## Resource Limits

On Linux, a job's `--cpus` and `--memory` reservations are enforced through a cgroup v2 subtree,
not merely tracked by the master. Exceeding the memory reservation kills the job and reports why.
Every job also gets a process cap, so a fork bomb cannot take the worker and its co-tenant jobs
down with it.

```yaml
max_pids_per_job: 4096   # 0 uses the built-in default
```

This needs the daemon to own a delegated cgroup. The packaged systemd unit sets
`Delegate=cpu memory pids`. If you run `tasch start` outside systemd, or inside a container with
no delegated subtree, the worker logs a warning at startup and runs jobs **without limits** —
look for that line rather than assuming limits apply.

GPUs are not covered: cgroup v2 has no GPU controller, so the injected `CUDA_VISIBLE_DEVICES`
remains advisory and a job can unset it.

## Persistence

Jobs and state are persisted to `~/.tasch/tasch.db` (BoltDB). On master restart:
- QUEUED jobs are re-enqueued
- RUNNING jobs are **kept running**. Workers report what they are executing when they reconnect,
  and the master adopts those jobs — preserving their dispatch attempt so the eventual result is
  still accepted — and re-books their CPU, memory, and GPUs. A job nobody claims within 90
  seconds is failed.
- Cordoned nodes stay cordoned
- Fairshare usage data is restored

Note that this only helps when the master and its workers are separate processes. In
`role: both`, killing the process takes the worker with it, so its jobs have nobody to claim
them.

The database holds every job's captured output and environment variables in plaintext. It is
mode 0600, but every job runs as the same service account that owns it — so treat any submitted
job as able to read every other job's stored secrets.

## Health Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/health` | 9090 | Liveness — 503 if the scheduling loop has stalled |
| `/ready` | 9090 | Readiness — 200 with member count, queue depth, drain status |
| `/metrics` | 9090 | Prometheus metrics |

```bash
curl http://localhost:9090/health
curl http://localhost:9090/ready
```

## Environment Variable Overrides

| Variable | Overrides |
|----------|-----------|
| `TASCH_MASTER_ADDR` | `master_addr` |
| `TASCH_GOSSIP_PORT` | `ports.gossip` |
| `TASCH_GRPC_PORT` | `ports.grpc` |
| `TASCH_METRICS_PORT` | `ports.metrics` |
| `TASCH_ADVERTISE_ADDR` | Worker's advertised IP |

## Troubleshooting

**"Tasch may already be running"** — A PID file exists. Run `tasch stop` or delete `~/.tasch/tasch.pid`.

**Workers can't join** — Check firewall: ports 7946 (UDP+TCP) and 50051. Verify `master_addr`, and that every node shares the same `gossip.encryption_key`.

**GPUs not detected (NVIDIA/AMD)** — Verify `nvidia-smi` or `rocm-smi` is in PATH and returns output.

**GPUs not detected (Intel/Windows)** — Ensure PowerShell is accessible and WMI is not blocked by policy.

**Jobs stay QUEUED** — Run `tasch nodes` to check: correct GPU vendor, OS, arch, and available resources (CPUs/memory) match the CEL expression.

**Distributed jobs stuck** — Gang scheduling needs ALL N nodes simultaneously. Check `tasch nodes`.

**Jobs keep failing** — Check `tasch jobs failed` for dead letter queue. Worker may be circuit-broken (3 consecutive failures = 5 min block). Check worker logs.

**Queue full** — Max 10,000 jobs by default. Increase `max_queue_size` in config or wait for jobs to complete.

**Unacknowledged dispatch warning** — If a worker receives a job but doesn't acknowledge within 10s, the master re-queues it. Check worker connectivity to the master's metrics port.

## High Availability

Off by default. A single master is a single point of failure: losing that host stops dispatch,
submission, and status until it comes back.

With HA enabled, several masters replicate scheduler state through Raft. One is the leader and
makes every scheduling decision; the others follow. If the leader is lost, the survivors elect a
new one that already holds the queue, the running jobs, the groups, the fairshare accounting and
the cordons — nothing that was acknowledged to a client is lost.

```yaml
ha:
  enabled: true
  node_id: master-1                    # stable across restarts
  bind_addr: 10.0.1.11:8300            # where peers reach this master
  data_dir: /var/lib/tasch/raft        # per-master, never shared
  bootstrap: true                      # first start only; ignored once state exists
  peers:
    - master-1=10.0.1.11:8300
    - master-2=10.0.1.12:8300
    - master-3=10.0.1.13:8300

# Every master and worker must share one gossip cluster, or a leader can only see the workers
# that happened to join it.
gossip:
  join: ["10.0.1.11:7946", "10.0.1.12:7946", "10.0.1.13:7946"]

# Clients and workers try each master until they find the leader.
master_addrs: ["10.0.1.11", "10.0.1.12", "10.0.1.13"]
```

**Use an odd number, three at minimum.** Raft needs a majority to make progress: three masters
tolerate one failure, five tolerate two. Two masters are worse than one — losing either leaves no
majority, so the cluster stops rather than continuing degraded. The config refuses an even count
for that reason.

`data_dir` must be local to each master and never shared; it holds that master's copy of the
replicated log.

Checking the cluster:

```bash
tasch cluster status
```

Notes and limits:

- Only the leader schedules. Followers serve reads — `tasch jobs`, `tasch nodes`, job status all
  work against any master — but writes are redirected.
- Failover takes a few seconds: the survivors must notice the leader is gone and hold an
  election. Workers keep executing during that window and are adopted by the new leader.
- Jobs already running are unaffected by a failover. Workers report what they are executing when
  they reconnect, and the new leader adopts it.
- HA replaces neither backups nor the drain-on-shutdown path. It protects against losing a
  master, not against a bad configuration replicated to all of them.
