package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func executionPod(backend *KubernetesBackend, name, taskID, executionID string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: backend.config.Namespace,
			Labels:    backend.baseLabels(taskID, executionID),
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "sidecar-init"}},
			Containers:     []corev1.Container{{Name: "task"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "task", RestartCount: restarts}},
		},
	}
}

func newKubernetesLogBackend(t *testing.T, pods ...*corev1.Pod) *KubernetesBackend {
	t.Helper()
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{WorkerID: "worker-123", Namespace: "agents"},
		jobs:   make(map[executionKey]*retainedJob),
	}
	client := fake.NewSimpleClientset()
	for _, pod := range pods {
		if _, err := client.CoreV1().Pods(backend.config.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to seed pod %s: %v", pod.Name, err)
		}
	}
	backend.clientset = client
	return backend
}

func TestKubernetesSnapshotRequiresARegisteredJob(t *testing.T) {
	backend := newKubernetesLogBackend(t)

	err := backend.SnapshotTaskLogs(context.Background(), &SnapshotParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Sink:        &captureSink{},
	})

	var snapshotErr *debuglog.SnapshotError
	if !errors.As(err, &snapshotErr) {
		t.Fatalf("error = %v, want a *debuglog.SnapshotError", err)
	}
	if snapshotErr.ReasonCode != types.DebugArchiveReasonResourceNotFound {
		t.Fatalf("reason = %q, want %q", snapshotErr.ReasonCode, types.DebugArchiveReasonResourceNotFound)
	}
}

func TestKubernetesSnapshotReportsMissingPodsAsNotFound(t *testing.T) {
	backend := newKubernetesLogBackend(t)
	backend.registerJob("task-1", "execution-1", "oz-task-task-1-exec-ution-1")

	err := backend.SnapshotTaskLogs(context.Background(), &SnapshotParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Sink:        &captureSink{},
	})

	var snapshotErr *debuglog.SnapshotError
	if !errors.As(err, &snapshotErr) {
		t.Fatalf("error = %v, want a *debuglog.SnapshotError", err)
	}
	if snapshotErr.ReasonCode != types.DebugArchiveReasonResourceNotFound {
		t.Fatalf("reason = %q, want %q", snapshotErr.ReasonCode, types.DebugArchiveReasonResourceNotFound)
	}
}

func TestListExecutionPodsRequiresEveryIdentityLabel(t *testing.T) {
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{WorkerID: "worker-123", Namespace: "agents"},
	}
	wanted := executionPod(backend, "wanted", "task-1", "execution-1", 0)

	// A pod from another execution, another task, and another worker must all
	// be excluded even though each shares one label with the target.
	otherExecution := executionPod(backend, "other-execution", "task-1", "execution-2", 0)
	otherTask := executionPod(backend, "other-task", "task-2", "execution-1", 0)

	otherWorker := &KubernetesBackend{
		config: KubernetesBackendConfig{WorkerID: "worker-999", Namespace: "agents"},
	}
	foreignWorkerPod := executionPod(otherWorker, "other-worker", "task-1", "execution-1", 0)

	backend = newKubernetesLogBackend(t, wanted, otherExecution, otherTask, foreignWorkerPod)

	pods, err := backend.listExecutionPods(context.Background(), "task-1", "execution-1")
	if err != nil {
		t.Fatalf("listExecutionPods: %v", err)
	}
	if len(pods) != 1 {
		names := make([]string, 0, len(pods))
		for _, pod := range pods {
			names = append(names, pod.Name)
		}
		t.Fatalf("selected pods = %v, want only the exactly-matching pod", names)
	}
	if pods[0].Name != "wanted" {
		t.Fatalf("selected pod = %q, want %q", pods[0].Name, "wanted")
	}
}

func TestKubernetesSnapshotVisitsPodsAndContainersDeterministically(t *testing.T) {
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{WorkerID: "worker-123", Namespace: "agents"},
	}
	// Seeded out of order so the snapshot's own sort is what produces the
	// deterministic result.
	second := executionPod(backend, "pod-b", "task-1", "execution-1", 0)
	first := executionPod(backend, "pod-a", "task-1", "execution-1", 0)
	backend = newKubernetesLogBackend(t, second, first)
	backend.registerJob("task-1", "execution-1", "oz-task-task-1-exec-ution-1")

	sink := &captureSink{}
	// The fake clientset returns a canned log body for every container, so
	// each visited container contributes at least one chunk.
	if err := backend.SnapshotTaskLogs(context.Background(), &SnapshotParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Sink:        sink,
	}); err != nil {
		t.Fatalf("SnapshotTaskLogs: %v", err)
	}

	var visited []string
	for _, chunk := range sink.chunks {
		entry := chunk.Source.Pod + "/" + chunk.Source.Container + "/" + chunk.Source.ContainerType
		if len(visited) == 0 || visited[len(visited)-1] != entry {
			visited = append(visited, entry)
		}
	}
	want := []string{
		"pod-a/sidecar-init/init",
		"pod-a/task/regular",
		"pod-b/sidecar-init/init",
		"pod-b/task/regular",
	}
	if len(visited) != len(want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("visited = %v, want %v", visited, want)
		}
	}

	for _, chunk := range sink.chunks {
		if chunk.Stream != debuglog.StreamCombined {
			t.Errorf("stream = %q, want %q: Kubernetes merges a container's streams", chunk.Stream, debuglog.StreamCombined)
		}
		if chunk.Source.Namespace != "agents" {
			t.Errorf("namespace = %q, want agents", chunk.Source.Namespace)
		}
	}
}

func TestKubernetesSnapshotAttemptsPreviousLogsForARestartedContainer(t *testing.T) {
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{WorkerID: "worker-123", Namespace: "agents"},
	}
	pod := executionPod(backend, "pod-a", "task-1", "execution-1", 2)
	backend = newKubernetesLogBackend(t, pod)
	backend.registerJob("task-1", "execution-1", "oz-task-task-1-exec-ution-1")

	sink := &captureSink{}
	if err := backend.SnapshotTaskLogs(context.Background(), &SnapshotParams{
		TaskID:      "task-1",
		ExecutionID: "execution-1",
		Sink:        sink,
	}); err != nil {
		t.Fatalf("SnapshotTaskLogs: %v", err)
	}

	var sawPrevious, sawCurrent bool
	for _, chunk := range sink.chunks {
		if chunk.Source.Container != "task" {
			continue
		}
		if chunk.Source.RestartAttempt == nil || *chunk.Source.RestartAttempt != 2 {
			t.Errorf("restart_attempt = %v, want 2", chunk.Source.RestartAttempt)
		}
		if chunk.Source.Previous {
			sawPrevious = true
		} else {
			sawCurrent = true
		}
	}
	if !sawPrevious {
		t.Error("a restarted container must also contribute its previous logs")
	}
	if !sawCurrent {
		t.Error("a restarted container must still contribute its current logs")
	}
}

func TestKubernetesCleanupIsANoOpForAnUnknownExecution(t *testing.T) {
	backend := newKubernetesLogBackend(t)

	if err := backend.CleanupTaskResources(context.Background(), &CancelParams{
		TaskID:      "unknown-task",
		ExecutionID: "unknown-execution",
	}); err != nil {
		t.Fatalf("CleanupTaskResources: %v", err)
	}
}
