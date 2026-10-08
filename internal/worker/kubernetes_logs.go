package worker

import (
	"context"
	"io"
	"sync"

	"github.com/warpdotdev/oz-agent-worker/internal/tasklogs"
	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
)

type kubernetesLogStreams struct {
	backend  *KubernetesBackend
	reporter *tasklogs.Reporter
	ctx      context.Context
	cancel   context.CancelFunc
	stop     func() bool
	seen     map[string]bool
	wg       sync.WaitGroup
}

func newKubernetesLogStreams(ctx context.Context, backend *KubernetesBackend, reporter *tasklogs.Reporter) *kubernetesLogStreams {
	if reporter == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	return &kubernetesLogStreams{
		backend: backend, reporter: reporter, ctx: ctx, cancel: cancel,
		stop: context.AfterFunc(reporter.Context(), cancel), seen: make(map[string]bool),
	}
}

func (s *kubernetesLogStreams) observePod(pod *corev1.Pod) {
	if s == nil || s.ctx.Err() != nil {
		return
	}
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		if status.State.Running == nil && status.State.Terminated == nil {
			continue
		}
		key := string(pod.UID) + "/" + status.Name
		if s.seen[key] || len(s.seen) >= 64 {
			continue
		}
		s.seen[key] = true
		s.wg.Go(func() {
			stream, err := s.backend.clientset.CoreV1().Pods(s.backend.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: status.Name, Follow: true,
			}).Stream(s.ctx)
			if err != nil {
				return
			}
			defer func() { _ = stream.Close() }()
			closeOnCancel := context.AfterFunc(s.ctx, func() { _ = stream.Close() })
			defer closeOnCancel()
			writer := s.reporter.Writer(s.ctx, "pod."+status.Name,
				attribute.String("k8s.container.name", status.Name),
				attribute.String("k8s.pod.name", pod.Name),
			)
			defer writer.Flush()
			_, _ = io.Copy(writer, stream)
		})
	}
}

func (s *kubernetesLogStreams) finish(ctx context.Context) {
	defer s.cancel()
	defer s.stop()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
