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
// OOM, preemption) are translated into clear, actionable messages instead of exposing
// raw Go error strings.
func userFacingTaskError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "The task was interrupted due to an infrastructure issue (context canceled). This is typically transient — please try again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The task exceeded its maximum allowed execution time and was terminated. Consider breaking the task into smaller steps or increasing the timeout."
	}
	_, reason := taskFailureLabels(err)
	switch reason {
	case "container_oom":
		return "The task container was terminated because it ran out of memory (OOM killed). Consider increasing the runner memory limit or reducing the task's memory usage."
	case "pod_preempted":
		return "The task pod was preempted by the Kubernetes scheduler (evicted to make room for higher-priority workloads). The run was not checkpointed — please retry. Consider using higher-priority pods or nodes with sufficient capacity."
	default:
		return fmt.Sprintf("Failed to execute task: %v", err)
	}
}
