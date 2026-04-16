package worker

import (
	"context"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
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
		}, "")
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
		}, "")
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
		}, "")
		if len(params.Sidecars) != 0 {
			t.Errorf("expected no sidecars when server sidecar image is empty, got %d", len(params.Sidecars))
		}
	})
}

func TestWorkerShutdownUsesFreshContextForBackendCleanup(t *testing.T) {
	workerCtx, cancel := context.WithCancel(context.Background())
	backend := &shutdownRecordingBackend{}
	w := &Worker{
		ctx:         workerCtx,
		cancel:      cancel,
		activeTasks: make(map[string]context.CancelFunc),
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
