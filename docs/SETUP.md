# Setup & Installation Guide

## Requirements

- **OS:** Linux or macOS (Windows via WSL2)
- **Go:** 1.21+ (only for building from source)
- **GPU:** Optional — NVIDIA (nvidia-smi) or AMD (rocm-smi)

## Install

```bash
git clone https://github.com/deziss/tasch.git
cd tasch && make build
sudo cp bin/tasch /usr/local/bin/
```

Verify:
```bash
tasch --help
```

## Setup

### Interactive (recommended)

```bash
tasch setup
```

The wizard asks:
1. **Role** — Master, Worker, or Both
2. **Node name** — defaults to hostname
3. **Master address** — auto-detected for master/both; required for worker
4. **Ports** — gossip (7946), gRPC (50051), ZMQ (5555)

Then shows detected hardware (CPU, RAM, GPUs) and writes `~/.tasch/config.yaml`.

### Non-interactive (scripting)

```bash
tasch setup --non-interactive --role=worker --node-name=gpu-20 --master-addr=10.0.1.10
```

### Config file

`~/.tasch/config.yaml`:
```yaml
role: both
node_name: gpu-server-10
master_addr: 10.0.1.10
ports:
  gossip: 7946
  grpc: 50051
  zmq: 5555
```

Override location: `tasch --config /path/to/config.yaml <command>`

## Start & Stop

```bash
tasch start    # starts master/worker/both based on config
tasch stop     # sends SIGTERM to running instance
```

## Deployment Examples

### Single machine (development)

```bash
tasch setup         # select "Both"
tasch start
tasch nodes         # verify
tasch jobs submit "ad.cpu_cores >= 1" "echo hello"
```

### Two servers (GPU training)

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
tasch nodes         # shows both servers
tasch jobs submit --gpus=1 "ad.gpu_count >= 1" "python train.py"
tasch jobs train --nodes=2 "torchrun ... train.py"
```

### Systemd service

```ini
# /etc/systemd/system/tasch.service
[Unit]
Description=Tasch Scheduler
After=network.target

[Service]
Type=simple
User=tasch
ExecStart=/usr/local/bin/tasch start
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now tasch
```

## Network Ports

| Port | Protocol | Service |
|------|----------|---------|
| 7946 | UDP + TCP | Gossip (cluster discovery) |
| 5555 | TCP | ZMQ (job dispatch) |
| 50051 | TCP | gRPC (CLI + result reporting) |

Workers must reach the master on all three ports.

## GPU Support

GPUs are auto-detected at startup. No configuration needed.

**NVIDIA:** Requires `nvidia-smi` in PATH
**AMD:** Requires `rocm-smi` in PATH

Verify detection:
```bash
tasch start &
tasch nodes    # should show GPU count, models, memory
```

## Environment Variable Overrides

Config values can be overridden by env vars:

| Variable | Overrides |
|----------|-----------|
| `TASCH_MASTER_ADDR` | `master_addr` |
| `TASCH_GOSSIP_PORT` | `ports.gossip` |
| `TASCH_GRPC_PORT` | `ports.grpc` |
| `TASCH_ZMQ_PORT` | `ports.zmq` |
| `TASCH_ADVERTISE_ADDR` | Worker's advertised IP |

## Troubleshooting

**"No config found"** — Run `tasch setup` first.

**Workers can't join** — Check firewall for ports 7946 (UDP+TCP), 5555, 50051. Verify `master_addr` in worker config.

**GPUs not detected** — Verify `nvidia-smi` or `rocm-smi` is in PATH on the worker machine.

**Jobs stay QUEUED** — Run `tasch nodes` to check if workers are visible. Check CEL expression matches available hardware.

**Distributed jobs stuck** — Gang scheduling requires ALL N nodes available simultaneously. Verify enough matching nodes with `tasch nodes`.
