# API Reference

**Proto:** `api/v1/scheduler.proto` | **Default port:** 50051 | **Optional TLS**

## SchedulerService (8 RPCs)

| RPC | Description |
|-----|-------------|
| `SubmitJob` | Submit single job (with retry, queue limit) |
| `SubmitDistributedJob` | Multi-node training (gang scheduling) |
| `CancelJob` | Cancel queued/running job |
| `GetJobStatus` | Job detail + output + retry count |
| `StreamLogs` | Stream logs (server-side streaming) |
| `WorkerStatus` | Cluster nodes + ClassAds |
| `ListJobs` | List all jobs (filterable) |
| `ReportResult` | Worker reports completion (internal) |

### SubmitJob

| Request Field | Type | Description |
|---------------|------|-------------|
| `cel_requirement` | string | CEL expression |
| `command` | string | Shell command |
| `priority` | int32 | Lower = higher (default: 10) |
| `user` | string | Fairshare tracking |
| `walltime_seconds` | int32 | Max time (0 = unlimited) |
| `gpus_required` | int32 | GPU count |
| `env_vars` | map | Custom env vars |

Returns `job_id` + `status`. Rejects with `RESOURCE_EXHAUSTED` if queue full. Rejects with `UNAVAILABLE` if draining.

### SubmitDistributedJob

| Request Field | Type | Description |
|---------------|------|-------------|
| `cel_requirement` | string | Must match ALL nodes |
| `command` | string | Same on all ranks |
| `num_nodes` | int32 | Node count |
| `gpus_per_node` | int32 | GPUs per node |
| `master_port` | int32 | DDP port (default: 29500) |
| `priority`, `user`, `walltime_seconds`, `env_vars` | | Same as SubmitJob |

Auto-injected: `RANK`, `WORLD_SIZE`, `MASTER_PORT`, `LOCAL_RANK`, `NPROC_PER_NODE`. `MASTER_ADDR` at dispatch.

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

---

## Health + Metrics Endpoints (port 9090)

| Endpoint | Description |
|----------|-------------|
| `/health` | Liveness (200 always) |
| `/ready` | Readiness (members, queue_depth, draining) |
| `/metrics` | Prometheus metrics |

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

```json
{
  "gpu_count": 2,
  "gpu_vendor": "nvidia",
  "gpu_models": ["NVIDIA A100-SXM4-40GB"],
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
