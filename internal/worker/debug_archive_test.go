package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/trace"
)

func TestWorkerVersionHeaderValue(t *testing.T) {
	tests := []struct {
		name    string
		version string
		wantOK  bool
	}{
		{name: "release build", version: "v2026-08-04-15-14-28", wantOK: true},
		{name: "local dev build", version: "dev", wantOK: true},
		{name: "arbitrary test build", version: "test-build-1234", wantOK: true},
		{name: "empty", version: "", wantOK: false},
		{name: "overlong", version: strings.Repeat("v", maxWorkerVersionBytes+1), wantOK: false},
		{name: "carries a newline", version: "v1\ninjected: header", wantOK: false},
		{name: "carries a NUL", version: "v1\x00", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := workerVersionHeaderValue(tc.version)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.version {
				t.Fatalf("value = %q, want the build identifier unchanged", got)
			}
			if !ok && got != "" {
				t.Fatalf("value = %q, want it omitted", got)
			}
		})
	}
}

// dialWorker connects a worker to a test WebSocket server and returns the
// headers the server observed on the upgrade request.
func dialWorker(t *testing.T, version string) http.Header {
	t.Helper()

	observed := make(chan http.Header, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Worker{
		ctx: ctx,
		config: Config{
			WebSocketURL: "ws" + strings.TrimPrefix(server.URL, "http"),
			WorkerID:     "test-worker",
			APIKey:       "wk-test",
			Version:      version,
		},
	}
	if err := w.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	w.connMutex.Lock()
	conn := w.conn
	w.connMutex.Unlock()
	if conn != nil {
		_ = conn.Close()
	}

	select {
	case headers := <-observed:
		return headers
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the upgrade request")
		return nil
	}
}

func TestConnectSendsTheWorkerVersionHeader(t *testing.T) {
	headers := dialWorker(t, "v2026-08-04-15-14-28")

	if got := headers.Get(types.WorkerVersionHeader); got != "v2026-08-04-15-14-28" {
		t.Fatalf("%s = %q, want the build identifier", types.WorkerVersionHeader, got)
	}
}

func TestConnectOmitsInvalidWorkerVersionWithoutFailing(t *testing.T) {
	// An unusable build identifier must not cost the worker its connection:
	// the server records provenance as not reported and the worker still runs
	// tasks.
	headers := dialWorker(t, "v1\ninjected: header")

	if got := headers.Get(types.WorkerVersionHeader); got != "" {
		t.Fatalf("%s = %q, want the invalid value omitted", types.WorkerVersionHeader, got)
	}
	if got := headers.Get("Injected"); got != "" {
		t.Fatalf("a control character in the version smuggled in header %q", got)
	}
}

func TestConnectOmitsTheHeaderWhenNoVersionIsStamped(t *testing.T) {
	headers := dialWorker(t, "")

	if _, present := headers[types.WorkerVersionHeader]; present {
		t.Fatal("an unstamped build must not report a version")
	}
}

// snapshotRecordingBackend records the debug-archive calls the worker makes.
type snapshotRecordingBackend struct {
	outcome ExecuteResult

	snapshots atomic.Int32
	cleanups  chan CancelParams
	output    string
}

func newSnapshotRecordingBackend(outcome ExecuteResult) *snapshotRecordingBackend {
	return &snapshotRecordingBackend{outcome: outcome, cleanups: make(chan CancelParams, 4)}
}

func (b *snapshotRecordingBackend) ExecuteTask(context.Context, *TaskParams) ExecuteResult {
	return b.outcome
}

func (b *snapshotRecordingBackend) CancelTask(context.Context, *CancelParams) error { return nil }

func (b *snapshotRecordingBackend) PreservesTasksOnShutdown() bool { return false }

func (b *snapshotRecordingBackend) Shutdown(context.Context) {}

func (b *snapshotRecordingBackend) SnapshotTaskLogs(_ context.Context, params *SnapshotParams) error {
	b.snapshots.Add(1)
	if b.output == "" {
		return nil
	}
	return params.Sink.WriteChunk(debuglog.Chunk{
		Phase:  debuglog.PhaseContainer,
		Stream: debuglog.StreamCombined,
		Data:   []byte(b.output),
	})
}

func (b *snapshotRecordingBackend) CleanupTaskResources(_ context.Context, params *CancelParams) error {
	b.cleanups <- *params
	return nil
}

func newDebugArchiveWorker(t *testing.T, backend Backend, idleOnComplete string) *Worker {
	t.Helper()

	captureConfig := debuglog.DefaultConfig()
	captureConfig.Directory = t.TempDir()
	store, err := debuglog.NewStore(captureConfig)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	w := &Worker{
		ctx:            ctx,
		cancel:         cancel,
		config:         Config{BackendType: "docker", IdleOnComplete: idleOnComplete},
		sendChan:       make(chan []byte, 8),
		tasks:          newTaskRegistry(),
		backend:        backend,
		debugLogStore:  store,
		reconnectDelay: InitialReconnectDelay,
	}
	w.debugLogs = debuglog.NewCoordinator(debuglog.CoordinatorOptions{
		Ownership: w.tasks,
		Source:    backendSnapshotSource{backend: backend},
		Sender:    w,
		Store:     store,
	})
	return w
}

func TestExecuteTaskMovesOwnershipToCleanupGraceBeforeTerminalMessage(t *testing.T) {
	tests := []struct {
		name    string
		outcome ExecuteResult
	}{
		{name: "success", outcome: executeCompleted()},
		{name: "backend failure", outcome: executeError(newBackendFailure("backend", "container_exit", context.DeadlineExceeded))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := newSnapshotRecordingBackend(tc.outcome)
			// A long grace keeps the entry in place for the assertion.
			w := newDebugArchiveWorker(t, backend, "60m")

			assignment := &types.TaskAssignmentMessage{
				TaskID:      "task-1",
				ExecutionID: "exec-1",
				Task:        &types.Task{ID: "task-1", Title: "test task"},
			}
			w.tasks.StartTask("task-1", activeTask{cancel: func() {}, executionID: "exec-1"}, debuglog.BackendDocker, nil)

			w.executeTask(context.Background(), func() {}, trace.SpanFromContext(context.Background()), assignment, time.Now())

			// The terminal message is only enqueued after the grace entry
			// exists, so a request triggered by it always finds an owner.
			if len(w.sendChan) == 0 {
				t.Fatal("expected a terminal lifecycle message")
			}
			owner, owned := w.tasks.LookupExecution("task-1", "exec-1")
			if !owned {
				t.Fatal("execution ownership was released before the cleanup grace expired")
			}
			if !owner.InCleanupGrace {
				t.Fatal("ownership should have moved to cleanup grace")
			}
		})
	}
}

func TestCleanupGraceExpiryReleasesBackendResourcesOnce(t *testing.T) {
	backend := newSnapshotRecordingBackend(executeCompleted())
	// A zero grace expires immediately, so the timer fires without a wait.
	w := newDebugArchiveWorker(t, backend, "0s")

	w.tasks.StartTask("task-1", activeTask{cancel: func() {}, executionID: "exec-1"}, debuglog.BackendDocker, nil)
	w.beginCleanupGrace(&types.TaskAssignmentMessage{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task:        &types.Task{ID: "task-1"},
	})

	select {
	case params := <-backend.cleanups:
		if params.TaskID != "task-1" || params.ExecutionID != "exec-1" {
			t.Fatalf("cleanup params = %+v, want {task-1 exec-1}", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend cleanup was not invoked at the grace deadline")
	}

	if _, owned := w.tasks.LookupExecution("task-1", "exec-1"); owned {
		t.Fatal("ownership survived the cleanup-grace deadline")
	}

	// A second expiry must be a no-op rather than a duplicate cleanup.
	w.expireCleanupGrace("task-1", "exec-1")
	select {
	case params := <-backend.cleanups:
		t.Fatalf("cleanup ran twice for %+v", params)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRegistryDistinguishesExecutionsOfTheSameRun(t *testing.T) {
	registry := newTaskRegistry()
	registry.StartTask("run-1", activeTask{cancel: func() {}, executionID: "exec-1"}, debuglog.BackendDocker, nil)

	if _, owned := registry.LookupExecution("run-1", "exec-1"); !owned {
		t.Fatal("the exact execution must be owned")
	}
	if _, owned := registry.LookupExecution("run-1", "exec-2"); owned {
		t.Fatal("a different execution of the same run must not be owned")
	}
	if _, owned := registry.LookupExecution("run-2", "exec-1"); owned {
		t.Fatal("a different run must not be owned")
	}
}

func TestHandleMessageDispatchesArchiveRequestsOffTheReadLoop(t *testing.T) {
	backend := newSnapshotRecordingBackend(executeCompleted())
	backend.output = "captured container output"
	w := newDebugArchiveWorker(t, backend, "60m")
	w.tasks.StartTask("run-1", activeTask{cancel: func() {}, executionID: "exec-1"}, debuglog.BackendDocker, nil)

	uploaded := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		uploaded <- struct{}{}
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	request := types.DebugArchiveLogsRequestedMessage{
		ProtocolVersion: types.DebugArchiveProtocolVersion,
		RequestID:       "request-1",
		ArchiveID:       "archive-1",
		CollectionID:    "collection-1",
		RunID:           "run-1",
		ExecutionID:     "exec-1",
		RequestedFormat: types.DebugArchiveFormatNDJSON,
		ExpiresAt:       time.Now().Add(5 * time.Minute),
		MaxBytes:        1 << 15,
		ContentTransformer: types.ContentTransformerDescriptor{
			Kind:    debuglog.TransformerKindNoop,
			Version: 1,
		},
		UploadTarget: types.UploadTarget{URL: server.URL, Method: http.MethodPut},
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	message, err := json.Marshal(types.WebSocketMessage{
		Type: types.MessageTypeDebugArchiveLogsRequested,
		Data: data,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	// handleMessage returning promptly is what keeps heartbeats and later
	// assignments flowing while a snapshot runs.
	w.handleMessage(message)

	select {
	case <-uploaded:
	case <-time.After(10 * time.Second):
		t.Fatal("the archive request never reached the upload target")
	}

	w.debugLogs.Wait()
	ack := readAckMessage(t, w.sendChan)
	if ack.Outcome != types.DebugArchiveOutcomeUploaded {
		t.Fatalf("outcome = %q (%s), want %q", ack.Outcome, ack.ReasonCode, types.DebugArchiveOutcomeUploaded)
	}
	if ack.RunID != "run-1" || ack.ExecutionID != "exec-1" {
		t.Fatalf("acknowledgement identity = %s/%s, want run-1/exec-1", ack.RunID, ack.ExecutionID)
	}
}

func TestHandleMessageIgnoresArchiveRequestsForUnownedExecutions(t *testing.T) {
	backend := newSnapshotRecordingBackend(executeCompleted())
	w := newDebugArchiveWorker(t, backend, "60m")

	request := types.DebugArchiveLogsRequestedMessage{
		ProtocolVersion: types.DebugArchiveProtocolVersion,
		RequestID:       "request-1",
		ArchiveID:       "archive-1",
		CollectionID:    "collection-1",
		RunID:           "run-1",
		ExecutionID:     "exec-1",
		RequestedFormat: types.DebugArchiveFormatNDJSON,
		ExpiresAt:       time.Now().Add(5 * time.Minute),
		MaxBytes:        1 << 15,
		ContentTransformer: types.ContentTransformerDescriptor{
			Kind:    debuglog.TransformerKindNoop,
			Version: 1,
		},
		UploadTarget: types.UploadTarget{URL: "https://storage.example.com/candidate", Method: http.MethodPut},
	}
	data, _ := json.Marshal(request)
	message, _ := json.Marshal(types.WebSocketMessage{
		Type: types.MessageTypeDebugArchiveLogsRequested,
		Data: data,
	})

	w.handleMessage(message)
	w.debugLogs.Wait()

	if len(w.sendChan) != 0 {
		t.Fatalf("a non-owning process enqueued %d messages, want 0", len(w.sendChan))
	}
	if backend.snapshots.Load() != 0 {
		t.Fatal("a non-owning process read the backend")
	}
}

func readAckMessage(t *testing.T, ch <-chan []byte) types.DebugArchiveLogsUploadedMessage {
	t.Helper()
	select {
	case raw := <-ch:
		var envelope types.WebSocketMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("failed to unmarshal websocket message: %v", err)
		}
		if envelope.Type != types.MessageTypeDebugArchiveLogsUploaded {
			t.Fatalf("message type = %q, want %q", envelope.Type, types.MessageTypeDebugArchiveLogsUploaded)
		}
		var ack types.DebugArchiveLogsUploadedMessage
		if err := json.Unmarshal(envelope.Data, &ack); err != nil {
			t.Fatalf("failed to unmarshal acknowledgement: %v", err)
		}
		return ack
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an acknowledgement message")
		return types.DebugArchiveLogsUploadedMessage{}
	}
}
