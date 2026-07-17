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
// failure. Well-known infrastructure errors (context cancellation, deadline exceeded,
// and specific backend failure reasons) are translated into clear, actionable messages
// instead of exposing raw Go error strings.
func userFacingTaskError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "The task was interrupted due to an infrastructure issue (context canceled). This is typically transient — please try again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The task exceeded its maximum allowed execution time and was terminated. Consider breaking the task into smaller steps or increasing the timeout."
	default:
		var failure *backendFailureError
		if errors.As(err, &failure) {
			switch failure.reason {
			case metrics.TaskFailureReasonActiveDeadline:
				return "The agent sandbox did not complete before the job deadline was exceeded. This may indicate slow container image pulls, cluster resource pressure, or sandbox startup delays — please try again."
			case metrics.TaskFailureReasonContainerCreate, metrics.TaskFailureReasonContainerStart:
				return "The agent sandbox failed to start. This may indicate a container image issue, insufficient cluster resources, or a configuration problem — please try again."
			case metrics.TaskFailureReasonSidecarPrep:
				return "The agent sandbox failed to prepare its required dependencies. This may indicate a container image issue or a network connectivity problem — please try again."
			case metrics.TaskFailureReasonImagePull:
				return "The agent sandbox could not pull its container image. Verify the image is accessible from the worker and try again."
			case metrics.TaskFailureReasonUnschedulable:
				return "The agent sandbox could not be scheduled due to insufficient cluster resources. Check available capacity and try again."
			case metrics.TaskFailureReasonContainerOOM:
				return "The agent sandbox ran out of memory and was terminated. Consider breaking the task into smaller steps or requesting a larger runner."
			}
		}
		return fmt.Sprintf("Failed to execute task: %v", err)
	}
}
