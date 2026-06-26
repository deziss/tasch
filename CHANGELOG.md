# Changelog

## [v0.8.0] — 2026-06-26

Cross-platform multi-GPU support, OS-aware GPU binding, platform-agnostic worker execution, and enhanced test coverage.

### Added
- **Cross-platform GPU detection** — Platform-specific profiler files via Go build tags:
  - `profiler_linux.go`: NVIDIA (`nvidia-smi`), AMD (`rocm-smi`), Jetson Tegra (sysfs)
  - `profiler_windows.go`: WMI/PowerShell `Win32_VideoController` (NVIDIA, AMD, Intel, Qualcomm Adreno) + `nvidia-smi.exe` fallback
  - `profiler_darwin.go`: Apple Metal models + Unified Memory from `sysctl hw.memsize` + `system_profiler SPDisplaysDataType`
  - `profiler_fallback.go`: Compilation stub for unsupported OS (FreeBSD, etc.)
- **OS-aware GPU env binding** — Master injects the correct env var per GPU vendor on dispatch:
  - NVIDIA: `CUDA_VISIBLE_DEVICES`
  - AMD: `HIP_VISIBLE_DEVICES`
  - Intel: `ONEAPI_DEVICE_SELECTOR` + `SYCL_DEVICE_FILTER`
  - Apple: `METAL_DEVICE_INDEX`
- **Platform-agnostic worker command execution** — `exec_unix.go` (sh -c) and `exec_windows.go` (cmd.exe /d /c, hidden console window)
- **Cross-compilation build script** — `build.sh` targets 6 platforms: `linux/amd64`, `linux/arm64`, `windows/amd64`, `windows/arm64`, `darwin/amd64`, `darwin/arm64` (32-bit excluded). Strips debug symbols with `-ldflags="-s -w"`.
- **CEL ad.gpu_vendor** extended to include `intel` and `apple` values
- **Integration test improvements** — `test.sh` updated to 13 scenarios:
  - Explicit `metrics` port in test config (`9092`) to avoid conflicts
  - Cluster topology validation (both nodes must be visible before tests run)
  - `wait_for_job` helper with state-assertion and configurable timeout
  - Test 12: Multi-resource constraint (CPUs + Memory)
  - Test 13: Resource over-subscription stays QUEUED, cancel validation

### Changed
- `profiler.go` refactored — GPU detection delegated to OS-specific files; shared `Host` struct retained
- Worker command builder replaced hardcoded `sh -c` with `prepareCommand()` platform abstraction

---

## [v0.7.0] — 2026-04-10

Debian and RPM packaging, systemd integration, and system-wide configuration.

### Added
- **.deb and .rpm packaging** via `nfpm`. Build with `make deb`, `make rpm`, or `make package`.
- **systemd integration** — `tasch.service` unit file included in packages.
- **System-wide configuration** — Prioritizes `/etc/tasch/config.yaml` and `/var/lib/tasch/` for database/PID when running as a global service.
- **Automated user creation** — `postinstall.sh` creates `tasch` system user/group with isolated home in `/var/lib/tasch`.
- **Makefile targets** — `package`, `deb`, `rpm` added for automated distribution builds.

---

## [v0.6.0] — 2026-04-06

Production robustness: persistence, retry, health checks, circuit breaker, GPU tracking, TLS.

### Added
- **BoltDB persistence** — Jobs, groups, fairshare to `~/.tasch/tasch.db`. Master restart resumes queued jobs.
- **Job retry** — Auto-retry failed jobs (default 3×) with exponential backoff (10s, 40s, 90s).
- **Dead letter queue** — Jobs exhausting all retries archived. `tasch jobs failed` to view.
- **Health endpoints** — `/health` (liveness), `/ready` (readiness with members, queue depth, drain status).
- **Queue size limits** — Max 10,000 jobs (configurable). Rejects with `RESOURCE_EXHAUSTED` when full.
- **Graceful drain** — `tasch stop` → stop accepting → wait for running jobs → shutdown.
- **Double-start prevention** — PID file check on `tasch start`.
- **Stop with grace period** — SIGTERM → 15s wait → SIGKILL fallback.
- **ZMQ auto-reconnection** — Worker reconnects with exponential backoff (1s–30s).
- **gRPC keepalive** — 30s heartbeat, 10s timeout on worker connections.
- **ReportResult retry** — Worker retries result reporting with exponential backoff (up to 30s) indefinitely.
- **Circuit breaker** — 3 consecutive failures on a worker → blocked 5 minutes.
- **GPU resource tracking** — Allocated GPUs tracked per node, prevents oversubscription.
- **Multi-resource tracking** — CPU cores and memory also tracked per node alongside GPUs.
- **ZMQ dispatch handshake** — Worker sends `/acknowledge_start` HTTP POST to master; unacknowledged jobs re-queued after 10s timeout.
- **Async DB writes** — Buffered `dbWriteChan` channel decouples scheduler hot path from BoltDB disk writes.
- **TLS support** — Optional mTLS for gRPC. Config fields: `tls.enabled`, `tls.cert_file`, `tls.key_file`, `tls.ca_file`.
- **Persistence hooks** — `OnJobChange`/`OnGroupChange` callbacks on GlobalScheduler for write-through persistence.
- Config fields: `max_queue_size`, `max_retries`, `drain_timeout`, `tls`, `ports.metrics`.

### Changed
- `Enqueue()` returns `error` (queue full check).
- `StartMaster()` returns `*MasterHandle` with `Draining` flag and `Queue` ref for drain orchestration.
- Scheduler state changes fire persistence hooks (outside lock).
- Circuit breaker now excludes cancelled jobs, walltime kills, and poison-pill duplicates from failure counter.

---

## [v0.5.0] — 2026-04-06

Observability and failure handling.

### Added
- **Prometheus metrics** — 10 metrics: jobs submitted/completed, queue depth, running jobs, cluster nodes, dispatch duration, job duration, groups pending, walltime kills, worker lost.
- **Worker loss detection** — Gossip EventHooks `OnLeave` → fail running jobs on departed worker.
- **Gang scheduling timeout** — 5 min timeout for distributed groups waiting for nodes.
- **`CreatedAt` on JobGroup** — Enables gang timeout tracking.
- **`RunningJobsOnNode()`** — Enables worker loss cleanup.
- **Metrics HTTP server** on configurable port (default 9090).

---

## [v0.4.0] — 2026-04-06

Unified binary, interactive setup, AMD GPU support, CLI redesign.

### Breaking Changes
- Single `tasch` binary replaces `master`, `worker`, `cli`.
- CLI renamed: `tasch submit` → `tasch jobs submit`, `tasch status` → `tasch nodes`, etc.

### Added
- `tasch setup` interactive wizard → `~/.tasch/config.yaml`.
- `tasch start` / `tasch stop` — config-driven daemon.
- AMD GPU detection via `rocm-smi`. `gpu_vendor` ClassAd field. Auto `HIP_VISIBLE_DEVICES`.
- CLI works from any machine (reads master addr from config).

---

## [v0.3.0] — 2026-04-06

GPU-aware scheduling, cross-server clusters, distributed training.

### Added
- NVIDIA GPU detection, `TASCH_MASTER_ADDR` for remote workers, `SubmitDistributedJob` RPC, gang scheduling, group completion tracking, `--gpus`/`--env` flags, `CUDA_VISIBLE_DEVICES` auto-set.

---

## [v0.2.0] — 2026-04-04

Job lifecycle, backfill, fairshare, walltime, log streaming.

---

## [v0.1.0] — Initial Release

Memberlist gossip, ZMQ dispatch, CEL matchmaking, min-heap scheduler.
