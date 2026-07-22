package worker

import (
	"context"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// ExecuteOutcome describes how a backend handled a task in ExecuteTask.
type ExecuteOutcome int

const (
	// ExecuteOutcomeError means the task did not run to completion: it failed
	// to start, failed while running, or was cancelled. ExecuteResult.Error
	// carries the details.
	ExecuteOutcomeError ExecuteOutcome = iota
	// ExecuteOutcomeCompleted means the task was started and the backend
	// waited for it to finish successfully.
	//
	// The worker holds a concurrency slot until the task completes. After
	// the task completes, the worker also sends a terminal completion
	// message to the server.
	ExecuteOutcomeCompleted
	// ExecuteOutcomeSpawned means the task was started but the backend did not
	// wait for completion.
	//
	// The worker treats the task as accepted-but-not-finalized and must NOT
	// send a terminal completion message. Because ExecuteTask returns at
	// hand-off, a spawned task holds its concurrency slot only for the
	// duration of the dispatch, so MaxConcurrentTasks does not bound the
	// number of spawned tasks running remotely.
	ExecuteOutcomeSpawned
)

// ExecuteResult contains the outcome of a backend's ExecuteTask call.
//
// Error is set only when Outcome is ExecuteOutcomeError.
type ExecuteResult struct {
	Outcome ExecuteOutcome
	Error   error
}

func executeError(err error) ExecuteResult {
	return ExecuteResult{Outcome: ExecuteOutcomeError, Error: err}
}

func executeCompleted() ExecuteResult {
	return ExecuteResult{Outcome: ExecuteOutcomeCompleted}
}

func executeSpawned() ExecuteResult {
	return ExecuteResult{Outcome: ExecuteOutcomeSpawned}
}

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
	ExecuteTask(ctx context.Context, params *TaskParams) ExecuteResult
	// CancelTask makes a best-effort attempt to cancel a task. The worker
	// invokes it for every task cancellation, alongside cancelling the
	// ExecuteTask context.
	CancelTask(ctx context.Context, params *CancelParams) error
	// PreservesTasksOnShutdown reports whether active task execution units can
	// safely outlive the worker process during shutdown.
	PreservesTasksOnShutdown() bool
	// Shutdown cleans up backend resources.
	Shutdown(ctx context.Context)
}

// CancelParams carries the minimal, non-secret identifiers a backend needs to
// cancel a task. It deliberately excludes env/secrets so the worker need not
// retain secrets for the lifetime of a spawned task.
type CancelParams struct {
	TaskID      string
	ExecutionID string
}
