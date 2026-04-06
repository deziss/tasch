package messaging

// DispatchPayload is the structure published across the ZeroMQ cluster for jobs.
type DispatchPayload struct {
	TargetNode      string            `json:"target_node"`
	JobID           string            `json:"job_id"`
	Command         string            `json:"command"`
	WalltimeSeconds int               `json:"walltime_seconds"`
	Action          string            `json:"action"`                    // "execute" or "cancel"
	EnvVars         map[string]string `json:"env_vars,omitempty"`        // Custom environment variables
}
