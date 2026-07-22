package worker

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
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

// classifyFailure maps a task execution failure to the failure cause reported
// to warp-server. Backends classify the mechanics they can observe (OOM,
// eviction, exit codes) on the TaskFailure they return; this overlays
// worker-lifecycle context backends cannot see — whether a cancellation or
// agent signal exit happened because the worker itself was shutting down.
func classifyFailure(err error, source taskCancellationSource) types.TaskFailureCause {
	var failure *TaskFailure
	if errors.As(err, &failure) && failure.cause != "" {
		return failure.cause
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return types.TaskFailureCauseInfrastructureTimeout
	}
	if errors.Is(err, context.Canceled) {
		if source == taskCancellationSourceShutdown {
			return types.TaskFailureCauseOperatorShutdown
		}
		return types.TaskFailureCauseRuntimeCrash
	}

	// Signal-coded agent exit (128+N): SIGTERM while the worker is gracefully
	// shutting down is the operator stopping the worker; any other signal exit
	// (SIGABRT, SIGKILL, etc.) is a crash.
	if failure != nil && failure.agentExitCode >= 128 {
		if source == taskCancellationSourceShutdown && failure.agentExitCode-128 == int(syscall.SIGTERM) {
			return types.TaskFailureCauseOperatorShutdown
		}
		return types.TaskFailureCauseRuntimeCrash
	}

	// No backend-classified cause: fall back to the reason the backend recorded
	// (OOM, timeout, bad image, etc.).
	if failure != nil {
		switch failure.metricsReason {
		case metrics.TaskFailureReasonContainerOOM:
			return types.TaskFailureCauseOOM
		case metrics.TaskFailureReasonActiveDeadline, metrics.TaskFailureReasonTaskTimeout:
			return types.TaskFailureCauseInfrastructureTimeout
		case metrics.TaskFailureReasonWorkspaceSetup, metrics.TaskFailureReasonSetupCommand, metrics.TaskFailureReasonInvalidImage:
			return types.TaskFailureCauseUserError
		default:
			return types.TaskFailureCauseBackendFailure
		}
	}

	return types.TaskFailureCauseBackendFailure
}

func signalFromExitCode(exitCode int) (int, bool) {
	if exitCode < 129 || exitCode > 192 {
		return 0, false
	}
	sig := exitCode - 128
	return sig, sig > 0
}
