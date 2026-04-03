package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestKubernetesSidecarMaterializationScriptMatchesExpectedShell(t *testing.T) {
	expected := strings.Join([]string{
		"tar \\",
		"  --exclude=./target \\",
		"  --exclude=./proc \\",
		"  --exclude=./sys \\",
		"  --exclude=./dev \\",
		"  --exclude=./.dockerenv \\",
		"  --exclude=./var/run/secrets \\",
		"  --exclude=./run/secrets \\",
		"  -C / -cf - . | tar --no-same-owner --no-same-permissions -C /target -xf -",
	}, "\n")
	if script := kubernetesSidecarMaterializationScript(); script != expected {
		t.Fatalf("unexpected materialization script:\n%s", script)
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
		if !ok {
			t.Fatal("watch channel unexpectedly closed")
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

func TestBuildTaskPodSpecUsesPodTemplateFieldsAndCLIEnv(t *testing.T) {
	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			PodTemplate: &corev1.PodSpec{
				ServiceAccountName: "task-runner",
				ImagePullSecrets: []corev1.LocalObjectReference{
					{Name: "registry-creds"},
				},
				NodeSelector: map[string]string{
					"pool": "agents",
				},
				Containers: []corev1.Container{
					{
						Name:            "task",
						ImagePullPolicy: corev1.PullAlways,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
						Env: []corev1.EnvVar{
							{Name: "FROM_TEMPLATE", Value: "1"},
						},
					},
				},
			},
			TaskEnv: map[string]string{
				"FROM_CLI": "1",
			},
		},
	}

	mainContainer := corev1.Container{
		Name:            "task",
		Image:           "ubuntu:22.04",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", "run-task"},
		Args:            []string{"arg1"},
		Env: []corev1.EnvVar{
			{Name: "FROM_CLI", Value: "1"},
		},
		WorkingDir: "/workspace",
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
		},
	}

	podSpec := backend.buildTaskPodSpec(nil, []corev1.Volume{{Name: "workspace"}}, mainContainer)

	if podSpec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("RestartPolicy = %q, want %q", podSpec.RestartPolicy, corev1.RestartPolicyNever)
	}
	if podSpec.ServiceAccountName != "task-runner" {
		t.Fatalf("ServiceAccountName = %q, want %q", podSpec.ServiceAccountName, "task-runner")
	}
	if len(podSpec.ImagePullSecrets) != 1 || podSpec.ImagePullSecrets[0].Name != "registry-creds" {
		t.Fatalf("ImagePullSecrets = %+v, want registry-creds", podSpec.ImagePullSecrets)
	}
	if podSpec.NodeSelector["pool"] != "agents" {
		t.Fatalf("NodeSelector[pool] = %q, want %q", podSpec.NodeSelector["pool"], "agents")
	}

	var taskContainer *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == "task" {
			taskContainer = &podSpec.Containers[i]
			break
		}
	}
	if taskContainer == nil {
		t.Fatal("expected task container to be present")
	}
	if taskContainer.Image != "ubuntu:22.04" {
		t.Fatalf("task image = %q, want %q", taskContainer.Image, "ubuntu:22.04")
	}
	if taskContainer.ImagePullPolicy != corev1.PullAlways {
		t.Fatalf("task imagePullPolicy = %q, want %q", taskContainer.ImagePullPolicy, corev1.PullAlways)
	}
	cpuRequest := taskContainer.Resources.Requests[corev1.ResourceCPU]
	if got := cpuRequest.String(); got != "500m" {
		t.Fatalf("task cpu request = %q, want %q", got, "500m")
	}

	envMap := make(map[string]string, len(taskContainer.Env))
	for _, env := range taskContainer.Env {
		envMap[env.Name] = env.Value
	}
	if envMap["FROM_TEMPLATE"] != "1" {
		t.Fatalf("FROM_TEMPLATE = %q, want %q", envMap["FROM_TEMPLATE"], "1")
	}
	if envMap["FROM_CLI"] != "1" {
		t.Fatalf("FROM_CLI = %q, want %q", envMap["FROM_CLI"], "1")
	}
}
func TestValidateTaskSidecarsAllowsReadWriteMountsByDefault(t *testing.T) {
	err := validateTaskSidecars([]types.SidecarMount{
		{
			Image:     "registry.internal/agent:1.0",
			MountPath: "/agent",
			ReadWrite: true,
		},
	}, false)
	if err != nil {
		t.Fatalf("expected read-write sidecar to be allowed by default, got %v", err)
	}
}

func TestValidateTaskSidecarsRejectsReadWriteMountsWhenImageVolumesEnabled(t *testing.T) {
	err := validateTaskSidecars([]types.SidecarMount{
		{
			Image:     "registry.internal/agent:1.0",
			MountPath: "/agent",
			ReadWrite: true,
		},
	}, true)
	if err == nil {
		t.Fatal("expected read-write sidecar validation failure")
	}
	if !strings.Contains(err.Error(), "read-write mount") {
		t.Fatalf("expected read-write validation detail, got %v", err)
	}
}

func TestExecuteTaskUsesImageVolumesForSidecars(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	jobWatch := watch.NewFake()
	podWatch := watch.NewFake()
	defer jobWatch.Stop()
	defer podWatch.Stop()

	var createdJob *batchv1.Job
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			t.Fatalf("expected create action, got %T", action)
		}
		job, ok := createAction.GetObject().(*batchv1.Job)
		if !ok {
			t.Fatalf("expected Job object, got %T", createAction.GetObject())
		}
		createdJob = job.DeepCopy()
		return false, nil, nil
	})
	fakeClient.PrependWatchReactor("jobs", func(action k8stesting.Action) (bool, watch.Interface, error) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			if createdJob == nil {
				return
			}
			completedJob := createdJob.DeepCopy()
			completedJob.Status.Conditions = []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			}
			jobWatch.Modify(completedJob)
		}()
		return true, jobWatch, nil
	})
	fakeClient.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		return true, podWatch, nil
	})

	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID:        "worker-123",
			Namespace:       "agents",
			ImagePullPolicy: string(corev1.PullAlways),
			SetupCommand:    "true",
			UseImageVolumes: true,
		},
		clientset: fakeClient,
	}

	err := backend.ExecuteTask(context.Background(), &TaskParams{
		TaskID:      "task-1",
		DockerImage: "ubuntu:22.04",
		BaseArgs:    []string{"run"},
		Sidecars: []types.SidecarMount{
			{
				Image:     "registry.internal/agent:1.0",
				MountPath: "/agent",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected ExecuteTask error: %v", err)
	}
	if createdJob == nil {
		t.Fatal("expected task job to be created")
	}

	if len(createdJob.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected only setup init container, got %d", len(createdJob.Spec.Template.Spec.InitContainers))
	}
	if createdJob.Spec.Template.Spec.InitContainers[0].Name != "setup" {
		t.Fatalf("expected setup init container, got %q", createdJob.Spec.Template.Spec.InitContainers[0].Name)
	}
	if len(createdJob.Spec.Template.Spec.Volumes) != 2 {
		t.Fatalf("expected workspace and sidecar image volumes, got %d", len(createdJob.Spec.Template.Spec.Volumes))
	}

	var sidecarVolume *corev1.Volume
	for i := range createdJob.Spec.Template.Spec.Volumes {
		if createdJob.Spec.Template.Spec.Volumes[i].Name == "sidecar-0-image" {
			sidecarVolume = &createdJob.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	if sidecarVolume == nil {
		t.Fatal("expected sidecar image volume to be present")
	}
	if sidecarVolume.Image == nil {
		t.Fatalf("expected image volume source, got %+v", sidecarVolume.VolumeSource)
	}
	if sidecarVolume.Image.Reference != "registry.internal/agent:1.0" {
		t.Fatalf("image volume reference = %q, want %q", sidecarVolume.Image.Reference, "registry.internal/agent:1.0")
	}
	if sidecarVolume.Image.PullPolicy != corev1.PullAlways {
		t.Fatalf("image volume pullPolicy = %q, want %q", sidecarVolume.Image.PullPolicy, corev1.PullAlways)
	}

	taskContainer := createdJob.Spec.Template.Spec.Containers[0]
	var taskSidecarMount *corev1.VolumeMount
	for i := range taskContainer.VolumeMounts {
		if taskContainer.VolumeMounts[i].Name == "sidecar-0-image" {
			taskSidecarMount = &taskContainer.VolumeMounts[i]
			break
		}
	}
	if taskSidecarMount == nil {
		t.Fatal("expected task container sidecar mount")
	}
	if taskSidecarMount.MountPath != "/agent" {
		t.Fatalf("task sidecar mount path = %q, want %q", taskSidecarMount.MountPath, "/agent")
	}
	if !taskSidecarMount.ReadOnly {
		t.Fatal("expected task sidecar mount to be read-only")
	}

	setupContainer := createdJob.Spec.Template.Spec.InitContainers[0]
	var setupSidecarMount *corev1.VolumeMount
	for i := range setupContainer.VolumeMounts {
		if setupContainer.VolumeMounts[i].Name == "sidecar-0-image" {
			setupSidecarMount = &setupContainer.VolumeMounts[i]
			break
		}
	}
	if setupSidecarMount == nil {
		t.Fatal("expected setup init container sidecar mount")
	}
	if !setupSidecarMount.ReadOnly {
		t.Fatal("expected setup sidecar mount to be read-only")
	}
}

func TestExecuteTaskUsesCopyInitContainersByDefault(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	jobWatch := watch.NewFake()
	podWatch := watch.NewFake()
	defer jobWatch.Stop()
	defer podWatch.Stop()

	var createdJob *batchv1.Job
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			t.Fatalf("expected create action, got %T", action)
		}
		job, ok := createAction.GetObject().(*batchv1.Job)
		if !ok {
			t.Fatalf("expected Job object, got %T", createAction.GetObject())
		}
		createdJob = job.DeepCopy()
		return false, nil, nil
	})
	fakeClient.PrependWatchReactor("jobs", func(action k8stesting.Action) (bool, watch.Interface, error) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			if createdJob == nil {
				return
			}
			completedJob := createdJob.DeepCopy()
			completedJob.Status.Conditions = []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			}
			jobWatch.Modify(completedJob)
		}()
		return true, jobWatch, nil
	})
	fakeClient.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		return true, podWatch, nil
	})

	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID:        "worker-123",
			Namespace:       "agents",
			ImagePullPolicy: string(corev1.PullAlways),
			SetupCommand:    "true",
		},
		clientset: fakeClient,
	}

	err := backend.ExecuteTask(context.Background(), &TaskParams{
		TaskID:      "task-1",
		DockerImage: "ubuntu:22.04",
		BaseArgs:    []string{"run"},
		Sidecars: []types.SidecarMount{
			{
				Image:     "registry.internal/agent:1.0",
				MountPath: "/agent",
				ReadWrite: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected ExecuteTask error: %v", err)
	}
	if createdJob == nil {
		t.Fatal("expected task job to be created")
	}

	if len(createdJob.Spec.Template.Spec.InitContainers) != 2 {
		t.Fatalf("expected copy init container plus setup, got %d", len(createdJob.Spec.Template.Spec.InitContainers))
	}
	copyInit := createdJob.Spec.Template.Spec.InitContainers[0]
	if copyInit.Name != "copy-sidecar-0" {
		t.Fatalf("expected copy-sidecar init container, got %q", copyInit.Name)
	}
	if copyInit.Image != "registry.internal/agent:1.0" {
		t.Fatalf("copy init image = %q, want %q", copyInit.Image, "registry.internal/agent:1.0")
	}
	if copyInit.SecurityContext == nil || copyInit.SecurityContext.RunAsUser == nil || *copyInit.SecurityContext.RunAsUser != 0 {
		t.Fatalf("expected root copy init security context, got %+v", copyInit.SecurityContext)
	}
	if len(copyInit.VolumeMounts) != 1 || copyInit.VolumeMounts[0].MountPath != sidecarCopyTargetMountPath {
		t.Fatalf("expected copy init to mount %q, got %+v", sidecarCopyTargetMountPath, copyInit.VolumeMounts)
	}

	if len(createdJob.Spec.Template.Spec.Volumes) != 2 {
		t.Fatalf("expected workspace and copied sidecar data volumes, got %d", len(createdJob.Spec.Template.Spec.Volumes))
	}
	var sidecarVolume *corev1.Volume
	for i := range createdJob.Spec.Template.Spec.Volumes {
		if createdJob.Spec.Template.Spec.Volumes[i].Name == "sidecar-0-data" {
			sidecarVolume = &createdJob.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	if sidecarVolume == nil {
		t.Fatal("expected copied sidecar data volume to be present")
	}
	if sidecarVolume.EmptyDir == nil {
		t.Fatalf("expected emptyDir sidecar volume, got %+v", sidecarVolume.VolumeSource)
	}
	if sidecarVolume.Image != nil {
		t.Fatalf("expected no image volume on default path, got %+v", sidecarVolume.Image)
	}

	taskContainer := createdJob.Spec.Template.Spec.Containers[0]
	var taskSidecarMount *corev1.VolumeMount
	for i := range taskContainer.VolumeMounts {
		if taskContainer.VolumeMounts[i].Name == "sidecar-0-data" {
			taskSidecarMount = &taskContainer.VolumeMounts[i]
			break
		}
	}
	if taskSidecarMount == nil {
		t.Fatal("expected task container copied sidecar mount")
	}
	if taskSidecarMount.MountPath != "/agent" {
		t.Fatalf("task sidecar mount path = %q, want %q", taskSidecarMount.MountPath, "/agent")
	}
	if taskSidecarMount.ReadOnly {
		t.Fatal("expected task sidecar mount to remain writable on default path")
	}
}

func TestRunStartupPreflightCreatesLegacyRootInitJobAndWaitsForPodCreationByDefault(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	const preflightImage = "registry.internal/platform/preflight:1.0"
	var createdJob *batchv1.Job
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			t.Fatalf("expected create action, got %T", action)
		}
		job, ok := createAction.GetObject().(*batchv1.Job)
		if !ok {
			t.Fatalf("expected Job object, got %T", createAction.GetObject())
		}
		if len(job.Spec.Template.Spec.InitContainers) != 1 {
			t.Fatalf("expected one legacy init container, got %d", len(job.Spec.Template.Spec.InitContainers))
		}
		if job.Spec.Template.Spec.InitContainers[0].Name != "root-init-preflight" {
			t.Fatalf("init container name = %q, want %q", job.Spec.Template.Spec.InitContainers[0].Name, "root-init-preflight")
		}
		if job.Spec.Template.Spec.InitContainers[0].Image != preflightImage {
			t.Fatalf("init container image = %q, want %q", job.Spec.Template.Spec.InitContainers[0].Image, preflightImage)
		}
		if job.Spec.Template.Spec.InitContainers[0].SecurityContext == nil || job.Spec.Template.Spec.InitContainers[0].SecurityContext.RunAsUser == nil || *job.Spec.Template.Spec.InitContainers[0].SecurityContext.RunAsUser != 0 {
			t.Fatalf("expected root init security context, got %+v", job.Spec.Template.Spec.InitContainers[0].SecurityContext)
		}
		if len(job.Spec.Template.Spec.Volumes) != 0 {
			t.Fatalf("expected no image volumes on legacy path, got %d", len(job.Spec.Template.Spec.Volumes))
		}
		createdJob = job.DeepCopy()
		createdJob.UID = "preflight-job-uid"
		return true, createdJob, nil
	})
	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if createdJob == nil {
			t.Fatal("expected preflight job to be created before listing pods")
		}
		return true, &corev1.PodList{
			Items: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "preflight-pod",
						Namespace: "agents",
						Labels: map[string]string{
							"job-name": createdJob.Name,
						},
					},
				},
			},
		}, nil
	})
	fakeClient.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.EventList{}, nil
	})

	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID:       "worker-123",
			Namespace:      "agents",
			PreflightImage: preflightImage,
			PodTemplate: &corev1.PodSpec{
				ServiceAccountName: "oz-agent-worker",
				ImagePullSecrets: []corev1.LocalObjectReference{
					{Name: "registry-creds"},
				},
			},
		},
		clientset: fakeClient,
	}

	if err := backend.runStartupPreflight(context.Background()); err != nil {
		t.Fatalf("unexpected preflight error: %v", err)
	}
}

func TestRunStartupPreflightCreatesImageVolumeJobAndWaitsForSuccess(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	const preflightImage = "registry.internal/platform/preflight:1.0"
	var createdJob *batchv1.Job
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			t.Fatalf("expected create action, got %T", action)
		}

		job, ok := createAction.GetObject().(*batchv1.Job)
		if !ok {
			t.Fatalf("expected Job object, got %T", createAction.GetObject())
		}
		if got := createAction.GetCreateOptions().DryRun; len(got) != 0 {
			t.Fatalf("expected real create without dry-run options, got %v", got)
		}
		if len(job.Spec.Template.Spec.InitContainers) != 0 {
			t.Fatalf("expected no init containers, got %d", len(job.Spec.Template.Spec.InitContainers))
		}
		if len(job.Spec.Template.Spec.Volumes) != 1 {
			t.Fatalf("expected one preflight image volume, got %d", len(job.Spec.Template.Spec.Volumes))
		}
		if job.Spec.Template.Spec.Volumes[0].Name != startupPreflightImageVolumeName {
			t.Fatalf("volume name = %q, want %q", job.Spec.Template.Spec.Volumes[0].Name, startupPreflightImageVolumeName)
		}
		if job.Spec.Template.Spec.Volumes[0].Image == nil {
			t.Fatalf("expected preflight image volume source, got %+v", job.Spec.Template.Spec.Volumes[0].VolumeSource)
		}
		if job.Spec.Template.Spec.Volumes[0].Image.Reference != preflightImage {
			t.Fatalf("image volume reference = %q, want %q", job.Spec.Template.Spec.Volumes[0].Image.Reference, preflightImage)
		}
		if len(job.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("expected one main container, got %d", len(job.Spec.Template.Spec.Containers))
		}
		if job.Spec.Template.Spec.Containers[0].Image != preflightImage {
			t.Fatalf("main container image = %q, want %q", job.Spec.Template.Spec.Containers[0].Image, preflightImage)
		}
		if len(job.Spec.Template.Spec.Containers[0].VolumeMounts) != 1 {
			t.Fatalf("expected one main container volume mount, got %d", len(job.Spec.Template.Spec.Containers[0].VolumeMounts))
		}
		if job.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name != startupPreflightImageVolumeName {
			t.Fatalf("volumeMount name = %q, want %q", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].Name, startupPreflightImageVolumeName)
		}
		if job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != startupPreflightImageMountPath {
			t.Fatalf("volumeMount path = %q, want %q", job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath, startupPreflightImageMountPath)
		}
		if !job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly {
			t.Fatal("expected preflight volume mount to be read-only")
		}
		if job.Spec.Template.Spec.ServiceAccountName != "oz-agent-worker" {
			t.Fatalf("serviceAccountName = %q, want %q", job.Spec.Template.Spec.ServiceAccountName, "oz-agent-worker")
		}
		if len(job.Spec.Template.Spec.ImagePullSecrets) != 1 || job.Spec.Template.Spec.ImagePullSecrets[0].Name != "registry-creds" {
			t.Fatalf("imagePullSecrets = %+v, want registry-creds", job.Spec.Template.Spec.ImagePullSecrets)
		}
		createdJob = job.DeepCopy()
		createdJob.UID = "preflight-job-uid"

		return true, createdJob, nil
	})
	fakeClient.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if createdJob == nil {
			t.Fatal("expected preflight job to be created before getting job state")
		}
		completedJob := createdJob.DeepCopy()
		completedJob.Status.Conditions = []batchv1.JobCondition{
			{
				Type:   batchv1.JobComplete,
				Status: corev1.ConditionTrue,
			},
		}
		return true, completedJob, nil
	})
	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if createdJob == nil {
			t.Fatal("expected preflight job to be created before listing pods")
		}
		return true, &corev1.PodList{
			Items: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "preflight-pod",
						Namespace: "agents",
						Labels: map[string]string{
							"job-name": createdJob.Name,
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				},
			},
		}, nil
	})
	fakeClient.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.EventList{}, nil
	})

	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID:        "worker-123",
			Namespace:       "agents",
			PreflightImage:  preflightImage,
			UseImageVolumes: true,
			PodTemplate: &corev1.PodSpec{
				ServiceAccountName: "oz-agent-worker",
				ImagePullSecrets: []corev1.LocalObjectReference{
					{Name: "registry-creds"},
				},
			},
		},
		clientset: fakeClient,
	}

	if err := backend.runStartupPreflight(context.Background()); err != nil {
		t.Fatalf("unexpected preflight error: %v", err)
	}
}

func TestRunStartupPreflightFailsOnFailedMountEvent(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	var createdJob *batchv1.Job
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			t.Fatalf("expected create action, got %T", action)
		}
		job, ok := createAction.GetObject().(*batchv1.Job)
		if !ok {
			t.Fatalf("expected Job object, got %T", createAction.GetObject())
		}
		createdJob = job.DeepCopy()
		createdJob.UID = "preflight-job-uid"
		return true, createdJob, nil
	})
	fakeClient.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if createdJob == nil {
			t.Fatal("expected preflight job to be created before getting job state")
		}
		return true, createdJob.DeepCopy(), nil
	})
	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if createdJob == nil {
			t.Fatal("expected preflight job to be created before listing pods")
		}
		return true, &corev1.PodList{
			Items: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "preflight-pod",
						Namespace: "agents",
						UID:       "preflight-pod-uid",
						Labels: map[string]string{
							"job-name": createdJob.Name,
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				},
			},
		}, nil
	})
	fakeClient.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.EventList{
			Items: []corev1.Event{
				{
					Reason:  "FailedMount",
					Message: "MountVolume.SetUp failed for volume \"preflight-image\": image volumes are not supported on this node",
				},
			},
		}, nil
	})

	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID:        "worker-123",
			Namespace:       "agents",
			PreflightImage:  "registry.internal/platform/preflight:1.0",
			UseImageVolumes: true,
		},
		clientset: fakeClient,
	}

	err := backend.runStartupPreflight(context.Background())
	if err == nil {
		t.Fatal("expected preflight failure")
	}
	if !strings.Contains(err.Error(), "failed to mount a volume") {
		t.Fatalf("expected mount failure detail, got %v", err)
	}
	if !strings.Contains(err.Error(), "startup preflight failed") {
		t.Fatalf("expected wrapped startup preflight error, got %v", err)
	}
}

func TestRunStartupPreflightFailsOnFailedCreateEvent(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()
	fakeClient.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createAction, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			t.Fatalf("expected create action, got %T", action)
		}
		job, ok := createAction.GetObject().(*batchv1.Job)
		if !ok {
			t.Fatalf("expected Job object, got %T", createAction.GetObject())
		}
		job = job.DeepCopy()
		job.UID = "preflight-job-uid"
		return true, job, nil
	})
	fakeClient.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "preflight-job",
				Namespace: "agents",
				UID:       "preflight-job-uid",
			},
		}
		return true, job, nil
	})
	fakeClient.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{}, nil
	})
	fakeClient.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.EventList{
			Items: []corev1.Event{
				{
					Type:    corev1.EventTypeWarning,
					Reason:  "FailedCreate",
					Message: "pods \"oz-preflight-abc\" is forbidden: violates PodSecurity \"restricted:latest\"",
				},
			},
		}, nil
	})

	backend := &KubernetesBackend{
		config: KubernetesBackendConfig{
			WorkerID:       "worker-123",
			Namespace:      "agents",
			PreflightImage: "registry.internal/platform/preflight:1.0",
		},
		clientset: fakeClient,
	}

	err := backend.runStartupPreflight(context.Background())
	if err == nil {
		t.Fatal("expected preflight failure")
	}
	if !strings.Contains(err.Error(), "startup preflight failed") {
		t.Fatalf("expected wrapped startup preflight error, got %v", err)
	}
	if !strings.Contains(err.Error(), "could not create a Pod") {
		t.Fatalf("expected pod creation failure detail, got %v", err)
	}
}
