package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
)

func taskFailureLabels(err error) (metrics.TaskFailurePhase, metrics.TaskFailureReason) {
	if errors.Is(err, context.DeadlineExceeded) {
		return metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskTimeout
	}
	if errors.Is(err, context.Canceled) {
		return metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled
	}
	var failure *TaskFailure
	if errors.As(err, &failure) {
		return failure.metricsPhase, failure.metricsReason
	}
	return metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonUnknown
}

// userFacingTaskError returns a user-friendly error message for a task execution
// failure. Well-known infrastructure errors are translated into actionable text
// instead of exposing raw Go error strings.
func userFacingTaskError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "The task was interrupted due to an infrastructure issue (context canceled). This is typically transient — please try again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The task exceeded its maximum allowed execution time and was terminated. Consider breaking the task into smaller steps or increasing the timeout."
	default:
		return fmt.Sprintf("Failed to execute task: %v", err)
	}
}

// failureExitCode returns the exit status recorded on the failure, or 0 when
// none was observed.
func failureExitCode(err error) int {
	var failure *TaskFailure
	if errors.As(err, &failure) {
		return failure.exitCode
	}
	return 0
}
