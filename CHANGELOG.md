# Changelog

## [v0.6.0] — 2026-04-06

Production robustness: persistence, retry, health checks, circuit breaker, GPU tracking, TLS.

### Added
- **BoltDB persistence** — Jobs, groups, fairshare to `~/.tasch/tasch.db`. Master restart resumes queued jobs.
- **Job retry** — Auto-retry failed jobs (default 3x) with exponential backoff (10s, 40s, 90s).
- **Dead letter queue** — Jobs exhausting all retries archived. `tasch jobs failed` to view.
- **Health endpoints** — `/health` (liveness), `/ready` (readiness with members, queue depth, drain status).
- **Queue size limits** — Max 10,000 jobs (configurable). Rejects with `RESOURCE_EXHAUSTED` when full.
- **Graceful drain** — `tasch stop` → stop accepting → wait for running jobs → shutdown.
- **Double-start prevention** — PID file check on `tasch start`.
- **Stop with grace period** — SIGTERM → 15s wait → SIGKILL fallback.
- **ZMQ auto-reconnection** — Worker reconnects with exponential backoff (1s-30s).
- **gRPC keepalive** — 30s heartbeat, 10s timeout on worker connections.
- **ReportResult retry** — Worker retries result reporting once on failure (10s timeout).
- **Circuit breaker** — 3 consecutive failures on a worker → blocked 5 minutes.
- **GPU resource tracking** — Allocated GPUs tracked per node, prevents oversubscription.
- **TLS support** — Optional mTLS for gRPC. Config fields: `tls.enabled`, `tls.cert_file`, `tls.key_file`, `tls.ca_file`.
- **Persistence hooks** — `OnJobChange`/`OnGroupChange` callbacks on GlobalScheduler for write-through persistence.
- Config fields: `max_queue_size`, `max_retries`, `drain_timeout`, `tls`, `ports.metrics`.

### Changed
- `Enqueue()` returns `error` (queue full check).
- `StartMaster()` returns `*MasterHandle` with `Draining` flag and `Queue` ref for drain orchestration.
- Scheduler state changes fire persistence hooks (outside lock).

---

## [v0.5.0] — 2026-04-06

Observability and failure handling.

### Added
- **Prometheus metrics** — 9 metrics: jobs submitted/completed, queue depth, running jobs, cluster nodes, dispatch duration, job duration, groups pending, walltime kills, worker lost.
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
