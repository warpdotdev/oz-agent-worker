package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/trace"
)

type shutdownRecordingBackend struct {
	shutdownCalled bool
	shutdownCtxErr error
}

func (b *shutdownRecordingBackend) ExecuteTask(context.Context, *TaskParams) error {
	return nil
}

func (b *shutdownRecordingBackend) Shutdown(ctx context.Context) {
	b.shutdownCalled = true
	b.shutdownCtxErr = ctx.Err()
}
func (b *shutdownRecordingBackend) PreservesTasksOnShutdown() bool {
	return false
}

type preservingShutdownRecordingBackend struct {
	shutdownRecordingBackend
}

func (b *preservingShutdownRecordingBackend) PreservesTasksOnShutdown() bool {
	return true
}

type recordingBackend struct {
	err error
}

func (b *recordingBackend) ExecuteTask(context.Context, *TaskParams) error {
	return b.err
}

func (b *recordingBackend) Shutdown(context.Context) {}
func (b *recordingBackend) PreservesTasksOnShutdown() bool {
	return false
}

func TestTaskFailureLabels(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantPhase  string
		wantReason string
	}{
		{
			name:       "deadline exceeded",
			err:        context.DeadlineExceeded,
			wantPhase:  metrics.TaskFailurePhaseBackend,
			wantReason: metrics.TaskFailureReasonTaskTimeout,
		},
		{
			name:       "canceled",
			err:        context.Canceled,
			wantPhase:  metrics.TaskFailurePhaseBackend,
			wantReason: metrics.TaskFailureReasonTaskCancelled,
		},
		{
			name: "wrapped backend failure",
			err: fmt.Errorf("wrapped: %w", newBackendFailure(
				metrics.TaskFailurePhaseBackend,
				metrics.TaskFailureReasonImagePull,
				errors.New("pull failed"),
			)),
			wantPhase:  metrics.TaskFailurePhaseBackend,
			wantReason: metrics.TaskFailureReasonImagePull,
		},
		{
			name:       "unknown error",
			err:        errors.New("boom"),
			wantPhase:  metrics.TaskFailurePhaseBackend,
			wantReason: metrics.TaskFailureReasonUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, reason := taskFailureLabels(tt.err)
			if phase != tt.wantPhase || reason != tt.wantReason {
				t.Fatalf("taskFailureLabels() = (%q, %q), want (%q, %q)", phase, reason, tt.wantPhase, tt.wantReason)
			}
		})
	}
}

func TestTaskFailureMetadataSignalExits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exitCode string
	}{
		{name: "sigterm", exitCode: "143"},
		{name: "sigabrt", exitCode: "134"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := exec.Command("sh", "-c", "exit "+tc.exitCode).Run()
			if err == nil {
				t.Fatal("expected command to exit non-zero")
			}
			cause := classifyFailure(err, "")
			if cause != types.TaskFailureCauseRuntimeCrash {
				t.Fatalf("failure cause = %q, want %q", cause, types.TaskFailureCauseRuntimeCrash)
			}
		})
	}
}

func TestTaskFailedMessageIncludesFailureCause(t *testing.T) {
	w := &Worker{ctx: context.Background(), sendChan: make(chan []byte, 1)}
	if err := w.sendTaskFailed("task-1", "terminated", types.TaskFailureCauseRuntimeCrash); err != nil {
		t.Fatalf("sendTaskFailed returned error: %v", err)
	}
	msg := readWebSocketMessage(t, w.sendChan)
	var failed types.TaskFailedMessage
	if err := json.Unmarshal(msg.Data, &failed); err != nil {
		t.Fatalf("failed to decode task_failed: %v", err)
	}
	if failed.FailureCause != types.TaskFailureCauseRuntimeCrash {
		t.Fatalf("failure_cause = %q, want %q", failed.FailureCause, types.TaskFailureCauseRuntimeCrash)
	}
}

func TestExecuteTaskReportsTaskCancelledOnUserCancellation(t *testing.T) {
	taskCtx, taskCancel := context.WithCancel(context.Background())
	taskCancel()
	w := &Worker{
		ctx:      context.Background(),
		config:   Config{},
		sendChan: make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {
			cancel:             func() {},
			cancellationSource: taskCancellationSourceUser,
		}},
		backend: &recordingBackend{err: context.Canceled},
	}
	w.executeTask(taskCtx, func() {}, trace.SpanFromContext(taskCtx), &types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task:   &types.Task{ID: "task-1", Title: "test task"},
	}, time.Now())

	msg := readWebSocketMessage(t, w.sendChan)
	if msg.Type != types.MessageTypeTaskCompleted {
		t.Fatalf("message type = %q, want %q", msg.Type, types.MessageTypeTaskCompleted)
	}

	var completed types.TaskCompletedMessage
	if err := json.Unmarshal(msg.Data, &completed); err != nil {
		t.Fatalf("failed to unmarshal task completed message: %v", err)
	}
	if completed.TaskID != "task-1" {
		t.Errorf("task ID = %q, want %q", completed.TaskID, "task-1")
	}
	if completed.TaskState == nil || *completed.TaskState != types.TaskStateCancelled {
		t.Fatalf("task state = %v, want %q", completed.TaskState, types.TaskStateCancelled)
	}
	if completed.Message != "Task cancelled by user request." {
		t.Errorf("message = %q, want %q", completed.Message, "Task cancelled by user request.")
	}
	if _, ok := w.activeTasks["task-1"]; ok {
		t.Fatal("task should be removed from active tasks")
	}
}

func TestExecuteTaskDoesNotReportTaskCancelledOnBackendCancellationError(t *testing.T) {
	w := &Worker{
		ctx:         context.Background(),
		config:      Config{},
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {cancel: func() {}}},
		backend:     &recordingBackend{err: fmt.Errorf("backend request failed: %w", context.Canceled)},
	}

	w.executeTask(context.Background(), func() {}, trace.SpanFromContext(context.Background()), &types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task:   &types.Task{ID: "task-1", Title: "test task"},
	}, time.Now())

	msg := readWebSocketMessage(t, w.sendChan)
	if msg.Type != types.MessageTypeTaskFailed {
		t.Fatalf("message type = %q, want %q", msg.Type, types.MessageTypeTaskFailed)
	}
}

func TestHandleMessageCancelsActiveTask(t *testing.T) {
	taskCtx, taskCancel := context.WithCancel(context.Background())
	defer taskCancel()

	w := &Worker{
		ctx:      context.Background(),
		sendChan: make(chan []byte, 1),
		activeTasks: map[string]activeTask{
			"task-1": {
				ctx:    taskCtx,
				cancel: taskCancel,
			},
		},
	}

	data, err := json.Marshal(types.TaskCancellationMessage{TaskID: "task-1"})
	if err != nil {
		t.Fatalf("failed to marshal cancellation message: %v", err)
	}
	message, err := json.Marshal(types.WebSocketMessage{
		Type: types.MessageTypeTaskCancellation,
		Data: data,
	})
	if err != nil {
		t.Fatalf("failed to marshal websocket message: %v", err)
	}

	w.handleMessage(message)

	if taskCtx.Err() != context.Canceled {
		t.Fatalf("task context error = %v, want %v", taskCtx.Err(), context.Canceled)
	}
	if task := w.activeTasks["task-1"]; task.cancellationSource != taskCancellationSourceUser {
		t.Fatalf("task cancellation source = %q, want %q", task.cancellationSource, taskCancellationSourceUser)
	}
}

func TestExecuteTaskReportsTaskCompletedOnSuccess(t *testing.T) {
	w := &Worker{
		ctx:         context.Background(),
		config:      Config{},
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {cancel: func() {}}},
		backend:     &recordingBackend{},
	}

	w.executeTask(context.Background(), func() {}, trace.SpanFromContext(context.Background()), &types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task:   &types.Task{ID: "task-1", Title: "test task"},
	}, time.Now())

	msg := readWebSocketMessage(t, w.sendChan)
	if msg.Type != types.MessageTypeTaskCompleted {
		t.Fatalf("message type = %q, want %q", msg.Type, types.MessageTypeTaskCompleted)
	}

	var completed types.TaskCompletedMessage
	if err := json.Unmarshal(msg.Data, &completed); err != nil {
		t.Fatalf("failed to unmarshal task completed message: %v", err)
	}
	if completed.TaskID != "task-1" {
		t.Errorf("task ID = %q, want %q", completed.TaskID, "task-1")
	}
	if completed.Message != "Task completed successfully" {
		t.Errorf("message = %q, want %q", completed.Message, "Task completed successfully")
	}
	if _, ok := w.activeTasks["task-1"]; ok {
		t.Fatal("task should be removed from active tasks")
	}
}

func TestExecuteTaskReportsTaskFailedOnBackendError(t *testing.T) {
	w := &Worker{
		ctx:         context.Background(),
		config:      Config{},
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {cancel: func() {}}},
		backend:     &recordingBackend{err: errors.New("boom")},
	}

	w.executeTask(context.Background(), func() {}, trace.SpanFromContext(context.Background()), &types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task:   &types.Task{ID: "task-1", Title: "test task"},
	}, time.Now())

	msg := readWebSocketMessage(t, w.sendChan)
	if msg.Type != types.MessageTypeTaskFailed {
		t.Fatalf("message type = %q, want %q", msg.Type, types.MessageTypeTaskFailed)
	}

	var failed types.TaskFailedMessage
	if err := json.Unmarshal(msg.Data, &failed); err != nil {
		t.Fatalf("failed to unmarshal task failed message: %v", err)
	}
	if failed.TaskID != "task-1" {
		t.Errorf("task ID = %q, want %q", failed.TaskID, "task-1")
	}
	if failed.Message != "Failed to execute task: boom" {
		t.Errorf("message = %q, want %q", failed.Message, "Failed to execute task: boom")
	}
	if _, ok := w.activeTasks["task-1"]; ok {
		t.Fatal("task should be removed from active tasks")
	}
}

func TestExecuteTaskReportsUserFriendlyMessageOnDeadlineExceeded(t *testing.T) {
	w := &Worker{
		ctx:         context.Background(),
		config:      Config{},
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {cancel: func() {}}},
		backend:     &recordingBackend{err: context.DeadlineExceeded},
	}

	w.executeTask(context.Background(), func() {}, trace.SpanFromContext(context.Background()), &types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task:   &types.Task{ID: "task-1", Title: "test task"},
	}, time.Now())

	msg := readWebSocketMessage(t, w.sendChan)
	if msg.Type != types.MessageTypeTaskFailed {
		t.Fatalf("message type = %q, want %q", msg.Type, types.MessageTypeTaskFailed)
	}

	var failed types.TaskFailedMessage
	if err := json.Unmarshal(msg.Data, &failed); err != nil {
		t.Fatalf("failed to unmarshal task failed message: %v", err)
	}
	want := "The task exceeded its maximum allowed execution time and was terminated. Consider breaking the task into smaller steps or increasing the timeout."
	if failed.Message != want {
		t.Errorf("message = %q, want %q", failed.Message, want)
	}
}

func TestUserFacingTaskError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "context canceled",
			err:  context.Canceled,
			want: "The task was interrupted due to an infrastructure issue (context canceled). This is typically transient — please try again.",
		},
		{
			name: "wrapped context canceled",
			err:  fmt.Errorf("exec failed: %w", context.Canceled),
			want: "The task was interrupted due to an infrastructure issue (context canceled). This is typically transient — please try again.",
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
			want: "The task exceeded its maximum allowed execution time and was terminated. Consider breaking the task into smaller steps or increasing the timeout.",
		},
		{
			name: "generic error",
			err:  errors.New("boom"),
			want: "Failed to execute task: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := userFacingTaskError(tt.err)
			if got != tt.want {
				t.Errorf("userFacingTaskError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func readWebSocketMessage(t *testing.T, messages <-chan []byte) types.WebSocketMessage {
	t.Helper()

	select {
	case msgBytes := <-messages:
		var msg types.WebSocketMessage
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			t.Fatalf("failed to unmarshal websocket message: %v", err)
		}
		return msg
	default:
		t.Fatal("expected websocket message")
	}

	return types.WebSocketMessage{}
}

// TestRunHeartbeatAndWritesAreConcurrencySafe is a regression test for the
// "concurrent write to websocket connection" panic: the heartbeat loop used
// to send pings via WriteMessage, racing writeLoop's data writes on the same
// connection and crashing the whole worker process (taking the metrics
// exporter down with it). It floods the send channel while heartbeats fire
// every millisecond; before the fix this panicked within the test window.
func TestRunHeartbeatAndWritesAreConcurrencySafe(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var messagesReceived, pingsReceived atomic.Int64

	serverConnClosed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			t.Errorf("failed to upgrade connection: %v", err)
			return
		}
		defer close(serverConnClosed)
		defer conn.Close()
		conn.SetPingHandler(func(string) error {
			pingsReceived.Add(1)
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			messagesReceived.Add(1)
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial test server: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Worker{
		config:            Config{},
		conn:              conn,
		ctx:               ctx,
		cancel:            cancel,
		sendChan:          make(chan []byte, 256),
		activeTasks:       make(map[string]activeTask),
		heartbeatInterval: time.Millisecond,
	}

	runDone := make(chan struct{})
	go func() {
		w.run()
		close(runDone)
	}()

	// Flood data writes while heartbeats fire so the two write paths overlap
	// constantly for the duration of the test window.
	message := []byte(`{"type":"task_claimed","data":{"task_id":"task-1"}}`)
	deadline := time.After(500 * time.Millisecond)
flood:
	for {
		select {
		case <-deadline:
			break flood
		case w.sendChan <- message:
		}
	}

	// Tear down: cancelling the context stops writeLoop/heartbeatLoop, and
	// closing the connection unblocks readLoop so run() returns.
	cancel()
	conn.Close()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return after context cancellation and connection close")
	}
	select {
	case <-serverConnClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("server connection was not closed")
	}

	if messagesReceived.Load() == 0 {
		t.Error("expected the server to receive data messages")
	}
	if pingsReceived.Load() == 0 {
		t.Error("expected the server to receive heartbeat pings")
	}
}

func TestDefaultImageForTask(t *testing.T) {
	newWorker := func(defaultImage string) *Worker {
		ctx := context.Background()
		var k8sConfig *KubernetesBackendConfig
		if defaultImage != "" {
			k8sConfig = &KubernetesBackendConfig{DefaultImage: defaultImage}
		}
		return &Worker{
			ctx: ctx,
			config: Config{
				Kubernetes: k8sConfig,
			},
		}
	}

	envID := "env-123"

	t.Run("server-provided image wins over default_image", func(t *testing.T) {
		w := newWorker("my-registry.io/default:v1")
		got := w.defaultImageForTask("server-image:latest", &types.Task{})
		if got != "server-image:latest" {
			t.Errorf("got %q, want %q", got, "server-image:latest")
		}
	})

	t.Run("default_image used when server image empty", func(t *testing.T) {
		w := newWorker("my-registry.io/default:v1")
		got := w.defaultImageForTask("", &types.Task{})
		if got != "my-registry.io/default:v1" {
			t.Errorf("got %q, want %q", got, "my-registry.io/default:v1")
		}
	})

	t.Run("hardcoded fallback when no default_image configured", func(t *testing.T) {
		w := newWorker("")
		got := w.defaultImageForTask("", &types.Task{})
		if got != "ubuntu:22.04" {
			t.Errorf("got %q, want %q", got, "ubuntu:22.04")
		}
	})

	t.Run("hardcoded fallback when kubernetes config nil", func(t *testing.T) {
		w := &Worker{
			ctx:    context.Background(),
			config: Config{},
		}
		got := w.defaultImageForTask("", &types.Task{})
		if got != "ubuntu:22.04" {
			t.Errorf("got %q, want %q", got, "ubuntu:22.04")
		}
	})

	t.Run("hardcoded fallback with environment ID logs warning", func(t *testing.T) {
		w := newWorker("")
		task := &types.Task{
			AgentConfigSnapshot: &types.AmbientAgentConfig{
				EnvironmentID: &envID,
			},
		}
		got := w.defaultImageForTask("", task)
		if got != "ubuntu:22.04" {
			t.Errorf("got %q, want %q", got, "ubuntu:22.04")
		}
	})
}

func TestPrepareTaskParamsSidecarImageOverride(t *testing.T) {
	newWorker := func(sidecarImage string) *Worker {
		ctx := context.Background()
		var k8sConfig *KubernetesBackendConfig
		if sidecarImage != "" {
			k8sConfig = &KubernetesBackendConfig{SidecarImage: sidecarImage}
		} else {
			k8sConfig = &KubernetesBackendConfig{}
		}
		return &Worker{
			ctx: ctx,
			config: Config{
				Kubernetes: k8sConfig,
			},
		}
	}

	t.Run("config sidecar_image overrides server-provided image", func(t *testing.T) {
		w := newWorker("my-registry.io/warpdotdev/warp-agent:latest")
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID:       "task-1",
			Task:         &types.Task{ID: "task-1"},
			SidecarImage: "docker.io/warpdotdev/warp-agent:latest",
		})
		if len(params.Sidecars) == 0 {
			t.Fatal("expected at least one sidecar")
		}
		if params.Sidecars[0].Image != "my-registry.io/warpdotdev/warp-agent:latest" {
			t.Errorf("sidecar image = %q, want %q", params.Sidecars[0].Image, "my-registry.io/warpdotdev/warp-agent:latest")
		}
	})

	t.Run("server-provided image used when config sidecar_image empty", func(t *testing.T) {
		w := newWorker("")
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID:       "task-1",
			Task:         &types.Task{ID: "task-1"},
			SidecarImage: "docker.io/warpdotdev/warp-agent:latest",
		})
		if len(params.Sidecars) == 0 {
			t.Fatal("expected at least one sidecar")
		}
		if params.Sidecars[0].Image != "docker.io/warpdotdev/warp-agent:latest" {
			t.Errorf("sidecar image = %q, want %q", params.Sidecars[0].Image, "docker.io/warpdotdev/warp-agent:latest")
		}
	})

	t.Run("no sidecar when server provides empty sidecar image", func(t *testing.T) {
		w := newWorker("my-registry.io/warpdotdev/warp-agent:latest")
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID:       "task-1",
			Task:         &types.Task{ID: "task-1"},
			SidecarImage: "",
		})
		if len(params.Sidecars) != 0 {
			t.Errorf("expected no sidecars when server sidecar image is empty, got %d", len(params.Sidecars))
		}
	})
}

func TestPrepareTaskParamsCodingCLISidecarOverride(t *testing.T) {
	newWorker := func(codingCLISidecars map[string]string) *Worker {
		return &Worker{
			ctx: context.Background(),
			config: Config{
				Kubernetes: &KubernetesBackendConfig{
					CodingCLISidecars: codingCLISidecars,
				},
			},
		}
	}

	strPtr := func(s string) *string { return &s }

	// harnessTask builds a task whose agent config snapshot advertises the given
	// harness type. A nil harnessType produces a snapshot with a harness whose
	// Type pointer is nil.
	harnessTask := func(harnessType *string) *types.Task {
		return &types.Task{
			ID: "task-1",
			AgentConfigSnapshot: &types.AmbientAgentConfig{
				Harness: &types.Harness{Type: harnessType},
			},
		}
	}

	findSidecar := func(sidecars []types.SidecarMount, mountPath string) (types.SidecarMount, bool) {
		for _, s := range sidecars {
			if s.MountPath == mountPath {
				return s, true
			}
		}
		return types.SidecarMount{}, false
	}

	t.Run("overrides server-provided coding CLI sidecar at mount path", func(t *testing.T) {
		w := newWorker(map[string]string{"claude": "registry.internal/my-claude:v1"})
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID:       "task-1",
			Task:         harnessTask(strPtr("claude")),
			SidecarImage: "docker.io/warpdotdev/warp-agent:latest",
			AdditionalSidecars: []types.SidecarMount{
				{Image: "docker.io/warpdotdev/claude-cli:latest", MountPath: "/mnt/claude-cli-sidecar", ReadWrite: true},
			},
		})

		got, ok := findSidecar(params.Sidecars, "/mnt/claude-cli-sidecar")
		if !ok {
			t.Fatalf("expected coding CLI sidecar at /mnt/claude-cli-sidecar, got %+v", params.Sidecars)
		}
		if got.Image != "registry.internal/my-claude:v1" {
			t.Errorf("image = %q, want %q", got.Image, "registry.internal/my-claude:v1")
		}
		// Overriding only replaces the image; other mount options are preserved.
		if !got.ReadWrite {
			t.Error("expected ReadWrite to be preserved on overridden sidecar")
		}
		// No new sidecar should be injected: the /agent + claude sidecars remain.
		if len(params.Sidecars) != 2 {
			t.Fatalf("sidecar count = %d, want 2: %+v", len(params.Sidecars), params.Sidecars)
		}
		// The warp-agent sidecar at /agent must be untouched.
		if agent, ok := findSidecar(params.Sidecars, "/agent"); !ok || agent.Image != "docker.io/warpdotdev/warp-agent:latest" {
			t.Errorf("agent sidecar = %+v, want image docker.io/warpdotdev/warp-agent:latest", agent)
		}
	})

	t.Run("injects coding CLI sidecar when server did not provide one", func(t *testing.T) {
		w := newWorker(map[string]string{"claude": "registry.internal/my-claude:v1"})
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-1",
			Task:   harnessTask(strPtr("claude")),
		})

		got, ok := findSidecar(params.Sidecars, "/mnt/claude-cli-sidecar")
		if !ok {
			t.Fatalf("expected injected coding CLI sidecar at /mnt/claude-cli-sidecar, got %+v", params.Sidecars)
		}
		if got.Image != "registry.internal/my-claude:v1" {
			t.Errorf("image = %q, want %q", got.Image, "registry.internal/my-claude:v1")
		}
	})

	t.Run("trims whitespace from harness type", func(t *testing.T) {
		w := newWorker(map[string]string{"claude": "registry.internal/my-claude:v1"})
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-1",
			Task:   harnessTask(strPtr("  claude  ")),
		})
		if _, ok := findSidecar(params.Sidecars, "/mnt/claude-cli-sidecar"); !ok {
			t.Fatalf("expected coding CLI sidecar at /mnt/claude-cli-sidecar, got %+v", params.Sidecars)
		}
	})

	t.Run("no override when harness type not configured", func(t *testing.T) {
		w := newWorker(map[string]string{"claude": "registry.internal/my-claude:v1"})
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-1",
			Task:   harnessTask(strPtr("codex")),
		})
		if _, ok := findSidecar(params.Sidecars, "/mnt/codex-cli-sidecar"); ok {
			t.Error("did not expect coding CLI sidecar for unconfigured harness")
		}
		if _, ok := findSidecar(params.Sidecars, "/mnt/claude-cli-sidecar"); ok {
			t.Error("did not expect claude coding CLI sidecar for codex harness")
		}
	})

	t.Run("no override when configured image is empty", func(t *testing.T) {
		w := newWorker(map[string]string{"claude": ""})
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-1",
			Task:   harnessTask(strPtr("claude")),
		})
		if _, ok := findSidecar(params.Sidecars, "/mnt/claude-cli-sidecar"); ok {
			t.Error("did not expect coding CLI sidecar when configured image is empty")
		}
	})

	t.Run("no override when task has no harness snapshot", func(t *testing.T) {
		w := newWorker(map[string]string{"claude": "registry.internal/my-claude:v1"})
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-1",
			Task:   &types.Task{ID: "task-1"},
		})
		if len(params.Sidecars) != 0 {
			t.Errorf("expected no sidecars when task has no harness, got %d: %+v", len(params.Sidecars), params.Sidecars)
		}
	})
}

func TestPrepareTaskParamsTeamShareConditional(t *testing.T) {
	newWorker := func() *Worker {
		return &Worker{
			ctx: context.Background(),
			config: Config{
				ServerRootURL: "https://app.warp.dev",
				Kubernetes:    &KubernetesBackendConfig{},
			},
		}
	}

	containsShareTeamEdit := func(args []string) bool {
		for i, arg := range args {
			if arg == "--share" && i+1 < len(args) && args[i+1] == "team:edit" {
				return true
			}
		}
		return false
	}

	t.Run("includes --share team:edit for team-owned task", func(t *testing.T) {
		w := newWorker()
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-1",
			Task: &types.Task{
				ID:    "task-1",
				Owner: &types.TaskOwner{Type: "TEAM", Id: 42},
			},
		})
		if !containsShareTeamEdit(params.BaseArgs) {
			t.Fatalf("expected --share team:edit in args for team-owned task, got %v", params.BaseArgs)
		}
	})

	t.Run("omits --share team:edit for user-owned task", func(t *testing.T) {
		w := newWorker()
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-2",
			Task: &types.Task{
				ID:    "task-2",
				Owner: &types.TaskOwner{Type: "USER", Id: 99},
			},
		})
		if containsShareTeamEdit(params.BaseArgs) {
			t.Fatalf("did not expect --share team:edit in args for user-owned task, got %v", params.BaseArgs)
		}
	})

	t.Run("omits --share team:edit when owner is nil", func(t *testing.T) {
		w := newWorker()
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-3",
			Task: &types.Task{
				ID: "task-3",
			},
		})
		if containsShareTeamEdit(params.BaseArgs) {
			t.Fatalf("did not expect --share team:edit in args when owner is nil, got %v", params.BaseArgs)
		}
	})
}

func TestPrepareTaskParamsThreadsInstanceShape(t *testing.T) {
	w := &Worker{
		ctx: context.Background(),
		config: Config{
			ServerRootURL: "https://app.warp.dev",
			Kubernetes:    &KubernetesBackendConfig{},
		},
	}

	t.Run("threads shape when present", func(t *testing.T) {
		shape := &types.InstanceShape{Vcpus: 4, MemoryGb: 16}
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID:        "task-1",
			Task:          &types.Task{ID: "task-1"},
			InstanceShape: shape,
		})
		if params.InstanceShape != shape {
			t.Fatalf("InstanceShape = %v, want %v", params.InstanceShape, shape)
		}
	})

	t.Run("nil shape when absent", func(t *testing.T) {
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-2",
			Task:   &types.Task{ID: "task-2"},
		})
		if params.InstanceShape != nil {
			t.Fatalf("InstanceShape = %v, want nil", params.InstanceShape)
		}
	})
}

func TestPrepareTaskParamsIncludesServerRootURLForHarnessSupport(t *testing.T) {
	w := &Worker{
		ctx: context.Background(),
		config: Config{
			ServerRootURL: "https://staging.example.com",
			Kubernetes:    &KubernetesBackendConfig{},
		},
	}

	params := w.prepareTaskParams(&types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task:   &types.Task{ID: "task-1"},
	})

	want := warpServerRootURLEnv + "=https://staging.example.com"
	for _, entry := range params.EnvVars {
		if entry == want {
			return
		}
	}
	t.Fatalf("expected %s in env vars, got %v", want, params.EnvVars)
}

func TestPrepareTaskParamsAdditionalOzArgs(t *testing.T) {
	newWorker := func() *Worker {
		return &Worker{
			ctx: context.Background(),
			config: Config{
				ServerRootURL: "https://app.warp.dev",
				Kubernetes:    &KubernetesBackendConfig{},
			},
		}
	}

	containsSkipInitialTurn := func(args []string) bool {
		for _, arg := range args {
			if arg == "--skip-initial-turn" {
				return true
			}
		}
		return false
	}

	t.Run("forwards server supplemental oz args", func(t *testing.T) {
		w := newWorker()
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID:           "task-skip",
			Task:             &types.Task{ID: "task-skip"},
			AdditionalOzArgs: []string{"--skip-initial-turn"},
		})
		if !containsSkipInitialTurn(params.BaseArgs) {
			t.Fatalf("expected --skip-initial-turn in args, got %v", params.BaseArgs)
		}
	})
	t.Run("does not add omitted supplemental oz args", func(t *testing.T) {
		w := newWorker()
		params := w.prepareTaskParams(&types.TaskAssignmentMessage{
			TaskID: "task-no-skip",
			Task:   &types.Task{ID: "task-no-skip"},
		})
		if containsSkipInitialTurn(params.BaseArgs) {
			t.Fatalf("did not expect --skip-initial-turn in args, got %v", params.BaseArgs)
		}
	})
}

func TestWorkerShutdownUsesFreshContextForBackendCleanup(t *testing.T) {
	workerCtx, cancel := context.WithCancel(context.Background())
	backend := &shutdownRecordingBackend{}
	w := &Worker{
		ctx:         workerCtx,
		cancel:      cancel,
		activeTasks: make(map[string]activeTask),
		backend:     backend,
	}

	w.Shutdown()

	if !backend.shutdownCalled {
		t.Fatal("expected backend shutdown to be called")
	}
	if backend.shutdownCtxErr != nil {
		t.Fatalf("expected backend shutdown context to be active, got %v", backend.shutdownCtxErr)
	}
}

func TestWorkerShutdownPreservesActiveTasksForPreservingBackend(t *testing.T) {
	workerCtx, cancel := context.WithCancel(context.Background())
	backend := &preservingShutdownRecordingBackend{}
	cancelledTask := false
	w := &Worker{
		ctx:    workerCtx,
		cancel: cancel,
		activeTasks: map[string]activeTask{
			"task-1": {cancel: func() {
				cancelledTask = true
			}},
		},
		backend: backend,
	}

	w.Shutdown()

	if cancelledTask {
		t.Fatal("expected active task to be preserved, but cancel function was called")
	}
	if !backend.shutdownCalled {
		t.Fatal("expected backend shutdown to be called")
	}
}

func TestHandleTaskAssignmentDoesNotStartTaskAfterShutdownDuringClaim(t *testing.T) {
	workerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &Worker{
		ctx:         workerCtx,
		cancel:      cancel,
		config:      Config{},
		sendChan:    make(chan []byte, 1),
		activeTasks: make(map[string]activeTask),
		backend:     &preservingShutdownRecordingBackend{},
	}

	w.handleTaskAssignment(&types.TaskAssignmentMessage{
		TaskID: "task-1",
		Task:   &types.Task{ID: "task-1", Title: "test task"},
	})

	if len(w.activeTasks) != 0 {
		t.Fatalf("expected no active tasks to start after shutdown during claim, got %d", len(w.activeTasks))
	}
}
