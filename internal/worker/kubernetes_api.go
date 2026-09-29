package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	kubernetesAPIRequestTimeout = 30 * time.Second
	kubernetesCleanupTimeout    = 2 * time.Minute
)

type jobFailureDisposition uint8

const (
	jobTerminationRequired jobFailureDisposition = iota
	jobExecutionStopped
)

func withJobDisposition(err error, disposition jobFailureDisposition) error {
	var failure *TaskFailure
	if errors.As(err, &failure) {
		failure.jobDisposition = disposition
	}
	return err
}

func jobDisposition(err error) jobFailureDisposition {
	var failure *TaskFailure
	if errors.As(err, &failure) {
		return failure.jobDisposition
	}
	return jobTerminationRequired
}

func kubernetesAPIBackoff() wait.Backoff {
	return wait.Backoff{Duration: time.Second, Factor: 2, Jitter: 0.1, Cap: 10 * time.Second, Steps: 6}
}

type cancellableKubernetesWatch struct {
	watch.Interface
	cancel context.CancelCauseFunc
}

func (w *cancellableKubernetesWatch) Stop() {
	w.cancel(context.Canceled)
	w.Interface.Stop()
}

func openKubernetesWatch(ctx context.Context, timeout time.Duration, open func(context.Context) (watch.Interface, error)) (watch.Interface, error) {
	watchCtx, cancel := context.WithCancelCause(ctx)
	// Only establishment is timed out; a successful stream lives until Stop or
	// parent cancellation, rather than inheriting a short request deadline.
	timer := time.AfterFunc(timeout, func() { cancel(context.DeadlineExceeded) })
	watcher, err := open(watchCtx)
	if !timer.Stop() {
		cancel(context.DeadlineExceeded)
	}
	if cause := context.Cause(watchCtx); cause != nil {
		if watcher != nil {
			watcher.Stop()
		}
		cancel(cause)
		return nil, cause
	}
	if err != nil {
		cancel(err)
		return nil, err
	}
	return &cancellableKubernetesWatch{Interface: watcher, cancel: cancel}, nil
}

func retryKubernetesAPI[T any](ctx context.Context, backoff wait.Backoff, operation func() (T, error)) (T, error) {
	var result T
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		result, lastErr = operation()
		if lastErr == nil {
			return true, nil
		}
		if !isTransientKubernetesAPIError(lastErr) {
			return false, lastErr
		}
		log.Warnf(ctx, "Retrying transient Kubernetes API error: %v", lastErr)
		return false, nil
	})
	if err != nil && lastErr != nil && !errors.Is(err, lastErr) {
		return result, fmt.Errorf("kubernetes API operation failed: %w (last error: %w)", err, lastErr)
	}
	return result, err
}

func isTransientKubernetesAPIError(err error) bool {
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) {
		return true
	}
	var networkErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &networkErr) && networkErr.Timeout())
}

func (b *KubernetesBackend) getTaskJob(ctx context.Context, jobName string) (*batchv1.Job, error) {
	return retryKubernetesAPI(ctx, kubernetesAPIBackoff(), func() (*batchv1.Job, error) {
		requestCtx, cancel := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
		defer cancel()
		return b.clientset.BatchV1().Jobs(b.config.Namespace).Get(requestCtx, jobName, metav1.GetOptions{})
	})
}

func (b *KubernetesBackend) finalizeFailedJob(ctx context.Context, observedJob *batchv1.Job, failure error) error {
	if jobComplete(observedJob) || jobFailed(observedJob) {
		return nil
	}
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, kubernetesAPIBackoff(), func(ctx context.Context) (bool, error) {
		lastErr = b.expireAbandonedJob(ctx, observedJob, jobDisposition(failure))
		if lastErr == nil {
			return true, nil
		}
		if apierrors.IsConflict(lastErr) || isTransientKubernetesAPIError(lastErr) {
			return false, nil
		}
		return false, lastErr
	})
	if err != nil && lastErr != nil && !errors.Is(err, lastErr) {
		return fmt.Errorf("job expiration failed: %w (last error: %w)", err, lastErr)
	}
	return err
}

func (b *KubernetesBackend) expireAbandonedJob(ctx context.Context, observedJob *batchv1.Job, disposition jobFailureDisposition) error {
	refreshCtx, cancelRefresh := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
	job, err := b.clientset.BatchV1().Jobs(b.config.Namespace).Get(refreshCtx, observedJob.Name, metav1.GetOptions{})
	cancelRefresh()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		// The UID and resource version fence the patch when observation is unavailable.
		log.Warnf(ctx, "Cannot refresh abandoned Job %s; attempting expiration using its last observed identity: %v", observedJob.Name, err)
		job = observedJob
	}
	if job.UID != observedJob.UID {
		return fmt.Errorf("job %s was replaced; refusing to expire another execution", observedJob.Name)
	}
	if jobComplete(job) || jobFailed(job) {
		return nil
	}
	if disposition == jobExecutionStopped {
		checkCtx, cancelCheck := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
		stopped, checkErr := b.jobPodsStopped(checkCtx, job)
		cancelCheck()
		if checkErr == nil && stopped {
			return nil
		}
		if checkErr != nil {
			log.Warnf(ctx, "Cannot confirm execution stopped for Job %s; requesting expiration: %v", job.Name, checkErr)
		}
	}
	// Zero parallelism also prevents startup if the controller has not set startTime.
	// Resuming a suspended Job lets its shortened deadline take effect.
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": job.UID, "resourceVersion": job.ResourceVersion},
		"spec":     map[string]any{"activeDeadlineSeconds": 1, "parallelism": 0, "suspend": false},
	})
	if err != nil {
		return err
	}
	patchCtx, cancelPatch := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
	defer cancelPatch()
	_, err = b.clientset.BatchV1().Jobs(b.config.Namespace).Patch(patchCtx, job.Name, k8stypes.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	log.Infof(ctx, "Requested expiration of abandoned Kubernetes Job %s; retaining it according to its TTL", job.Name)
	return nil
}
func (b *KubernetesBackend) jobPodsStopped(ctx context.Context, job *batchv1.Job) (bool, error) {
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.PodFailurePolicy != nil {
		return false, nil
	}
	pods, err := b.clientset.CoreV1().Pods(b.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: batchv1.ControllerUidLabel + "=" + string(job.UID),
	})
	if err != nil {
		return false, err
	}
	hasFailedPod := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, job) {
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodFailed:
			hasFailedPod = true
		case corev1.PodSucceeded:
		default:
			return false, nil
		}
	}
	return hasFailedPod, nil
}

func (b *KubernetesBackend) deleteTaskJob(ctx context.Context, jobName string, uid *k8stypes.UID) error {
	_, err := retryKubernetesAPI(ctx, kubernetesAPIBackoff(), func() (struct{}, error) {
		requestCtx, cancel := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
		defer cancel()
		propagation := metav1.DeletePropagationBackground
		options := metav1.DeleteOptions{PropagationPolicy: &propagation}
		if uid != nil {
			options.Preconditions = &metav1.Preconditions{UID: uid}
		}
		err := b.clientset.BatchV1().Jobs(b.config.Namespace).Delete(requestCtx, jobName, options)
		if apierrors.IsNotFound(err) {
			err = nil
		}
		return struct{}{}, err
	})
	return err
}
