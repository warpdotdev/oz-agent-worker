package types

import (
	"encoding/json"
	"time"
)

// TaskFailure is the bounded, additive failure envelope sent with task_failed.
// The fields intentionally contain only normalized process/platform evidence;
// raw command output and unbounded error strings remain in Message.
type TaskFailure struct {
	Kind      string `json:"kind,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Signal    *int   `json:"signal,omitempty"`
	OOMKilled bool   `json:"oom_killed,omitempty"`
	Evicted   bool   `json:"evicted,omitempty"`
}

const (
	TaskFailureKindOperatorShutdown      = "operator_shutdown"
	TaskFailureKindRuntimeCrash          = "runtime_crash"
	TaskFailureKindWorkerDisconnect      = "worker_disconnect"
	TaskFailureKindOOM                   = "oom"
	TaskFailureKindEviction              = "eviction"
	TaskFailureKindInfrastructureTimeout = "infrastructure_timeout"
	TaskFailureKindBackendFailure        = "backend_failure"
	TaskFailureKindUserError             = "user_error"
)

// MessageType represents the type of WebSocket message
type MessageType string

const (
	MessageTypeTaskAssignment   MessageType = "task_assignment"
	MessageTypeTaskClaimed      MessageType = "task_claimed"
	MessageTypeTaskCompleted    MessageType = "task_completed"
	MessageTypeTaskFailed       MessageType = "task_failed"
	MessageTypeTaskRejected     MessageType = "task_rejected"
	MessageTypeTaskCancellation MessageType = "task_cancellation"
	MessageTypeHeartbeat        MessageType = "heartbeat"
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

// InstanceShape is the resolved compute size for a task. Containerized backends apply it
// as CPU/memory limits (Docker) or resource requests/limits (Kubernetes); the direct
// backend ignores it. Mirrors warp-server's runner instance shape JSON; unset axes are
// omitted on the wire, and backends additionally treat non-positive axes as unset.
type InstanceShape struct {
	Vcpus    int `json:"vcpus,omitempty"`
	MemoryGb int `json:"memory_gb,omitempty"`
}

// TaskAssignmentMessage is sent from server to worker when a task is available
type TaskAssignmentMessage struct {
	TaskID string `json:"task_id"`
	// ExecutionID identifies the concrete run execution being launched. It is
	// distinct from TaskID/Task.ID, which identify the logical run and can be
	// reused by follow-up or handoff executions.
	ExecutionID string `json:"execution_id,omitempty"`
	Task        *Task  `json:"task"`
	DockerImage string `json:"docker_image,omitempty"`
	// The "sidecar image" contains the warp agent binary and a couple other dependencies.
	SidecarImage string `json:"sidecar_image,omitempty"`
	// EnvVars contains environment variables to set in the container (e.g. WARP_API_KEY, GITHUB_ACCESS_TOKEN)
	EnvVars map[string]string `json:"env_vars,omitempty"`
	// AdditionalSidecars is a list of extra sidecar images to mount into the task container.
	AdditionalSidecars []SidecarMount `json:"additional_sidecars,omitempty"`
	// AdditionalOzArgs are server-resolved supplemental arguments for the oz
	// CLI. The worker forwards these tokens without deriving task semantics.
	AdditionalOzArgs []string `json:"additional_oz_args,omitempty"`
	// InstanceShape, when set, is the runner's resolved compute size. Containerized
	// backends size the task container/pod from it; omitted when the run has no explicit
	// runner instance shape, in which case the worker keeps its default sizing.
	InstanceShape *InstanceShape `json:"instance_shape,omitempty"`
}

// TaskClaimedMessage is sent from worker to server after successfully claiming a task
type TaskClaimedMessage struct {
	TaskID   string `json:"task_id"`
	WorkerID string `json:"worker_id"`
}

// TaskCompletedMessage tells the server to end the active run execution after a successful agent process exit.
type TaskCompletedMessage struct {
	TaskID    string     `json:"task_id"`
	Message   string     `json:"message"`
	TaskState *TaskState `json:"task_state,omitempty"`
}

// TaskFailedMessage is sent from worker to server if task launch fails
type TaskFailedMessage struct {
	TaskID    string       `json:"task_id"`
	Message   string       `json:"message"`
	TaskState *TaskState   `json:"task_state,omitempty"`
	Failure   *TaskFailure `json:"failure,omitempty"`
}

// TaskRejectedMessage is sent from worker to server when the worker cannot accept the task
// (e.g. at maximum concurrency). The server should keep the task queued rather than marking it failed.
type TaskRejectedMessage struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// TaskCancellationMessage is sent from server to worker to cancel an active task.
type TaskCancellationMessage struct {
	TaskID string `json:"task_id"`
}

// TaskState is the serialized terminal task state accepted by warp-server.
type TaskState string

const (
	TaskStateFailed    TaskState = "FAILED"
	TaskStateError     TaskState = "ERROR"
	TaskStateCancelled TaskState = "CANCELLED"
)

type TaskDefinition struct {
	Prompt string `json:"prompt"`
}

// Harness defines a third-party harness to run a cloud agent with.
type Harness struct {
	// Type is the name of the harness, e.g. "claude".
	Type *string `json:"type,omitempty"`
}

// IsOz returns true when the harness is the built-in Oz harness (nil, empty,
// or explicitly "oz"). Third-party harnesses (claude, codex, gemini, …) carry
// their own model on the harness config, so the top-level model_id should not
// be forwarded to them as --model.
func (h *Harness) IsOz() bool {
	return h == nil || h.Type == nil || *h.Type == "" || *h.Type == "oz"
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
	InferenceProviders        *InferenceProviders        `json:"inference_providers,omitempty"`
	SessionSharing            *SessionSharingConfig      `json:"session_sharing,omitempty"`
	SnapshotDisabled          *bool                      `json:"snapshot_disabled,omitempty"`
	SnapshotUploadTimeoutSecs *int                       `json:"snapshot_upload_timeout_secs,omitempty"`
	SnapshotScriptTimeoutSecs *int                       `json:"snapshot_script_timeout_secs,omitempty"`
}

// TaskOwner identifies the ownership scope of a task.
// Matches the server's PermissionSubjectAndID serialization.
type TaskOwner struct {
	Type string `json:"Type"` // "USER" or "TEAM"
	Id   int    `json:"Id"`
}

// IsTeamOwned returns true when the task owner is a team.
func (o *TaskOwner) IsTeamOwned() bool {
	return o != nil && o.Type == "TEAM"
}

// InferenceProviders carries per-provider inference configuration.
type InferenceProviders struct {
	Aws *AwsInferenceProvider `json:"aws,omitempty"`
}

// AwsInferenceProvider mirrors warp-server's snapshot-local representation of
// the AWS Bedrock block. When Disabled is false and RoleARN is non-empty, the
// worker forwards the role to the Warp client as --bedrock-inference-role and,
// when Region is set, pairs it with --bedrock-role-region so the STS
// AssumeRoleWithWebIdentity call targets the right regional endpoint.
type AwsInferenceProvider struct {
	Disabled bool   `json:"disabled,omitempty"`
	RoleARN  string `json:"role_arn,omitempty"`
	Region   string `json:"region,omitempty"`
}

// Task represents an ambient agent job.
type Task struct {
	ID                  string              `json:"id"`
	Title               string              `json:"title"`
	Definition          TaskDefinition      `json:"task_definition"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
	Owner               *TaskOwner          `json:"owner,omitempty"`
	AgentConfigSnapshot *AmbientAgentConfig `json:"agent_config_snapshot,omitempty"`
	AgentConversationID *string             `json:"agent_conversation_id,omitempty"`
}
