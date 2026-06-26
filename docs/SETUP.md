# Setup & Installation Guide

## Requirements

- **OS:** Linux, macOS, or Windows (native — no WSL required)
- **Arch:** amd64 or arm64 (32-bit not supported)
- **Go:** 1.21+ (only for building from source)
- **GPU (optional):**
  - NVIDIA — `nvidia-smi` in PATH
  - AMD — `rocm-smi` in PATH
  - Intel — detected via WMI (Windows) or sysfs (Linux)
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
# Outputs: dist/tasch-linux-amd64, dist/tasch-linux-arm64,
#          dist/tasch-windows-amd64.exe, dist/tasch-windows-arm64.exe,
#          dist/tasch-darwin-amd64, dist/tasch-darwin-arm64
```

### From Package (.deb / .rpm)
```bash
# Debian/Ubuntu
sudo dpkg -i tasch_0.1.0_amd64.deb

# RHEL/CentOS/Fedora
sudo rpm -i tasch-0.1.0-1.x86_64.rpm
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
  zmq: 5555
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
tasch stop     # graceful drain → SIGTERM → 15s wait → SIGKILL if stuck
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

```ini
[Unit]
Description=Tasch Scheduler
After=network.target

[Service]
Type=simple
User=tasch
ExecStart=/usr/local/bin/tasch start
ExecStop=/usr/local/bin/tasch stop
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Network Ports

| Port | Protocol | Service |
|------|----------|---------|
| 7946 | UDP + TCP | Gossip (cluster discovery) |
| 5555 | TCP | ZMQ (job dispatch) |
| 50051 | TCP | gRPC (CLI + result reporting) |
| 9090 | TCP | Health checks + Prometheus metrics + dispatch handshake |

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

## TLS Configuration

Enable mTLS for gRPC communication:

```yaml
tls:
  enabled: true
  cert_file: /etc/tasch/server.pem     # Master: server cert. Worker: ignored.
  key_file: /etc/tasch/server-key.pem  # Master: server key.
  ca_file: /etc/tasch/ca.pem           # Worker: CA cert to verify master.
```

## Persistence

Jobs and state are persisted to `~/.tasch/tasch.db` (BoltDB). On master restart:
- QUEUED jobs are re-enqueued
- RUNNING jobs are marked FAILED ("master restarted")
- Fairshare usage data is restored

## Health Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/health` | 9090 | Liveness — always 200 |
| `/ready` | 9090 | Readiness — 200 with member count, queue depth, drain status |
| `/metrics` | 9090 | Prometheus metrics |
| `/acknowledge_start` | 9090 | Internal: worker → master job start handshake (POST) |

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
| `TASCH_ZMQ_PORT` | `ports.zmq` |
| `TASCH_METRICS_PORT` | `ports.metrics` |
| `TASCH_ADVERTISE_ADDR` | Worker's advertised IP |

## Troubleshooting

**"Tasch may already be running"** — A PID file exists. Run `tasch stop` or delete `~/.tasch/tasch.pid`.

**Workers can't join** — Check firewall: ports 7946 (UDP+TCP), 5555, 50051. Verify `master_addr` in worker config.

**GPUs not detected (NVIDIA/AMD)** — Verify `nvidia-smi` or `rocm-smi` is in PATH and returns output.

**GPUs not detected (Intel/Windows)** — Ensure PowerShell is accessible and WMI is not blocked by policy.

**Jobs stay QUEUED** — Run `tasch nodes` to check: correct GPU vendor, OS, arch, and available resources (CPUs/memory) match the CEL expression.

**Distributed jobs stuck** — Gang scheduling needs ALL N nodes simultaneously. Check `tasch nodes`.

**Jobs keep failing** — Check `tasch jobs failed` for dead letter queue. Worker may be circuit-broken (3 consecutive failures = 5 min block). Check worker logs.

**Queue full** — Max 10,000 jobs by default. Increase `max_queue_size` in config or wait for jobs to complete.

**Unacknowledged dispatch warning** — If a worker receives a job but doesn't acknowledge within 10s, the master re-queues it. Check worker connectivity to the master's metrics port.
