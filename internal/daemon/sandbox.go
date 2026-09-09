package daemon

// Job isolation.
//
// cgroups made the master's resource reservations real: a job cannot use more CPU, memory or
// processes than it asked for. They say nothing about what it can *see*. Without namespaces a
// job runs as the service account, with the account's whole filesystem readable — including
// Tasch's own auth tokens and TLS keys — every other job's scratch files, a shared /tmp, a
// shared System V IPC namespace, and a process table it can signal at will. Two jobs from
// different users on one node were separated by nothing.
//
// The sandbox closes that with Linux namespaces. The mechanism is the usual one: the worker
// cannot set up namespaces for a child from the parent (mounts have to happen after the clone,
// from inside the new mount namespace, and Go's runtime makes post-fork work in the child
// unsafe), so it re-executes its own binary as `tasch sandbox-init`. That helper starts already
// inside the new namespaces, builds the filesystem view, and only then runs the job.
//
// The helper is also the job's PID 1, which is not incidental. A process that is PID 1 in a
// namespace ignores signals it has no handler for, so execing the job directly would make it
// immune to the SIGTERM a walltime kill sends. Keeping the helper as init, forwarding signals
// and reaping orphans, preserves cancellation — and when it exits the kernel tears the whole
// namespace down, so nothing the job backgrounded can survive.

// sandboxInitCommand is the hidden subcommand the worker re-executes itself as. It is not
// part of the CLI surface: nothing but the worker should ever run it.
const sandboxInitCommand = "sandbox-init"

// sandboxSpecEnv carries the plan from the worker to the re-executed helper. The helper strips
// it from the environment before running the job.
const sandboxSpecEnv = "TASCH_SANDBOX_SPEC"

// sandboxWorkdir is where a job's scratch directory appears inside a strict sandbox. In
// private mode the scratch keeps its host path, because the host filesystem is still visible.
const sandboxWorkdir = "/workspace"

// sandboxSpec is the isolation plan for one job: everything the helper needs, and nothing the
// worker would have to recompute after the clone.
type sandboxSpec struct {
	Mode     string `json:"mode"`
	Command  string `json:"command"`
	Hostname string `json:"hostname"`

	// Scratch is the job's working directory on the host. WorkdirIn is where it appears to the
	// job, which differs from Scratch only under pivot_root.
	Scratch   string `json:"scratch"`
	WorkdirIn string `json:"workdir_in"`

	// RootStage is an empty host directory the helper mounts the new rootfs tmpfs onto before
	// pivot_root. Strict mode only.
	RootStage string `json:"root_stage,omitempty"`

	ReadOnly []string `json:"readonly,omitempty"`
	Writable []string `json:"writable,omitempty"`
	Masked   []string `json:"masked,omitempty"`

	TmpfsSizeMB int  `json:"tmpfs_size_mb"`
	NoNewPrivs  bool `json:"no_new_privs"`
	GPUDevices  bool `json:"gpu_devices"`
	PrivateNet  bool `json:"private_net"`
}
