package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func durationPtr(value time.Duration) *time.Duration {
	return &value
}

func TestSanitizeKubernetesJobNameUsesHashSuffix(t *testing.T) {
	first := sanitizeKubernetesJobName("Task A")
	second := sanitizeKubernetesJobName("Task-A")

	if first == second {
		t.Fatalf("expected distinct job names for distinct task IDs, got %q", first)
	}
	if !strings.HasPrefix(first, "oz-task-task-a-") {
		t.Fatalf("unexpected job name prefix: %q", first)
	}
	if len(first) > 63 {
		t.Fatalf("job name too long: %d", len(first))
	}
	if strings.ContainsAny(first, "_.") {
		t.Fatalf("job name contains invalid DNS label characters: %q", first)
	}
}

func TestKubernetesBackendBaseLabelsIncludeStableHashes(t *testing.T) {
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID: "Worker A",
		},
	}

	labels := backend.baseLabels("Task A")
	if labels[kubernetesWorkerIDLabel] != "worker-a" {
		t.Fatalf("worker label = %q, want %q", labels[kubernetesWorkerIDLabel], "worker-a")
	}
	if labels[kubernetesTaskIDLabel] != "task-a" {
		t.Fatalf("task label = %q, want %q", labels[kubernetesTaskIDLabel], "task-a")
	}
	if labels[kubernetesWorkerHashLabel] != kubernetesLabelHash("Worker A") {
		t.Fatalf("worker hash = %q, want %q", labels[kubernetesWorkerHashLabel], kubernetesLabelHash("Worker A"))
	}
	if labels[kubernetesTaskHashLabel] != kubernetesLabelHash("Task A") {
		t.Fatalf("task hash = %q, want %q", labels[kubernetesTaskHashLabel], kubernetesLabelHash("Task A"))
	}
}

func TestInspectPodFailureRespectsUnschedulableTimeout(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	ctx := context.Background()
	unschedulableCondition := corev1.PodCondition{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  corev1.PodReasonUnschedulable,
		Message: "no nodes available",
	}

	t.Run("fails after timeout", func(t *testing.T) {
		backend := &KubernetesBackend{
			config: KubernetesBackendConfig{
				Namespace:            "agents",
				UnschedulableTimeout: durationPtr(30 * time.Second),
			},
			clientset: fakeClient,
		}

		err := backend.inspectPodFailure(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "task-pod",
				Namespace:         "agents",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-31 * time.Second)),
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{unschedulableCondition},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "unschedulable") {
			t.Fatalf("expected unschedulable error, got %v", err)
		}
	})

	t.Run("does not fail before timeout", func(t *testing.T) {
		backend := &KubernetesBackend{
			config: KubernetesBackendConfig{
				Namespace:            "agents",
				UnschedulableTimeout: durationPtr(30 * time.Second),
			},
			clientset: fakeClient,
		}

		err := backend.inspectPodFailure(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "task-pod",
				Namespace:         "agents",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Second)),
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{unschedulableCondition},
			},
		})
		if err != nil {
			t.Fatalf("expected no error before timeout, got %v", err)
		}
	})

	t.Run("does not fail when disabled", func(t *testing.T) {
		backend := &KubernetesBackend{
			config: KubernetesBackendConfig{
				Namespace:            "agents",
				UnschedulableTimeout: durationPtr(0),
			},
			clientset: fakeClient,
		}

		err := backend.inspectPodFailure(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "task-pod",
				Namespace:         "agents",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{unschedulableCondition},
			},
		})
		if err != nil {
			t.Fatalf("expected no error when timeout is disabled, got %v", err)
		}
	})
}

func TestHandleJobStateDetectsCompletion(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			Namespace: "agents",
			WorkerID:  "worker-123",
		},
		clientset: fakeClient,
	}
	ctx := context.Background()

	t.Run("returns nil for in-progress job", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "agents"},
			Status:     batchv1.JobStatus{},
		}
		if result := backend.handleJobState(ctx, job, "task-1"); result != nil {
			t.Fatalf("expected nil for in-progress job, got %v", result.err)
		}
	})

	t.Run("returns nil error for completed job", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "agents"},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		}
		result := backend.handleJobState(ctx, job, "task-1")
		if result == nil {
			t.Fatal("expected non-nil result for completed job")
		}
		if result.err != nil {
			t.Fatalf("expected nil error for completed job, got %v", result.err)
		}
	})

	t.Run("returns error for failed job", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "agents"},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			},
		}
		result := backend.handleJobState(ctx, job, "task-1")
		if result == nil {
			t.Fatal("expected non-nil result for failed job")
		}
		if result.err == nil || !strings.Contains(result.err.Error(), "failed") {
			t.Fatalf("expected failure error, got %v", result.err)
		}
	})
}

func TestWatchJobReturnsWatchInterface(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			Namespace: "agents",
		},
		clientset: fakeClient,
	}

	watcher, err := backend.watchJob(context.Background(), "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer watcher.Stop()

	// The fake client's watch should be open (channel not closed).
	select {
	case _, ok := <-watcher.ResultChan():
		if ok {
			// Got an event — fine for the fake client.
		}
	case <-time.After(50 * time.Millisecond):
		// No events yet — expected for an empty cluster.
	}
}

func TestWatchReconnectsOnChannelClose(t *testing.T) {
	// Verify that closing a watch channel and reopening works with the fake client.
	fakeClient := fake.NewSimpleClientset()
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			Namespace: "agents",
		},
		clientset: fakeClient,
	}

	watcher1, err := backend.watchJob(context.Background(), "my-job")
	if err != nil {
		t.Fatalf("unexpected error on first watch: %v", err)
	}
	watcher1.Stop()

	// After stopping, ResultChan should be closed.
	_, ok := <-watcher1.ResultChan()
	if ok {
		t.Fatal("expected channel to be closed after Stop")
	}

	// Reopening should succeed.
	watcher2, err := backend.watchJob(context.Background(), "my-job")
	if err != nil {
		t.Fatalf("unexpected error on re-watch: %v", err)
	}
	defer watcher2.Stop()
}

func TestWatchTaskPodsReceivesEvents(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			Namespace: "agents",
		},
		clientset: fakeClient,
	}

	taskID := "task-abc"
	watcher, err := backend.watchTaskPods(context.Background(), taskID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer watcher.Stop()

	// Create a pod that matches the label selector.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-pod",
			Namespace: "agents",
			Labels: map[string]string{
				kubernetesTaskHashLabel: kubernetesLabelHash(taskID),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "task", Image: "ubuntu:22.04"}},
		},
	}
	if _, err := fakeClient.CoreV1().Pods("agents").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	// The watch should receive the ADDED event.
	select {
	case event := <-watcher.ResultChan():
		if event.Type != watch.Added {
			t.Fatalf("expected Added event, got %v", event.Type)
		}
		gotPod, ok := event.Object.(*corev1.Pod)
		if !ok {
			t.Fatalf("expected Pod object, got %T", event.Object)
		}
		if gotPod.Name != "task-pod" {
			t.Fatalf("pod name = %q, want %q", gotPod.Name, "task-pod")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pod watch event")
	}
}

func TestMergeKubernetesEnvVars(t *testing.T) {
	t.Run("override wins on key conflict", func(t *testing.T) {
		base := []corev1.EnvVar{
			{Name: "FOO", Value: "base-foo"},
			{Name: "BAR", Value: "base-bar"},
		}
		override := []corev1.EnvVar{
			{Name: "FOO", Value: "override-foo"},
			{Name: "BAZ", Value: "override-baz"},
		}
		result := mergeKubernetesEnvVars(base, override)
		resultMap := make(map[string]string, len(result))
		for _, e := range result {
			resultMap[e.Name] = e.Value
		}
		if resultMap["FOO"] != "override-foo" {
			t.Errorf("FOO = %q, want %q", resultMap["FOO"], "override-foo")
		}
		if resultMap["BAR"] != "base-bar" {
			t.Errorf("BAR = %q, want %q", resultMap["BAR"], "base-bar")
		}
		if resultMap["BAZ"] != "override-baz" {
			t.Errorf("BAZ = %q, want %q", resultMap["BAZ"], "override-baz")
		}
		if len(result) != 3 {
			t.Errorf("result length = %d, want 3", len(result))
		}
	})

	t.Run("empty slices", func(t *testing.T) {
		result := mergeKubernetesEnvVars(nil, nil)
		if len(result) != 0 {
			t.Errorf("expected empty result, got %v", result)
		}
	})

	t.Run("only base", func(t *testing.T) {
		base := []corev1.EnvVar{{Name: "A", Value: "1"}}
		result := mergeKubernetesEnvVars(base, nil)
		if len(result) != 1 || result[0].Value != "1" {
			t.Errorf("unexpected result: %v", result)
		}
	})

	t.Run("only override", func(t *testing.T) {
		override := []corev1.EnvVar{{Name: "B", Value: "2"}}
		result := mergeKubernetesEnvVars(nil, override)
		if len(result) != 1 || result[0].Value != "2" {
			t.Errorf("unexpected result: %v", result)
		}
	})
}

func TestRunStartupPreflightUsesDryRunAndRootInitContainer(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	const preflightImage = "registry.internal/platform/preflight:1.0"
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			t.Fatalf("expected create action, got %T", action)
		}

		job, ok := createAction.GetObject().(*batchv1.Job)
		if !ok {
			t.Fatalf("expected Job object, got %T", createAction.GetObject())
		}
		if got := createAction.GetCreateOptions().DryRun; len(got) != 1 || got[0] != metav1.DryRunAll {
			t.Fatalf("unexpected dry-run options: %v", got)
		}
		if len(job.Spec.Template.Spec.InitContainers) != 1 {
			t.Fatalf("expected one init container, got %d", len(job.Spec.Template.Spec.InitContainers))
		}
		if job.Spec.Template.Spec.InitContainers[0].Image != preflightImage {
			t.Fatalf("init container image = %q, want %q", job.Spec.Template.Spec.InitContainers[0].Image, preflightImage)
		}
		if job.Spec.Template.Spec.InitContainers[0].SecurityContext == nil || job.Spec.Template.Spec.InitContainers[0].SecurityContext.RunAsUser == nil || *job.Spec.Template.Spec.InitContainers[0].SecurityContext.RunAsUser != 0 {
			t.Fatalf("expected root init container security context, got %+v", job.Spec.Template.Spec.InitContainers[0].SecurityContext)
		}
		if len(job.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("expected one main container, got %d", len(job.Spec.Template.Spec.Containers))
		}
		if job.Spec.Template.Spec.Containers[0].Image != preflightImage {
			t.Fatalf("main container image = %q, want %q", job.Spec.Template.Spec.Containers[0].Image, preflightImage)
		}

		return true, job, nil
	})

	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID:        "worker-123",
			Namespace:       "agents",
			PreflightImage:  preflightImage,
			ServiceAccount:  "oz-agent-worker",
			ImagePullSecret: "registry-creds",
		},
		clientset: fakeClient,
	}

	if err := backend.runStartupPreflight(context.Background()); err != nil {
		t.Fatalf("unexpected preflight error: %v", err)
	}
}
