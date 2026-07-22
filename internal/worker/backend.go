package worker

import (
	"context"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
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

	// InstanceShape, when non-nil, is the resolved compute size for this task.
	// Containerized backends apply it as CPU/memory limits (Docker) or resource
	// requests/limits (Kubernetes). Backends that cannot enforce a shape (direct) ignore it.
	InstanceShape *types.InstanceShape
}

// Backend defines the interface for task execution backends.
type Backend interface {
	// ExecuteTask runs the agent for the given task parameters. Execution
	// failures are returned as (or wrapped around) *TaskFailure.
	ExecuteTask(ctx context.Context, params *TaskParams) error
	// PreservesTasksOnShutdown reports whether active task execution units can
	// safely outlive the worker process during shutdown.
	PreservesTasksOnShutdown() bool
	// Shutdown cleans up backend resources.
	Shutdown(ctx context.Context)
}

// TaskFailure is the structured error backends return from ExecuteTask when
// task execution fails. Backends record only the facts they observe (metrics
// labels and the failing process's exit status); the failure cause reported
// to warp-server is derived from these facts plus lifecycle context backends
// cannot see.
type TaskFailure struct {
	// metricsPhase and metricsReason label the worker's task-failure metrics.
	metricsPhase  metrics.TaskFailurePhase
	metricsReason metrics.TaskFailureReason
	// exitCode is the failing process's exit status, normalized to 128+signal
	// for signal terminations. Zero means no exit status was observed.
	exitCode int
	err      error
}

func (e *TaskFailure) Error() string {
	return e.err.Error()
}

func (e *TaskFailure) Unwrap() error {
	return e.err
}

func newBackendFailure(metricsPhase metrics.TaskFailurePhase, metricsReason metrics.TaskFailureReason, err error) error {
	if err == nil {
		return nil
	}
	return &TaskFailure{metricsPhase: metricsPhase, metricsReason: metricsReason, err: err}
}

// newBackendFailureWithExitCode additionally records the failing process's
// exit status, normalized to 128+signal for signal terminations.
func newBackendFailureWithExitCode(metricsPhase metrics.TaskFailurePhase, metricsReason metrics.TaskFailureReason, err error, exitCode int) error {
	if err == nil {
		return nil
	}
	return &TaskFailure{metricsPhase: metricsPhase, metricsReason: metricsReason, err: err, exitCode: exitCode}
}
