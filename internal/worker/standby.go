package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"golang.org/x/sync/semaphore"
)

const (
	standbySweepTimeout   = 30 * time.Second
	sweptStandbyExitCode  = 137
	standbyDestroyTimeout = BackendShutdownTimeout
)

// errStandbyNotWaited reports a backend that returned before its standby sandbox exited.
var errStandbyNotWaited = errors.New("backend returned before the standby sandbox exited")

// standbyEntry tracks one running standby sandbox. cancelSource is guarded by tasksMutex.
type standbyEntry struct {
	cancel          context.CancelFunc
	stopObservation context.CancelCauseFunc
	done            chan struct{}
	cancelSource    taskCancellationSource
	// slots is the semaphore that holds this standby's unit, or nil when unlimited.
	slots *semaphore.Weighted
}

// standbysSupported reports whether this worker can run standby sandboxes.
func (w *Worker) standbysSupported() bool {
	if !w.capabilities.supportsStandbys || w.config.OneShot {
		return false
	}
	return w.config.Direct == nil || w.config.Direct.TargetDir == ""
}

// standbySlotsLocked returns the semaphore that standbys acquire from, or nil when unlimited.
func (w *Worker) standbySlotsLocked() *semaphore.Weighted {
	if w.standbySemaphore != nil {
		return w.standbySemaphore
	}
	return w.taskSemaphore
}

// standbyBaseContext returns the parent context for standby executions.
func (w *Worker) standbyBaseContext() context.Context {
	if w.backend.PreservesTasksOnShutdown() {
		return context.WithoutCancel(w.ctx)
	}
	return w.ctx
}

// admitStandbyLocked reserves a capacity slot, or returns the reason to reject the create.
func (w *Worker) admitStandbyLocked() (*semaphore.Weighted, string) {
	switch {
	case w.shuttingDown || w.ctx.Err() != nil:
		return nil, "worker is shutting down"
	case w.config.OneShot:
		return nil, "one-shot worker does not run standby sandboxes"
	case !w.standbysSupported():
		return nil, "backend does not support standby sandboxes"
	}
	slots := w.standbySlotsLocked()
	if slots != nil && !slots.TryAcquire(1) {
		if w.standbySemaphore != nil {
			return nil, "worker at maximum prewarmed sandboxes"
		}
		return nil, "worker at maximum concurrency"
	}
	return slots, ""
}

func (w *Worker) handlePrewarmCreate(create *types.PrewarmCreateMessage) {
	sandboxID := create.SandboxID
	w.tasksMutex.Lock()
	if _, known := w.standbys[sandboxID]; known {
		w.tasksMutex.Unlock()
		log.Infof(w.ctx, "Ignoring duplicate prewarm create for sandbox %s", sandboxID)
		return
	}
	slots, reason := w.admitStandbyLocked()
	if reason != "" {
		w.tasksMutex.Unlock()
		w.rejectPrewarm(sandboxID, reason)
		return
	}
	cancelCtx, cancel := context.WithCancel(w.standbyBaseContext())
	runCtx, stopObservation := context.WithCancelCause(cancelCtx)
	entry := &standbyEntry{
		cancel:          cancel,
		stopObservation: stopObservation,
		done:            make(chan struct{}),
		slots:           slots,
	}
	w.standbys[sandboxID] = entry
	w.taskWG.Add(1)
	w.tasksMutex.Unlock()

	if err := w.sendPrewarmCreated(sandboxID); err != nil {
		log.Errorf(w.ctx, "Failed to send prewarm created message: %v", err)
	}
	go func() {
		defer w.taskWG.Done()
		w.runStandby(create, runCtx, entry)
	}()
}

func (w *Worker) rejectPrewarm(sandboxID, reason string) {
	log.Warnf(w.ctx, "Rejecting prewarm sandbox %s: %s", sandboxID, reason)
	err := w.sendStandbyMessage(types.MessageTypePrewarmRejected, types.PrewarmRejectedMessage{
		SandboxID: sandboxID,
		Reason:    reason,
	})
	if err != nil {
		log.Errorf(w.ctx, "Failed to send prewarm rejected message: %v", err)
	}
}

func (w *Worker) runStandby(create *types.PrewarmCreateMessage, runCtx context.Context, entry *standbyEntry) {
	sandboxID := create.SandboxID
	execCtx := runCtx
	if create.DeadlineCapSeconds > 0 {
		var stopDeadline context.CancelFunc
		execCtx, stopDeadline = context.WithTimeout(runCtx, time.Duration(create.DeadlineCapSeconds)*time.Second)
		defer stopDeadline()
	}

	log.Infof(w.ctx, "Starting standby sandbox %s", sandboxID)
	result := w.backend.ExecuteTask(execCtx, w.prepareStandbyTaskParams(create))
	err := result.Error
	if err == nil && result.Outcome != ExecuteOutcomeCompleted {
		err = errStandbyNotWaited
	}

	if errors.Is(context.Cause(runCtx), errObservationInterrupted) {
		log.Infof(w.ctx, "Standby sandbox %s keeps running after worker shutdown", sandboxID)
	} else {
		w.reportStandbyExit(sandboxID, entry, err)
	}
	w.releaseStandby(sandboxID, entry)
}

func (w *Worker) reportStandbyExit(sandboxID string, entry *standbyEntry, err error) {
	w.tasksMutex.Lock()
	source := entry.cancelSource
	w.tasksMutex.Unlock()

	exitCode, reason := standbyExitStatus(err, source)
	log.Infof(w.ctx, "Standby sandbox %s exited: exit_code=%d reason=%q", sandboxID, exitCode, reason)
	sendErr := w.sendStandbyMessage(types.MessageTypePrewarmExited, types.PrewarmExitedMessage{
		SandboxID: sandboxID,
		ExitCode:  exitCode,
		Reason:    reason,
	})
	if sendErr != nil {
		log.Errorf(w.ctx, "Failed to send prewarm exited message: %v", sendErr)
	}
}

func (w *Worker) releaseStandby(sandboxID string, entry *standbyEntry) {
	entry.cancel()
	if entry.slots != nil {
		entry.slots.Release(1)
	}
	w.tasksMutex.Lock()
	delete(w.standbys, sandboxID)
	w.tasksMutex.Unlock()
	close(entry.done)
}

// standbyExitStatus maps a standby's execution error to the exit code and reason sent to the server.
func standbyExitStatus(err error, source taskCancellationSource) (int, string) {
	if err == nil {
		return 0, ""
	}
	exitCode := failureExitCode(err)
	if exitCode == 0 {
		exitCode = 1
	}
	if source == taskCancellationSourceUser {
		return exitCode, string(metrics.TaskFailureReasonTaskCancelled)
	}
	_, reason := taskFailureLabels(err)
	if source == taskCancellationSourceShutdown && (reason == metrics.TaskFailureReasonTaskCancelled || exitCode == sigtermExitCode) {
		reason = metrics.TaskFailureReasonGracefulShutdown
	}
	return exitCode, string(reason)
}

func (w *Worker) handlePrewarmDestroy(destroy *types.PrewarmDestroyMessage) {
	sandboxID := destroy.SandboxID
	w.tasksMutex.Lock()
	if entry, known := w.standbys[sandboxID]; known {
		if entry.cancelSource == "" {
			entry.cancelSource = taskCancellationSourceUser
		}
		entry.cancel()
	}
	w.tasksMutex.Unlock()

	destroyer, ok := w.backend.(StandbyDestroyer)
	if !ok {
		return
	}
	w.cancellationWG.Add(1)
	go func() {
		defer w.cancellationWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), standbyDestroyTimeout)
		defer cancel()
		if err := destroyer.DestroyStandby(ctx, sandboxID); err != nil {
			log.Warnf(w.ctx, "Failed to destroy standby sandbox %s on backend: %v", sandboxID, err)
		}
	}()
}

// stopStandbysLocked stops every standby for shutdown and returns the observer channels to wait on.
func (w *Worker) stopStandbysLocked(preserve bool) ([]chan struct{}, int) {
	var observersDone []chan struct{}
	for _, entry := range w.standbys {
		if preserve {
			if entry.stopObservation != nil {
				entry.stopObservation(errObservationInterrupted)
			}
			observersDone = append(observersDone, entry.done)
			continue
		}
		if entry.cancelSource == "" {
			entry.cancelSource = taskCancellationSourceShutdown
		}
		entry.cancel()
	}
	return observersDone, len(w.standbys)
}

// sweepStandbys reports standbys that a previous worker process left behind.
func (w *Worker) sweepStandbys() {
	sweeper, ok := w.backend.(StandbySweeper)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, standbySweepTimeout)
	defer cancel()
	sandboxIDs, err := sweeper.SweepStandbys(ctx)
	if err != nil {
		log.Warnf(w.ctx, "Failed to sweep leftover standby sandboxes: %v", err)
	}
	for _, sandboxID := range sandboxIDs {
		sendErr := w.sendStandbyMessage(types.MessageTypePrewarmExited, types.PrewarmExitedMessage{
			SandboxID: sandboxID,
			ExitCode:  sweptStandbyExitCode,
			Reason:    string(metrics.TaskFailureReasonGracefulShutdown),
		})
		if sendErr != nil {
			log.Errorf(w.ctx, "Failed to send prewarm exited message for swept sandbox %s: %v", sandboxID, sendErr)
		}
	}
}

// startStandbyExitWatcher reports standby exits for the worker lifetime.
func (w *Worker) startStandbyExitWatcher() {
	watcher, ok := w.backend.(StandbyExitWatcher)
	if !ok {
		return
	}
	go watcher.WatchStandbyExits(w.ctx, w.reportWatchedStandbyExit)
}

// reportWatchedStandbyExit reports an exit that this worker does not track locally.
func (w *Worker) reportWatchedStandbyExit(sandboxID string, exitCode int, reason string) {
	w.tasksMutex.Lock()
	_, local := w.standbys[sandboxID]
	w.tasksMutex.Unlock()
	if local {
		return
	}
	err := w.sendStandbyMessage(types.MessageTypePrewarmExited, types.PrewarmExitedMessage{
		SandboxID: sandboxID,
		ExitCode:  exitCode,
		Reason:    reason,
	})
	if err != nil {
		log.Errorf(w.ctx, "Failed to send prewarm exited message for watched sandbox %s: %v", sandboxID, err)
	}
}

func (w *Worker) sendPrewarmCreated(sandboxID string) error {
	hostname, err := os.Hostname()
	if err != nil {
		log.Warnf(w.ctx, "Failed to read hostname for prewarm created message: %v", err)
	}
	return w.sendStandbyMessage(types.MessageTypePrewarmCreated, types.PrewarmCreatedMessage{
		SandboxID:   sandboxID,
		InstanceRef: StandbyInstanceRef(sandboxID),
		ReportedBy:  hostname,
	})
}

func (w *Worker) sendStandbyMessage(messageType types.MessageType, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal %s message: %w", messageType, err)
	}
	msgBytes, err := json.Marshal(types.WebSocketMessage{Type: messageType, Data: data})
	if err != nil {
		return fmt.Errorf("failed to marshal websocket message: %w", err)
	}
	return w.sendMessage(msgBytes)
}

// prepareStandbyTaskParams builds backend parameters for a standby sandbox, which has no task or run ID.
func (w *Worker) prepareStandbyTaskParams(create *types.PrewarmCreateMessage) *TaskParams {
	envVars := w.commonEnvVars()
	for key, value := range create.EnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}
	return &TaskParams{
		StandbyID:     create.SandboxID,
		EnvVars:       envVars,
		BaseArgs:      create.Args,
		DockerImage:   w.defaultImageForTask(create.DockerImage, nil),
		Sidecars:      w.sidecarMounts(create.SidecarImage, create.AdditionalSidecars),
		InstanceShape: create.InstanceShape,
	}
}
