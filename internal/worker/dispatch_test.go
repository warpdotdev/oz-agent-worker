package worker

import (
	"context"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"
)

// dispatchBackend is a fake Backend that reports a successful fire-and-forget
// dispatch. Its CancelTask is a no-op.
type dispatchBackend struct{}

func (b *dispatchBackend) ExecuteTask(context.Context, *TaskParams) ExecuteResult {
	return executeSpawned()
}
func (b *dispatchBackend) CancelTask(context.Context, *CancelParams) error { return nil }
func (b *dispatchBackend) Shutdown(context.Context)                        {}
func (b *dispatchBackend) PreservesTasksOnShutdown() bool                  { return true }

// cancelableDispatchBackend is a dispatchBackend that also records CancelTask calls.
type cancelableDispatchBackend struct {
	dispatchBackend
	cancelCalled chan *CancelParams
}

func (b *cancelableDispatchBackend) CancelTask(_ context.Context, params *CancelParams) error {
	b.cancelCalled <- params
	return nil
}

func newDispatchWorker(backend Backend) *Worker {
	return &Worker{
		ctx:         context.Background(),
		config:      Config{},
		sendChan:    make(chan []byte, 4),
		activeTasks: map[string]activeTask{"task-1": {cancel: func() {}, executionID: "exec-1"}},
		backend:     backend,
	}
}

func runDispatchTask(w *Worker) {
	ctx := context.Background()
	w.executeTask(ctx, func() {}, trace.SpanFromContext(ctx), &types.TaskAssignmentMessage{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task:        &types.Task{ID: "task-1", Title: "test task"},
	}, time.Now())
}

func TestExecuteTaskDispatchedSuppressesTerminalMessage(t *testing.T) {
	w := newDispatchWorker(&dispatchBackend{})

	runDispatchTask(w)

	if len(w.sendChan) != 0 {
		msg := readWebSocketMessage(t, w.sendChan)
		t.Fatalf("expected no terminal message after dispatch, got %q", msg.Type)
	}
	w.tasksMutex.Lock()
	task, ok := w.activeTasks["task-1"]
	w.tasksMutex.Unlock()
	if !ok {
		t.Fatal("spawned task should remain in activeTasks")
	}
	if !task.spawned {
		t.Error("spawned task entry should be marked spawned")
	}
	if task.executionID != "exec-1" {
		t.Errorf("executionID = %q, want exec-1", task.executionID)
	}
}

func TestExecuteTaskDispatchedReleasesSemaphore(t *testing.T) {
	w := newDispatchWorker(&dispatchBackend{})
	w.config.MaxConcurrentTasks = 1
	w.taskSemaphore = semaphore.NewWeighted(1)
	if !w.taskSemaphore.TryAcquire(1) {
		t.Fatal("failed to acquire the only slot before dispatch")
	}

	runDispatchTask(w)

	// The deferred cleanup must have released the slot so it can be re-acquired.
	if !w.taskSemaphore.TryAcquire(1) {
		t.Fatal("expected the concurrency slot to be released after dispatch")
	}
}

func spawnedActiveTask(executionID string) activeTask {
	return activeTask{
		ctx:         context.Background(),
		cancel:      func() {},
		executionID: executionID,
		spawned:     true,
	}
}

func TestHandleTaskCancellationRoutesToBackendCancelTask(t *testing.T) {
	backend := &cancelableDispatchBackend{cancelCalled: make(chan *CancelParams, 1)}
	w := &Worker{
		ctx:         context.Background(),
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": spawnedActiveTask("exec-1")},
		backend:     backend,
	}

	w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "task-1"})

	select {
	case cp := <-backend.cancelCalled:
		if cp.TaskID != "task-1" || cp.ExecutionID != "exec-1" {
			t.Errorf("CancelTask params = %+v, want {task-1 exec-1}", cp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CancelTask was not invoked for a spawned task")
	}

	w.tasksMutex.Lock()
	_, ok := w.activeTasks["task-1"]
	w.tasksMutex.Unlock()
	if ok {
		t.Error("spawned task should be removed after cancellation is routed")
	}
}

func TestHandleTaskCancellationRunningTaskCancelsContextAndBackend(t *testing.T) {
	backend := &cancelableDispatchBackend{cancelCalled: make(chan *CancelParams, 1)}
	taskCtx, taskCancel := context.WithCancel(context.Background())
	defer taskCancel()
	w := &Worker{
		ctx:      context.Background(),
		sendChan: make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {
			ctx:         taskCtx,
			cancel:      taskCancel,
			executionID: "exec-1",
		}},
		backend: backend,
	}

	w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "task-1"})

	if taskCtx.Err() != context.Canceled {
		t.Fatalf("task context error = %v, want %v", taskCtx.Err(), context.Canceled)
	}
	select {
	case cp := <-backend.cancelCalled:
		if cp.TaskID != "task-1" || cp.ExecutionID != "exec-1" {
			t.Errorf("CancelTask params = %+v, want {task-1 exec-1}", cp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CancelTask was not invoked for a running task")
	}

	// The entry stays until executeTask's deferred cleanup removes it.
	w.tasksMutex.Lock()
	task, ok := w.activeTasks["task-1"]
	w.tasksMutex.Unlock()
	if !ok {
		t.Fatal("running task should remain in activeTasks until executeTask returns")
	}
	if task.cancellationSource != taskCancellationSourceUser {
		t.Fatalf("cancellation source = %q, want %q", task.cancellationSource, taskCancellationSourceUser)
	}
}

func TestHandleTaskCancellationSpawnedNoopCancelEmitsNoMessage(t *testing.T) {
	w := &Worker{
		ctx:         context.Background(),
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": spawnedActiveTask("exec-1")},
		backend:     &dispatchBackend{},
	}

	// Must not panic and must not emit any task status message when the
	// backend's CancelTask is a no-op.
	w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "task-1"})

	if len(w.sendChan) != 0 {
		t.Fatalf("expected no message for no-op-cancel spawned task, got %d", len(w.sendChan))
	}
}

func TestExecuteTaskSuccessStillReportsCompleted(t *testing.T) {
	w := newDispatchWorker(&recordingBackend{err: nil})

	runDispatchTask(w)

	msg := readWebSocketMessage(t, w.sendChan)
	if msg.Type != types.MessageTypeTaskCompleted {
		t.Fatalf("message type = %q, want %q", msg.Type, types.MessageTypeTaskCompleted)
	}
}
