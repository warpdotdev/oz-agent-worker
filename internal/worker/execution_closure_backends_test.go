package worker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDirectExecutionWorkspaceIsolation(t *testing.T) {
	for _, predecessor := range []string{"", "old"} {
		t.Run("predecessor="+predecessor, func(t *testing.T) {
			root := t.TempDir()
			ozPath := filepath.Join(root, "oz")
			if err := os.WriteFile(ozPath, []byte("#!/bin/sh\nprintf '%s' \"$PWD\" > \"$CAPTURE\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			backend, err := NewDirectBackend(context.Background(), DirectBackendConfig{
				WorkspaceRoot: filepath.Join(root, "workspaces"), OzPath: ozPath, NoCleanup: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			var workspaces []string
			for _, id := range []string{predecessor, "new"} {
				capture := filepath.Join(root, "capture-"+id)
				result := backend.ExecuteTask(context.Background(), &TaskParams{
					TaskID: "run", ExecutionID: id, EnvVars: []string{"CAPTURE=" + capture},
				})
				if result.Error != nil {
					t.Fatal(result.Error)
				}
				workspace, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				workspaces = append(workspaces, string(workspace))
			}
			backend.cleanup(context.Background(), "run", workspaces[0], "")
			if _, err := os.Stat(workspaces[1]); err != nil {
				t.Fatalf("predecessor cleanup removed successor workspace: %v", err)
			}
		})
	}
}

type closureDockerTransport func(*http.Request) (*http.Response, error)

func (f closureDockerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDockerExecutionClosure(t *testing.T) {
	t.Run("failed removal retains exact resource for retry", func(t *testing.T) {
		fail := true
		var removed []string
		transport := closureDockerTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/_ping" {
				return mockEngineResponse(r, http.StatusOK, "text/plain", "OK"), nil
			}
			removed = append(removed, r.URL.Path)
			if r.Method != http.MethodDelete || !strings.HasSuffix(r.URL.Path, "/containers/old-container") {
				t.Fatalf("unexpected cleanup request: %s %s", r.Method, r.URL.Path)
			}
			if fail {
				return mockEngineErrorResponse(t, r, http.StatusInternalServerError, "unavailable"), nil
			}
			return mockEngineResponse(r, http.StatusNoContent, "application/json", ""), nil
		})
		dockerClient, err := client.New(client.WithHTTPClient(&http.Client{Transport: transport}))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = dockerClient.Close() }()
		backend := &DockerBackend{dockerClient: dockerClient}
		old := taskExecution{"run", "old"}
		successor := taskExecution{"run", "new"}
		backend.taskContainers.Store(old, "old-container")
		backend.taskContainers.Store(successor, "new-container")
		params := &CancelParams{TaskID: "run", ExecutionID: "old"}
		if err := backend.CancelTask(context.Background(), params); err == nil {
			t.Fatal("expected retryable removal failure")
		}
		if closed, _ := backend.ConfirmTaskClosed(context.Background(), params); closed {
			t.Fatal("failed removal reported closure")
		}
		fail = false
		if err := backend.CancelTask(context.Background(), params); err != nil {
			t.Fatal(err)
		}
		if closed, err := backend.ConfirmTaskClosed(context.Background(), params); !closed || err != nil {
			t.Fatalf("closed=%v, error=%v", closed, err)
		}
		if _, found := backend.taskContainers.Load(successor); !found {
			t.Fatal("successor tracking removed")
		}
		if len(removed) != 2 {
			t.Fatalf("removal attempts = %d", len(removed))
		}
	})

	t.Run("unknown create outcome cannot acknowledge absence", func(t *testing.T) {
		backend := &DockerBackend{}
		backend.taskContainers.Store(taskExecution{"run", "old"}, "")
		if err := backend.CancelTask(context.Background(), &CancelParams{TaskID: "run", ExecutionID: "old"}); err == nil {
			t.Fatal("ambiguous creation treated as closure")
		}
	})
}

func TestKubernetesExecutionClosure(t *testing.T) {
	t.Run("deleting job is insufficient while pods remain", func(t *testing.T) {
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}}
		params := &CancelParams{TaskID: "run", ExecutionID: "old"}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "old-pod", Namespace: "agents", Labels: backend.baseLabels("run", "old"),
		}}
		clientset := fake.NewSimpleClientset(pod)
		backend.clientset = clientset
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			cancel()
			return false, nil, nil
		})
		if closed, err := backend.ConfirmTaskClosed(ctx, params); closed || !errors.Is(err, context.Canceled) {
			t.Fatalf("remaining pod yielded closed=%v, err=%v", closed, err)
		}
		if err := clientset.CoreV1().Pods("agents").Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if closed, err := backend.ConfirmTaskClosed(context.Background(), params); !closed || err != nil {
			t.Fatalf("removed pod yielded closed=%v, err=%v", closed, err)
		}
	})

	t.Run("API errors are not absence", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "job", errors.New("denied"))
		})
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: clientset}
		if closed, err := backend.ConfirmTaskClosed(context.Background(), &CancelParams{TaskID: "run", ExecutionID: "old"}); closed || err == nil {
			t.Fatalf("API failure yielded closed=%v, err=%v", closed, err)
		}
	})

	t.Run("name suffix collision never deletes successor", func(t *testing.T) {
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}}
		oldID, newID := "old-12345678", "new-12345678"
		jobName := kubernetesTaskJobName("run", newID)
		if jobName != kubernetesTaskJobName("run", oldID) {
			t.Fatal("test requires colliding names")
		}
		clientset := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: jobName, Namespace: "agents", Labels: backend.baseLabels("run", newID),
		}})
		backend.clientset = clientset
		if err := backend.CancelTask(context.Background(), &CancelParams{TaskID: "run", ExecutionID: oldID}); err == nil {
			t.Fatal("expected execution mismatch")
		}
		if _, err := clientset.BatchV1().Jobs("agents").Get(context.Background(), jobName, metav1.GetOptions{}); err != nil {
			t.Fatalf("successor removed: %v", err)
		}
	})

	t.Run("existing job cannot be acknowledged closed", func(t *testing.T) {
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}}
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: kubernetesTaskJobName("run", "old"), Namespace: "agents", Labels: backend.baseLabels("run", "old"),
		}}
		backend.clientset = fake.NewSimpleClientset(job)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if closed, err := backend.ConfirmTaskClosed(ctx, &CancelParams{TaskID: "run", ExecutionID: "old"}); closed || err == nil {
			t.Fatalf("live Job yielded closed=%v, err=%v", closed, err)
		}
	})
}
