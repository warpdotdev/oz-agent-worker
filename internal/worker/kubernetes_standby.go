package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
)

var (
	_ StandbyDestroyer   = (*KubernetesBackend)(nil)
	_ StandbyExitWatcher = (*KubernetesBackend)(nil)
)

const kubernetesStandbyWatchRetryInterval = 5 * time.Second

func kubernetesStandbyJobName(sandboxID string) (string, error) {
	name := StandbyInstanceRef(sandboxID)
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return "", fmt.Errorf("standby sandbox ID %q cannot name a Kubernetes Job: %s", sandboxID, strings.Join(errs, "; "))
	}
	return name, nil
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
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kubernetesCleanupTimeout)
	defer cancel()
	if err := b.deleteTaskJob(cleanupCtx, job.Name, &job.UID); err != nil {
		log.Warnf(ctx, "Failed to delete standby Job %s: %v", job.Name, err)
	}
}

// DestroyStandby deletes the Jobs labeled with sandboxID and returns nil when none exists.
func (b *KubernetesBackend) DestroyStandby(ctx context.Context, sandboxID string) error {
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
	reported := make(map[k8stypes.UID]bool)
	selector := fmt.Sprintf("%s,%s=%s", StandbyKubernetesLabel, kubernetesWorkerIDLabel, sanitizeKubernetesLabelValue(b.config.WorkerID))
	for {
		watcher, err := b.watchStandbyJobs(ctx, selector)
		if err != nil {
			log.Warnf(ctx, "Failed to watch standby Jobs: %v", err)
		} else {
			b.consumeStandbyJobEvents(ctx, watcher, reported, report)
			watcher.Stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(kubernetesStandbyWatchRetryInterval):
		}
	}
}

func (b *KubernetesBackend) consumeStandbyJobEvents(ctx context.Context, watcher watch.Interface, reported map[k8stypes.UID]bool, report func(sandboxID string, exitCode int, reason string)) {
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
			exitCode, reason := b.kubernetesStandbyExitStatus(ctx, job)
			report(sandboxID, exitCode, reason)
			b.kubernetesStandbyDeleteJob(ctx, job)
		}
	}
}

func (b *KubernetesBackend) kubernetesStandbyExitStatus(ctx context.Context, job *batchv1.Job) (int, string) {
	if jobComplete(job) {
		return b.kubernetesStandbyTaskExitCode(ctx, job, 0), ""
	}
	return b.kubernetesStandbyTaskExitCode(ctx, job, 1), string(classifyJobFailure(job))
}

func (b *KubernetesBackend) kubernetesStandbyTaskExitCode(ctx context.Context, job *batchv1.Job, fallback int) int {
	pods, err := b.listPods(ctx, kubernetesStandbyLabelSelector(job.Labels[StandbyKubernetesLabel]))
	if err != nil {
		log.Warnf(ctx, "Failed to list Pods for standby Job %s: %v", job.Name, err)
		return fallback
	}
	for i := range pods {
		if !metav1.IsControlledBy(&pods[i], job) {
			continue
		}
		for _, status := range pods[i].Status.ContainerStatuses {
			if status.Name == kubernetesTaskContainerName && status.State.Terminated != nil {
				return terminatedExitCode(status.State.Terminated)
			}
		}
	}
	return fallback
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
