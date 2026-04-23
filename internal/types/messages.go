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
	MessageTypeTaskCompleted  MessageType = "task_completed"
	MessageTypeTaskFailed     MessageType = "task_failed"
	MessageTypeTaskRejected   MessageType = "task_rejected"
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
	// ConversationID is the UUID of an existing AI conversation to continue.
	ConversationID string `json:"conversation_id,omitempty"`
}

// TaskClaimedMessage is sent from worker to server after successfully claiming a task
type TaskClaimedMessage struct {
	TaskID   string `json:"task_id"`
	WorkerID string `json:"worker_id"`
}

// TaskCompletedMessage tells the server to end the active run execution after a successful agent process exit.
type TaskCompletedMessage struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

// TaskFailedMessage is sent from worker to server if task launch fails
type TaskFailedMessage struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

// TaskRejectedMessage is sent from worker to server when the worker cannot accept the task
// (e.g. at maximum concurrency). The server should keep the task queued rather than marking it failed.
type TaskRejectedMessage struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

type TaskDefinition struct {
	Prompt string `json:"prompt"`
}

// Harness defines a third-party harness to run a cloud agent with.
type Harness struct {
	// Type is the name of the harness, e.g. "claude".
	Type *string `json:"type,omitempty"`
}

// HarnessAuthSecrets holds authentication secrets for third-party harnesses.
// Only the secret for the harness specified gets injected into the environment.
type HarnessAuthSecrets struct {
	// ClaudeAuthSecretName is the name of a managed secret for Claude Code harness authentication.
	ClaudeAuthSecretName *string `json:"claude_auth_secret_name,omitempty"`
}

// AccessLevel is the serialized access-level string used inside SessionSharingConfig.
// Values mirror warp-server's model/types/enums.AccessLevel JSON representation.
type AccessLevel string

const (
	AccessLevelViewer AccessLevel = "VIEWER"
	AccessLevelEditor AccessLevel = "EDITOR"
)

// SessionSharingConfig mirrors warp-server's sources.SessionSharingConfig and
// carries the session-sharing choices snapshotted onto the run.
type SessionSharingConfig struct {
	// PublicAccess, when set, causes the worker to emit --share public:<level>
	// so the bundled Warp client applies an anyone-with-link ACL after the
	// shared session bootstraps.
	PublicAccess *AccessLevel `json:"public_access,omitempty"`
}

// AmbientAgentConfig represents the agent configuration.
type AmbientAgentConfig struct {
	EnvironmentID             *string                    `json:"environment_id,omitempty"`
	BasePrompt                *string                    `json:"base_prompt,omitempty"`
	ModelID                   *string                    `json:"model_id,omitempty"`
	ProfileID                 *string                    `json:"profile_id,omitempty"`
	SkillSpec                 *string                    `json:"skill_spec,omitempty"`
	MCPServers                map[string]json.RawMessage `json:"mcp_servers,omitempty"`
	ComputerUseEnabled        *bool                      `json:"computer_use_enabled,omitempty"`
	IdleTimeoutMinutes        *int                       `json:"idle_timeout_minutes,omitempty"`
	Harness                   *Harness                   `json:"harness,omitempty"`
	HarnessAuthSecrets        *HarnessAuthSecrets        `json:"harness_auth_secrets,omitempty"`
	BedrockInferenceRole      *string                    `json:"bedrock_inference_role,omitempty"`
	SessionSharing            *SessionSharingConfig      `json:"session_sharing,omitempty"`
	SnapshotDisabled          *bool                      `json:"snapshot_disabled,omitempty"`
	SnapshotUploadTimeoutSecs *int                       `json:"snapshot_upload_timeout_secs,omitempty"`
	SnapshotScriptTimeoutSecs *int                       `json:"snapshot_script_timeout_secs,omitempty"`
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
