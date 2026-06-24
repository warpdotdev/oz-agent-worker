package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
)

// defaultDispatchTimeout bounds how long the operator dispatch command may run.
// The command is expected to hand the task off to a remote runtime and return
// promptly; it is not meant to stay alive for the task's lifetime.
const defaultDispatchTimeout = 60 * time.Second

// CommandBackendConfig configures the command backend, which dispatches tasks to
// an operator-owned runtime over any transport by invoking a shell command.
type CommandBackendConfig struct {
	// DispatchCommand is the shell command invoked (via /bin/sh -c) to dispatch
	// a task. It receives the DispatchPayload as JSON on stdin. Required.
	DispatchCommand string
	// CancelCommand, when set, is invoked (via /bin/sh -c) to best-effort cancel
	// a previously dispatched task.
	CancelCommand string
	// DispatchTimeout bounds the dispatch command's runtime. Zero uses defaultDispatchTimeout.
	DispatchTimeout time.Duration
	// Env contains extra environment variables exposed to the dispatch and
	// cancel commands. Task env/secrets are NOT placed here; they travel only in
	// the JSON payload on stdin.
	Env map[string]string
	// ServerRootURL and WorkerID are copied into the dispatch payload.
	ServerRootURL string
	WorkerID      string
}

// CommandBackend hands task execution to an operator-configured command, which
// dispatches the task to a remote runtime over any transport. Execution is
// fire-and-forget: a successful dispatch returns ErrTaskDispatched, and the
// remote oz agent reports terminal state to warp-server itself.
type CommandBackend struct {
	config CommandBackendConfig
}

var (
	_ Backend           = (*CommandBackend)(nil)
	_ CancelableBackend = (*CommandBackend)(nil)
)

// NewCommandBackend constructs a command backend, requiring a dispatch command.
func NewCommandBackend(ctx context.Context, config CommandBackendConfig) (*CommandBackend, error) {
	if config.DispatchCommand == "" {
		return nil, fmt.Errorf("command backend requires a dispatch command")
	}
	if config.DispatchTimeout <= 0 {
		config.DispatchTimeout = defaultDispatchTimeout
	}

	hasCancel := config.CancelCommand != ""
	log.Infof(ctx, "Using command backend (cancel command configured: %t, dispatch timeout: %s)", hasCancel, config.DispatchTimeout)

	return &CommandBackend{config: config}, nil
}

// ExecuteTask dispatches the task by invoking the configured dispatch command
// with the JSON payload on stdin. It returns ErrTaskDispatched on success.
func (b *CommandBackend) ExecuteTask(ctx context.Context, params *TaskParams) error {
	payload := NewDispatchPayload(params, b.config.ServerRootURL, b.config.WorkerID)
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonDispatchCommand, fmt.Errorf("failed to marshal dispatch payload: %w", err))
	}

	dctx, cancel := context.WithTimeout(ctx, b.config.DispatchTimeout)
	defer cancel()

	env := b.commandEnv([]string{
		fmt.Sprintf("OZ_TASK_ID=%s", params.TaskID),
		fmt.Sprintf("OZ_EXECUTION_ID=%s", params.ExecutionID),
		"OZ_WORKER_BACKEND=command",
		fmt.Sprintf("OZ_SERVER_ROOT_URL=%s", b.config.ServerRootURL),
		fmt.Sprintf("OZ_DOCKER_IMAGE=%s", params.DockerImage),
	})

	cmd := exec.CommandContext(dctx, "/bin/sh", "-c", b.config.DispatchCommand) // #nosec G204 -- dispatch command is explicit operator configuration.
	cmd.Stdin = bytes.NewReader(payloadJSON)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Infof(ctx, "Dispatching task %s via command backend", params.TaskID)
	if err := cmd.Run(); err != nil {
		// The parent context being cancelled means the worker is cancelling the
		// task (user/shutdown), not a dispatch failure.
		if ctx.Err() != nil {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err())
		}
		// The dispatch-scoped context expiring means the command overran its budget.
		if dctx.Err() == context.DeadlineExceeded {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonDispatchTimeout, fmt.Errorf("dispatch command timed out after %s: %w", b.config.DispatchTimeout, err))
		}
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonDispatchCommand, fmt.Errorf("dispatch command failed: %w", err))
	}

	log.Infof(ctx, "Task %s dispatched successfully", params.TaskID)
	return ErrTaskDispatched
}

// CancelTask best-effort cancels a dispatched task via the configured cancel
// command. It is a no-op when no cancel command is configured.
func (b *CommandBackend) CancelTask(ctx context.Context, params *CancelParams) error {
	if b.config.CancelCommand == "" {
		return nil
	}

	env := b.commandEnv([]string{
		fmt.Sprintf("OZ_TASK_ID=%s", params.TaskID),
		fmt.Sprintf("OZ_EXECUTION_ID=%s", params.ExecutionID),
		"OZ_WORKER_BACKEND=command",
	})

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", b.config.CancelCommand) // #nosec G204 -- cancel command is explicit operator configuration.
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Infof(ctx, "Cancelling dispatched task %s via command backend", params.TaskID)
	if err := cmd.Run(); err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonCancelCommand, fmt.Errorf("cancel command failed: %w", err))
	}
	return nil
}

// PreservesTasksOnShutdown reports true: dispatched tasks run on the operator's
// remote runtime, independent of this worker, so worker shutdown must not cancel them.
func (b *CommandBackend) PreservesTasksOnShutdown() bool { return true }

// Shutdown has nothing to clean up; the backend owns no local resources.
func (b *CommandBackend) Shutdown(ctx context.Context) {
	log.Debugf(ctx, "Command backend shutdown (no-op)")
}

// commandEnv builds the subprocess environment: the host environment overlaid
// with the well-known OZ_* vars and the operator-configured Env. Task env vars
// (which may include secrets) are deliberately excluded — they travel only in
// the JSON payload on stdin.
func (b *CommandBackend) commandEnv(wellKnown []string) []string {
	overlay := make([]string, 0, len(wellKnown)+len(b.config.Env))
	overlay = append(overlay, wellKnown...)
	for key, value := range b.config.Env {
		overlay = append(overlay, fmt.Sprintf("%s=%s", key, value))
	}
	return mergeEnvVars(os.Environ(), overlay)
}
