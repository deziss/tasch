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

## HTTP API

A browser cannot speak gRPC — it has no way to produce the trailers and framing the protocol
needs — so nothing could talk to Tasch from a web page. The HTTP API fixes that, and gives
anything that would rather send JSON a way in.

```yaml
api:
  enabled: true
  bind: 127.0.0.1:8080
  cors_origins: ["http://localhost:5173"]   # exact origins; "*" is refused
  tls: false                                 # reuses the tls: block when true
```

It uses [Connect](https://connectrpc.com), which serves the Connect protocol (JSON over a plain
HTTP POST), gRPC-Web and gRPC from a single handler — all from the same service definition the
CLI and workers use. There is no second API to keep in step with the first, and no path with
weaker rules: every method calls the identical handler the gRPC server calls, so ownership
checks, quotas and leader redirects apply exactly as they do over gRPC.

```bash
curl -X POST http://127.0.0.1:8080/v1.SchedulerService/SubmitJob   -H 'Content-Type: application/json'   -H 'Authorization: Bearer <token>'   -d '{"celRequirement":"ad.gpu_count > 0","command":"./train.sh","gpusRequired":1}'
```

Three things to know before exposing it:

- **Authentication is the same token.** The `Authorization: Bearer` header replaces gRPC
  metadata; the principal list, roles and per-job ownership rules are unchanged.
- **`cors_origins` must be exact origins, and `"*"` is refused at config load.** This endpoint
  accepts job submissions, so a wildcard would let any page a user visits run commands on the
  cluster with a token they granted to something else.
- **It defaults to loopback.** Bound anywhere else without `tls: true` — or without a proxy
  terminating TLS — tokens and job commands travel in cleartext, and the master says so at
  startup.

`WatchDispatch` is deliberately not served here. Workers receive work over gRPC with a client
certificate; putting the dispatch channel on the browser-facing endpoint would widen a leaked
user token from "can submit jobs" to "can read every node's work, environment variables
included".

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

## GPU Detection

NVIDIA hardware is read from `nvidia-smi -q -x`, the driver's own structured output. That gives
per-device memory, live free memory and utilisation, MIG instances, and the driver and CUDA
versions, in a schema that either parses or reports an error.

Two consequences worth knowing:

**A partitioned card is reported as its MIG instances, not as one GPU.** An instance is what a
job can actually be given, and advertising the whole card's memory would promise capacity no
single instance has.

**Live GPU state is matchable.** The class ad carries `gpu_free_mb` and `gpu_util_pct`, so a
requirement can ask for a node with a genuinely idle accelerator:

```
tasch jobs submit 'ad.gpu_count > 0 && ad.gpu_free_mb > 20000 && ad.gpu_util_pct < 10' ./train.sh
```

Both are aggregates: `gpu_free_mb` is the *least* free memory across devices and `gpu_util_pct`
the *busiest* device's utilisation. That way a requirement written against them holds for at
least one device, and an idle-node requirement is not satisfied by an average across a card that
is pinned. The class ad is capped at 512 bytes by the gossip protocol, which is why these are
aggregates rather than per-device arrays.

> **Why not NVML directly?** NVML is a C library, so binding it means cgo — and cgo means giving
> up the property this project is built around: one static binary that cross-compiles to six
> targets from any machine. A cgo build needs a C toolchain per target and links against a
> driver library absent from most of them. `nvidia-smi -q -x` *is* NVML's output, serialized, so
> reading that gets the data without the trade.

AMD is read from `rocm-smi`, and Jetson boards from sysfs, since neither is covered by
`nvidia-smi`.

## Partitions, Accounts and Quotas

Without these, every job competes for every node and the only ordering is priority plus
fairshare. That works for one team. It stops working the moment two teams share a cluster: one
team's long CPU batch sits in front of the other's GPU work purely because it was submitted
first, and nothing caps how much of the cluster any one group can hold.

Both are off by default — no partitions and no accounts means the previous behaviour exactly.

### Partitions

A partition is a named pool of nodes with its own admission rules.

```yaml
partitions:
  - name: gpu
    node_selector: 'ad.gpu_count > 0'     # CEL, the same language job requirements use
    max_walltime_seconds: 86400
    default_walltime_seconds: 3600
    priority_boost: -5                    # negative sorts earlier
    allowed_accounts: [research]
  - name: cpu
    node_selector: 'ad.gpu_count == 0'
    default: true
    max_running_jobs: 200
```

Submit with `--partition gpu`, or leave it out to land in whichever partition is marked
`default`. A job that names a partition only ever runs on nodes matching its selector, checked
before the resource arithmetic — so a job never lands somewhere an operator excluded merely
because that node happened to have room.

`max_walltime_seconds` is the setting that earns a partition its keep: it is what stops one job
holding a scarce node indefinitely. A partition with a ceiling and no default gets the ceiling
as its default, because a ceiling that still admits jobs which never end is rarely what the
ceiling was for.

Node selectors are compiled when the master starts. An invalid one fails the start rather than
silently matching no node on every scheduling cycle forever.

### Accounts and quotas

An account is a group of users that quotas apply to, and accounts nest.

```yaml
accounts:
  - name: research
    max_gpus: 32
    max_running_jobs: 100
  - name: ml-team
    parent: research
    users: [alice, bob]
    max_gpus: 24
    max_queued_jobs: 500
  - name: vision-team
    parent: research
    users: [carol]
    max_gpus: 24
```

Nesting is what makes a quota a budget rather than a per-user cap. A job counts against its own
account *and* every account above it, so the two teams above can each be given 24 GPUs while
the department as a whole can never exceed 32. Whichever ceiling binds first is the one
reported, by name.

A job is charged to the submitter's first account, or to `--account NAME` — which must be one
they belong to. Without that check quotas would be advisory: anyone out of budget could simply
name a fuller account.

`max_queued_jobs` is checked at submit rather than at dispatch, so a runaway script is refused
at the door instead of after it has filled the queue for everyone else.

`tasch jobs status` says which limit is holding a job:

```
State:   QUEUED
Blocked: account research would exceed its quota of 32 GPUs (30 in use, 4 needed)
```

## Reservations

A cordon stops a node taking work now and stays until someone lifts it. That is the wrong shape
for planned work: cordon an hour before a maintenance window and you waste the hour; cordon at
the start of it and whatever is still running gets killed.

A reservation carries the window, so the scheduler drains the node itself.

```
tasch reserve create --nodes gpu-01,gpu-02 --start 2026-09-11T22:00:00Z --for 4h --reason firmware
tasch reserve create --nodes gpu-03 --for 12h --account research --reason "paper deadline"
tasch reserve list
tasch reserve delete <id>
```

With neither `--user` nor `--account`, nobody may run during the window: that is a maintenance
reservation. Naming users or accounts holds the nodes *for* them instead.

Two rules apply, and the second is the one a cordon cannot express:

1. While the window is open, only the people it was reserved for may run on those nodes.
2. **Before** it opens, a job may only start if it will have finished by then. A job with no
   walltime has no such guarantee, so it cannot start on a node with a reservation ahead of it.

That second rule is what empties the node on time with nothing killed. It also means a cluster
where nobody sets `--walltime` cannot drain: consider a partition `max_walltime_seconds` if you
plan to use reservations.

Reservations survive a master restart and, under HA, are replicated like everything else.
Closed windows are cleaned up automatically.

## Preemption

Priority decides the order jobs start in. On a full cluster it decides nothing — a job
submitted at the highest priority still waits behind whatever bulk work happens to be running,
which can be hours. Preemption makes priority bind by evicting lower-priority work.

```yaml
preemption:
  enabled: true
  priority_margin: 5        # how much higher-priority the incoming job must be
  min_runtime_seconds: 60   # never evict work younger than this
  max_victims_per_job: 4    # cap what one placement throws away

partitions:
  - name: bulk
    preemptible: true       # only jobs here can be evicted
```

It is off by default, because the cost is real: the evicted job's progress is discarded.

**Evicted jobs are requeued, not failed.** They go back at their own priority with their retry
budget intact, so preemption delays work rather than destroying it.

Preemptibility is a property of the partition, not of the job. Given the choice, every submitter
would mark their own work unpreemptible and the setting would mean nothing. A job in no
partition is never preempted, so turning this on cannot surprise jobs that predate it.

Three guards, each for a specific failure:

- **`priority_margin`** stops ordinary priority jitter — including fairshare adjustments —
  becoming eviction churn. It makes preemption a statement about class of work, not a tie-break.
- **`min_runtime_seconds`** protects work that has just started. Without it a loaded cluster can
  spend its time starting and killing the same jobs and make no progress at all.
- **`max_victims_per_job`** bounds what one placement discards. A job wanting a whole large node
  could otherwise evict everything on it at once.

Gang jobs are excluded on both sides: a rank cannot be evicted without failing every other rank,
and freeing room for one rank achieves nothing unless room appears for all of them at once.

The `tasch_preemptions_total` metric counts evictions. A number that climbs steadily usually
means the margin is too small or the cluster is simply short of capacity.

## Job Isolation

Resource limits say how *much* a job may use. They say nothing about what it can *see*: with no
sandbox, a job runs as the service account with that account's whole filesystem readable,
including Tasch's own auth token and TLS key, every other job's scratch files, a shared `/tmp`,
and a process table it can signal at will.

`sandbox.mode` closes that using Linux namespaces. It is `none` by default, because turning it
on changes what a job can reach and an upgrade should not do that silently.

```yaml
sandbox:
  mode: private          # none | private | strict
  network: host          # host | none
  scratch_dir: /var/lib/tasch/scratch
  tmpfs_size_mb: 512
  hostname: tasch-job

  # strict only: what to map in from the host
  readonly_paths: [/usr, /bin, /sbin, /lib, /lib64, /etc, /opt]
  writable_paths: [/mnt/datasets, /mnt/model-cache]

  # private only: extra paths to hide
  masked_paths: [/home/shared]
```

**`private`** gives each job its own mount, PID, IPC and UTS namespaces. The job gets a private
`/tmp` and `/dev/shm`, a process table holding only its own processes, and a working directory
of its own under `scratch_dir`. The service account's home, `/etc/tasch`, `/var/lib/tasch` and
`/sys/fs/cgroup` are masked. The rest of the filesystem stays visible and writable, so shared
data paths keep working with no further configuration. This is the setting to reach for first.

**`strict`** adds a `pivot_root` into a rootfs assembled from read-only bind mounts. The job
sees `readonly_paths`, `writable_paths`, a minimal `/dev`, a private `/proc`, a private `/tmp`,
and its own writable `/workspace` — and nothing else. A job's `pwd` is `/workspace`; what it
writes there lands in `scratch_dir/job-<id>` on the host and is removed when the job ends.

Neither mode needs root. On an unprivileged worker the sandbox is built inside a user namespace
with the account's uid mapped to 0, so jobs report `uid=0(root)`: that is the mapping, not a
privilege. Outside the namespace the job has exactly the rights the service account always had.
Some distributions gate this; if `kernel.unprivileged_userns_clone` is 0, the worker says so at
startup and names the sysctl.

Two behaviours worth knowing before you turn this on:

- **It fails closed.** If a mode is configured and the sandbox cannot be built, the job is
  failed with the reason, not run unconfined.
- **GPU device nodes follow the pinning.** `strict` builds its own `/dev`, so `/dev/nvidia*`,
  `/dev/dri` and `/dev/kfd` are mapped in only for jobs the master pinned devices to. Set
  `sandbox.gpu_devices: false` to withhold them entirely.

`sandbox.network: none` puts the job in an empty network namespace with only loopback, which
stops it reaching the cluster's own ports — and also stops it fetching packages or datasets, and
breaks multi-node distributed jobs. Leave it at `host` unless you need it.

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

### How many masters

Raft commits nothing without a majority of the configured cluster, so the useful number is
`quorum = floor(N/2) + 1`, and what you can survive is `N - quorum`:

| Masters | Quorum | Failures tolerated |
|---------|--------|--------------------|
| 1       | 1      | 0                  |
| 2       | 2      | **0**              |
| 3       | 2      | 1                  |
| 4       | 3      | 1                  |
| 5       | 3      | 2                  |

**Two masters are worse than one, which is why the config refuses them.** Quorum with two is
two: you need both. Lose either and the survivor cannot elect a leader or commit a single entry
— it sits holding a complete copy of the state, refusing to act. So two hosts tolerate exactly
as many failures as one, zero, while doubling the hardware that can cause an outage.

Recovery is also harder. A dead single master is a restart, or restoring its database onto a new
host. A two-master cluster with one host permanently gone needs its Raft configuration manually
rewritten to shrink to one — a delicate operation, and doing it while the "dead" master is
merely partitioned leaves two masters both accepting writes, at which point one set of jobs
disappears at the next election.

The same arithmetic explains the odd-number rule: four masters tolerate one failure, exactly
like three, while adding a fourth machine that can fail. Every even size is strictly worse than
the odd size below it.

> Two nodes *can* be made to work with a witness — a third voting member that holds no state and
> exists only to break ties, as etcd learners and MongoDB arbiters do. Tasch does not implement
> one, so three real masters is the floor here.

If three masters are not practical, run a single master. It survives its own restart without
losing running jobs (see **Master Restart** in the architecture guide) — it just cannot survive
losing the host.

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
