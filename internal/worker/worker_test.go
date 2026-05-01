package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"testing"
	"time"

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

type recordingBackend struct {
	err error
}

func (b *recordingBackend) ExecuteTask(context.Context, *TaskParams) error {
	return b.err
}

func (b *recordingBackend) Shutdown(context.Context) {}

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

func TestExecuteTaskReportsTaskCancelledOnContextCancellation(t *testing.T) {
	w := &Worker{
		ctx:         context.Background(),
		config:      Config{},
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {cancel: func() {}}},
		backend:     &recordingBackend{err: context.Canceled},
	}

	w.executeTask(context.Background(), trace.SpanFromContext(context.Background()), &types.TaskAssignmentMessage{
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
	if _, ok := w.activeTasks["task-1"]; ok {
		t.Fatal("task should be removed from active tasks")
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
}

func TestExecuteTaskReportsTaskCompletedOnSuccess(t *testing.T) {
	w := &Worker{
		ctx:         context.Background(),
		config:      Config{},
		sendChan:    make(chan []byte, 1),
		activeTasks: map[string]activeTask{"task-1": {cancel: func() {}}},
		backend:     &recordingBackend{},
	}

	w.executeTask(context.Background(), trace.SpanFromContext(context.Background()), &types.TaskAssignmentMessage{
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

	w.executeTask(context.Background(), trace.SpanFromContext(context.Background()), &types.TaskAssignmentMessage{
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
