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
	// MessageTypeDebugArchiveLogsRequested is sent from server to worker to ask
	// the process that executed an assignment for a bounded log snapshot.
	MessageTypeDebugArchiveLogsRequested MessageType = "debug_archive_logs_requested"
	// MessageTypeDebugArchiveLogsUploaded is the owning worker's acknowledgement
	// of a debug-archive log request.
	MessageTypeDebugArchiveLogsUploaded MessageType = "debug_archive_logs_uploaded"
)

// WorkerVersionHeader carries the worker's build-time version on every
// authenticated WebSocket dial so warp-server can snapshot the exact build
// that claims an execution.
const WorkerVersionHeader = "X-Warp-Worker-Version"

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

// TaskFailedMessage is sent from worker to server if task launch fails.
// FailureReason is the worker-classified failure reason (a
// metrics.TaskFailureReason value) and ExitCode is the failing process's
// exit status normalized to 128+signal.
type TaskFailedMessage struct {
	TaskID        string     `json:"task_id"`
	Message       string     `json:"message"`
	TaskState     *TaskState `json:"task_state,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	ExitCode      int        `json:"exit_code,omitempty"`
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

// DebugArchiveProtocolVersion is the only worker-log protocol version this
// worker implements. A request carrying any other version is refused with
// DebugArchiveReasonUnsupportedProtocolVersion.
const DebugArchiveProtocolVersion = 1

// DebugArchiveFormatNDJSON is the only snapshot encoding this worker produces.
const DebugArchiveFormatNDJSON = "application/x-ndjson"

// Debug-archive acknowledgement outcomes.
const (
	DebugArchiveOutcomeUploaded    = "uploaded"
	DebugArchiveOutcomeUnavailable = "unavailable"
	DebugArchiveOutcomeFailed      = "failed"
)

// Debug-archive capture statuses, reported only alongside an uploaded outcome.
const (
	DebugArchiveCaptureComplete = "complete"
	DebugArchiveCapturePartial  = "partial"
)

// Debug-archive reason codes. These are the complete, stable set warp-server
// keys off for non-upload outcomes.
const (
	DebugArchiveReasonUnsupportedProtocolVersion    = "unsupported_protocol_version"
	DebugArchiveReasonUnsupportedContentTransformer = "unsupported_content_transformer"
	DebugArchiveReasonInvalidRequest                = "invalid_request"
	DebugArchiveReasonRequestExpired                = "request_expired"
	DebugArchiveReasonBackendNotSupported           = "backend_not_supported"
	DebugArchiveReasonResourceNotReady              = "resource_not_ready"
	DebugArchiveReasonResourceNotFound              = "resource_not_found"
	DebugArchiveReasonCleanupGraceExpired           = "cleanup_grace_expired"
	DebugArchiveReasonCaptureUnavailable            = "capture_unavailable"
	DebugArchiveReasonSnapshotFailed                = "snapshot_failed"
	DebugArchiveReasonUploadRejected                = "upload_rejected"
	DebugArchiveReasonUploadExpired                 = "upload_expired"
	DebugArchiveReasonUploadFailed                  = "upload_failed"
	DebugArchiveReasonWorkerShuttingDown            = "worker_shutting_down"
	DebugArchiveReasonRequestCapacityExhausted      = "request_capacity_exhausted"
)

// Debug-archive warning codes. A partial capture reports these so warp-server
// can mark a source partial without parsing provider text.
const (
	DebugArchiveWarningContainerLogsUnavailable   = "container_logs_unavailable"
	DebugArchiveWarningPreviousLogsUnavailable    = "previous_logs_unavailable"
	DebugArchiveWarningOutputDropped              = "output_dropped"
	DebugArchiveWarningProviderSnapshotIncomplete = "provider_snapshot_incomplete"
)

// ContentTransformerDescriptor names the versioned transform applied to log
// message data while the snapshot is encoded. V1 defines only the byte
// preserving "noop" transformer.
type ContentTransformerDescriptor struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
}

// UploadTarget is the provider-neutral destination warp-server signs for one
// immutable snapshot object. Its URL, headers, and multipart fields are
// credential-bearing and must never be logged or echoed in an acknowledgement.
type UploadTarget struct {
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	Headers         map[string]string `json:"headers,omitempty"`
	MultipartFields map[string]string `json:"multipart_fields,omitempty"`
}

// DebugArchiveLogsRequestedMessage is the server's request for a bounded log
// snapshot of one exact execution. Unknown fields are ignored so a newer
// server can add optional data without breaking this worker.
type DebugArchiveLogsRequestedMessage struct {
	ProtocolVersion    int                          `json:"protocol_version"`
	RequestID          string                       `json:"request_id"`
	ArchiveID          string                       `json:"archive_id"`
	CollectionID       string                       `json:"collection_id"`
	RunID              string                       `json:"run_id"`
	ExecutionID        string                       `json:"execution_id"`
	RequestedFormat    string                       `json:"requested_format"`
	ExpiresAt          time.Time                    `json:"expires_at"`
	MaxBytes           int64                        `json:"max_bytes"`
	ContentTransformer ContentTransformerDescriptor `json:"content_transformer"`
	UploadTarget       UploadTarget                 `json:"upload_target"`
}

// DebugArchiveLogsUploadedMessage is the owning worker's acknowledgement. Byte,
// checksum, and capture fields are present only for an uploaded outcome; the
// reason code and sanitized message describe every other outcome.
type DebugArchiveLogsUploadedMessage struct {
	ProtocolVersion           int      `json:"protocol_version"`
	RequestID                 string   `json:"request_id"`
	ArchiveID                 string   `json:"archive_id"`
	CollectionID              string   `json:"collection_id"`
	RunID                     string   `json:"run_id"`
	ExecutionID               string   `json:"execution_id"`
	Outcome                   string   `json:"outcome"`
	BackendKind               string   `json:"backend_kind,omitempty"`
	Bytes                     int64    `json:"bytes,omitempty"`
	CRC32C                    string   `json:"crc32c,omitempty"`
	SHA256                    string   `json:"sha256,omitempty"`
	Truncated                 bool     `json:"truncated"`
	ContentTransformerVersion int      `json:"content_transformer_version,omitempty"`
	CaptureStatus             string   `json:"capture_status,omitempty"`
	WarningCodes              []string `json:"warning_codes"`
	ReasonCode                string   `json:"reason_code"`
	Message                   string   `json:"message"`
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
