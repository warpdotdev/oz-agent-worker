package worker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultKubernetesNamespace       = "default"
	defaultWorkspaceMountPath        = "/workspace"
	defaultSetupEnvironmentFile      = "/workspace/.oz-env"
	watchSafetyInterval              = 30 * time.Second
	defaultUnschedulableFailureDelay = 30 * time.Second
	startupPreflightPollInterval     = 500 * time.Millisecond
	startupPreflightTimeout          = 15 * time.Second
	kubernetesBackendTypeName        = "kubernetes"
	sidecarCopyTargetMountPath       = "/target"
	kubernetesStartupPreflightImage  = "busybox:1.36"
	defaultJobTTLSecondsAfterFinish  = int32(24 * 60 * 60)
	startupPreflightImageVolumeName  = "preflight-image"
	startupPreflightImageMountPath   = "/preflight-image"
	kubernetesWorkerIDLabel          = "oz-worker-id"
	kubernetesWorkerHashLabel        = "oz-worker-hash"
	kubernetesTaskIDLabel            = "oz-task-id"
	kubernetesTaskHashLabel          = "oz-task-hash"
	kubernetesExecutionIDLabel       = "oz-execution-id"
	kubernetesExecutionHashLabel     = "oz-execution-hash"

	// maxLogBytes caps the amount of container log data read into memory per
	// container to avoid OOM when a task produces excessive output.
	maxLogBytes = 1 << 20 // 1 MiB
)

// KubernetesBackendConfig holds configuration specific to the Kubernetes backend.
type KubernetesBackendConfig struct {
	WorkerID              string
	Namespace             string
	Kubeconfig            string
	DefaultImage          string
	ImagePullPolicy       string
	UseImageVolumes       bool
	PreflightImage        string
	SidecarImage          string
	SetupCommand          string
	TeardownCommand       string
	NoCleanup             bool
	ExtraLabels           map[string]string
	ExtraAnnotations      map[string]string
	ActiveDeadlineSeconds *int64
	TTLSecondsAfterFinish *int32
	WorkspaceSizeLimit    *resource.Quantity
	UnschedulableTimeout  *time.Duration
	// CodingCLISidecars maps harness config name (e.g. "claude", "codex") to a custom
	// Docker image for the coding CLI sidecar. When non-empty for a task's harness,
	// the worker overrides or injects the sidecar at /mnt/{harness}-cli-sidecar.
	CodingCLISidecars map[string]string
	// TaskEnv contains runtime-only env overrides from CLI -e/--env. Declarative
	// task container env in file or Helm config should be set in PodTemplate.
	TaskEnv map[string]string
	// PodTemplate, when non-nil, is the declarative PodSpec template for task
	// Jobs. Worker-required fields are overlaid at execution time.
	PodTemplate *corev1.PodSpec
}

// KubernetesBackend executes tasks in Kubernetes Jobs.
type KubernetesBackend struct {
	config    KubernetesBackendConfig
	clientset kubernetes.Interface
}

func (b *KubernetesBackend) PreservesTasksOnShutdown() bool {
	return true
}

// NewKubernetesBackend creates a new Kubernetes backend and validates startup requirements.
func NewKubernetesBackend(ctx context.Context, config KubernetesBackendConfig) (*KubernetesBackend, error) {
	if config.Namespace == "" {
		config.Namespace = defaultKubernetesNamespace
	}
	if config.UnschedulableTimeout == nil {
		timeout := defaultUnschedulableFailureDelay
		config.UnschedulableTimeout = &timeout
	}
	if config.PreflightImage == "" {
		config.PreflightImage = kubernetesStartupPreflightImage
	}

	clientConfig, err := loadKubernetesClientConfig(config.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build Kubernetes client config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to reach Kubernetes API server: %w", err)
	}
	log.Debugf(ctx, "Connected to Kubernetes API server version %s", serverVersion.String())

	backend := &KubernetesBackend{
		config:    config,
		clientset: clientset,
	}
	if err := backend.runStartupPreflight(ctx); err != nil {
		return nil, err
	}

	return backend, nil
}

// ExecuteTask runs the agent in a Kubernetes Job.
func (b *KubernetesBackend) ExecuteTask(ctx context.Context, params *TaskParams) error {
	if err := validateTaskSidecars(params.Sidecars, b.config.UseImageVolumes); err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonSidecarPrep, err)
	}

	executionID := taskExecutionID(params)
	jobName := kubernetesTaskJobName(params.TaskID, executionID)
	jobLabels := b.baseLabels(params.TaskID, executionID)
	jobAnnotations := copyStringMap(b.config.ExtraAnnotations)
	pullPolicy := normalizePullPolicy(b.config.ImagePullPolicy)

	log.Debugf(ctx, "Using Kubernetes task image: %s", params.DockerImage)

	baseEnv := envSliceFromMap(b.config.TaskEnv)
	mainEnv := mergeEnvVars(params.EnvVars, append(baseEnv,
		fmt.Sprintf("OZ_ENVIRONMENT_FILE=%s", defaultSetupEnvironmentFile),
		fmt.Sprintf("OZ_WORKSPACE_ROOT=%s", defaultWorkspaceMountPath),
		"OZ_WORKER_BACKEND="+kubernetesBackendTypeName,
		fmt.Sprintf("OZ_RUN_ID=%s", params.TaskID),
	))

	volumes := []corev1.Volume{
		workspaceVolume(b.config.WorkspaceSizeLimit),
	}
	mainVolumeMounts := []corev1.VolumeMount{
		{
			Name:      workspaceVolumeName(),
			MountPath: defaultWorkspaceMountPath,
		},
	}
	setupVolumeMounts := []corev1.VolumeMount{
		{
			Name:      workspaceVolumeName(),
			MountPath: defaultWorkspaceMountPath,
		},
	}

	var initContainers []corev1.Container
	for i, sidecar := range params.Sidecars {
		if b.config.UseImageVolumes {
			volumeName := fmt.Sprintf("sidecar-%d-image", i)
			volumes = append(volumes, imageVolume(volumeName, sidecar.Image, pullPolicy))

			volumeMount := corev1.VolumeMount{
				Name:      volumeName,
				MountPath: sidecar.MountPath,
				ReadOnly:  true,
			}
			mainVolumeMounts = append(mainVolumeMounts, volumeMount)
			setupVolumeMounts = append(setupVolumeMounts, volumeMount)
			continue
		}

		dataVolumeName := fmt.Sprintf("sidecar-%d-data", i)
		volumes = append(volumes, corev1.Volume{
			Name: dataVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
		initContainers = append(initContainers, corev1.Container{
			Name:            fmt.Sprintf("copy-sidecar-%d", i),
			Image:           sidecar.Image,
			ImagePullPolicy: pullPolicy,
			Command: []string{
				"/bin/sh",
				"-c",
				kubernetesSidecarMaterializationScript(),
			},
			SecurityContext: rootSecurityContext(),
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      dataVolumeName,
					MountPath: sidecarCopyTargetMountPath,
				},
			},
		})

		mainVolumeMounts = append(mainVolumeMounts, corev1.VolumeMount{
			Name:      dataVolumeName,
			MountPath: sidecar.MountPath,
			ReadOnly:  !sidecar.ReadWrite,
		})
		setupVolumeMounts = append(setupVolumeMounts, corev1.VolumeMount{
			Name:      dataVolumeName,
			MountPath: sidecar.MountPath,
			ReadOnly:  !sidecar.ReadWrite,
		})
	}

	if b.config.SetupCommand != "" {
		setupEnv := envStringsToKubernetesEnv(mainEnv)
		initContainers = append(initContainers, corev1.Container{
			Name:            "setup",
			Image:           params.DockerImage,
			ImagePullPolicy: pullPolicy,
			Command:         []string{"/bin/sh", "-c", b.config.SetupCommand},
			WorkingDir:      defaultWorkspaceMountPath,
			Env:             setupEnv,
			VolumeMounts:    setupVolumeMounts,
		})
	}

	mainEnvVars := envStringsToKubernetesEnv(mainEnv)
	mainContainer := corev1.Container{
		Name:            "task",
		Image:           params.DockerImage,
		ImagePullPolicy: pullPolicy,
		Command: []string{
			"/bin/sh",
			"-c",
			kubernetesTaskWrapperScript(),
			"oz-task",
		},
		Args:         params.BaseArgs,
		Env:          mainEnvVars,
		WorkingDir:   defaultWorkspaceMountPath,
		VolumeMounts: mainVolumeMounts,
	}
	if b.config.TeardownCommand != "" {
		mainContainer.Lifecycle = &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "-c", b.config.TeardownCommand},
				},
			},
		}
	}

	// Size the task container from the runner's instance shape when one is set. Self-hosted
	// compute is operator-owned, so this is applied as-is (the server does the entitlement
	// capping). A nil shape leaves pod-template/cluster defaults untouched.
	applyInstanceShapeToContainer(&mainContainer, params.InstanceShape)

	podSpec := b.buildTaskPodSpec(initContainers, volumes, mainContainer)

	backoffLimit := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   b.config.Namespace,
			Labels:      jobLabels,
			Annotations: jobAnnotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   b.config.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: b.taskJobTTLSecondsAfterFinished(),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      jobLabels,
					Annotations: jobAnnotations,
				},
				Spec: podSpec,
			},
		},
	}

	log.Infof(ctx, "Creating Kubernetes Job %s in namespace %s", jobName, b.config.Namespace)
	if _, err := b.clientset.BatchV1().Jobs(b.config.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonJobCreate, fmt.Errorf("failed to create Kubernetes Job: %w", err))
	}

	deleted := false
	defer func() {
		if deleted {
			return
		}
		if ctx.Err() != nil {
			log.Infof(ctx, "Leaving Kubernetes Job %s in place after task context cancellation", jobName)
			return
		}
		if !b.config.NoCleanup {
			if err := b.deleteJob(context.Background(), jobName); err != nil {
				log.Warnf(ctx, "Failed to delete Job %s: %v", jobName, err)
			}
		}
	}()

	jobWatcher, err := b.watchJob(ctx, jobName)
	if err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonJobWatch, fmt.Errorf("failed to watch Job %s: %w", jobName, err))
	}
	defer jobWatcher.Stop()

	podWatcher, err := b.watchTaskPods(ctx, executionID)
	if err != nil {
		return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonPodWatch, fmt.Errorf("failed to watch Pods for Job %s: %w", jobName, err))
	}
	defer podWatcher.Stop()

	safetyTicker := time.NewTicker(watchSafetyInterval)
	defer safetyTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Infof(ctx, "Stopping local watch for Kubernetes Job %s after task context cancellation", jobName)
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err())

		case event, ok := <-jobWatcher.ResultChan():
			if !ok {
				// Watch closed; reopen.
				jobWatcher.Stop()
				jobWatcher, err = b.watchJob(ctx, jobName)
				if err != nil {
					return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonJobWatch, fmt.Errorf("failed to re-watch Job %s: %w", jobName, err))
				}
				continue
			}
			if event.Type == watch.Error {
				log.Warnf(ctx, "Job watch error for %s, reopening", jobName)
				jobWatcher.Stop()
				jobWatcher, err = b.watchJob(ctx, jobName)
				if err != nil {
					return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonJobWatch, fmt.Errorf("failed to re-watch Job %s: %w", jobName, err))
				}
				continue
			}
			jobState, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			if result := b.handleJobState(ctx, jobState, params.TaskID, executionID); result != nil {
				return result.err
			}

		case event, ok := <-podWatcher.ResultChan():
			if !ok {
				// Watch closed; reopen.
				podWatcher.Stop()
				podWatcher, err = b.watchTaskPods(ctx, executionID)
				if err != nil {
					return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonPodWatch, fmt.Errorf("failed to re-watch Pods for Job %s: %w", jobName, err))
				}
				continue
			}
			if event.Type == watch.Error {
				log.Warnf(ctx, "Pod watch error for Job %s, reopening", jobName)
				podWatcher.Stop()
				podWatcher, err = b.watchTaskPods(ctx, executionID)
				if err != nil {
					return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonPodWatch, fmt.Errorf("failed to re-watch Pods for Job %s: %w", jobName, err))
				}
				continue
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if failure := b.inspectPodFailure(ctx, pod); failure != nil {
				logs := b.collectPodLogs(ctx, []corev1.Pod{*pod})
				if logs != "" {
					log.Infof(ctx, "Pod %s output:\n%s", pod.Name, logs)
				}
				return failure
			}

		case <-safetyTicker.C:
			// Safety-net poll: catch anything the watches may have missed.
			jobState, err := b.clientset.BatchV1().Jobs(b.config.Namespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) && ctx.Err() != nil {
					return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonTaskCancelled, ctx.Err())
				}
				return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonJobWatch, fmt.Errorf("failed to get Job %s: %w", jobName, err))
			}
			if result := b.handleJobState(ctx, jobState, params.TaskID, executionID); result != nil {
				return result.err
			}

			pods, err := b.listTaskPods(ctx, executionID)
			if err != nil {
				return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonPodWatch, fmt.Errorf("failed to list task pods for Job %s: %w", jobName, err))
			}
			if failure := b.detectPodFailure(ctx, pods); failure != nil {
				return failure
			}
		}
	}
}

// Shutdown intentionally does not delete task Jobs.
//
// Kubernetes Jobs are the durable execution unit for this backend. During
// worker pod disruption (for example Karpenter node rotation), deleting Jobs
// here would kill otherwise healthy running sessions.
func (b *KubernetesBackend) Shutdown(ctx context.Context) {
	log.Infof(ctx, "Preserving Kubernetes task Jobs during worker shutdown")
}

func (b *KubernetesBackend) taskJobTTLSecondsAfterFinished() *int32 {
	if b.config.NoCleanup {
		return nil
	}
	if b.config.TTLSecondsAfterFinish != nil {
		return b.config.TTLSecondsAfterFinish
	}
	value := defaultJobTTLSecondsAfterFinish
	return &value
}

// jobResult wraps a terminal job outcome so handleJobState can signal the
// select loop with a nil error (success) or a non-nil error (failure).
type jobResult struct {
	err error
}

// handleJobState checks whether a Job has reached a terminal state and, if so,
// returns a *jobResult. A nil return means the Job is still in progress.
func (b *KubernetesBackend) handleJobState(ctx context.Context, jobState *batchv1.Job, taskID, executionID string) *jobResult {
	jobName := jobState.Name
	if jobComplete(jobState) {
		if zerolog.GlobalLevel() <= zerolog.DebugLevel {
			pods, _ := b.listTaskPods(ctx, executionID)
			logs := b.collectPodLogs(ctx, pods)
			if logs != "" {
				log.Debugf(ctx, "Job %s output:\n%s", jobName, logs)
			}
		}
		log.Infof(ctx, "Task %s execution completed successfully", taskID)
		return &jobResult{err: nil}
	}
	if jobFailed(jobState) {
		pods, _ := b.listTaskPods(ctx, executionID)
		logs := b.collectPodLogs(ctx, pods)
		if logs != "" {
			log.Infof(ctx, "Job %s output:\n%s", jobName, logs)
		}
		if failure := b.detectPodFailure(ctx, pods); failure != nil {
			return &jobResult{err: failure}
		}
		reason := classifyJobFailure(jobState)
		return &jobResult{err: newBackendFailure(metrics.TaskFailurePhaseBackend, reason, b.jobFailureError(jobState))}
	}
	return nil
}

func (b *KubernetesBackend) watchJob(ctx context.Context, jobName string) (watch.Interface, error) {
	return b.clientset.BatchV1().Jobs(b.config.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", jobName),
	})
}

func (b *KubernetesBackend) watchTaskPods(ctx context.Context, executionID string) (watch.Interface, error) {
	return b.clientset.CoreV1().Pods(b.config.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", kubernetesExecutionHashLabel, kubernetesLabelHash(executionID)),
	})
}

func loadKubernetesClientConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	if inCluster, err := rest.InClusterConfig(); err == nil {
		return inCluster, nil
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	return loader.ClientConfig()
}

func (b *KubernetesBackend) basePodSpec() corev1.PodSpec {
	if b.config.PodTemplate != nil {
		return *b.config.PodTemplate.DeepCopy()
	}
	return corev1.PodSpec{}
}

func (b *KubernetesBackend) buildTaskPodSpec(initContainers []corev1.Container, volumes []corev1.Volume, mainContainer corev1.Container) corev1.PodSpec {
	podSpec := b.basePodSpec()
	podSpec.RestartPolicy = corev1.RestartPolicyNever
	podSpec.InitContainers = append(initContainers, podSpec.InitContainers...)
	podSpec.Volumes = append(podSpec.Volumes, volumes...)

	taskIdx := -1
	for i, c := range podSpec.Containers {
		if c.Name == "task" {
			taskIdx = i
			break
		}
	}
	if taskIdx >= 0 {
		tc := &podSpec.Containers[taskIdx]
		tc.Image = mainContainer.Image
		if tc.ImagePullPolicy == "" {
			tc.ImagePullPolicy = mainContainer.ImagePullPolicy
		}
		tc.Command = mainContainer.Command
		tc.Args = mainContainer.Args
		tc.WorkingDir = mainContainer.WorkingDir
		tc.VolumeMounts = append(tc.VolumeMounts, mainContainer.VolumeMounts...)
		tc.Env = mergeKubernetesEnvVars(tc.Env, mainContainer.Env)
		if mainContainer.Lifecycle != nil {
			tc.Lifecycle = mainContainer.Lifecycle
		}
		// Runner instance shape (carried on mainContainer.Resources) overrides the matching
		// pod-template task-container resources per axis, preserving unrelated entries.
		tc.Resources = mergeResourceRequirements(tc.Resources, mainContainer.Resources)
	} else {
		podSpec.Containers = append(podSpec.Containers, mainContainer)
	}

	return podSpec
}

// applyInstanceShapeToContainer sets CPU/memory requests and limits on the container from
// the instance shape. Each axis is applied only when positive, so a partial shape overrides
// only the resource it specifies. Setting requests == limits pins the task container to the
// requested size, which drives pod scheduling (node fit) and in-pod enforcement. Only the
// task container is sized here (the setup/sidecar init containers are not), so the pod is
// not strictly Guaranteed QoS. A nil shape is a no-op, leaving pod-template/cluster defaults.
func applyInstanceShapeToContainer(c *corev1.Container, shape *types.InstanceShape) {
	if shape == nil {
		return
	}
	setResource := func(name corev1.ResourceName, qty resource.Quantity) {
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Requests[name] = qty
		c.Resources.Limits[name] = qty
	}
	if shape.Vcpus > 0 {
		setResource(corev1.ResourceCPU, *resource.NewQuantity(int64(shape.Vcpus), resource.DecimalSI))
	}
	if shape.MemoryGb > 0 {
		setResource(corev1.ResourceMemory, *resource.NewQuantity(int64(shape.MemoryGb)<<30, resource.BinarySI))
	}
}

// mergeResourceRequirements overlays override's requests/limits onto base per resource
// name, preserving base entries for resources the override does not set.
func mergeResourceRequirements(base, override corev1.ResourceRequirements) corev1.ResourceRequirements {
	for name, qty := range override.Requests {
		if base.Requests == nil {
			base.Requests = corev1.ResourceList{}
		}
		base.Requests[name] = qty
	}
	for name, qty := range override.Limits {
		if base.Limits == nil {
			base.Limits = corev1.ResourceList{}
		}
		base.Limits[name] = qty
	}
	return base
}

func workspaceVolume(sizeLimit *resource.Quantity) corev1.Volume {
	volume := corev1.Volume{
		Name: workspaceVolumeName(),
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	if sizeLimit != nil {
		copy := sizeLimit.DeepCopy()
		volume.EmptyDir.SizeLimit = &copy
	}
	return volume
}

func imageVolume(name, reference string, pullPolicy corev1.PullPolicy) corev1.Volume {
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Image: &corev1.ImageVolumeSource{
				Reference:  reference,
				PullPolicy: pullPolicy,
			},
		},
	}
}

func workspaceVolumeName() string {
	return "workspace"
}

func validateTaskSidecars(sidecars []types.SidecarMount, useImageVolumes bool) error {
	seenMountPaths := make(map[string]bool)
	for _, sidecar := range sidecars {
		if sidecar.Image == "" {
			return fmt.Errorf("additional sidecar has empty image")
		}
		if sidecar.MountPath == "" {
			return fmt.Errorf("additional sidecar %s has empty mount path", sidecar.Image)
		}
		if sidecar.MountPath == defaultWorkspaceMountPath {
			return fmt.Errorf("additional sidecar %s cannot mount at reserved path %s", sidecar.Image, defaultWorkspaceMountPath)
		}
		if seenMountPaths[sidecar.MountPath] {
			return fmt.Errorf("duplicate mount path %s for additional sidecar %s", sidecar.MountPath, sidecar.Image)
		}
		if useImageVolumes && sidecar.ReadWrite {
			return fmt.Errorf("additional sidecar %s cannot request a read-write mount: kubernetes image volumes are read-only", sidecar.Image)
		}
		seenMountPaths[sidecar.MountPath] = true
	}
	return nil
}

func (b *KubernetesBackend) runStartupPreflight(ctx context.Context) error {
	job := b.startupPreflightJob()
	createdJob, err := b.clientset.BatchV1().Jobs(b.config.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return b.startupPreflightError(err)
	}
	defer func() {
		if err := b.deleteJob(context.Background(), createdJob.Name); err != nil {
			log.Warnf(ctx, "Failed to delete startup preflight Job %s: %v", createdJob.Name, err)
		}
	}()

	preflightCtx, cancel := context.WithTimeout(ctx, startupPreflightTimeout)
	defer cancel()

	if err := b.waitForStartupPreflight(ctx, preflightCtx, createdJob); err != nil {
		if err == context.Canceled && ctx.Err() != nil {
			return ctx.Err()
		}
		return b.startupPreflightError(err)
	}

	return nil
}

func (b *KubernetesBackend) startupPreflightJob() *batchv1.Job {
	backoffLimit := int32(0)
	pullPolicy := normalizePullPolicy(b.config.ImagePullPolicy)
	podSpec := b.basePodSpec()
	podSpec.RestartPolicy = corev1.RestartPolicyNever
	if b.config.UseImageVolumes {
		podSpec.InitContainers = nil
		podSpec.Volumes = append(podSpec.Volumes, imageVolume(startupPreflightImageVolumeName, b.config.PreflightImage, pullPolicy))
		podSpec.Containers = []corev1.Container{
			{
				Name:            "main",
				Image:           b.config.PreflightImage,
				ImagePullPolicy: pullPolicy,
				Command:         []string{"/bin/sh", "-c", "test -d " + startupPreflightImageMountPath},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      startupPreflightImageVolumeName,
						MountPath: startupPreflightImageMountPath,
						ReadOnly:  true,
					},
				},
			},
		}
	} else {
		podSpec.InitContainers = []corev1.Container{
			{
				Name:            "root-init-preflight",
				Image:           b.config.PreflightImage,
				ImagePullPolicy: pullPolicy,
				Command:         []string{"/bin/sh", "-c", "true"},
				SecurityContext: rootSecurityContext(),
			},
		}
		podSpec.Containers = []corev1.Container{
			{
				Name:            "main",
				Image:           b.config.PreflightImage,
				ImagePullPolicy: pullPolicy,
				Command:         []string{"/bin/sh", "-c", "true"},
			},
		}
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("oz-preflight-%s-%d", kubernetesLabelHash(b.config.WorkerID), time.Now().UnixNano()),
			Namespace: b.config.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: podSpec,
			},
		},
	}
	return job
}

func (b *KubernetesBackend) waitForStartupPreflight(logCtx, ctx context.Context, job *batchv1.Job) error {
	if b.config.UseImageVolumes {
		return b.waitForImageVolumeStartupPreflight(logCtx, ctx, job)
	}
	return b.waitForLegacyStartupPreflight(logCtx, ctx, job)
}

func (b *KubernetesBackend) waitForLegacyStartupPreflight(logCtx, ctx context.Context, job *batchv1.Job) error {
	podSelector := fmt.Sprintf("job-name=%s", job.Name)
	eventSelector := fmt.Sprintf("involvedObject.uid=%s", job.UID)
	ticker := time.NewTicker(startupPreflightPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out waiting for startup preflight Job %q to create a Pod or surface a controller failure", job.Name)
			}
			return ctx.Err()
		default:
		}

		pods, err := b.clientset.CoreV1().Pods(b.config.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: podSelector,
		})
		if err != nil {
			return fmt.Errorf("failed to list startup preflight Pods: %w", err)
		}
		if len(pods.Items) > 0 {
			return nil
		}

		events, err := b.clientset.CoreV1().Events(b.config.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: eventSelector,
		})
		if err != nil {
			return fmt.Errorf("failed to list startup preflight events: %w", err)
		}
		if err := startupPreflightFailureFromEvents(job.Name, events.Items); err != nil {
			log.Warnf(logCtx, "Startup preflight Job %s failed before creating a Pod: %v", job.Name, err)
			return err
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (b *KubernetesBackend) waitForImageVolumeStartupPreflight(logCtx, ctx context.Context, job *batchv1.Job) error {
	podSelector := fmt.Sprintf("job-name=%s", job.Name)
	eventSelector := fmt.Sprintf("involvedObject.uid=%s", job.UID)
	ticker := time.NewTicker(startupPreflightPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out waiting for startup preflight Job %q to complete or surface a failure", job.Name)
			}
			return ctx.Err()
		default:
		}

		jobState, err := b.clientset.BatchV1().Jobs(b.config.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get startup preflight Job: %w", err)
		}
		if jobComplete(jobState) {
			return nil
		}

		pods, err := b.clientset.CoreV1().Pods(b.config.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: podSelector,
		})
		if err != nil {
			return fmt.Errorf("failed to list startup preflight Pods: %w", err)
		}
		if len(pods.Items) > 0 {
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodSucceeded {
					return nil
				}
			}
			if failure := b.detectPodFailure(logCtx, pods.Items); failure != nil {
				log.Warnf(logCtx, "Startup preflight Job %s failed after creating a Pod: %v", job.Name, failure)
				return failure
			}
		}
		if jobFailed(jobState) {
			return fmt.Errorf("startup preflight Job %q failed", job.Name)
		}

		events, err := b.clientset.CoreV1().Events(b.config.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: eventSelector,
		})
		if err != nil {
			return fmt.Errorf("failed to list startup preflight events: %w", err)
		}
		if err := startupPreflightFailureFromEvents(job.Name, events.Items); err != nil {
			log.Warnf(logCtx, "Startup preflight Job %s failed before creating a Pod: %v", job.Name, err)
			return err
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func startupPreflightFailureFromEvents(jobName string, events []corev1.Event) error {
	for _, event := range events {
		if event.Type != corev1.EventTypeWarning || event.Reason != "FailedCreate" {
			continue
		}
		message := strings.TrimSpace(event.Message)
		if message == "" {
			message = event.Reason
		}
		return fmt.Errorf("preflight Job %q could not create a Pod: %s", jobName, message)
	}
	return nil
}

func (b *KubernetesBackend) startupPreflightError(err error) error {
	if b.config.UseImageVolumes {
		return fmt.Errorf("kubernetes startup preflight failed: the kubernetes backend requires creating task Jobs that mount sidecars via image volumes; verify service account/RBAC, Pod Security or admission policy, and Kubernetes/runtime image-volume support for namespace %q: %w", b.config.Namespace, err)
	}
	return fmt.Errorf("kubernetes startup preflight failed: the kubernetes backend requires creating task Jobs with a root init container for sidecar materialization; verify service account/RBAC and Pod Security or admission policy for namespace %q: %w", b.config.Namespace, err)
}

func (b *KubernetesBackend) baseLabels(taskID, executionID string) map[string]string {
	if strings.TrimSpace(executionID) == "" {
		executionID = taskID
	}
	labels := copyStringMap(b.config.ExtraLabels)
	if labels == nil {
		labels = make(map[string]string, 6)
	}
	labels[kubernetesWorkerIDLabel] = sanitizeKubernetesLabelValue(b.config.WorkerID)
	labels[kubernetesWorkerHashLabel] = kubernetesLabelHash(b.config.WorkerID)
	labels[kubernetesTaskIDLabel] = sanitizeKubernetesLabelValue(taskID)
	labels[kubernetesTaskHashLabel] = kubernetesLabelHash(taskID)
	labels[kubernetesExecutionIDLabel] = sanitizeKubernetesLabelValue(executionID)
	labels[kubernetesExecutionHashLabel] = kubernetesLabelHash(executionID)
	return labels
}

func (b *KubernetesBackend) listTaskPods(ctx context.Context, executionID string) ([]corev1.Pod, error) {
	podList, err := b.clientset.CoreV1().Pods(b.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", kubernetesExecutionHashLabel, kubernetesLabelHash(executionID)),
	})
	if err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func (b *KubernetesBackend) detectPodFailure(ctx context.Context, pods []corev1.Pod) error {
	for _, pod := range pods {
		if failure := b.inspectPodFailure(ctx, &pod); failure != nil {
			logs := b.collectPodLogs(ctx, []corev1.Pod{pod})
			if logs != "" {
				log.Infof(ctx, "Pod %s output:\n%s", pod.Name, logs)
			}
			return failure
		}
	}
	return nil
}

func (b *KubernetesBackend) inspectPodFailure(ctx context.Context, pod *corev1.Pod) error {

	for _, status := range pod.Status.InitContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonInitContainer, b.containerTerminatedFailureError(pod, "init container", status.Name, status.State.Terminated))
		}
		if status.State.Waiting != nil && isImmediateContainerFailure(status.State.Waiting.Reason) {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, classifyWaitingReason(status.State.Waiting.Reason, true), b.containerWaitingFailureError(pod, "init container", status.Name, status.State.Waiting))
		}
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, classifyTerminatedReason(status.State.Terminated.Reason), b.containerTerminatedFailureError(pod, "container", status.Name, status.State.Terminated))
		}
		if status.State.Waiting != nil && isImmediateContainerFailure(status.State.Waiting.Reason) {
			return newBackendFailure(metrics.TaskFailurePhaseBackend, classifyWaitingReason(status.State.Waiting.Reason, false), b.containerWaitingFailureError(pod, "container", status.Name, status.State.Waiting))
		}
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
			if b.shouldFailUnschedulablePod(pod) {
				return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonUnschedulable, b.podConditionFailureError(pod, "is unschedulable", condition.Reason, condition.Message))
			}
		}
	}

	// Only query Events when the Pod shows signs of trouble to avoid
	// expensive API calls on every healthy poll/watch cycle.
	if pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodFailed {
		events, err := b.clientset.CoreV1().Events(b.config.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.uid=%s", pod.UID),
		})
		if err != nil {
			log.Warnf(ctx, "Failed to list events for pod %s: %v", pod.Name, err)
			return nil
		}
		for _, event := range events.Items {
			if event.Reason == "FailedMount" || strings.Contains(event.Message, "MountVolume.SetUp failed") {
				return newBackendFailure(metrics.TaskFailurePhaseBackend, metrics.TaskFailureReasonVolumeMount, b.podEventFailureError(pod, "failed to mount a volume", event))
			}
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		reason := metrics.TaskFailureReasonJobFailed
		if strings.Contains(strings.ToLower(pod.Status.Reason+" "+pod.Status.Message), "deadline") {
			reason = metrics.TaskFailureReasonActiveDeadline
		}
		return newBackendFailure(metrics.TaskFailurePhaseBackend, reason, b.podFailureError(pod))
	}

	return nil
}

func (b *KubernetesBackend) collectPodLogs(ctx context.Context, pods []corev1.Pod) string {
	var sections []string
	for _, pod := range pods {
		for _, container := range pod.Spec.InitContainers {
			if output, err := b.readContainerLogs(ctx, pod.Name, container.Name); err == nil && output != "" {
				sections = append(sections, fmt.Sprintf("[%s/%s]\n%s", pod.Name, container.Name, output))
			}
		}
		for _, container := range pod.Spec.Containers {
			if output, err := b.readContainerLogs(ctx, pod.Name, container.Name); err == nil && output != "" {
				sections = append(sections, fmt.Sprintf("[%s/%s]\n%s", pod.Name, container.Name, output))
			}
		}
	}
	return strings.Join(sections, "\n")
}

func (b *KubernetesBackend) readContainerLogs(ctx context.Context, podName, containerName string) (string, error) {
	limitBytes := int64(maxLogBytes)
	req := b.clientset.CoreV1().Pods(b.config.Namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container:  containerName,
		LimitBytes: &limitBytes,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := stream.Close(); err != nil {
			log.Warnf(ctx, "Failed to close log stream for %s/%s: %v", podName, containerName, err)
		}
	}()

	data, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (b *KubernetesBackend) deleteJob(ctx context.Context, jobName string) error {
	propagation := metav1.DeletePropagationBackground
	err := b.clientset.BatchV1().Jobs(b.config.Namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func normalizePullPolicy(policy string) corev1.PullPolicy {
	switch corev1.PullPolicy(policy) {
	case corev1.PullAlways, corev1.PullNever, corev1.PullIfNotPresent:
		return corev1.PullPolicy(policy)
	default:
		return corev1.PullIfNotPresent
	}
}

func envSliceFromMap(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(values))
	for _, key := range keys {
		result = append(result, fmt.Sprintf("%s=%s", key, values[key]))
	}
	return result
}

func envStringsToKubernetesEnv(entries []string) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(entries))
	for _, entry := range entries {
		key, value, hasEquals := strings.Cut(entry, "=")
		if !hasEquals || key == "" {
			continue
		}
		result = append(result, corev1.EnvVar{Name: key, Value: value})
	}
	return result
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

// taskJobExecIDSuffixLen is how many trailing characters of the execution ID are
// appended after the full run ID in a task Job name. It disambiguates re-executions
// (follow-up/handoff) of the same run while keeping the Job name within Kubernetes'
// 63-character limit for the job-name label the controller stamps on Pods.
const taskJobExecIDSuffixLen = 8

// kubernetesTaskJobName builds the task Job name as oz-task-<run>-exec-<exec-suffix>.
// The Job controller appends a random suffix when it creates the task Pod, so the Pod
// name becomes oz-task-<run>-exec-<exec-suffix>-<random>. The full run ID is embedded so
// Jobs and Pods stay greppable by run, while a short execution-ID suffix keeps the name
// unique across re-executions of the same run. The run fragment is truncated only as a
// defensive fallback so the name stays within the 63-character limit; a standard
// 36-character UUID run ID always fits without truncation.
func kubernetesTaskJobName(runID, executionID string) string {
	execFragment := lastKubernetesIDFragment(executionID, taskJobExecIDSuffixLen)
	if execFragment == "" {
		execFragment = "exec"
	}

	const prefix, sep = "oz-task-", "-exec-"
	maxRunLen := 63 - len(prefix) - len(sep) - len(execFragment)
	if maxRunLen < 0 {
		maxRunLen = 0
	}
	runFragment := sanitizeKubernetesNameFragment(runID)
	if len(runFragment) > maxRunLen {
		runFragment = strings.Trim(runFragment[:maxRunLen], "-")
	}
	if runFragment == "" {
		runFragment = "run"
	}

	return prefix + runFragment + sep + execFragment
}

// lastKubernetesIDFragment returns a DNS-label-safe fragment built from the last n
// characters of the sanitized id. It returns an empty string when id has no usable
// characters, so callers can substitute a fallback.
func lastKubernetesIDFragment(id string, n int) string {
	sanitized := sanitizeKubernetesNameFragment(id)
	if len(sanitized) > n {
		sanitized = strings.Trim(sanitized[len(sanitized)-n:], "-")
	}
	return sanitized
}

func sanitizeKubernetesLabelValue(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}

	sanitized := strings.Trim(builder.String(), "-_.")
	if sanitized == "" {
		return "unknown"
	}
	if len(sanitized) > 63 {
		sanitized = sanitized[:63]
		sanitized = strings.TrimRight(sanitized, "-_.")
	}
	if sanitized == "" {
		return "unknown"
	}
	return sanitized
}

func sanitizeKubernetesNameFragment(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}

	return strings.Trim(builder.String(), "-")
}

func kubernetesLabelHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}

func taskExecutionID(params *TaskParams) string {
	if params == nil {
		return ""
	}
	if executionID := strings.TrimSpace(params.ExecutionID); executionID != "" {
		return executionID
	}
	return params.TaskID
}

func kubernetesTaskWrapperScript() string {
	return strings.Join([]string{
		"if [ -f \"$OZ_ENVIRONMENT_FILE\" ]; then",
		"  set -a",
		"  . \"$OZ_ENVIRONMENT_FILE\"",
		"  set +a",
		"fi",
		"",
		"exec /agent/entrypoint.sh \"$@\"",
	}, "\n")
}

func kubernetesSidecarMaterializationScript() string {
	return strings.Join([]string{
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
}

// mergeKubernetesEnvVars merges base and override env var slices.
// Override entries take precedence on name conflict.
func mergeKubernetesEnvVars(base, override []corev1.EnvVar) []corev1.EnvVar {
	seen := make(map[string]int, len(base)+len(override))
	result := make([]corev1.EnvVar, 0, len(base)+len(override))
	for _, e := range base {
		seen[e.Name] = len(result)
		result = append(result, e)
	}
	for _, e := range override {
		if idx, ok := seen[e.Name]; ok {
			result[idx] = e
		} else {
			result = append(result, e)
		}
	}
	return result
}

func rootSecurityContext() *corev1.SecurityContext {
	rootUser := int64(0)
	rootGroup := int64(0)
	return &corev1.SecurityContext{
		RunAsUser:  &rootUser,
		RunAsGroup: &rootGroup,
	}
}

func (b *KubernetesBackend) jobFailureError(job *batchv1.Job) error {
	details := []string{
		fmt.Sprintf("kubernetes Job %s in namespace %s failed", job.Name, b.objectNamespace(job.Namespace)),
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type != batchv1.JobFailed || condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Reason != "" {
			details = append(details, "Job reason: "+condition.Reason)
		}
		if condition.Message != "" {
			details = append(details, "Job message: "+condition.Message)
		}
		break
	}
	return fmt.Errorf("%s", strings.Join(details, ". "))
}

// TODO: Send these Kubernetes diagnostics to the server as structured failure
// details so the UI/API can format them without parsing this human-readable
// message string.
func (b *KubernetesBackend) containerTerminatedFailureError(pod *corev1.Pod, kind, containerName string, terminated *corev1.ContainerStateTerminated) error {
	message := fmt.Sprintf("%s %s in %s exited with code %d", kind, containerName, b.podContext(pod), terminated.ExitCode)
	if signal := exitSignalName(terminated); signal != "" {
		message = fmt.Sprintf("%s (%s)", message, signal)
	}
	details := append([]string{message}, b.podStatusDetails(pod, terminated.Reason, terminated.Message)...)
	if guidance := b.terminationGuidance(kind, terminated); guidance != "" {
		details = append(details, guidance)
	}
	return fmt.Errorf("%s", strings.Join(details, ". "))
}

func (b *KubernetesBackend) containerWaitingFailureError(pod *corev1.Pod, kind, containerName string, waiting *corev1.ContainerStateWaiting) error {
	message := fmt.Sprintf("%s %s in %s is waiting: %s", kind, containerName, b.podContext(pod), waitingMessage(waiting))
	return fmt.Errorf("%s", strings.Join(append([]string{message}, b.podStatusDetails(pod, waiting.Reason, waiting.Message)...), ". "))
}

func (b *KubernetesBackend) podConditionFailureError(pod *corev1.Pod, summary, reason, message string) error {
	details := []string{fmt.Sprintf("%s %s", b.podContext(pod), summary)}
	if reason != "" {
		details = append(details, "Pod reason: "+reason)
	}
	if message != "" {
		details = append(details, "Pod message: "+message)
	}
	return fmt.Errorf("%s", strings.Join(details, ". "))
}

func (b *KubernetesBackend) podEventFailureError(pod *corev1.Pod, summary string, event corev1.Event) error {
	details := []string{fmt.Sprintf("%s %s", b.podContext(pod), summary)}
	if event.Reason != "" {
		details = append(details, "Kubernetes event reason: "+event.Reason)
	}
	if event.Message != "" {
		details = append(details, "Kubernetes event message: "+event.Message)
	}
	return fmt.Errorf("%s", strings.Join(details, ". "))
}

func (b *KubernetesBackend) podFailureError(pod *corev1.Pod) error {
	details := []string{fmt.Sprintf("%s failed", b.podContext(pod))}
	return fmt.Errorf("%s", strings.Join(append(details, b.podStatusDetails(pod, "", "")...), ". "))
}

func (b *KubernetesBackend) podContext(pod *corev1.Pod) string {
	details := []string{"namespace " + b.objectNamespace(pod.Namespace)}
	if pod.Labels["job-name"] != "" {
		details = append(details, "job "+pod.Labels["job-name"])
	}
	return fmt.Sprintf("pod %s (%s)", pod.Name, strings.Join(details, ", "))
}

func (b *KubernetesBackend) objectNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	return b.config.Namespace
}

func (b *KubernetesBackend) podStatusDetails(pod *corev1.Pod, reason, message string) []string {
	var details []string
	if pod.Status.Phase != "" {
		details = append(details, "Pod phase: "+string(pod.Status.Phase))
	}
	if reason == "" {
		reason = pod.Status.Reason
	}
	if reason != "" {
		details = append(details, "Pod reason: "+reason)
	}
	if message == "" {
		message = pod.Status.Message
	}
	if message != "" {
		details = append(details, "Pod message: "+message)
	}
	return details
}

func (b *KubernetesBackend) terminationGuidance(kind string, terminated *corev1.ContainerStateTerminated) string {
	if exitSignalName(terminated) != "SIGTERM" {
		return ""
	}
	switch kind {
	case "container":
		return "SIGTERM usually means Kubernetes or the container runtime asked the task process to stop; check pod events and node activity for eviction, preemption, node drain, activeDeadlineSeconds, or manual deletion."
	case "init container":
		return "SIGTERM usually means Kubernetes or the container runtime asked the setup process to stop; check pod events and node activity for eviction, preemption, node drain, activeDeadlineSeconds, or manual deletion."
	default:
		return "SIGTERM usually means Kubernetes or the container runtime asked the process to stop; check pod events and node activity for eviction, preemption, node drain, activeDeadlineSeconds, or manual deletion."
	}
}

func exitSignalName(terminated *corev1.ContainerStateTerminated) string {
	if signal := signalName(terminated.Signal); signal != "" {
		return signal
	}
	return signalName(terminated.ExitCode - 128)
}

func signalName(signal int32) string {
	switch signal {
	case 1:
		return "SIGHUP"
	case 2:
		return "SIGINT"
	case 3:
		return "SIGQUIT"
	case 6:
		return "SIGABRT"
	case 9:
		return "SIGKILL"
	case 15:
		return "SIGTERM"
	default:
		if signal > 0 {
			return fmt.Sprintf("signal %d", signal)
		}
		return ""
	}
}

func (b *KubernetesBackend) shouldFailUnschedulablePod(pod *corev1.Pod) bool {
	if b.config.UnschedulableTimeout == nil || *b.config.UnschedulableTimeout <= 0 {
		return false
	}
	return time.Since(pod.CreationTimestamp.Time) >= *b.config.UnschedulableTimeout
}

func isImmediateContainerFailure(reason string) bool {
	switch reason {
	case "CreateContainerConfigError", "CreateContainerError", "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
		return true
	default:
		return false
	}
}

func classifyJobFailure(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type != batchv1.JobFailed || condition.Status != corev1.ConditionTrue {
			continue
		}
		detail := strings.ToLower(condition.Reason + " " + condition.Message)
		if strings.Contains(detail, "deadline") {
			return metrics.TaskFailureReasonActiveDeadline
		}
	}
	return metrics.TaskFailureReasonJobFailed
}

func classifyWaitingReason(reason string, initContainer bool) string {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff":
		return metrics.TaskFailureReasonImagePull
	case "InvalidImageName":
		return metrics.TaskFailureReasonInvalidImage
	case "CreateContainerConfigError", "CreateContainerError":
		if initContainer {
			return metrics.TaskFailureReasonInitContainer
		}
		return metrics.TaskFailureReasonContainerCreate
	default:
		if initContainer {
			return metrics.TaskFailureReasonInitContainer
		}
		return metrics.TaskFailureReasonContainerWait
	}
}

func classifyTerminatedReason(reason string) string {
	if reason == "OOMKilled" {
		return metrics.TaskFailureReasonContainerOOM
	}
	return metrics.TaskFailureReasonContainerExit
}

func waitingMessage(waiting *corev1.ContainerStateWaiting) string {
	if waiting.Message == "" {
		return waiting.Reason
	}
	return fmt.Sprintf("%s: %s", waiting.Reason, waiting.Message)
}

func jobComplete(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
