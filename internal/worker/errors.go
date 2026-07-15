package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
)

type backendFailureError struct {
	phase  string
	reason string
	err    error
}

func newBackendFailure(phase, reason string, err error) error {
	if err == nil {
		return nil
	}
	return &backendFailureError{phase: phase, reason: reason, err: err}
}

func (e *backendFailureError) Error() string {
	return e.err.Error()
}

func (e *backendFailureError) Unwrap() error {
	return e.err
}

func taskFailureLabels(err error) (phase, reason string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskTimeout
	}
	if errors.Is(err, context.Canceled) {
		return metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled
	}
	var failure *backendFailureError
	if errors.As(err, &failure) {
		return failure.phase, failure.reason
	}
	return metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonUnknown
}

// userFacingTaskError returns a user-friendly error message for a task execution
// failure. Well-known infrastructure errors (context cancellation, deadline exceeded)
// are translated into clear, actionable messages instead of exposing raw Go error strings.
func userFacingTaskError(err error) string {
	var failure *backendFailureError
	switch {
	case errors.Is(err, context.Canceled):
		return "The task was interrupted due to an infrastructure issue (context canceled). This is typically transient — please try again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The task exceeded its maximum allowed execution time and was terminated. Consider breaking the task into smaller steps or increasing the timeout."
	case errors.As(err, &failure) && failure.reason == metrics.TaskFailureReasonAgentOOM:
		return "The agent process was unexpectedly killed (possible out-of-memory)." +
			" Check VM memory usage and kernel OOM logs (dmesg)." +
			" Consider reducing parallelism (e.g. CARGO_BUILD_JOBS=1) or increasing available memory."
	default:
		return fmt.Sprintf("Failed to execute task: %v", err)
	}
}
