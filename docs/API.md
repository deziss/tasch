# API Reference

**Proto:** `api/v1/scheduler.proto` | **Default port:** 50051

## Service: SchedulerService (8 RPCs)

| RPC | Description |
|-----|-------------|
| `SubmitJob` | Submit single job |
| `SubmitDistributedJob` | Submit multi-node training job |
| `CancelJob` | Cancel queued/running job |
| `GetJobStatus` | Get job detail + output |
| `StreamLogs` | Stream job logs (server-side streaming) |
| `WorkerStatus` | Get cluster nodes |
| `ListJobs` | List all jobs |
| `ReportResult` | Worker reports completion (internal) |

---

### SubmitJob

| Request Field | Type | Description |
|---------------|------|-------------|
| `cel_requirement` | string | CEL expression |
| `command` | string | Shell command |
| `priority` | int32 | Lower = higher (default: 10) |
| `user` | string | Fairshare tracking |
| `walltime_seconds` | int32 | Max time (0 = no limit) |
| `gpus_required` | int32 | GPU count |
| `env_vars` | map\<string,string\> | Custom env vars |

**Response:** `job_id`, `status` ("QUEUED")

### SubmitDistributedJob

| Request Field | Type | Description |
|---------------|------|-------------|
| `cel_requirement` | string | Must match ALL nodes |
| `command` | string | Same on all ranks |
| `num_nodes` | int32 | Node count |
| `gpus_per_node` | int32 | GPUs per node |
| `priority` | int32 | |
| `user` | string | |
| `walltime_seconds` | int32 | Per-rank limit |
| `master_port` | int32 | DDP port (default: 29500) |
| `env_vars` | map\<string,string\> | Additional env vars |

**Response:** `group_id`, `job_ids` (per rank), `status`

Auto-injected per rank: `RANK`, `WORLD_SIZE`, `MASTER_PORT`, `LOCAL_RANK`, `NPROC_PER_NODE`. `MASTER_ADDR` injected at dispatch time.

### CancelJob

**Request:** `job_id` | **Response:** `job_id`, `status`, `message`

### GetJobStatus

**Response:** `job_id`, `state`, `worker_node`, `command`, `output`, `error`, `submit_time`, `start_time`, `end_time`, `group_id`

**States:** QUEUED, RUNNING, COMPLETED, FAILED, CANCELLED, NOT_FOUND

### ListJobs

**Request:** `state_filter` (optional) | **Response:** repeated `JobInfo` (`job_id`, `state`, `command`, `requirement`, `worker_node`, `priority`, `user`, `submit_time`, `group_id`)

### StreamLogs

**Request:** `job_id` | **Response (stream):** `timestamp` (ms), `level`, `message`, `job_id`

### WorkerStatus

**Response:** `worker_nodes` map (node name → ClassAd JSON)

### ReportResult (internal — workers only)

**Request:** `job_id`, `worker_node`, `success`, `output`, `error`, `start_time`, `end_time`

---

## ZeroMQ Internal Protocol

**Port:** 5555 | **Encoding:** JSON

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

## ClassAd JSON (Worker Advertisement)

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
