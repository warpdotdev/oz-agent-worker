package worker

import (
	"context"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

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

// ReattachableTask identifies an in-flight task execution unit that a
// replacement worker can resume monitoring after a restart or relocation.
type ReattachableTask struct {
	// TaskID is the logical run ID, used to report terminal task state back to
	// the control plane.
	TaskID string
	// ExecutionID identifies the concrete run execution. It falls back to
	// TaskID when the execution unit predates execution-scoped identity.
	ExecutionID string
	// BackendRef is an opaque backend-specific handle (e.g. a Kubernetes Job
	// name) used to re-establish monitoring for the existing execution unit.
	BackendRef string
}

// ReattachingBackend is an optional capability implemented by backends whose
// task execution units (for example Kubernetes Jobs) outlive the worker
// process. It lets a freshly started worker re-adopt and finalize executions
// that a previous worker pod left running when it was disrupted (for example by
// Karpenter consolidation or spot-instance reclamation of the worker node).
type ReattachingBackend interface {
	Backend
	// ListReattachableTasks returns the in-flight, non-terminal task execution
	// units this worker created in a previous lifetime that still need a worker
	// to monitor them to completion.
	ListReattachableTasks(ctx context.Context) ([]ReattachableTask, error)
	// MonitorTask re-establishes monitoring for an already-created execution
	// unit and blocks until it reaches a terminal state, returning the terminal
	// outcome the same way ExecuteTask does.
	MonitorTask(ctx context.Context, task ReattachableTask) error
}
