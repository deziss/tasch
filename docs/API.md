# API Reference

**Proto:** `api/v1/scheduler.proto` | **Default port:** 50051 | **Optional TLS**

## SchedulerService (8 RPCs)

| RPC | Description |
|-----|-------------|
| `SubmitJob` | Submit single job (with retry, queue limit, multi-resource) |
| `SubmitDistributedJob` | Multi-node training (gang scheduling) |
| `CancelJob` | Cancel queued/running job |
| `GetJobStatus` | Job detail + output |
| `StreamLogs` | Stream logs (server-side streaming) |
| `WorkerStatus` | Cluster nodes + ClassAds (OS, arch, GPU info) |
| `ListJobs` | List all jobs (filterable by state) |
| `ReportResult` | Worker reports completion (internal) |

### SubmitJob

| Request Field | Type | Description |
|---------------|------|-------------|
| `cel_requirement` | string | CEL expression — supports `ad.os`, `ad.architecture`, `ad.gpu_vendor`, etc. |
| `command` | string | Shell command (platform-appropriate shell used at worker) |
| `priority` | int32 | Lower = higher (default: 10) |
| `user` | string | Fairshare tracking |
| `walltime_seconds` | int32 | Max time (0 = unlimited) |
| `gpus_required` | int32 | GPU count (0 = no GPU constraint) |
| `env_vars` | map | Custom env vars (GPU visibility env injected automatically) |

Returns `job_id` + `status`. Rejects with `RESOURCE_EXHAUSTED` if queue full. Rejects with `UNAVAILABLE` if draining.

CPU cores and memory requirements are **parsed from the CEL expression** at submit time:
- `ad.cpu_cores >= N` → sets `CPUsRequired = N`
- `ad.total_memory_mb >= N` → sets `MemoryRequiredMB = N`

### SubmitDistributedJob

| Request Field | Type | Description |
|---------------|------|-------------|
| `cel_requirement` | string | Must match ALL nodes |
| `command` | string | Same on all ranks |
| `num_nodes` | int32 | Node count |
| `gpus_per_node` | int32 | GPUs per node |
| `master_port` | int32 | DDP port (default: 29500) |
| `priority`, `user`, `walltime_seconds`, `env_vars` | | Same as SubmitJob |

Auto-injected env vars at dispatch: `RANK`, `WORLD_SIZE`, `MASTER_ADDR`, `MASTER_PORT`, `LOCAL_RANK`, `NPROC_PER_NODE`.

### GetJobStatus Response

Includes: `job_id`, `state`, `worker_node`, `command`, `output`, `error`, `submit_time`, `start_time`, `end_time`, `group_id`.

States: `QUEUED`, `RUNNING`, `COMPLETED`, `FAILED`, `CANCELLED`, `NOT_FOUND`.

---

## ZeroMQ Dispatch Payload

```go
type DispatchPayload struct {
    TargetNode      string            `json:"target_node"`
    JobID           string            `json:"job_id"`
    Command         string            `json:"command"`
    WalltimeSeconds int               `json:"walltime_seconds"`
    Action          string            `json:"action"`            // "execute" or "cancel"
    EnvVars         map[string]string `json:"env_vars,omitempty"`
}
```

`EnvVars` includes both user-specified env vars and the auto-injected GPU visibility variable (`CUDA_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `ONEAPI_DEVICE_SELECTOR`, or `METAL_DEVICE_INDEX`) based on the worker's GPU vendor.

---

## HTTP Endpoints (port 9090)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Liveness (200 always) |
| `/ready` | GET | Readiness (members, queue_depth, draining) |
| `/metrics` | GET | Prometheus metrics |
| `/acknowledge_start` | POST | Worker → master job start handshake |

### `/acknowledge_start` Request

```json
{ "job_id": "a3f2b1c0" }
```

Response: `200 {"status":"acknowledged"}` or `404 {"status":"not_found"}` if job not in pending map (already timed out or re-queued).

### Prometheus Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `tasch_jobs_submitted_total` | Counter | user |
| `tasch_jobs_completed_total` | Counter | user, status |
| `tasch_queue_depth` | Gauge | |
| `tasch_running_jobs` | Gauge | |
| `tasch_cluster_nodes` | Gauge | |
| `tasch_dispatch_duration_seconds` | Histogram | |
| `tasch_job_duration_seconds` | Histogram | user, status |
| `tasch_groups_pending` | Gauge | |
| `tasch_walltime_kills_total` | Counter | |
| `tasch_worker_lost_total` | Counter | |

---

## ClassAd JSON

Workers broadcast their hardware as a JSON ClassAd via gossip. Example from a Linux NVIDIA node:

```json
{
  "gpu_count": 2,
  "gpu_vendor": "nvidia",
  "gpu_models": ["NVIDIA A100-SXM4-40GB", "NVIDIA A100-SXM4-40GB"],
  "gpu_memory_mb": [40960, 40960],
  "cuda_version": "12.2",
  "rocm_version": "",
  "cpu_cores": 32,
  "total_memory_mb": 128000,
  "os": "linux",
  "architecture": "amd64",
  "host_type": "vm_or_baremetal"
}
```

Example from a macOS Apple Silicon node:

```json
{
  "gpu_count": 1,
  "gpu_vendor": "apple",
  "gpu_models": ["Apple M2 Ultra"],
  "gpu_memory_mb": [76800],
  "cpu_cores": 24,
  "total_memory_mb": 96000,
  "os": "darwin",
  "architecture": "arm64",
  "host_type": "vm_or_baremetal"
}
```

Example from a Windows Intel GPU node:

```json
{
  "gpu_count": 1,
  "gpu_vendor": "intel",
  "gpu_models": ["Intel Arc A770"],
  "gpu_memory_mb": [16384],
  "cpu_cores": 16,
  "total_memory_mb": 32000,
  "os": "windows",
  "architecture": "amd64",
  "host_type": "vm_or_baremetal"
}
```
