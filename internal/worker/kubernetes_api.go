package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	batchv1 "k8s.io/api/batch/v1"
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

func (b *KubernetesBackend) cleanupFailedJob(ctx context.Context, observedJob *batchv1.Job) error {
	if jobComplete(observedJob) || jobFailed(observedJob) {
		return nil
	}
	// Reserve time for deletion even if refreshing the Job keeps failing.
	refreshCtx, cancel := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
	job, err := b.getTaskJob(refreshCtx, observedJob.Name)
	cancel()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err == nil {
		if job.UID != observedJob.UID {
			return fmt.Errorf("job %s was replaced; refusing to delete another execution", observedJob.Name)
		}
		if jobComplete(job) || jobFailed(job) {
			return nil
		}
	} else {
		// A failed observation must not leave an execution we have ended runnable.
		log.Warnf(ctx, "Cannot refresh failed Job %s before cleanup; deleting its original UID: %v", observedJob.Name, err)
	}
	uid := observedJob.UID
	if err := b.deleteTaskJob(ctx, observedJob.Name, &uid); err != nil {
		return err
	}
	log.Infof(ctx, "Requested deletion of active Kubernetes Job %s after terminal task failure", observedJob.Name)
	return nil
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
