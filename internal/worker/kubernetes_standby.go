package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
)

const kubernetesStandbyWatchRetryInterval = 5 * time.Second
const kubernetesStandbyJobUIDIndex = "controllingJobUID"

func kubernetesStandbyJobName(sandboxID string) (string, error) {
	if err := validateStandbyID(sandboxID); err != nil {
		return "", err
	}
	return StandbyInstanceRef(sandboxID), nil
}

func (b *KubernetesBackend) kubernetesStandbyLabels(sandboxID string) map[string]string {
	labels := b.workerLabels()
	labels[StandbyKubernetesLabel] = sandboxID
	return labels
}

func kubernetesStandbyLabelSelector(sandboxID string) string {
	return StandbyKubernetesLabel + "=" + sandboxID
}

func kubernetesStandbyActiveDeadlineSeconds(ctx context.Context, configured *int64) *int64 {
	deadline, ok := ctx.Deadline()
	if !ok {
		return configured
	}
	remaining := max(int64(time.Until(deadline)/time.Second), 1)
	if configured != nil && *configured <= remaining {
		return configured
	}
	return &remaining
}

func (b *KubernetesBackend) kubernetesStandbyDeleteJob(ctx context.Context, job *batchv1.Job) {
	cleanupCtx, cancel := context.WithTimeout(ctx, kubernetesCleanupTimeout)
	defer cancel()
	if err := b.deleteTaskJob(cleanupCtx, job.Name, &job.UID); err != nil {
		log.Warnf(ctx, "Failed to delete standby Job %s: %v", job.Name, err)
	}
}

func (b *KubernetesBackend) cancelStandby(ctx context.Context, sandboxID string) error {
	// No Job can carry a label value that is invalid, so there is nothing to delete.
	if errs := validation.IsValidLabelValue(sandboxID); len(errs) > 0 {
		return nil
	}
	jobs, err := retryKubernetesAPI(ctx, kubernetesAPIBackoff(), func() (*batchv1.JobList, error) {
		requestCtx, cancel := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
		defer cancel()
		return b.clientset.BatchV1().Jobs(b.config.Namespace).List(requestCtx, metav1.ListOptions{
			LabelSelector: kubernetesStandbyLabelSelector(sandboxID),
		})
	})
	if err != nil {
		return fmt.Errorf("failed to list standby Jobs for sandbox %s: %w", sandboxID, err)
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if err := b.deleteTaskJob(ctx, job.Name, &job.UID); err != nil {
			return fmt.Errorf("failed to delete standby Job %s: %w", job.Name, err)
		}
		log.Infof(ctx, "Deleted standby Job %s", job.Name)
	}
	return nil
}

// WatchStandbyExits reports each standby Job that completes or fails once, until ctx ends.
func (b *KubernetesBackend) WatchStandbyExits(ctx context.Context, report func(sandboxID string, exitCode int, reason string)) {
	pods := b.newStandbyPodInformer()
	informerCtx, stopInformer := context.WithCancel(ctx)
	informerDone := make(chan struct{})
	go func() {
		defer close(informerDone)
		pods.Run(informerCtx.Done())
	}()
	defer func() {
		stopInformer()
		<-informerDone
	}()
	waitedForSync := false
	reported := make(map[k8stypes.UID]bool)
	selector := fmt.Sprintf("%s,%s=%s", StandbyKubernetesLabel, kubernetesWorkerIDLabel, sanitizeKubernetesLabelValue(b.config.WorkerID))
	for {
		watcher, err := b.watchStandbyJobs(ctx, selector)
		if err != nil {
			log.Warnf(ctx, "Failed to watch standby Jobs: %v", err)
		} else {
			if !waitedForSync {
				syncCtx, cancelSync := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
				if !cache.WaitForCacheSync(syncCtx.Done(), pods.HasSynced) && ctx.Err() == nil {
					log.Warnf(ctx, "Standby Pod cache did not sync before timeout; using live lookups until synced")
				}
				cancelSync()
				waitedForSync = true
			}
			b.consumeStandbyJobEvents(ctx, watcher, pods, reported, report)
			watcher.Stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(kubernetesStandbyWatchRetryInterval):
		}
	}
}

func (b *KubernetesBackend) newStandbyPodInformer() cache.SharedIndexInformer {
	return coreinformers.NewFilteredPodInformer(b.clientset, b.config.Namespace, 0, cache.Indexers{
		kubernetesStandbyJobUIDIndex: func(obj interface{}) ([]string, error) {
			pod := obj.(*corev1.Pod)
			owner := metav1.GetControllerOf(pod)
			if owner == nil || owner.Kind != "Job" {
				return nil, nil
			}
			return []string{string(owner.UID)}, nil
		},
	}, func(options *metav1.ListOptions) {
		options.LabelSelector = fmt.Sprintf("%s=%s", kubernetesWorkerHashLabel, kubernetesLabelHash(b.config.WorkerID))
	})
}

func (b *KubernetesBackend) consumeStandbyJobEvents(ctx context.Context, watcher watch.Interface, pods cache.SharedIndexInformer, reported map[k8stypes.UID]bool, report func(sandboxID string, exitCode int, reason string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok || event.Type == watch.Error {
				return
			}
			job, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			if event.Type == watch.Deleted {
				delete(reported, job.UID)
				continue
			}
			sandboxID := job.Labels[StandbyKubernetesLabel]
			if reported[job.UID] || sandboxID == "" || (!jobComplete(job) && !jobFailed(job)) {
				continue
			}
			reported[job.UID] = true
			exitCode, reason := b.kubernetesStandbyExitStatus(ctx, job, pods)
			report(sandboxID, exitCode, reason)
			b.kubernetesStandbyDeleteJob(ctx, job)
		}
	}
}

func (b *KubernetesBackend) kubernetesStandbyExitStatus(ctx context.Context, job *batchv1.Job, pods cache.SharedIndexInformer) (int, string) {
	if jobComplete(job) {
		return b.kubernetesStandbyTaskExitCode(ctx, job, pods, 0), ""
	}
	exitCode := b.kubernetesStandbyTaskExitCode(ctx, job, pods, 1)
	if exitCode == 0 {
		exitCode = 1
	}
	return exitCode, string(classifyJobFailure(job))
}

func (b *KubernetesBackend) kubernetesStandbyTaskExitCode(ctx context.Context, job *batchv1.Job, pods cache.SharedIndexInformer, fallback int) int {
	if pods.HasSynced() {
		objects, err := pods.GetIndexer().ByIndex(kubernetesStandbyJobUIDIndex, string(job.UID))
		if err == nil {
			for _, obj := range objects {
				if code, ok := kubernetesStandbyPodExitCode(obj.(*corev1.Pod), job); ok {
					return code
				}
			}
		}
	}

	// Job and Pod watches can deliver updates in either order. Refresh missing
	// terminal status without waiting for the Pod watch to catch up.
	lookupCtx, cancel := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
	defer cancel()
	selector := fmt.Sprintf("%s,%s=%s", kubernetesStandbyLabelSelector(job.Labels[StandbyKubernetesLabel]), kubernetesWorkerHashLabel, kubernetesLabelHash(b.config.WorkerID))
	livePods, err := b.listPods(lookupCtx, selector)
	if err != nil {
		log.Warnf(ctx, "Failed to list Pods for standby Job %s: %v", job.Name, err)
	}
	for i := range livePods {
		if code, ok := kubernetesStandbyPodExitCode(&livePods[i], job); ok {
			return code
		}
	}
	log.Warnf(ctx, "Using fallback exit code %d for standby Job %s (uid=%s): terminal task container status unavailable", fallback, job.Name, job.UID)
	return fallback
}

func kubernetesStandbyPodExitCode(pod *corev1.Pod, job *batchv1.Job) (int, bool) {
	if !metav1.IsControlledBy(pod, job) {
		return 0, false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == kubernetesTaskContainerName && status.State.Terminated != nil {
			return terminatedExitCode(status.State.Terminated), true
		}
	}
	return 0, false
}

func (b *KubernetesBackend) watchStandbyJobs(ctx context.Context, labelSelector string) (watch.Interface, error) {
	return retryKubernetesAPI(ctx, kubernetesAPIBackoff(), func() (watch.Interface, error) {
		return openKubernetesWatch(ctx, kubernetesAPIRequestTimeout, func(watchCtx context.Context) (watch.Interface, error) {
			return b.clientset.BatchV1().Jobs(b.config.Namespace).Watch(watchCtx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
		})
	})
}
