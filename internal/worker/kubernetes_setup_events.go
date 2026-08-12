package worker

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	kubernetesTaskContainerName  = "task"
	kubernetesSetupContainerName = "setup"
	kubernetesSidecarInitPrefix  = "copy-sidecar-"
)

// kubernetesSetupPhaseTracker derives task setup phase durations from pod
// status snapshots and reports each phase once. The kubelet performs the
// image pulls and container starts for this backend, so the worker cannot
// time them directly; instead, every phase is computed from timestamps the
// pod object carries (creation time, the PodScheduled condition transition,
// init container terminations, and the task container start). Late or
// repeated watch deliveries therefore do not skew the measurements.
type kubernetesSetupPhaseTracker struct {
	reporter *setupEventReporter
	reported map[string]bool
}

func newKubernetesSetupPhaseTracker(reporter *setupEventReporter) *kubernetesSetupPhaseTracker {
	return &kubernetesSetupPhaseTracker{
		reporter: reporter,
		reported: make(map[string]bool),
	}
}

// observePod inspects a pod status snapshot and reports any newly completed
// setup phases. It is safe to call repeatedly with successive snapshots and
// is a no-op when reporting is disabled.
func (t *kubernetesSetupPhaseTracker) observePod(ctx context.Context, pod *corev1.Pod) {
	if t == nil || t.reporter == nil || pod == nil {
		return
	}
	t.observeSchedule(ctx, pod)
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
	t.reported[eventName] = true
	t.reporter.reportPhase(ctx, eventName, start, finish, isError)
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
}

// observeSidecarPrep reports one aggregate phase spanning the sequential
// copy-sidecar-* init containers, from the first start to the last finish.
// The kubelet's pull of each sidecar image is included in this span. The
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
		if !strings.HasPrefix(status.Name, kubernetesSidecarInitPrefix) || status.State.Terminated == nil {
			continue
		}
		terminated++
		term := status.State.Terminated
		if start.IsZero() || term.StartedAt.Time.Before(start) {
			start = term.StartedAt.Time
		}
		if term.FinishedAt.Time.After(finish) {
			finish = term.FinishedAt.Time
		}
		if term.ExitCode != 0 {
			failed = true
		}
	}
	if terminated == 0 {
		return
	}
	if failed || terminated == expected {
		t.report(ctx, SetupEventSidecarPrep, start, finish, failed)
	}
}

// observeSetupCommand reports the operator-configured setup init container's
// run time once it terminates.
func (t *kubernetesSetupPhaseTracker) observeSetupCommand(ctx context.Context, pod *corev1.Pod) {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != kubernetesSetupContainerName || status.State.Terminated == nil {
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
		return
	}
	t.report(ctx, SetupEventContainerStart, t.taskStartAnchor(pod), startedAt, false)
}

// taskStartAnchor returns the moment the task container start effectively
// began: the last init container finish when init containers ran, the
// scheduling time otherwise, and the pod creation time as a final fallback.
// Native sidecar init containers (restartPolicy Always) never terminate and
// therefore never move the anchor.
func (t *kubernetesSetupPhaseTracker) taskStartAnchor(pod *corev1.Pod) time.Time {
	var anchor time.Time
	for _, status := range pod.Status.InitContainerStatuses {
		if status.State.Terminated != nil && status.State.Terminated.FinishedAt.Time.After(anchor) {
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
