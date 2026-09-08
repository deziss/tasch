# Security

## Current status: alpha

Tasch's core function is "accept a command string over the network and run it in a shell on
other machines." Authentication, transport encryption, and per-job resource limits all exist as
of v0.9.0 — but **authentication is off by default**, and jobs are resource-limited rather than
isolated. Read this before deploying.

## What is exposed

| Port | Service | Encryption | Authentication |
|------|---------|-----------|----------------|
| 50051 | gRPC — submit, cancel, list, logs, dispatch | optional TLS / mTLS | token, off by default |
| 7946 | memberlist gossip — cluster membership | optional, off by default | shared key, off by default |
| 9090 | HTTP — `/metrics`, `/health`, `/ready` | **none** | read-only |

### Consequences you should assume are true

- **Remote code execution.** With `auth.enabled` off, any host that can reach `:50051` can
  submit a job, and the worker runs it via `sh -c` as the `tasch` service user. Turn
  authentication on.
- **No filesystem or network isolation.** Even with authentication, a job runs as the service
  account: it shares that account's filesystem and network, and can read anything the account
  can. CPU, memory, and process limits *are* enforced (see below), but confinement is not
  isolation. Real isolation needs namespaces or a container runtime, which Tasch does not yet
  implement. **Treat the ability to submit a job as equivalent to a shell on every worker.**
- **Job tampering, with auth off.** `CancelJob`, `GetJobStatus`, and `StreamLogs` take a job
  ID, and `ReportResult` is accepted from anyone, so results and resource accounting can be
  forged. With auth on, these are ownership-checked and results carry a fencing token.
- **Cluster join, without a gossip key.** Any host can join, advertise fabricated resources,
  and be selected as a dispatch target.
- **Secrets at rest.** Job environment variables are stored in BoltDB as plaintext JSON.
  Because every job runs as the `tasch` user that owns that file, any submitted job can read
  every other job's stored secrets.

## What is enforced

Since v0.9.0, a job's reservations are real limits on Linux, applied through a cgroup v2
subtree the daemon creates per job:

- `--cpus` becomes a `cpu.max` quota.
- `--memory` becomes a hard `memory.max`. Exceeding it kills the job with a stated reason
  rather than an opaque "killed".
- Every job gets a process cap (`max_pids_per_job`, default 4096) whether or not it reserved
  anything, so a fork bomb cannot take the worker and its co-tenant jobs down.

This requires the daemon to own a delegated cgroup. The shipped systemd unit sets
`Delegate=cpu memory pids`. Run `tasch start` outside systemd, or in a container without a
delegated subtree, and the worker logs a warning at startup and runs jobs **unconfined** —
check for that line before assuming limits apply.

GPUs are not covered: cgroup v2 has no GPU controller, so `CUDA_VISIBLE_DEVICES` remains
advisory and a job can unset it and reach every card on the node.

## Deploying safely today

Tasch is safe only on a **trusted network where everyone who can authenticate is already
permitted to run arbitrary code on the cluster** — a private lab cluster, a single-tenant VPC
subnet, or a lab VLAN.

Turn on `auth.enabled`, `gossip.encryption_key`, and `tls.enabled`. `tasch config validate`
reports which of these are missing.

- Firewall ports 50051, 7946, and 9090 to known cluster hosts only. Never expose them
  to a shared network, a VPN with untrusted peers, or the internet.
- Do not run mutually distrusting workloads. Jobs are resource-limited but not isolated: they
  share a user account, a filesystem, and a network namespace.
- Prefer not to pass long-lived credentials via `--env`. If you must, scope them tightly and
  rotate them, and assume every node has seen them.
- Enabling `tls.enabled` encrypts the gRPC leg only. It authenticates nothing, and the
  `tasch` CLI cannot currently connect to a TLS-enabled master.

## Planned

Authentication, mutual TLS, gossip encryption, removal of the plaintext dispatch broadcast, and
worker-side resource enforcement all landed in v0.9.0.

What remains before Tasch can claim to be safe for mutually distrusting users: per-job
filesystem and network isolation (namespaces or a container runtime), and running jobs as the
submitting user rather than as one shared service account. Until then, the guidance above is
the security model.

## Reporting a vulnerability

Open a GitHub issue for anything that is not itself sensitive. For a finding that would put
existing deployments at risk, contact the maintainers privately rather than filing publicly.

Findings already known and documented above — in particular the absence of per-job filesystem
and network isolation — do not need to be reported; they are tracked.
