# Security

## Current status: alpha — no authentication

Tasch does **not** authenticate or authorize anything. Read this before deploying it.

Tasch's core function is "accept a command string over the network and run it in a shell on
other machines." Today it does that for **any host that can reach the master**, with no
credential of any kind. Treat every listening port as equivalent to an unauthenticated root
shell on every worker.

## What is exposed

| Port | Service | Encryption | Authentication |
|------|---------|-----------|----------------|
| 50051 | gRPC — job submit, cancel, list, logs | optional, server-side TLS | **none** |
| 7946 | memberlist gossip — cluster membership | **none** | **none** |
| 9090 | HTTP — `/metrics`, `/health`, `/ready` | **none** | read-only |

### Consequences you should assume are true

- **Remote code execution.** Any host that can reach `:50051` can submit a job. The worker
  runs it via `sh -c` as the `tasch` service user, with no sandbox, no cgroups, no
  namespaces, and no per-user identity. The `--user` flag is a fairshare label only; it
  grants no identity and is not verified.
- **Job tampering.** `CancelJob`, `GetJobStatus`, and `StreamLogs` take a job ID and perform
  no ownership check. `ReportResult` is unauthenticated and does not verify that the caller
  is the node the job ran on, so results, resource accounting, and the circuit breaker can
  all be forged.
- **Cluster join.** Gossip has no shared key. Any host can join, advertise fabricated
  resources, and be selected as a dispatch target.
- **Secrets at rest.** Job environment variables are stored in BoltDB as plaintext JSON.
  Because every job runs as the `tasch` user that owns that file, any submitted job can read
  every other job's stored secrets.

## Deploying safely today

Tasch is safe only on a **trusted, isolated network where every host that can reach it is
already permitted to run arbitrary code on the cluster** — a private lab cluster, a
single-tenant VPC subnet, or a lab VLAN.

- Firewall ports 50051, 7946, and 9090 to known cluster hosts only. Never expose them
  to a shared network, a VPN with untrusted peers, or the internet.
- Do not run multi-tenant workloads. There is no isolation between users' jobs.
- Prefer not to pass long-lived credentials via `--env`. If you must, scope them tightly and
  rotate them, and assume every node has seen them.
- Enabling `tls.enabled` encrypts the gRPC leg only. It authenticates nothing, and the
  `tasch` CLI cannot currently connect to a TLS-enabled master.

## Planned

Authentication and authorization on gRPC, real mutual TLS, a gossip encryption key, removal
of the plaintext dispatch broadcast, and worker-side resource enforcement are all tracked as
blockers for a 1.0 release. Until they land, the guidance above is the security model.

## Reporting a vulnerability

Open a GitHub issue for anything that is not itself sensitive. For a finding that would put
existing deployments at risk, contact the maintainers privately rather than filing publicly.

Findings already known and documented above — in particular the absence of job sandboxing —
do not need to be reported; they are tracked.
