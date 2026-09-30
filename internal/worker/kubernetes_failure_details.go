package worker

import (
	"context"
	"errors"
	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func (b *KubernetesBackend) kubernetesFailureDetails(
	ctx context.Context,
	source kubernetesFailureObservationSource,
	job *batchv1.Job,
	pod *corev1.Pod,
	container *types.KubernetesContainerFailureDetails,
) *types.FailureDetails {
	events := b.collectKubernetesFailureEvents(ctx, job, pod)
	return buildKubernetesFailureDetails(source, job, pod, container, events)
}

func buildKubernetesFailureDetails(
	source kubernetesFailureObservationSource,
	job *batchv1.Job,
	pod *corev1.Pod,
	container *types.KubernetesContainerFailureDetails,
	events []corev1.Event,
) *types.FailureDetails {
	kubernetes := &types.KubernetesFailureDetails{
		SchemaVersion:     kubernetesFailureDetailsSchemaVersion,
		ObservationSource: string(source),
		ObservedAt:        time.Now().UTC(),
	}
	if container != nil {
		var truncated bool
		kubernetes.Container, truncated = projectKubernetesContainer(container)
		kubernetes.Truncated = truncated
	}

	if job != nil {
		var truncated bool
		kubernetes.Job, truncated = projectKubernetesJob(job)
		kubernetes.Truncated = kubernetes.Truncated || truncated
	}
	if pod != nil {
		var truncated bool
		kubernetes.Pod, truncated = projectKubernetesPod(pod)
		kubernetes.Truncated = kubernetes.Truncated || truncated
	}

	var truncated bool
	kubernetes.Events, truncated = projectKubernetesEvents(events)
	kubernetes.Truncated = kubernetes.Truncated || truncated

	return &types.FailureDetails{Kubernetes: kubernetes}
}

func (b *KubernetesBackend) collectKubernetesFailureEvents(ctx context.Context, job *batchv1.Job, pod *corev1.Pod) []corev1.Event {
	ctx, cancel := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
	defer cancel()
	type objectReference struct {
		kind string
		name string
		uid  k8stypes.UID
	}

	references := make([]objectReference, 0, 2)
	if pod != nil && pod.UID != "" {
		references = append(references, objectReference{kind: "Pod", name: pod.Name, uid: pod.UID})
	}
	if job != nil && job.UID != "" && (pod == nil || job.UID != pod.UID) {
		references = append(references, objectReference{kind: "Job", name: job.Name, uid: job.UID})
	}

	var events []corev1.Event
	for _, reference := range references {
		eventList, err := b.clientset.CoreV1().Events(b.config.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.uid=" + string(reference.uid),
		})
		if err != nil {
			log.Warnf(ctx, "Failed to list events for %s %s: %v", reference.kind, reference.name, err)
			continue
		}
		events = append(events, eventList.Items...)
	}
	return events
}

func (b *KubernetesBackend) refreshFailureJobDetails(ctx context.Context, err error, jobName string) {
	ctx, cancel := context.WithTimeout(ctx, kubernetesAPIRequestTimeout)
	defer cancel()
	job, getErr := b.clientset.BatchV1().Jobs(b.config.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if getErr != nil {
		log.Warnf(ctx, "Failed to refresh Job %s for failure details: %v", jobName, getErr)
		return
	}

	var failure *TaskFailure
	if !errors.As(err, &failure) || failure.failureDetails == nil || failure.failureDetails.Kubernetes == nil {
		return
	}

	jobDetails, truncated := projectKubernetesJob(job)
	failure.failureDetails.Kubernetes.Job = jobDetails
	failure.failureDetails.Kubernetes.Truncated = failure.failureDetails.Kubernetes.Truncated || truncated
}

func projectKubernetesJob(job *batchv1.Job) (*types.KubernetesJobFailureDetails, bool) {
	details := &types.KubernetesJobFailureDetails{
		Name: job.Name,
		UID:  string(job.UID),
	}

	var truncated bool
	for _, condition := range job.Status.Conditions {
		if condition.Type != batchv1.JobComplete && condition.Type != batchv1.JobFailed && condition.Type != batchv1.JobConditionType("FailureTarget") {
			continue
		}
		if len(details.Conditions) == maxKubernetesFailureConditions {
			truncated = true
			break
		}
		projected, conditionTruncated := projectKubernetesCondition(
			string(condition.Type),
			string(condition.Status),
			condition.Reason,
			condition.LastTransitionTime,
		)
		details.Conditions = append(details.Conditions, projected)
		truncated = truncated || conditionTruncated
	}
	return details, truncated
}

func projectKubernetesPod(pod *corev1.Pod) (*types.KubernetesPodFailureDetails, bool) {
	reason, truncated := boundedKubernetesString(pod.Status.Reason)
	details := &types.KubernetesPodFailureDetails{
		Name:   pod.Name,
		UID:    string(pod.UID),
		Phase:  string(pod.Status.Phase),
		Reason: reason,
	}
	if pod.DeletionTimestamp != nil {
		deletionTimestamp := pod.DeletionTimestamp.Time
		details.DeletionTimestamp = &deletionTimestamp
	}

	for _, condition := range pod.Status.Conditions {
		if len(details.Conditions) == maxKubernetesFailureConditions {
			truncated = true
			break
		}
		projected, conditionTruncated := projectKubernetesCondition(
			string(condition.Type),
			string(condition.Status),
			condition.Reason,
			condition.LastTransitionTime,
		)
		details.Conditions = append(details.Conditions, projected)
		truncated = truncated || conditionTruncated
	}
	return details, truncated
}

func projectKubernetesCondition(conditionType, status, reason string, transitionTime metav1.Time) (types.KubernetesConditionDetails, bool) {
	reason, truncated := boundedKubernetesString(reason)
	details := types.KubernetesConditionDetails{
		Type:   conditionType,
		Status: status,
		Reason: reason,
	}
	if !transitionTime.IsZero() {
		value := transitionTime.Time
		details.LastTransitionTime = &value
	}
	return details, truncated
}

func terminatedContainerFailureDetails(kind, name string, terminated *corev1.ContainerStateTerminated) *types.KubernetesContainerFailureDetails {
	rawExitCode := terminated.ExitCode
	normalizedExitCode := terminatedExitCode(terminated)
	details := &types.KubernetesContainerFailureDetails{
		Kind:               kind,
		Name:               name,
		State:              "terminated",
		TerminationReason:  terminated.Reason,
		RawExitCode:        &rawExitCode,
		NormalizedExitCode: &normalizedExitCode,
	}
	if terminated.Signal > 0 {
		signal := terminated.Signal
		details.Signal = &signal
	}
	if !terminated.StartedAt.IsZero() {
		startedAt := terminated.StartedAt.Time
		details.StartedAt = &startedAt
	}
	if !terminated.FinishedAt.IsZero() {
		finishedAt := terminated.FinishedAt.Time
		details.FinishedAt = &finishedAt
	}
	return details
}

func waitingContainerFailureDetails(kind, name string, waiting *corev1.ContainerStateWaiting) *types.KubernetesContainerFailureDetails {
	return &types.KubernetesContainerFailureDetails{
		Kind:          kind,
		Name:          name,
		State:         "waiting",
		WaitingReason: waiting.Reason,
	}
}

func projectKubernetesContainer(container *types.KubernetesContainerFailureDetails) (*types.KubernetesContainerFailureDetails, bool) {
	details := *container
	var waitingTruncated, terminationTruncated bool
	details.WaitingReason, waitingTruncated = boundedKubernetesString(container.WaitingReason)
	details.TerminationReason, terminationTruncated = boundedKubernetesString(container.TerminationReason)
	return &details, waitingTruncated || terminationTruncated
}

func projectKubernetesEvents(events []corev1.Event) ([]types.KubernetesEventDetails, bool) {
	filtered := make([]corev1.Event, 0, len(events))
	for _, event := range events {
		if event.Type == corev1.EventTypeWarning || event.Reason == "Killing" {
			filtered = append(filtered, event)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return kubernetesEventLastObserved(filtered[i]).After(kubernetesEventLastObserved(filtered[j]))
	})

	truncated := len(filtered) > maxKubernetesFailureEvents
	if truncated {
		filtered = filtered[:maxKubernetesFailureEvents]
	}

	result := make([]types.KubernetesEventDetails, 0, len(filtered))
	for _, event := range filtered {
		eventType, typeTruncated := boundedKubernetesString(event.Type)
		reason, reasonTruncated := boundedKubernetesString(event.Reason)
		count := event.Count
		if event.Series != nil {
			count = event.Series.Count
		}
		details := types.KubernetesEventDetails{
			Type:   eventType,
			Reason: reason,
			Count:  count,
		}
		if firstObservedAt := kubernetesEventFirstObserved(event); !firstObservedAt.IsZero() {
			details.FirstObservedAt = &firstObservedAt
		}
		if lastObservedAt := kubernetesEventLastObserved(event); !lastObservedAt.IsZero() {
			details.LastObservedAt = &lastObservedAt
		}
		result = append(result, details)
		truncated = truncated || typeTruncated || reasonTruncated
	}
	return result, truncated
}

func kubernetesEventFirstObserved(event corev1.Event) time.Time {
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}

func kubernetesEventLastObserved(event corev1.Event) time.Time {
	if event.Series != nil && !event.Series.LastObservedTime.IsZero() {
		return event.Series.LastObservedTime.Time
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}

func boundedKubernetesString(value string) (string, bool) {
	value = string([]rune(value))
	if len(value) <= maxKubernetesFailureReasonBytes {
		return value, false
	}

	value = value[:maxKubernetesFailureReasonBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}
