package worker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

type backendFailureError struct {
	phase   string
	reason  string
	err     error
	failure *types.TaskFailure
}

func newBackendFailure(phase, reason string, err error) error {
	return newBackendFailureWithMetadata(phase, reason, err, nil)
}

func newBackendFailureWithMetadata(phase, reason string, err error, failure *types.TaskFailure) error {
	if err == nil {
		return nil
	}
	return &backendFailureError{phase: phase, reason: reason, err: err, failure: failure}
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

// classifyFailure inspects a task execution error and returns a structured
// TaskFailure envelope describing why it failed. It distinguishes two root causes:
// signal exits from the agent subprocess (operator_shutdown, runtime_crash) and
// failures from backend infrastructure code — Docker, Kubernetes — that ran
// before or after the agent process (oom, eviction, user_error, backend_failure, etc.).
func classifyFailure(err error, source taskCancellationSource) *types.TaskFailure {
	failure := &types.TaskFailure{}
	var wrapped *backendFailureError
	if errors.As(err, &wrapped) && wrapped.failure != nil {
		copy := *wrapped.failure
		return &copy
	}

	if errors.Is(err, context.DeadlineExceeded) {
		failure.Cause = types.TaskFailureCauseInfrastructureTimeout
		return failure
	}
	if errors.Is(err, context.Canceled) {
		if source == taskCancellationSourceShutdown {
			failure.Cause = types.TaskFailureCauseOperatorShutdown
		} else {
			failure.Cause = types.TaskFailureCauseRuntimeCrash
		}
		return failure
	}

	// Path 1: the agent subprocess exited with a signal or signal-encoded exit code.
	// Extracts the numeric signal, then classifies as operator_shutdown when the
	// worker is gracefully shutting down and the signal is SIGTERM, or runtime_crash
	// for any other signal exit (SIGABRT, SIGKILL, etc.).
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			exitCode := status.ExitStatus()
			if status.Signaled() {
				sig := int(status.Signal())
				exitCode = 128 + sig
				failure.Signal = &sig
			} else if sig, ok := signalFromExitCode(exitCode); ok {
				failure.Signal = &sig
			}
			if exitCode >= 128 {
				failure.ExitCode = &exitCode
				if source == taskCancellationSourceShutdown && failure.Signal != nil && *failure.Signal == int(syscall.SIGTERM) {
					failure.Cause = types.TaskFailureCauseOperatorShutdown
				} else {
					failure.Cause = types.TaskFailureCauseRuntimeCrash
				}
				return failure
			}
		}
	}

	// Path 2: the failure came from backend infrastructure code (Docker, Kubernetes)
	// before or after the agent process, not from the agent process itself.
	// Classifies by the reason the backend recorded (OOM, timeout, bad image, etc.).
	if errors.As(err, &wrapped) {
		switch wrapped.reason {
		case metrics.TaskFailureReasonContainerOOM:
			failure.Cause = types.TaskFailureCauseOOM
			failure.OOMKilled = true
		case metrics.TaskFailureReasonActiveDeadline, metrics.TaskFailureReasonTaskTimeout:
			failure.Cause = types.TaskFailureCauseInfrastructureTimeout
		case metrics.TaskFailureReasonWorkspaceSetup, metrics.TaskFailureReasonSetupCommand, metrics.TaskFailureReasonInvalidImage:
			failure.Cause = types.TaskFailureCauseUserError
		default:
			failure.Cause = types.TaskFailureCauseBackendFailure
		}
		return failure
	}

	failure.Cause = types.TaskFailureCauseBackendFailure
	return failure
}

func signalFromExitCode(exitCode int) (int, bool) {
	if exitCode < 129 || exitCode > 192 {
		return 0, false
	}
	sig := exitCode - 128
	return sig, sig > 0
}

