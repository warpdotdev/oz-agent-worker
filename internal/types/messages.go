package types

import (
	"encoding/json"
	"time"
)

// MessageType represents the type of WebSocket message
type MessageType string

const (
	MessageTypeTaskAssignment MessageType = "task_assignment"
	MessageTypeTaskClaimed    MessageType = "task_claimed"
	MessageTypeTaskFailed     MessageType = "task_failed"
	MessageTypeHeartbeat      MessageType = "heartbeat"
)

// WebSocketMessage is the base structure for all WebSocket messages
type WebSocketMessage struct {
	Type MessageType     `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// SidecarMount describes an additional sidecar image to mount into the task container.
type SidecarMount struct {
	Image     string `json:"image"`      // Docker image to pull.
	MountPath string `json:"mount_path"` // Path to mount the sidecar filesystem in the task container.
	ReadWrite bool   `json:"read_write"` // If false (default), the mount is read-only.
}

// TaskAssignmentMessage is sent from server to worker when a task is available
type TaskAssignmentMessage struct {
	TaskID      string `json:"task_id"`
	Task        *Task  `json:"task"`
	DockerImage string `json:"docker_image,omitempty"`
	// The "sidecar image" contains the warp agent binary and a couple other dependencies.
	SidecarImage string `json:"sidecar_image,omitempty"`
	// EnvVars contains environment variables to set in the container (e.g. WARP_API_KEY, GITHUB_ACCESS_TOKEN)
	EnvVars map[string]string `json:"env_vars,omitempty"`
	// AdditionalSidecars is a list of extra sidecar images to mount into the task container.
	AdditionalSidecars []SidecarMount `json:"additional_sidecars,omitempty"`
}

// TaskClaimedMessage is sent from worker to server after successfully claiming a task
type TaskClaimedMessage struct {
	TaskID   string `json:"task_id"`
	WorkerID string `json:"worker_id"`
}

// TaskFailedMessage is sent from worker to server if task launch fails
type TaskFailedMessage struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

type TaskDefinition struct {
	Prompt string `json:"prompt"`
}

// AmbientAgentConfig represents the agent configuration
type AmbientAgentConfig struct {
	EnvironmentID      *string                    `json:"environment_id,omitempty"`
	BasePrompt         *string                    `json:"base_prompt,omitempty"`
	ModelID            *string                    `json:"model_id,omitempty"`
	ProfileID          *string                    `json:"profile_id,omitempty"`
	SkillSpec          *string                    `json:"skill_spec,omitempty"`
	MCPServers         map[string]json.RawMessage `json:"mcp_servers,omitempty"`
	ComputerUseEnabled *bool                      `json:"computer_use_enabled,omitempty"`
}

// Task represents an ambient agent job.
type Task struct {
	ID                  string              `json:"id"`
	Title               string              `json:"title"`
	Definition          TaskDefinition      `json:"task_definition"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
	AgentConfigSnapshot *AmbientAgentConfig `json:"agent_config_snapshot,omitempty"`
}
