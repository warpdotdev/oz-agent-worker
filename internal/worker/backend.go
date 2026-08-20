package worker

import (
	"context"
	"strings"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

const (
	ozEnvVarPrefix   = "OZ_"
	warpEnvVarPrefix = "WARP_"
)

// withWarpAliases returns envVars followed by a WARP_-prefixed alias of every
// OZ_-prefixed entry, so a backend's well-known variables always reach the processes
// it starts under both names. Entries are in KEY=VALUE form; one without an `=` has no
// name to alias and is passed through untouched.
//
// Deriving the aliases here rather than writing them out per variable means an OZ_
// variable added to a backend's environment is aliased without a second edit.
//
// Only ever pass a worker-authored set. It aliases whatever it is handed, so running it
// over a slice that operator-configured or server-supplied task env has already been
// merged into would mirror those too.
func withWarpAliases(envVars []string) []string {
	aliased := make([]string, 0, 2*len(envVars))
	aliased = append(aliased, envVars...)
	for _, entry := range envVars {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if suffix, ok := strings.CutPrefix(name, ozEnvVarPrefix); ok {
			aliased = append(aliased, warpEnvVarPrefix+suffix+"="+value)
		}
	}
	return aliased
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
