package worker

import (
	"context"
	"errors"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// ErrTaskDispatched is returned by Backend.ExecuteTask to signal a successful
// fire-and-forget dispatch to a remote runtime. The worker treats the task as
// accepted-but-not-finalized: it must NOT send a terminal completion message,
// because the remote runtime/agent owns terminal reporting back to warp-server.
var ErrTaskDispatched = errors.New("task dispatched to remote runtime")

// TaskParams contains pre-processed task parameters common to all backends.
// This provides a layer of abstraction between the wire-format TaskAssignmentMessage
// and the backend interface, so backends don't need to handle common concerns like
// resolving environment variables, choosing default images, or building base CLI args.
type TaskParams struct {
	TaskID      string
	ExecutionID string
	Task        *types.Task

	// EnvVars contains pre-resolved common environment variables (TASK_ID, Git config,
	// assignment env vars). Backends append their own config-specific env vars.
	EnvVars []string

	// BaseArgs contains the base CLI arguments for the agent command
	// (agent run --share ... --task-id ... --server-root-url ... + augmented args).
	// Backends prepend their invocation prefix and append backend-specific flags.
	BaseArgs []string

	// DockerImage is the resolved Docker image name (with default fallback applied).
	// Backends that don't use Docker can ignore this.
	DockerImage string

	// Sidecars is the unified list of sidecar mounts for this task. The Warp agent
	// sidecar (mounted at /agent) is included as the first entry when present, followed
	// by any additional sidecars from the assignment. Backends that don't use sidecars
	// can ignore this.
	Sidecars []types.SidecarMount

	// InstanceShape, when non-nil, is the resolved compute size for this task.
	// Containerized backends apply it as CPU/memory limits (Docker) or resource
	// requests/limits (Kubernetes). Backends that cannot enforce a shape (direct) ignore it.
	InstanceShape *types.InstanceShape
}

// Backend defines the interface for task execution backends.
type Backend interface {
	// ExecuteTask runs the agent for the given task parameters.
	ExecuteTask(ctx context.Context, params *TaskParams) error
	// PreservesTasksOnShutdown reports whether active task execution units can
	// safely outlive the worker process during shutdown.
	PreservesTasksOnShutdown() bool
	// Shutdown cleans up backend resources.
	Shutdown(ctx context.Context)
}

// CancelParams carries the minimal, non-secret identifiers needed to attempt
// cancellation of a task that has already been handed off. It deliberately
// excludes env/secrets so the worker need not retain secrets for the lifetime
// of a dispatched task.
type CancelParams struct {
	TaskID      string
	ExecutionID string
}

// CancelableBackend is implemented by backends that can attempt to cancel a
// task that has already been handed off (e.g. fire-and-forget dispatch). The
// worker invokes CancelTask when it receives a cancellation for a task that is
// no longer executing locally.
type CancelableBackend interface {
	CancelTask(ctx context.Context, params *CancelParams) error
}
