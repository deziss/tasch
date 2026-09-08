# Changelog

## [v0.9.0] — 2026-09-08

Correctness, security, and release-engineering release. Four crash- or wedge-class defects are
fixed, the API is no longer open to anonymous callers, and the project now has CI.

**Licence change:** Tasch is now under the **GNU Affero General Public License v3.0**
(previously MIT). Running a modified version as a network service obliges you to offer its
source to the users of that service.

### Fixed — crashes and stalls

- **8-GPU nodes could not join a cluster.** The gossip delegate ignored memberlist's 512-byte
  metadata limit, and memberlist panics rather than erroring above it. A realistic 8×A100
  ClassAd serialises to 553 bytes, so the worker panicked at startup on exactly the hardware
  Tasch targets. The ClassAd is now fitted to the limit, preserving every field reachable from
  a CEL requirement.
- **The master died with an unrecoverable fatal error.** The 60-second fairshare persistence
  tick marshalled the live usage map without holding its lock while job completions wrote to
  it, producing `concurrent map read and map write` — which `recover()` cannot catch.
- **The scheduler wedged whenever a gang rank reached the head of the queue.** An early
  `continue` meant to skip the direct-match phase skipped backfill too, so no single job
  dispatched anywhere in the cluster — permanently, if a restart had orphaned the rank.
- **Every GPU job was pinned to device 0.** Device indices were computed as `0..N-1` without
  consulting what was already allocated, so concurrent jobs collided on one card while the
  rest of the node idled.

### Fixed — correctness

- Resource release is idempotent and keyed by job ID; the six paths that can end a job can no
  longer compound into a node reporting more free GPUs than it has.
- A reconciliation loop rebuilds resource accounting from the live job set every 60 seconds,
  so drift from a restart or a missed release is corrected rather than permanent.
- Matching and dequeuing are now atomic, closing a window where a job was dispatched to a node
  that had been matched against a different job.
- Dispatches carry a fencing token; results from a superseded attempt are discarded instead of
  releasing the current node's resources.
- Cancelled and walltime-killed jobs are no longer retried.
- `Requeue` refuses a job already in the heap, which used to corrupt the heap index and
  silently evict an unrelated job.
- A cancel landing between dequeue and dispatch is no longer overwritten by `MarkRunning`.
- Job IDs are 64-bit, and a duplicate is rejected rather than overwriting another job.
- Resource reservations can be stated explicitly (`--cpus`, `--memory`) instead of being
  scraped out of the CEL requirement with a regex that most expressions did not match.
- Walltime and cancel kill the whole process group, so backgrounded grandchildren no longer
  survive and hold their GPUs.
- CEL requirements are validated at submit time instead of failing to match forever in silence.
- In-memory job records, log buffers, and the CEL program cache are all bounded.
- A stale PID file no longer permanently prevents startup; `tasch stop` waits for the
  configured drain instead of a hardcoded 15 seconds.
- The dispatch handshake no longer depends on the worker guessing the master's metrics port.

### Added — resource enforcement

- **Job reservations are now real limits on Linux.** `--cpus` and `--memory` are applied as
  cgroup v2 `cpu.max` and `memory.max`, and every job gets a process cap whether or not it
  reserved anything. Previously the master's resource tracking was pure bookkeeping: it decided
  where a job fit and nothing on the worker enforced that decision, so a job reserving one core
  could consume the whole machine and a fork bomb could take a worker down with every job on it.
- Exceeding a memory reservation reports "out of memory: exceeded the N MB reservation" instead
  of an opaque "signal: killed", and peak memory is logged on completion.
- The child is placed into its cgroup at clone time, so there is no window in which it runs
  unconfined.
- Enforcement is best-effort: a worker that cannot obtain a delegated cgroup warns at startup
  and runs jobs without limits, rather than refusing to work. The packaged systemd unit sets
  `Delegate=cpu memory pids`, which is required for any of this to engage.
- This confines resource usage; it is not isolation. Jobs still share the service account's
  filesystem and network, and GPUs remain advisory since cgroup v2 has no GPU controller.

### Added — security

- **Token authentication** with `user`, `admin`, and `worker` roles. Job ownership is enforced
  on cancel, status, and logs, and listings are scoped to the caller. Identity comes from the
  verified principal, so `--user` can no longer be used to evade a fairshare penalty or
  impersonate another user. Off by default; see `SECURITY.md`.
- **Mutual TLS** — setting `tls.ca_file` on the master now genuinely requires and verifies
  client certificates. Previously the documentation claimed mTLS while the master never asked
  for a certificate.
- **Gossip encryption** via `gossip.encryption_key`, closing open cluster membership.
- **Per-node dispatch.** The ZeroMQ PUB/SUB bus is gone. It broadcast every job's command and
  environment variables in cleartext to every subscriber, with targeting enforced only by the
  receiving worker — anything that could reach port 5555 could harvest every credential the
  cluster dispatched. Dispatch now rides the authenticated gRPC connection, and nothing listens
  on 5555.
- The dispatch acknowledgement is an authenticated RPC rather than an unauthenticated HTTP
  endpoint that could be used to forge acknowledgements for guessed job IDs.
- HTTP timeouts on the metrics server, a configurable `metrics_bind`, worker-side concurrency
  and output caps, and a hardened systemd unit.

### Added — operations

- **CI**: build matrix across all six targets, `gofmt`, `go vet`, staticcheck, golangci-lint,
  race-enabled tests with a coverage floor, `govulncheck`, and dependabot.
- **Release workflow** producing checksums, cosign signatures, and an SBOM.
- `tasch version` and `tasch config validate`, with warnings for insecure settings.
- Structured JSON logging with a `job_id` field correlating master and worker.
- Thirteen new metrics, including queue-wait and scheduling-loop histograms, per-state job
  gauges, and GPU utilisation. Worker-only nodes now serve `/metrics`, `/health`, and `/ready`.
- `/health` reflects the scheduling loop instead of returning a constant 200.
- BoltDB schema versioning with a migration path; unreadable records are reported rather than
  silently skipped.
- Config validation at startup: role, ports, TLS files, and auth principals.

### Changed

- Go 1.25 is now required — `google.golang.org/grpc` v1.82.1 is the first release without
  GO-2026-6061, a vulnerability in the HTTP/2 server the master runs.
- `ports.zmq` is unused and nothing binds it.
- Documentation now matches the implementation; several claims that did not (mTLS,
  Intel-on-Linux GPU detection, "always 200" health, output paths) have been corrected.

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
