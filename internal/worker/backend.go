package worker

import (
	"context"
	"fmt"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// runIDEnvVars, workerBackendEnvVars and the helpers beside them return a backend's
// well-known variables under both their OZ_ and WARP_ names.
//
// Nothing derives one name from the other: each pair is written out, from a single value,
// so retiring the OZ_ half later is a line deletion rather than an unwinding of aliasing
// machinery. TestBackendEnvPairsEveryOZName is what keeps a newly added variable from
// arriving under only one name.
func runIDEnvVars(taskID string) []string {
	return []string{
		fmt.Sprintf("OZ_RUN_ID=%s", taskID),
		fmt.Sprintf("WARP_RUN_ID=%s", taskID),
	}
}

func executionIDEnvVars(executionID string) []string {
	return []string{
		fmt.Sprintf("OZ_EXECUTION_ID=%s", executionID),
		fmt.Sprintf("WARP_EXECUTION_ID=%s", executionID),
	}
}

func workerBackendEnvVars(backend string) []string {
	return []string{
		fmt.Sprintf("OZ_WORKER_BACKEND=%s", backend),
		fmt.Sprintf("WARP_WORKER_BACKEND=%s", backend),
	}
}

func workspaceRootEnvVars(workspaceRoot string) []string {
	return []string{
		fmt.Sprintf("OZ_WORKSPACE_ROOT=%s", workspaceRoot),
		fmt.Sprintf("WARP_WORKSPACE_ROOT=%s", workspaceRoot),
	}
}

func environmentFileEnvVars(environmentFile string) []string {
	return []string{
		fmt.Sprintf("OZ_ENVIRONMENT_FILE=%s", environmentFile),
		fmt.Sprintf("WARP_ENVIRONMENT_FILE=%s", environmentFile),
	}
}

func serverRootURLEnvVars(serverRootURL string) []string {
	return []string{
		fmt.Sprintf("OZ_SERVER_ROOT_URL=%s", serverRootURL),
		fmt.Sprintf("WARP_SERVER_ROOT_URL=%s", serverRootURL),
	}
}

func dockerImageEnvVars(dockerImage string) []string {
	return []string{
		fmt.Sprintf("OZ_DOCKER_IMAGE=%s", dockerImage),
		fmt.Sprintf("WARP_DOCKER_IMAGE=%s", dockerImage),
	}
}

// concatEnvVars flattens the per-variable pairs above into one KEY=VALUE slice.
func concatEnvVars(groups ...[]string) []string {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	envVars := make([]string, 0, total)
	for _, group := range groups {
		envVars = append(envVars, group...)
	}
	return envVars
}

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
	// OzLifecycleHooks is the authenticated, non-secret hook context forwarded
	// to the embedded first-party Oz runtime.
	OzLifecycleHooks *types.OzLifecycleHooksContext

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

	// SetupEvents reports worker-observed setup phase durations to warp-server.
	// It may be nil, and all of its methods are safe to call on a nil reporter.
	SetupEvents *setupEventReporter
}

// Backend defines the interface for task execution backends.
type Backend interface {
	// ExecuteTask runs the agent for the given task parameters. Execution
	// failures are surfaced as ExecuteOutcomeError with an error that is (or
	// wraps) *TaskFailure.
	ExecuteTask(ctx context.Context, params *TaskParams) ExecuteResult
	// CancelTask makes a best-effort attempt to cancel a task. The worker
	// invokes it for every task cancellation, alongside cancelling the
	// ExecuteTask context.
	CancelTask(ctx context.Context, params *CancelParams) error
	// PreservesTasksOnShutdown reports whether active task execution units can
	// safely outlive the worker process during shutdown.
	PreservesTasksOnShutdown() bool
	// SupportsOzLifecycleHooks reports whether the backend preserves Oz argv,
	// sandbox placement, and task cancellation for hook-enabled tasks.
	SupportsOzLifecycleHooks() bool
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

// TaskFailure is the structured error backends return from ExecuteTask when
// task execution fails. Backends record only the facts they observe (metrics
// labels and the failing process's exit status); the worker adds lifecycle
// context backends cannot see (graceful shutdown) before reporting the
// failure to warp-server.
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
