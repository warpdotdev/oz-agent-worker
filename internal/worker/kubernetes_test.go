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
