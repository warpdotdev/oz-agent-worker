package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

type executionTestBackend struct {
	dispatchBackend
	started    chan string
	release    map[string]chan struct{}
	cancelled  chan CancelParams
	cancelGate chan struct{}
	failCancel atomic.Bool
}

func TestClaimIncludesAssignedExecution(t *testing.T) {
	for _, id := range []string{"", "old", "new"} {
		t.Run(id, func(t *testing.T) {
			w := &Worker{config: Config{WorkerID: "worker"}, outbound: newOutboundQueue(1)}
			if err := w.sendTaskClaimed("run", id); err != nil {
				t.Fatal(err)
			}
			msg := readWebSocketMessage(t, w.outbound.messages)
			var claimed types.TaskClaimedMessage
			if err := json.Unmarshal(msg.Data, &claimed); err != nil {
				t.Fatal(err)
			}
			if msg.Type != types.MessageTypeTaskClaimed || claimed.TaskID != "run" || claimed.WorkerID != "worker" || claimed.ExecutionID != id {
				t.Fatalf("incorrect claim: type %q, data %+v", msg.Type, claimed)
			}
			if id == "" && strings.Contains(string(msg.Data), "execution_id") {
				t.Fatal("legacy empty execution ID should be omitted")
			}
		})
	}
}

func (b *executionTestBackend) ExecuteTask(ctx context.Context, params *TaskParams) ExecuteResult {
	b.started <- params.ExecutionID
	select {
	case <-b.release[params.ExecutionID]:
		return executeCompleted()
	case <-ctx.Done():
		return executeError(ctx.Err())
	}
}

func (b *executionTestBackend) CancelTask(ctx context.Context, params *CancelParams) error {
	b.cancelled <- *params
	if b.cancelGate != nil {
		select {
		case <-b.cancelGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if b.failCancel.Load() {
		return errors.New("cleanup unavailable")
	}
	return nil
}

func (b *executionTestBackend) ConfirmTaskClosed(context.Context, *CancelParams) (bool, error) {
	return true, nil
}

func newExecutionTestWorker(t *testing.T) (*Worker, *executionTestBackend) {
	t.Helper()
	backend := &executionTestBackend{
		started:   make(chan string, 4),
		release:   map[string]chan struct{}{"old": make(chan struct{}), "new": make(chan struct{})},
		cancelled: make(chan CancelParams, 4),
	}
	w := &Worker{
		ctx:         context.Background(),
		backend:     backend,
		activeTasks: make(map[taskExecution]activeTask),
		outbound:    newOutboundQueue(32),
	}
	t.Cleanup(func() {
		for _, release := range backend.release {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		w.taskWG.Wait()
	})
	return w, backend
}

func executionAssignment(id string) *types.TaskAssignmentMessage {
	return &types.TaskAssignmentMessage{TaskID: "run", ExecutionID: id, Task: &types.Task{ID: "run"}}
}

func awaitExecutionStarted(t *testing.T, backend *executionTestBackend, id string) {
	t.Helper()
	select {
	case got := <-backend.started:
		if got != id {
			t.Fatalf("started %q, want %q", got, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("execution did not start")
	}
}

func awaitExecutionClosed(t *testing.T, w *Worker, id string) {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		var msg types.WebSocketMessage
		select {
		case data := <-w.outbound.messages:
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatal(err)
			}
		case <-timeout.C:
			t.Fatal("execution closure was not acknowledged")
		}
		if msg.Type != types.MessageTypeExecutionClosed {
			continue
		}
		var closed types.ExecutionClosedMessage
		if err := json.Unmarshal(msg.Data, &closed); err != nil {
			t.Fatal(err)
		}
		if closed.TaskID != "run" || closed.ExecutionID != id {
			t.Fatalf("wrong closure identity: %+v", closed)
		}
		return
	}
}

func TestExecutionScopedCancellation(t *testing.T) {
	t.Run("delayed predecessor cancellation leaves successor running and replays closure", func(t *testing.T) {
		w, backend := newExecutionTestWorker(t)
		w.handleTaskAssignment(executionAssignment("old"))
		awaitExecutionStarted(t, backend, "old")
		w.handleTaskAssignment(executionAssignment("new"))
		awaitExecutionStarted(t, backend, "new")
		request := &types.TaskCancellationMessage{TaskID: "run", ExecutionID: "old"}
		w.handleTaskCancellation(request)
		awaitExecutionClosed(t, w, "old")
		w.tasksMutex.Lock()
		newTask, exists := w.activeTasks[taskExecution{"run", "new"}]
		w.tasksMutex.Unlock()
		if !exists || newTask.ctx.Err() != nil {
			t.Fatal("predecessor cancellation affected successor")
		}
		if params := <-backend.cancelled; params.ExecutionID != "old" {
			t.Fatalf("cancelled wrong execution: %+v", params)
		}
		w.handleTaskCancellation(request)
		awaitExecutionClosed(t, w, "old")
		select {
		case params := <-backend.cancelled:
			t.Fatalf("replay called backend again: %+v", params)
		default:
		}
	})

	t.Run("old completion cannot remove successor and remains closable", func(t *testing.T) {
		w, backend := newExecutionTestWorker(t)
		for _, id := range []string{"old", "new"} {
			w.handleTaskAssignment(executionAssignment(id))
			awaitExecutionStarted(t, backend, id)
		}
		close(backend.release["old"])
		waitFor(t, 2*time.Second, func() bool {
			w.tasksMutex.Lock()
			defer w.tasksMutex.Unlock()
			_, completed := w.completedTasks[taskExecution{"run", "old"}]
			return completed
		})
		if count := w.activeTaskCount(); count != 1 {
			t.Fatalf("active executions = %d, want 1", count)
		}
		w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "run", ExecutionID: "old"})
		awaitExecutionClosed(t, w, "old")
		if count := w.activeTaskCount(); count != 1 {
			t.Fatalf("cleanup removed successor: active executions = %d", count)
		}
	})

	t.Run("unknown execution and duplicate assignment are no-ops", func(t *testing.T) {
		w, backend := newExecutionTestWorker(t)
		w.handleTaskAssignment(executionAssignment("new"))
		awaitExecutionStarted(t, backend, "new")
		drainMessages(t, w.outbound.messages)
		w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "run", ExecutionID: "unknown"})
		w.handleTaskAssignment(executionAssignment("new"))
		if len(w.outbound.messages) != 0 || len(backend.cancelled) != 0 || len(backend.started) != 0 {
			t.Fatal("unknown cancellation or duplicate assignment produced effects")
		}
	})

	t.Run("legacy task-only cancellation cancels all matching executions", func(t *testing.T) {
		w, backend := newExecutionTestWorker(t)
		for _, id := range []string{"old", "new"} {
			w.handleTaskAssignment(executionAssignment(id))
			awaitExecutionStarted(t, backend, id)
		}
		w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "run"})
		waitFor(t, 2*time.Second, func() bool { return w.activeTaskCount() == 0 })
		got := map[string]bool{}
		for i := 0; i < 2; i++ {
			params := <-backend.cancelled
			got[params.ExecutionID] = true
		}
		if !got["old"] || !got["new"] {
			t.Fatalf("legacy cancellation targets = %v", got)
		}
	})

	t.Run("queued cleanup is not proof and failed cleanup remains retryable", func(t *testing.T) {
		w, backend := newExecutionTestWorker(t)
		backend.cancelGate = make(chan struct{})
		backend.failCancel.Store(true)
		w.handleTaskAssignment(executionAssignment("old"))
		awaitExecutionStarted(t, backend, "old")
		request := &types.TaskCancellationMessage{TaskID: "run", ExecutionID: "old"}
		w.handleTaskCancellation(request)
		select {
		case <-backend.cancelled:
		case <-time.After(2 * time.Second):
			t.Fatal("backend cleanup did not start")
		}
		for _, msg := range drainMessages(t, w.outbound.messages) {
			if msg.Type == types.MessageTypeExecutionClosed {
				t.Fatal("acknowledged queued cleanup")
			}
		}
		w.handleTaskCancellation(request)
		if len(backend.cancelled) != 0 {
			t.Fatal("concurrent retry duplicated cleanup")
		}
		close(backend.cancelGate)
		waitFor(t, 2*time.Second, func() bool {
			w.tasksMutex.Lock()
			defer w.tasksMutex.Unlock()
			task, tracked := w.activeTasks[taskExecution{"run", "old"}]
			return tracked && !task.cancelling
		})
		if len(w.outbound.messages) != 0 {
			t.Fatal("failed cleanup emitted closure")
		}
		backend.failCancel.Store(false)
		w.handleTaskCancellation(request)
		awaitExecutionClosed(t, w, "old")
	})

	t.Run("waits for dispatch before backend cancellation", func(t *testing.T) {
		w, backend := newExecutionTestWorker(t)
		key := taskExecution{"run", "old"}
		done := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		w.activeTasks[key] = activeTask{ctx: ctx, cancel: cancel, done: done}
		w.handleTaskCancellation(&types.TaskCancellationMessage{TaskID: "run", ExecutionID: "old"})
		if len(backend.cancelled) != 0 || len(w.outbound.messages) != 0 {
			t.Fatal("cancel acknowledged before dispatch returned")
		}
		close(done)
		awaitExecutionClosed(t, w, "old")
	})
}

func TestExecutionClosureReplayCacheBounded(t *testing.T) {
	w := &Worker{}
	w.rememberCompletedLocked(taskExecution{taskID: "legacy-run"}, true)
	if len(w.completedTasks) != 0 {
		t.Fatal("legacy completion must not suppress a later run-only assignment")
	}
	for i := 0; i < 5000; i++ {
		w.rememberCompletedLocked(taskExecution{taskID: "run", executionID: time.Unix(int64(i), 0).String()}, true)
	}
	if len(w.completedTasks) != 4096 || len(w.closedExecutions) != 4096 {
		t.Fatalf("cache unbounded: %d records, %d keys", len(w.completedTasks), len(w.closedExecutions))
	}
}

func TestExecutionCancellationCapabilityHandshake(t *testing.T) {
	capability := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		capability <- r.Header.Get("X-Warp-Worker-Capabilities")
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	w := &Worker{ctx: context.Background(), config: Config{WebSocketURL: "ws" + strings.TrimPrefix(server.URL, "http")}}
	conn, err := w.connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if got := <-capability; got != "execution-scoped-cancellation-v1" {
		t.Fatalf("capability = %q", got)
	}
}
