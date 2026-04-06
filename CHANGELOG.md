# Changelog

## [v0.4.0] — 2026-04-06

Unified binary, interactive setup, AMD GPU support, and CLI redesign.

### Breaking Changes

- **Single binary** — Three separate binaries (`master`, `worker`, `cli`) replaced by one `tasch` binary with subcommands. Old `cmd/master`, `cmd/worker`, `cmd/cli` directories removed.
- **CLI renamed** — `tasch submit` → `tasch jobs submit`, `tasch list` → `tasch jobs`, `tasch status` → `tasch nodes`, `tasch job <id>` → `tasch jobs status <id>`, `tasch logs` → `tasch jobs logs`.

### Added

- **`tasch setup`** — Interactive setup wizard. Asks role (master/worker/both), node name, master address, ports. Detects hardware (CPU, RAM, GPUs). Writes `~/.tasch/config.yaml`. Supports `--non-interactive` mode for scripting.

- **`tasch start` / `tasch stop`** — Config-driven start. Reads role from config, starts appropriate components. PID-based stop via SIGTERM.

- **Config file** — `~/.tasch/config.yaml` stores role, node name, master address, ports. CLI commands read config to find master. `TASCH_*` env vars override config values.

- **AMD GPU support** — `pkg/profiler` now tries `rocm-smi` when `nvidia-smi` is not found. New ClassAd fields: `gpu_vendor` ("nvidia"/"amd"), `rocm_version`. CEL: `ad.gpu_vendor == "amd"`.

- **`HIP_VISIBLE_DEVICES`** — Master auto-sets `HIP_VISIBLE_DEVICES` instead of `CUDA_VISIBLE_DEVICES` when dispatching to AMD GPU workers.

- **CLI works everywhere** — `tasch nodes` and `tasch jobs` work from any machine (master or worker) as long as the config points to the master.

### Changed

- **Project structure** — Code reorganized: `internal/config`, `internal/setup`, `internal/daemon`, `internal/cli`. Master and worker logic extracted from `cmd/` into importable packages with `StartMaster(cfg)` and `StartWorker(cfg)` functions.

- **Makefile** — Now builds single `bin/tasch` binary.

---

## [v0.3.0] — 2026-04-06

GPU-aware scheduling, cross-server clusters, distributed training with gang scheduling.

### Added
- GPU detection (NVIDIA) via nvidia-smi — ClassAd fields: `gpu_count`, `gpu_models`, `gpu_memory_mb`, `cuda_version`
- `TASCH_MASTER_ADDR` env var for cross-server worker joining
- `SubmitDistributedJob` RPC + `tasch train` command for multi-node training
- Gang scheduling — all ranks dispatched simultaneously
- Group completion tracking — failed rank cancels siblings
- `--gpus` and `--env` flags on submit
- `CUDA_VISIBLE_DEVICES` auto-set
- GPU pre-filter in dispatch loop

---

## [v0.2.0] — 2026-04-04

Job lifecycle, backfill, fairshare, walltime, log streaming.

### Added
- Job states: QUEUED → RUNNING → COMPLETED/FAILED/CANCELLED
- CancelJob, GetJobStatus, ListJobs, StreamLogs, ReportResult RPCs
- Backfill scheduling, fairshare accounting, walltime enforcement
- CLI: cancel, job, list, logs commands

---

## [v0.1.0] — Initial Release

### Added
- Memberlist gossip discovery, ZMQ dispatch, CEL matchmaking
- Min-heap scheduler, gRPC API (SubmitJob, WorkerStatus)
- Hardware ClassAd profiling, 3-binary architecture
