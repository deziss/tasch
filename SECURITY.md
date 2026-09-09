# Security

## Current status: alpha

Tasch's core function is "accept a command string over the network and run it in a shell on
other machines." Authentication, transport encryption, per-job resource limits and per-job
isolation all exist as of v0.9.0 — but **all of them are off by default**, so a cluster that has
not been configured is exactly as open as one running the previous release. Read this before
deploying.

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
- **No filesystem isolation with `sandbox.mode: none`**, which is the default. A job runs as
  the service account, shares that account's filesystem and network, and can read anything the
  account can — including Tasch's own auth token, TLS key and job database. CPU, memory and
  process limits are enforced regardless, but confinement is not isolation. **With the sandbox
  off, treat the ability to submit a job as equivalent to a shell on every worker.** Turning
  `sandbox.mode` on is what changes this; see "What is isolated" below.
- **Job tampering, with auth off.** `CancelJob`, `GetJobStatus`, and `StreamLogs` take a job
  ID, and `ReportResult` is accepted from anyone, so results and resource accounting can be
  forged. With auth on, these are ownership-checked and results carry a fencing token.
- **Cluster join, without a gossip key.** Any host can join, advertise fabricated resources,
  and be selected as a dispatch target.
- **Secrets at rest.** Job environment variables are stored in BoltDB as plaintext JSON. With
  the sandbox off, every job runs as the `tasch` user that owns that file, so any submitted job
  can read every other job's stored secrets. `sandbox.mode: private` masks the state directory
  and `strict` never maps it in, which closes this — but the file itself is still plaintext, so
  anyone with shell access to the node reads it.

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

## What is isolated

`sandbox.mode` decides what a job can see. It is `none` by default — an upgrade must not
silently change what already-running workloads can reach — so this is opt-in.

| Mode | Namespaces | Filesystem the job sees |
|---|---|---|
| `none` | none | everything the service account can reach |
| `private` | mount, PID, IPC, UTS | the host's, but with its own `/tmp` and `/dev/shm`, and with the account's home, `/etc/tasch`, `/var/lib/tasch` and `/sys/fs/cgroup` masked |
| `strict` | mount, PID, IPC, UTS (+ network if `sandbox.network: none`) | only `sandbox.readonly_paths` (read-only), `sandbox.writable_paths`, a minimal `/dev`, a private `/proc`, and its own writable `/workspace` |

Both modes run the job as PID 1's child in a fresh PID namespace, so it cannot see or signal
another job's processes, and when the job's supervisor exits the kernel destroys the namespace —
nothing it backgrounded survives. Both set `no_new_privs`, so a setuid binary or a file
capability cannot be used to gain privilege. Both mask `/sys/fs/cgroup`, because a job's own
cgroup is owned by the account it runs as and a writable cgroupfs would let a job raise the very
memory limit the scheduler set for it.

No root is required. On an unprivileged worker the sandbox is built inside a user namespace,
with the service account's uid mapped to 0 there — so a job sees itself as root, while outside
the namespace it holds exactly the privileges the service account always had. Jobs report
`uid=0(root)`; that is the mapping, not a privilege.

If isolation is configured and cannot be set up, the job is **failed, not run**. A cluster told
to sandbox its work must not quietly stop doing so.

### What this does not give you

- **One uid for every job.** All jobs still run as the same service account outside the
  namespace. Two jobs cannot reach each other through the filesystem in `strict` mode, but any
  path listed in `writable_paths` is shared by all of them, and nothing stops a job writing
  where another will read.
- **No syscall filter.** There is no seccomp profile, so the whole kernel API is reachable and a
  kernel vulnerability is a full escape. This is the main reason the isolation is comparable to
  a plain container rather than to a VM.
- **No GPU isolation.** Device nodes are exposed whole to a job the master pinned devices to;
  `CUDA_VISIBLE_DEVICES` remains advisory.
- **`network: host` by default.** A job can reach the cluster's own gRPC and gossip ports.
  `sandbox.network: none` removes that, at the cost of breaking anything that fetches packages
  or datasets, and any multi-node distributed job.

## Deploying safely today

Tasch is safe only on a **trusted network where everyone who can authenticate is already
permitted to run arbitrary code on the cluster** — a private lab cluster, a single-tenant VPC
subnet, or a lab VLAN.

Turn on `auth.enabled`, `gossip.encryption_key`, and `tls.enabled`. `tasch config validate`
reports which of these are missing.

- Firewall ports 50051, 7946, and 9090 to known cluster hosts only. Never expose them
  to a shared network, a VPN with untrusted peers, or the internet.
- Set `sandbox.mode`. `private` is the cheapest setting that stops jobs reading each other's
  files and Tasch's own credentials; `strict` is what to use if the submitters do not fully
  trust each other. Even then they share one service account and an unfiltered kernel, so do not
  treat a strict sandbox as a boundary against a determined attacker.
- Prefer not to pass long-lived credentials via `--env`. If you must, scope them tightly and
  rotate them, and assume every node has seen them.
- Enabling `tls.enabled` encrypts the gRPC leg only. It authenticates nothing, and the
  `tasch` CLI cannot currently connect to a TLS-enabled master.

## Planned

Authentication, mutual TLS, gossip encryption, removal of the plaintext dispatch broadcast, and
worker-side resource enforcement all landed in v0.9.0.

Per-job filesystem, PID, IPC and optional network isolation landed in v0.9.0 as `sandbox.mode`.

What remains before Tasch can claim to be safe for mutually distrusting users: a seccomp profile
bounding the syscalls a job can make, and running jobs as the submitting user rather than as one
shared service account. Until then, the guidance above is the security model.

## Reporting a vulnerability

Open a GitHub issue for anything that is not itself sensitive. For a finding that would put
existing deployments at risk, contact the maintainers privately rather than filing publicly.

Findings already known and documented above — in particular the absence of a seccomp profile and
of a per-job uid — do not need to be reported; they are tracked. A way to escape a `strict`
sandbox that does *not* rely on either of those is a genuine finding, and is worth reporting
privately.
