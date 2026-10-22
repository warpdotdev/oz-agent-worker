package worker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/tasktelemetry/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	corev1 "k8s.io/api/core/v1"
)

// kubernetesSetupPhaseTracker derives task setup phase durations from pod
// status snapshots and reports each phase once. The kubelet performs the
// image pulls and container starts for this backend, so the worker cannot
// time them directly; instead, every phase is computed from timestamps the
// pod object carries (creation time, the PodScheduled condition transition,
// init container terminations, and the task container start). Late or
// repeated watch deliveries therefore do not skew the measurements.
type kubernetesSetupPhaseTracker struct {
	reporter  *setupEventReporter
	reported  map[string]bool
	live      map[string]trace.Span
	execution trace.Span
}

func taskInitFinished(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.InitContainers {
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			continue
		}
		done := false
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name == container.Name && status.State.Terminated != nil && status.State.Terminated.ExitCode == 0 {
				done = true
			}
		}
		if !done {
			return false
		}
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// sidecarInitIndex returns the sidecar index encoded in a copy-sidecar-<i>
// init container name.
func sidecarInitIndex(containerName string) (int, bool) {
	suffix, ok := strings.CutPrefix(containerName, kubernetesSidecarInitPrefix)
	if !ok {
		return 0, false
	}
	index, err := strconv.Atoi(suffix)
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func newKubernetesSetupPhaseTracker(reporter *setupEventReporter) *kubernetesSetupPhaseTracker {
	return &kubernetesSetupPhaseTracker{
		reporter: reporter,
		reported: make(map[string]bool),
		live:     make(map[string]trace.Span),
	}
}

// observePod inspects a pod status snapshot and reports any newly completed
// setup phases. It is safe to call repeatedly with successive snapshots.
func (t *kubernetesSetupPhaseTracker) observePod(ctx context.Context, pod *corev1.Pod) {
	if t == nil || pod == nil {
		return
	}
	t.observeSchedule(ctx, pod)
	t.observeInitContainers(ctx, pod)
	t.observeSidecarPrep(ctx, pod)
	t.observeSetupCommand(ctx, pod)
	t.observeTaskStart(ctx, pod)
}

// report sends one phase at most once, skipping ranges the pod timestamps
// cannot support (zero or inverted times).
func (t *kubernetesSetupPhaseTracker) report(ctx context.Context, eventName string, start, finish time.Time, isError bool) {
	if t.reported[eventName] || start.IsZero() || finish.IsZero() || finish.Before(start) {
		return
	}
	t.observeInterval(ctx, eventName, setupTracePhase(eventName), start, finish, setupPhaseError(isError))
	t.reporter.reportPhase(ctx, eventName, start, finish, isError)
}

// reportEvent sends one client event at most once without opening a trace
// span, for intervals observeInterval already traces under another key.
func (t *kubernetesSetupPhaseTracker) reportEvent(ctx context.Context, eventName string, start, finish time.Time, isError bool) {
	if t.reported[eventName] || start.IsZero() || finish.IsZero() || finish.Before(start) {
		return
	}
	t.reported[eventName] = true
	t.reporter.reportPhase(ctx, eventName, start, finish, isError)
}

func (t *kubernetesSetupPhaseTracker) start(ctx context.Context, event string, start time.Time) {
	t.observeInterval(ctx, event, setupTracePhase(event), start, time.Time{}, nil)
}

func (t *kubernetesSetupPhaseTracker) observeInterval(ctx context.Context, key, phase string, start, finish time.Time, err error, attrs ...attribute.KeyValue) {
	if t.reported[key] || start.IsZero() || (!finish.IsZero() && finish.Before(start)) {
		return
	}
	if _, exists := t.live[key]; !exists {
		_, t.live[key] = tracing.Tracer(ctx).Start(ctx, "self_hosted_worker."+phase,
			trace.WithTimestamp(start), trace.WithAttributes(append(attrs, attribute.String("phase", phase))...))
	}
	if !finish.IsZero() {
		tracing.RecordError(t.live[key], err)
		t.live[key].End(trace.WithTimestamp(finish))
		delete(t.live, key)
		t.reported[key] = true
	}
}

func (t *kubernetesSetupPhaseTracker) finish(ctx context.Context, err error) {
	interrupted := errors.Is(context.Cause(ctx), errObservationInterrupted)
	spans := make([]trace.Span, 0, len(t.live)+1)
	for key, span := range t.live {
		spans = append(spans, span)
		delete(t.live, key)
		t.reported[key] = true
	}
	if t.execution != nil {
		spans = append(spans, t.execution)
		t.execution = nil
	}
	for _, span := range spans {
		if interrupted {
			span.SetAttributes(attribute.String("outcome", "interrupted"))
		} else {
			tracing.RecordError(span, err)
		}
		span.End()
	}
}

// observeSchedule reports the span from pod creation to a true PodScheduled
// condition. This surfaces node capacity and autoscaler waits.
func (t *kubernetesSetupPhaseTracker) observeSchedule(ctx context.Context, pod *corev1.Pod) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
			t.report(ctx, SetupEventPodSchedule, pod.CreationTimestamp.Time, condition.LastTransitionTime.Time, false)
			return
		}
	}
	if !t.reported[SetupEventPodSchedule] {
		t.start(ctx, SetupEventPodSchedule, pod.CreationTimestamp.Time)
	}
}

// Pod status has no image-pull timestamps. Preparation includes pulling images
// and other pre-start delays, separately from the init container's runtime.
// Copy-sidecar init containers additionally report both halves as client
// events so the per-boundary cost is visible outside traces.
func (t *kubernetesSetupPhaseTracker) observeInitContainers(ctx context.Context, pod *corev1.Pod) {
	var anchor time.Time
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
			anchor = condition.LastTransitionTime.Time
		}
	}
	for _, container := range pod.Spec.InitContainers {
		var started, finished time.Time
		var failed bool
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name != container.Name {
				continue
			}
			if status.State.Running != nil {
				started = status.State.Running.StartedAt.Time
			} else if status.State.Terminated != nil {
				started = status.State.Terminated.StartedAt.Time
				finished = status.State.Terminated.FinishedAt.Time
				failed = status.State.Terminated.ExitCode != 0
			}
		}
		attrs := []attribute.KeyValue{attribute.String("k8s.container.name", container.Name)}
		t.observeInterval(ctx, "image_prepare/"+container.Name, "image_prepare", anchor, started, nil, attrs...)
		sidecarIndex, isCopySidecar := sidecarInitIndex(container.Name)
		if isCopySidecar {
			t.reportEvent(ctx, setupEventSidecarImageWait(sidecarIndex), anchor, started, false)
		}
		if started.IsZero() {
			return
		}
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			// Native sidecars stay running while later init containers start.
			anchor = started
			continue
		}
		switch {
		case isCopySidecar:
			t.observeInterval(ctx, "sidecar_copy/"+container.Name, "sidecar_copy", started, finished, setupPhaseError(failed), attrs...)
			t.reportEvent(ctx, setupEventSidecarCopy(sidecarIndex), started, finished, failed)
		case container.Name != kubernetesSetupContainerName:
			t.observeInterval(ctx, "init_container/"+container.Name, "init_container", started, finished, setupPhaseError(failed), attrs...)
		}
		if finished.IsZero() || finished.Before(started) || failed {
			return
		}
		anchor = finished
	}
}

// observeSidecarPrep reports one aggregate phase spanning the sequential
// copy-sidecar-* init containers, from the first start to the last finish.
// Image preparation is tracked separately before each init starts. The
// image-volumes mode creates no copy init containers, so the phase does not
// fire there. A failed sidecar init reports early with is_error set.
func (t *kubernetesSetupPhaseTracker) observeSidecarPrep(ctx context.Context, pod *corev1.Pod) {
	expected := 0
	for _, container := range pod.Spec.InitContainers {
		if strings.HasPrefix(container.Name, kubernetesSidecarInitPrefix) {
			expected++
		}
	}
	if expected == 0 {
		return
	}

	var start, finish time.Time
	terminated := 0
	failed := false
	for _, status := range pod.Status.InitContainerStatuses {
		if !strings.HasPrefix(status.Name, kubernetesSidecarInitPrefix) {
			continue
		}
		if status.State.Running != nil && (start.IsZero() || status.State.Running.StartedAt.Time.Before(start)) {
			start = status.State.Running.StartedAt.Time
		}
		if status.State.Terminated == nil {
			continue
		}
		terminated++
		term := status.State.Terminated
		if start.IsZero() || term.StartedAt.Time.Before(start) {
			start = term.StartedAt.Time
		}
		if term.FinishedAt.After(finish) {
			finish = term.FinishedAt.Time
		}
		if term.ExitCode != 0 {
			failed = true
		}
	}
	if failed || terminated == expected {
		t.report(ctx, SetupEventSidecarPrep, start, finish, failed)
	} else if !t.reported[SetupEventSidecarPrep] {
		t.start(ctx, SetupEventSidecarPrep, start)
	}
}

// observeSetupCommand reports the operator-configured setup init container's
// run time once it terminates.
func (t *kubernetesSetupPhaseTracker) observeSetupCommand(ctx context.Context, pod *corev1.Pod) {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != kubernetesSetupContainerName {
			continue
		}
		if status.State.Running != nil && !t.reported[SetupEventSetupCommand] {
			t.start(ctx, SetupEventSetupCommand, status.State.Running.StartedAt.Time)
		}
		if status.State.Terminated == nil {
			continue
		}
		term := status.State.Terminated
		t.report(ctx, SetupEventSetupCommand, term.StartedAt.Time, term.FinishedAt.Time, term.ExitCode != 0)
		return
	}
}

// observeTaskStart reports the span from the end of init to the task
// container start. This includes the kubelet's pull of the task image.
func (t *kubernetesSetupPhaseTracker) observeTaskStart(ctx context.Context, pod *corev1.Pod) {
	var startedAt time.Time
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != kubernetesTaskContainerName {
			continue
		}
		// A terminated state still carries the start time, so a container
		// that runs faster than the watch delivers updates is not missed.
		if status.State.Running != nil {
			startedAt = status.State.Running.StartedAt.Time
		} else if status.State.Terminated != nil {
			startedAt = status.State.Terminated.StartedAt.Time
		}
		break
	}
	if startedAt.IsZero() {
		if !t.reported[SetupEventContainerStart] && taskInitFinished(pod) {
			t.start(ctx, SetupEventContainerStart, t.taskStartAnchor(pod))
		}
		return
	}
	t.report(ctx, SetupEventContainerStart, t.taskStartAnchor(pod), startedAt, false)
	if t.execution == nil {
		_, t.execution = tracing.Tracer(ctx).Start(ctx, "self_hosted_worker.execution",
			trace.WithTimestamp(startedAt), trace.WithAttributes(attribute.String("phase", "execution")))
	}
}

// taskStartAnchor returns the moment the task container start effectively
// began: the last init container finish when init containers ran, the
// scheduling time otherwise, and the pod creation time as a final fallback.
// Native sidecar init containers (restartPolicy Always) never terminate and
// therefore never move the anchor.
func (t *kubernetesSetupPhaseTracker) taskStartAnchor(pod *corev1.Pod) time.Time {
	var anchor time.Time
	for _, status := range pod.Status.InitContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.FinishedAt.After(anchor) {
			anchor = status.State.Terminated.FinishedAt.Time
		}
	}
	if !anchor.IsZero() {
		return anchor
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
			return condition.LastTransitionTime.Time
		}
	}
	return pod.CreationTimestamp.Time
}
