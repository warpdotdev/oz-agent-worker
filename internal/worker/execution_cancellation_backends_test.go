package worker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

type cancellationDockerTransport func(*http.Request) (*http.Response, error)

func (f cancellationDockerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDockerExecutionCancellation(t *testing.T) {
	t.Run("failed removal retains exact resource for retry", func(t *testing.T) {
		fail := true
		var removed []string
		transport := cancellationDockerTransport(func(r *http.Request) (*http.Response, error) {
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
		if id, found := backend.taskContainers.Load(old); !found || id != "old-container" {
			t.Fatal("failed removal lost resource tracking")
		}
		fail = false
		if err := backend.CancelTask(context.Background(), params); err != nil {
			t.Fatal(err)
		}
		if err := backend.CancelTask(context.Background(), params); err != nil {
			t.Fatal(err)
		}
		if _, found := backend.taskContainers.Load(successor); !found {
			t.Fatal("successor tracking removed")
		}
		if len(removed) != 2 {
			t.Fatalf("removal attempts = %d", len(removed))
		}
	})

	t.Run("unknown create outcome remains an observable failure", func(t *testing.T) {
		backend := &DockerBackend{}
		backend.taskContainers.Store(taskExecution{"run", "old"}, "")
		if err := backend.CancelTask(context.Background(), &CancelParams{TaskID: "run", ExecutionID: "old"}); err == nil {
			t.Fatal("ambiguous creation treated as accepted cancellation")
		}
	})
}

func TestKubernetesExecutionCancellation(t *testing.T) {
	t.Run("accepted deletion succeeds even while job and pods remain", func(t *testing.T) {
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}}
		params := &CancelParams{TaskID: "run", ExecutionID: "old"}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "old-pod", Namespace: "agents", Labels: backend.baseLabels("run", "old"),
		}}
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: kubernetesTaskJobName("run", "old"), Namespace: "agents", Labels: backend.baseLabels("run", "old"),
		}}
		clientset := fake.NewSimpleClientset(job, pod)
		backend.clientset = clientset
		clientset.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, nil
		})
		if err := backend.CancelTask(context.Background(), params); err != nil {
			t.Fatal(err)
		}
		actions := clientset.Actions()
		if len(actions) != 2 || actions[0].GetVerb() != "get" || actions[1].GetVerb() != "delete" {
			t.Fatalf("cancellation should only get and delete the job: %v", actions)
		}
	})

	t.Run("API errors are not absence", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		clientset.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "job", errors.New("denied"))
		})
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}, clientset: clientset}
		if err := backend.CancelTask(context.Background(), &CancelParams{TaskID: "run", ExecutionID: "old"}); err == nil {
			t.Fatal("API failure treated as accepted cancellation")
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

	t.Run("missing job cancellation is idempotent", func(t *testing.T) {
		backend := &KubernetesBackend{config: KubernetesBackendConfig{Namespace: "agents"}}
		backend.clientset = fake.NewSimpleClientset()
		for i := 0; i < 2; i++ {
			if err := backend.CancelTask(context.Background(), &CancelParams{TaskID: "run", ExecutionID: "old"}); err != nil {
				t.Fatal(err)
			}
		}
	})
}
