package types

import (
	"encoding/json"
	"time"
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

// FailureDetails contains backend-specific structured diagnostics.
type FailureDetails struct {
	Kubernetes *KubernetesFailureDetails `json:"kubernetes,omitempty"`
}

// KubernetesFailureDetails contains bounded Kubernetes metadata captured when a task fails.
type KubernetesFailureDetails struct {
	SchemaVersion     int                                `json:"schema_version"`
	ObservationSource string                             `json:"observation_source,omitempty"`
	ObservedAt        time.Time                          `json:"observed_at"`
	Job               *KubernetesJobFailureDetails       `json:"job,omitempty"`
	Pod               *KubernetesPodFailureDetails       `json:"pod,omitempty"`
	Container         *KubernetesContainerFailureDetails `json:"container,omitempty"`
	Events            []KubernetesEventDetails           `json:"events,omitempty"`
	Truncated         bool                               `json:"truncated,omitempty"`
}

// KubernetesJobFailureDetails identifies a worker-created Job and its terminal conditions.
type KubernetesJobFailureDetails struct {
	Name       string                       `json:"name,omitempty"`
	UID        string                       `json:"uid,omitempty"`
	Conditions []KubernetesConditionDetails `json:"conditions,omitempty"`
}

// KubernetesPodFailureDetails identifies a task Pod and its structured status.
type KubernetesPodFailureDetails struct {
	Name              string                       `json:"name,omitempty"`
	UID               string                       `json:"uid,omitempty"`
	Phase             string                       `json:"phase,omitempty"`
	Reason            string                       `json:"reason,omitempty"`
	DeletionTimestamp *time.Time                   `json:"deletion_timestamp,omitempty"`
	Conditions        []KubernetesConditionDetails `json:"conditions,omitempty"`
}

// KubernetesConditionDetails contains condition fields that Kubernetes represents structurally.
type KubernetesConditionDetails struct {
	Type               string     `json:"type,omitempty"`
	Status             string     `json:"status,omitempty"`
	Reason             string     `json:"reason,omitempty"`
	LastTransitionTime *time.Time `json:"last_transition_time,omitempty"`
}

// KubernetesContainerFailureDetails identifies the failing container and its current state.
type KubernetesContainerFailureDetails struct {
	Kind               string     `json:"kind,omitempty"`
	Name               string     `json:"name,omitempty"`
	State              string     `json:"state,omitempty"`
	WaitingReason      string     `json:"waiting_reason,omitempty"`
	TerminationReason  string     `json:"termination_reason,omitempty"`
	RawExitCode        *int32     `json:"raw_exit_code,omitempty"`
	NormalizedExitCode *int       `json:"normalized_exit_code,omitempty"`
	Signal             *int32     `json:"signal,omitempty"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
}

// KubernetesEventDetails contains privacy-filtered Event metadata.
type KubernetesEventDetails struct {
	Type            string     `json:"type,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	Count           int32      `json:"count,omitempty"`
	FirstObservedAt *time.Time `json:"first_observed_at,omitempty"`
	LastObservedAt  *time.Time `json:"last_observed_at,omitempty"`
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
	TaskID      string     `json:"task_id"`
	ExecutionID string     `json:"execution_id,omitempty"`
	Message     string     `json:"message"`
	TaskState   *TaskState `json:"task_state,omitempty"`
}

// TaskFailedMessage is sent from worker to server if task launch fails.
// FailureReason is the worker-classified failure reason (a
// metrics.TaskFailureReason value) and ExitCode is the failing process's
// exit status normalized to 128+signal.
type TaskFailedMessage struct {
	TaskID         string          `json:"task_id"`
	ExecutionID    string          `json:"execution_id,omitempty"`
	Message        string          `json:"message"`
	TaskState      *TaskState      `json:"task_state,omitempty"`
	FailureReason  string          `json:"failure_reason,omitempty"`
	ExitCode       int             `json:"exit_code,omitempty"`
	FailureDetails *FailureDetails `json:"failure_details,omitempty"`
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

// RepositoryHeadType identifies how a prepared repository HEAD is resolved.
type RepositoryHeadType string

const (
	RepositoryHeadTypeCommitSHA RepositoryHeadType = "COMMIT_SHA"
	RepositoryHeadTypeBranch    RepositoryHeadType = "BRANCH"
)

// RepositoryHeadRef identifies a repository HEAD by type and value.
type RepositoryHeadRef struct {
	Type  RepositoryHeadType `json:"type"`
	Value string             `json:"value"`
}

// RepositoryIdentity identifies one repository independently of checkout path spelling.
type RepositoryIdentity struct {
	CodeForge string `json:"code_forge"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
}

// RepositoryHeadOverride describes a server-computed repository checkout override.
// The server has already validated and frozen these values (e.g. for benchmark
// trials); the worker forwards them to the CLI as-is via --repository-head-override-json.
type RepositoryHeadOverride struct {
	CodeForge string            `json:"code_forge"`
	RepoOwner string            `json:"repo_owner"`
	RepoName  string            `json:"repo_name"`
	Head      RepositoryHeadRef `json:"head"`
	// CloneFrom, when set, identifies a different repository to clone from while
	// keeping this repository's own name/path for the checkout (substitution).
	CloneFrom      *RepositoryIdentity `json:"clone_from,omitempty"`
	PreserveOrigin bool                `json:"preserve_origin,omitempty"`
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
	ComputerUseModelID        *string                    `json:"computer_use_model_id,omitempty"`
	IdleTimeoutMinutes        *int                       `json:"idle_timeout_minutes,omitempty"`
	Harness                   *Harness                   `json:"harness,omitempty"`
	HarnessAuthSecrets        *HarnessAuthSecrets        `json:"harness_auth_secrets,omitempty"`
	InferenceProviders        *InferenceProviders        `json:"inference_providers,omitempty"`
	SessionSharing            *SessionSharingConfig      `json:"session_sharing,omitempty"`
	SnapshotDisabled          *bool                      `json:"snapshot_disabled,omitempty"`
	SnapshotUploadTimeoutSecs *int                       `json:"snapshot_upload_timeout_secs,omitempty"`
	SnapshotScriptTimeoutSecs *int                       `json:"snapshot_script_timeout_secs,omitempty"`
	// RepositoryHeadOverrides identify repositories and optionally configure alternate
	// clone remotes and origin retention. Currently only populated for benchmark trials.
	RepositoryHeadOverrides []RepositoryHeadOverride `json:"repository_head_overrides,omitempty"`
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
