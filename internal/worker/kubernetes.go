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
	kubernetesBackendTypeName        = "kubernetes"
	sidecarCopyTargetMountPath       = "/target"
	kubernetesStartupPreflightImage  = "busybox:1.36"
	kubernetesWorkerIDLabel          = "oz-worker-id"
	kubernetesWorkerHashLabel        = "oz-worker-hash"
	kubernetesTaskIDLabel            = "oz-task-id"
	kubernetesTaskHashLabel          = "oz-task-hash"

	// maxLogBytes caps the amount of container log data read into memory per
	// container to avoid OOM when a task produces excessive output.
	maxLogBytes = 1 << 20 // 1 MiB
)

// KubernetesBackendConfig holds configuration specific to the Kubernetes backend.
type KubernetesBackendConfig struct {
	WorkerID                      string
	Namespace                     string
	Kubeconfig                    string
	ImagePullSecret               string
	ImagePullPolicy               string
	PreflightImage                string
	ServiceAccount                string
	SetupCommand                  string
	TeardownCommand               string
	NoCleanup                     bool
	NodeSelector                  map[string]string
	Tolerations                   []corev1.Toleration
	Resources                     corev1.ResourceRequirements
	ExtraLabels                   map[string]string
	ExtraAnnotations              map[string]string
	ActiveDeadlineSeconds         *int64
	TerminationGracePeriodSeconds *int64
	WorkspaceSizeLimit            *resource.Quantity
	UnschedulableTimeout          *time.Duration
	Env                           map[string]string
	// PodTemplate, when non-nil, is merged with the worker's required Pod fields
	// at task execution time. It takes precedence over the individual scheduling
	// fields above (which must not be set simultaneously; enforced at config load).
	PodTemplate *corev1.PodSpec
}

// KubernetesBackend executes tasks in Kubernetes Jobs.
type KubernetesBackend struct {
	config    KubernetesBackendConfig
	clientset kubernetes.Interface
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
	if err := validateTaskSidecars(params.Sidecars); err != nil {
		return err
	}

	jobName := sanitizeKubernetesJobName(params.TaskID)
	jobLabels := b.baseLabels(params.TaskID)
	jobAnnotations := copyStringMap(b.config.ExtraAnnotations)
	pullPolicy := normalizePullPolicy(b.config.ImagePullPolicy)

	log.Debugf(ctx, "Using Kubernetes task image: %s", params.DockerImage)

	baseEnv := envSliceFromMap(b.config.Env)
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

	// TODO(k8s-image-volumes): When Kubernetes image volumes (KEP-4639) reach GA,
	// replace the tar-based init container materialization below with native image
	// volume mounts, which would be faster and remove the root init container requirement.
	for i, sidecar := range params.Sidecars {
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
		Resources:    b.config.Resources,
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

	var podSpec corev1.PodSpec
	if b.config.PodTemplate != nil {
		podSpec = *b.config.PodTemplate.DeepCopy()
		// Force required fields.
		podSpec.RestartPolicy = corev1.RestartPolicyNever
		// Prepend worker init containers before any user-provided ones.
		podSpec.InitContainers = append(initContainers, podSpec.InitContainers...)
		// Append worker volumes.
		podSpec.Volumes = append(podSpec.Volumes, volumes...)
		// Find or create the "task" container.
		taskIdx := -1
		for i, c := range podSpec.Containers {
			if c.Name == "task" {
				taskIdx = i
				break
			}
		}
		if taskIdx >= 0 {
			// Merge into existing user-provided task container.
			tc := &podSpec.Containers[taskIdx]
			tc.Image = mainContainer.Image
			tc.ImagePullPolicy = mainContainer.ImagePullPolicy
			tc.Command = mainContainer.Command
			tc.Args = mainContainer.Args
			tc.WorkingDir = mainContainer.WorkingDir
			tc.VolumeMounts = append(tc.VolumeMounts, mainContainer.VolumeMounts...)
			// Merge env: worker vars take precedence on key conflict.
			tc.Env = mergeKubernetesEnvVars(tc.Env, mainContainer.Env)
			if mainContainer.Lifecycle != nil {
				tc.Lifecycle = mainContainer.Lifecycle
			}
		} else {
			// No user-provided task container; use the worker's.
			podSpec.Containers = append(podSpec.Containers, mainContainer)
		}
	} else {
		// Legacy: build PodSpec from individual config fields.
		podSpec = corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ServiceAccountName:            b.config.ServiceAccount,
			NodeSelector:                  copyStringMap(b.config.NodeSelector),
			Tolerations:                   append([]corev1.Toleration(nil), b.config.Tolerations...),
			InitContainers:                initContainers,
			Containers:                    []corev1.Container{mainContainer},
			Volumes:                       volumes,
			TerminationGracePeriodSeconds: b.config.TerminationGracePeriodSeconds,
		}
		if b.config.ImagePullSecret != "" {
			podSpec.ImagePullSecrets = []corev1.LocalObjectReference{
				{Name: b.config.ImagePullSecret},
			}
		}
	}

	backoffLimit := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        jobName,
			Namespace:   b.config.Namespace,
			Labels:      jobLabels,
			Annotations: jobAnnotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: b.config.ActiveDeadlineSeconds,
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
		return fmt.Errorf("failed to create Kubernetes Job: %w", err)
	}

	deleted := false
	defer func() {
		if deleted {
			return
		}
		if ctx.Err() != nil || !b.config.NoCleanup {
			if err := b.deleteJob(context.Background(), jobName); err != nil {
				log.Warnf(ctx, "Failed to delete Job %s: %v", jobName, err)
			}
		}
	}()

	jobWatcher, err := b.watchJob(ctx, jobName)
	if err != nil {
		return fmt.Errorf("failed to watch Job %s: %w", jobName, err)
	}
	defer jobWatcher.Stop()

	podWatcher, err := b.watchTaskPods(ctx, params.TaskID)
	if err != nil {
		return fmt.Errorf("failed to watch Pods for Job %s: %w", jobName, err)
	}
	defer podWatcher.Stop()

	safetyTicker := time.NewTicker(watchSafetyInterval)
	defer safetyTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := b.deleteJob(context.Background(), jobName); err != nil {
				log.Warnf(ctx, "Failed to delete Job %s on cancellation: %v", jobName, err)
			}
			deleted = true
			return ctx.Err()

		case event, ok := <-jobWatcher.ResultChan():
			if !ok {
				// Watch closed; reopen.
				jobWatcher.Stop()
				jobWatcher, err = b.watchJob(ctx, jobName)
				if err != nil {
					return fmt.Errorf("failed to re-watch Job %s: %w", jobName, err)
				}
				continue
			}
			if event.Type == watch.Error {
				log.Warnf(ctx, "Job watch error for %s, reopening", jobName)
				jobWatcher.Stop()
				jobWatcher, err = b.watchJob(ctx, jobName)
				if err != nil {
					return fmt.Errorf("failed to re-watch Job %s: %w", jobName, err)
				}
				continue
			}
			jobState, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			if result := b.handleJobState(ctx, jobState, params.TaskID); result != nil {
				return result.err
			}

		case event, ok := <-podWatcher.ResultChan():
			if !ok {
				// Watch closed; reopen.
				podWatcher.Stop()
				podWatcher, err = b.watchTaskPods(ctx, params.TaskID)
				if err != nil {
					return fmt.Errorf("failed to re-watch Pods for Job %s: %w", jobName, err)
				}
				continue
			}
			if event.Type == watch.Error {
				log.Warnf(ctx, "Pod watch error for Job %s, reopening", jobName)
				podWatcher.Stop()
				podWatcher, err = b.watchTaskPods(ctx, params.TaskID)
				if err != nil {
					return fmt.Errorf("failed to re-watch Pods for Job %s: %w", jobName, err)
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
					return ctx.Err()
				}
				return fmt.Errorf("failed to get Job %s: %w", jobName, err)
			}
			if result := b.handleJobState(ctx, jobState, params.TaskID); result != nil {
				return result.err
			}

			pods, err := b.listTaskPods(ctx, params.TaskID)
			if err != nil {
				return fmt.Errorf("failed to list task pods for Job %s: %w", jobName, err)
			}
			if failure := b.detectPodFailure(ctx, pods); failure != nil {
				return failure
			}
		}
	}
}

// Shutdown deletes all Jobs still labeled for this worker.
func (b *KubernetesBackend) Shutdown(ctx context.Context) {
	selector := fmt.Sprintf("%s=%s", kubernetesWorkerHashLabel, kubernetesLabelHash(b.config.WorkerID))
	jobs, err := b.clientset.BatchV1().Jobs(b.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		log.Warnf(ctx, "Failed to list lingering Jobs during shutdown: %v", err)
		return
	}
	for _, job := range jobs.Items {
		if err := b.deleteJob(ctx, job.Name); err != nil {
			log.Warnf(ctx, "Failed to delete lingering Job %s: %v", job.Name, err)
		}
	}
}

// jobResult wraps a terminal job outcome so handleJobState can signal the
// select loop with a nil error (success) or a non-nil error (failure).
type jobResult struct {
	err error
}

// handleJobState checks whether a Job has reached a terminal state and, if so,
// returns a *jobResult. A nil return means the Job is still in progress.
func (b *KubernetesBackend) handleJobState(ctx context.Context, jobState *batchv1.Job, taskID string) *jobResult {
	jobName := jobState.Name
	if jobComplete(jobState) {
		if zerolog.GlobalLevel() <= zerolog.DebugLevel {
			pods, _ := b.listTaskPods(ctx, taskID)
			logs := b.collectPodLogs(ctx, pods)
			if logs != "" {
				log.Debugf(ctx, "Job %s output:\n%s", jobName, logs)
			}
		}
		log.Infof(ctx, "Task %s execution completed successfully", taskID)
		return &jobResult{err: nil}
	}
	if jobFailed(jobState) {
		pods, _ := b.listTaskPods(ctx, taskID)
		logs := b.collectPodLogs(ctx, pods)
		if logs != "" {
			log.Infof(ctx, "Job %s output:\n%s", jobName, logs)
		}
		return &jobResult{err: fmt.Errorf("job %s failed", jobName)}
	}
	return nil
}

func (b *KubernetesBackend) watchJob(ctx context.Context, jobName string) (watch.Interface, error) {
	return b.clientset.BatchV1().Jobs(b.config.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", jobName),
	})
}

func (b *KubernetesBackend) watchTaskPods(ctx context.Context, taskID string) (watch.Interface, error) {
	return b.clientset.CoreV1().Pods(b.config.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", kubernetesTaskHashLabel, kubernetesLabelHash(taskID)),
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

func workspaceVolume(sizeLimit *resource.Quantity) corev1.Volume {
	volume := corev1.Volume{
		Name: workspaceVolumeName(),
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	if sizeLimit != nil {
		copy := sizeLimit.DeepCopy()
		volume.VolumeSource.EmptyDir.SizeLimit = &copy
	}
	return volume
}

func workspaceVolumeName() string {
	return "workspace"
}

func validateTaskSidecars(sidecars []types.SidecarMount) error {
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
		seenMountPaths[sidecar.MountPath] = true
	}
	return nil
}

func (b *KubernetesBackend) runStartupPreflight(ctx context.Context) error {
	backoffLimit := int32(0)
	pullPolicy := normalizePullPolicy(b.config.ImagePullPolicy)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oz-preflight-" + kubernetesLabelHash(b.config.WorkerID),
			Namespace: b.config.Namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: b.config.ServiceAccount,
					InitContainers: []corev1.Container{
						{
							Name:            "root-init-preflight",
							Image:           b.config.PreflightImage,
							ImagePullPolicy: pullPolicy,
							Command:         []string{"/bin/sh", "-c", "true"},
							SecurityContext: rootSecurityContext(),
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "main",
							Image:           b.config.PreflightImage,
							ImagePullPolicy: pullPolicy,
							Command:         []string{"/bin/sh", "-c", "true"},
						},
					},
				},
			},
		},
	}
	if b.config.ImagePullSecret != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: b.config.ImagePullSecret},
		}
	}

	if _, err := b.clientset.BatchV1().Jobs(b.config.Namespace).Create(ctx, job, metav1.CreateOptions{
		DryRun: []string{metav1.DryRunAll},
	}); err != nil {
		return fmt.Errorf("kubernetes startup preflight failed: the kubernetes backend requires creating task Jobs with a root init container for sidecar materialization; verify service account/RBAC and Pod Security or admission policy for namespace %q: %w", b.config.Namespace, err)
	}
	return nil
}

func (b *KubernetesBackend) baseLabels(taskID string) map[string]string {
	labels := copyStringMap(b.config.ExtraLabels)
	if labels == nil {
		labels = make(map[string]string, 4)
	}
	labels[kubernetesWorkerIDLabel] = sanitizeKubernetesLabelValue(b.config.WorkerID)
	labels[kubernetesWorkerHashLabel] = kubernetesLabelHash(b.config.WorkerID)
	labels[kubernetesTaskIDLabel] = sanitizeKubernetesLabelValue(taskID)
	labels[kubernetesTaskHashLabel] = kubernetesLabelHash(taskID)
	return labels
}

func (b *KubernetesBackend) listTaskPods(ctx context.Context, taskID string) ([]corev1.Pod, error) {
	podList, err := b.clientset.CoreV1().Pods(b.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", kubernetesTaskHashLabel, kubernetesLabelHash(taskID)),
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
			return fmt.Errorf("init container %s in pod %s exited with code %d", status.Name, pod.Name, status.State.Terminated.ExitCode)
		}
		if status.State.Waiting != nil && isImmediateContainerFailure(status.State.Waiting.Reason) {
			return fmt.Errorf("init container %s in pod %s is waiting: %s", status.Name, pod.Name, waitingMessage(status.State.Waiting))
		}
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
			return fmt.Errorf("container %s in pod %s exited with code %d", status.Name, pod.Name, status.State.Terminated.ExitCode)
		}
		if status.State.Waiting != nil && isImmediateContainerFailure(status.State.Waiting.Reason) {
			return fmt.Errorf("container %s in pod %s is waiting: %s", status.Name, pod.Name, waitingMessage(status.State.Waiting))
		}
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
			if b.shouldFailUnschedulablePod(pod) {
				return fmt.Errorf("pod %s is unschedulable: %s", pod.Name, condition.Message)
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
				return fmt.Errorf("pod %s failed to mount a volume: %s", pod.Name, event.Message)
			}
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		if pod.Status.Message != "" {
			return fmt.Errorf("pod %s failed: %s", pod.Name, pod.Status.Message)
		}
		if pod.Status.Reason != "" {
			return fmt.Errorf("pod %s failed: %s", pod.Name, pod.Status.Reason)
		}
		return fmt.Errorf("pod %s failed", pod.Name)
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

func sanitizeKubernetesJobName(taskID string) string {
	suffix := sanitizeKubernetesNameFragment(taskID)
	if suffix == "" {
		suffix = "task"
	}

	hash := kubernetesLabelHash(taskID)
	maxSuffixLength := 63 - len("oz-task--") - len(hash)
	if len(suffix) > maxSuffixLength {
		suffix = suffix[:maxSuffixLength]
		suffix = strings.Trim(suffix, "-")
	}
	if suffix == "" {
		suffix = "task"
	}

	return fmt.Sprintf("oz-task-%s-%s", suffix, hash)
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

func kubernetesTaskWrapperScript() string {
	return strings.Join([]string{
		"if [ -f \"$OZ_ENVIRONMENT_FILE\" ]; then",
		"  set -a",
		"  . \"$OZ_ENVIRONMENT_FILE\"",
		"  set +a",
		"fi",
		"/bin/sh /agent/entrypoint.sh \"$@\"",
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
