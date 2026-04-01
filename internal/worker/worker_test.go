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
