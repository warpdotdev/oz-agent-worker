package worker

import (
	"context"
	"errors"
	"time"

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

func recordBackendOperation(operation string, start time.Time, err error) {
	result := metrics.BackendOperationResultSucceeded
	if err != nil {
		if errors.Is(err, context.Canceled) {
			result = metrics.BackendOperationResultCancelled
		} else {
			result = metrics.BackendOperationResultFailed
		}
	}
	metrics.RecordBackendOperation(operation, result, time.Since(start))
}
