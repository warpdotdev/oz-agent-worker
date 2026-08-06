package worker

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A resource whose deletion was not confirmed must stay registered. Dropping
// the identifier first leaves nothing to retry, so a transient API failure
// silently becomes a resource that outlives its cleanup grace.
func TestKubernetesCleanupRetainsTheJobWhenDeletionFails(t *testing.T) {
	client := fake.NewSimpleClientset()
	var deletes atomic.Int32
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if deletes.Add(1) == 1 {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})

	backend := &KubernetesBackend{
		config:    KubernetesBackendConfig{WorkerID: "worker-123", Namespace: "agents"},
		clientset: client,
		jobs:      make(map[executionKey]*retainedJob),
	}
	if _, err := client.BatchV1().Jobs("agents").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "oz-task-task-1-exec-ution-1", Namespace: "agents"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to seed Job: %v", err)
	}
	backend.registerJob("task-1", "execution-1", "oz-task-task-1-exec-ution-1")
	backend.setJobDeleteAtCleanup("task-1", "execution-1", true)

	params := &CancelParams{TaskID: "task-1", ExecutionID: "execution-1"}
	if err := backend.CleanupTaskResources(context.Background(), params); err == nil {
		t.Fatal("expected the failed deletion to be surfaced")
	}
	if !backend.ownsJob("task-1", "execution-1") {
		t.Fatal("the Job was forgotten despite an unconfirmed deletion, so nothing can retry it")
	}

	// The retry succeeds, and only then is the identifier released.
	if err := backend.CleanupTaskResources(context.Background(), params); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if backend.ownsJob("task-1", "execution-1") {
		t.Fatal("the Job should be forgotten once deletion is confirmed")
	}

	jobs, err := client.BatchV1().Jobs("agents").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected the Job to be deleted on retry, got %d", len(jobs.Items))
	}
}

func TestKubernetesCleanupTreatsAnAbsentJobAsDeleted(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, "oz-task-task-1-exec-ution-1")
	})

	backend := &KubernetesBackend{
		config:    KubernetesBackendConfig{WorkerID: "worker-123", Namespace: "agents"},
		clientset: client,
		jobs:      make(map[executionKey]*retainedJob),
	}
	backend.registerJob("task-1", "execution-1", "oz-task-task-1-exec-ution-1")
	backend.setJobDeleteAtCleanup("task-1", "execution-1", true)

	if err := backend.CleanupTaskResources(context.Background(), &CancelParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
	}); err != nil {
		t.Fatalf("an already-absent Job must count as deleted: %v", err)
	}
	if backend.ownsJob("task-1", "execution-1") {
		t.Fatal("an already-absent Job should be forgotten")
	}
}

func TestKubernetesCleanupReleasesAFailedJobWithoutDeletingIt(t *testing.T) {
	// A failed Job is deliberately left for the TTL controller, so cleanup has
	// nothing to confirm and must still release its registry entry.
	client := fake.NewSimpleClientset()
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		t.Error("a failed Job must not be deleted at the cleanup-grace deadline")
		return true, nil, nil
	})

	backend := &KubernetesBackend{
		config:    KubernetesBackendConfig{WorkerID: "worker-123", Namespace: "agents"},
		clientset: client,
		jobs:      make(map[executionKey]*retainedJob),
	}
	backend.registerJob("task-1", "execution-1", "oz-task-task-1-exec-ution-1")
	backend.setJobDeleteAtCleanup("task-1", "execution-1", false)

	if err := backend.CleanupTaskResources(context.Background(), &CancelParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
	}); err != nil {
		t.Fatalf("CleanupTaskResources: %v", err)
	}
	if backend.ownsJob("task-1", "execution-1") {
		t.Fatal("a TTL-owned Job should still be released from the registry")
	}
}

// failingCleanupBackend refuses cleanup a fixed number of times so a test can
// exercise the shutdown sweep's retry and its give-up path.
type failingCleanupBackend struct {
	noLogSnapshotBackend
	failures atomic.Int32
	attempts atomic.Int32
	// delay stalls each attempt so a test can prove one slow entry does not
	// starve another.
	delay time.Duration
}

func (b *failingCleanupBackend) ExecuteTask(context.Context, *TaskParams) ExecuteResult {
	return executeCompleted()
}

func (b *failingCleanupBackend) CancelTask(context.Context, *CancelParams) error { return nil }

func (b *failingCleanupBackend) PreservesTasksOnShutdown() bool { return false }

func (b *failingCleanupBackend) Shutdown(context.Context) {}

func (b *failingCleanupBackend) CleanupTaskResources(ctx context.Context, _ *CancelParams) error {
	b.attempts.Add(1)
	if b.delay > 0 {
		select {
		case <-time.After(b.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if b.failures.Load() > 0 {
		b.failures.Add(-1)
		return errors.New("transient backend failure")
	}
	return nil
}

func TestShutdownRetriesATransientCleanupFailure(t *testing.T) {
	backend := &failingCleanupBackend{}
	backend.failures.Store(2)
	w := newDebugArchiveWorker(t, backend, "60m")

	w.tasks.StartTask("task-1", activeTask{cancel: func() {}, executionID: "exec-1"}, debuglog.BackendKubernetes, nil)
	w.beginCleanupGrace(&types.TaskAssignmentMessage{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task:        &types.Task{ID: "task-1"},
	})
	w.tasks.Delete("task-1")

	w.Shutdown()

	if got := backend.attempts.Load(); got != 3 {
		t.Fatalf("cleanup attempts = %d, want the transient failures retried up to %d", got, shutdownCleanupAttempts)
	}
	if backend.failures.Load() != 0 {
		t.Fatal("expected the retry to eventually succeed")
	}
}

func TestShutdownGivesUpAfterTheRetryBudget(t *testing.T) {
	backend := &failingCleanupBackend{}
	backend.failures.Store(100)
	w := newDebugArchiveWorker(t, backend, "60m")

	w.tasks.StartTask("task-1", activeTask{cancel: func() {}, executionID: "exec-1"}, debuglog.BackendDocker, nil)
	w.beginCleanupGrace(&types.TaskAssignmentMessage{
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		Task:        &types.Task{ID: "task-1"},
	})
	w.tasks.Delete("task-1")

	w.Shutdown()

	if got := backend.attempts.Load(); got != shutdownCleanupAttempts {
		t.Fatalf("cleanup attempts = %d, want the bounded %d", got, shutdownCleanupAttempts)
	}
}

func TestShutdownGivesEveryEntryItsOwnBudget(t *testing.T) {
	// A shared budget let one slow backend call consume the whole allowance
	// and starve the entries behind it. Each entry now gets its own.
	backend := &failingCleanupBackend{delay: 300 * time.Millisecond}
	w := newDebugArchiveWorker(t, backend, "60m")

	const entries = 8
	for i := 0; i < entries; i++ {
		taskID := "task-" + string(rune('a'+i))
		w.tasks.StartTask(taskID, activeTask{cancel: func() {}, executionID: "exec-1"}, debuglog.BackendDocker, nil)
		w.beginCleanupGrace(&types.TaskAssignmentMessage{
			TaskID:      taskID,
			ExecutionID: "exec-1",
			Task:        &types.Task{ID: taskID},
		})
		w.tasks.Delete(taskID)
	}

	w.Shutdown()

	if got := backend.attempts.Load(); got != entries {
		t.Fatalf("cleanup attempts = %d, want every one of the %d entries attempted", got, entries)
	}
}

// unreachableDockerBackend points at a socket that does not exist, so every
// daemon call fails the way a transient outage would.
func unreachableDockerBackend(t *testing.T) *DockerBackend {
	t.Helper()
	dockerClient, err := client.New(client.WithHost("unix://" + filepath.Join(t.TempDir(), "absent.sock")))
	if err != nil {
		t.Fatalf("failed to build a Docker client: %v", err)
	}
	t.Cleanup(func() { _ = dockerClient.Close() })
	return &DockerBackend{dockerClient: dockerClient, containers: make(map[executionKey]string)}
}

func TestDockerCleanupRetainsTheContainerWhenRemovalFails(t *testing.T) {
	backend := unreachableDockerBackend(t)
	backend.registerContainer("task-1", "execution-1", "container-abc")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := backend.CleanupTaskResources(ctx, &CancelParams{TaskID: "task-1", ExecutionID: "execution-1"})
	if err == nil {
		t.Fatal("expected the failed removal to be surfaced rather than swallowed")
	}
	if _, ok := backend.lookupContainer("task-1", "execution-1"); !ok {
		t.Fatal("the container was forgotten despite an unconfirmed removal, so nothing can retry it")
	}
}

func TestDockerCleanupReleasesTheContainerWhenCleanupIsDisabled(t *testing.T) {
	// With cleanup disabled the container is intentionally left running, so
	// there is nothing to confirm and the registry entry must still be freed.
	backend := unreachableDockerBackend(t)
	backend.config.NoCleanup = true
	backend.registerContainer("task-1", "execution-1", "container-abc")

	if err := backend.CleanupTaskResources(context.Background(), &CancelParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
	}); err != nil {
		t.Fatalf("CleanupTaskResources: %v", err)
	}
	if _, ok := backend.lookupContainer("task-1", "execution-1"); ok {
		t.Fatal("the registry entry should be released when cleanup is disabled")
	}
}

func TestDockerCleanupIsANoOpForAnUnknownExecution(t *testing.T) {
	backend := &DockerBackend{containers: make(map[executionKey]string)}

	if err := backend.CleanupTaskResources(context.Background(), &CancelParams{
		TaskID:      "unknown-task",
		ExecutionID: "unknown-execution",
	}); err != nil {
		t.Fatalf("CleanupTaskResources: %v", err)
	}
}
