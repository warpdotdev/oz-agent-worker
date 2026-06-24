package worker

import (
	"strings"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// DispatchPayloadVersion is the schema version of DispatchPayload. It is bumped
// when the payload contract changes in a way operator dispatch commands must be
// aware of.
const DispatchPayloadVersion = 1

// DispatchPayload is the stable, versioned JSON contract handed to an operator's
// dispatch command (on stdin) by the command backend. It contains everything a
// remote runtime needs to launch the oz agent for a task. Secrets (e.g. GitHub
// tokens) travel only inside Env here, never via the dispatch subprocess's own
// environment or argv.
type DispatchPayload struct {
	Version       int                  `json:"version"`
	TaskID        string               `json:"task_id"`
	ExecutionID   string               `json:"execution_id"`
	ServerRootURL string               `json:"server_root_url"`
	WorkerID      string               `json:"worker_id"`
	DockerImage   string               `json:"docker_image"`
	BaseArgs      []string             `json:"base_args"`
	Env           map[string]string    `json:"env"`
	Sidecars      []types.SidecarMount `json:"sidecars"`
	Task          *types.Task          `json:"task"`
}

// NewDispatchPayload builds a DispatchPayload from backend-agnostic TaskParams
// plus the worker-level server URL and worker ID. The TaskParams.EnvVars slice
// (KEY=VALUE entries) is converted into the Env map, splitting on the first '='
// so values may themselves contain '='. Later entries win on duplicate keys.
func NewDispatchPayload(params *TaskParams, serverRootURL, workerID string) *DispatchPayload {
	env := make(map[string]string, len(params.EnvVars))
	for _, entry := range params.EnvVars {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}

	return &DispatchPayload{
		Version:       DispatchPayloadVersion,
		TaskID:        params.TaskID,
		ExecutionID:   params.ExecutionID,
		ServerRootURL: serverRootURL,
		WorkerID:      workerID,
		DockerImage:   params.DockerImage,
		BaseArgs:      params.BaseArgs,
		Env:           env,
		Sidecars:      params.Sidecars,
		Task:          params.Task,
	}
}
