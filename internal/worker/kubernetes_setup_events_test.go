package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// capturedSetupEvents collects client events received by a stub server.
// reportPhase sends asynchronously, so readers must poll via waitForEvents.
type capturedSetupEvents struct {
	mu     sync.Mutex
	events map[string]clientEventRequest
}

func (c *capturedSetupEvents) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body clientEventRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			c.mu.Lock()
			c.events[body.EventName] = body
			c.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c *capturedSetupEvents) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (c *capturedSetupEvents) get(name string) (clientEventRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	event, ok := c.events[name]
	return event, ok
}

func (c *capturedSetupEvents) waitForEvents(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events, got %d", want, c.count())
}

func newTrackerForTest(t *testing.T) (*kubernetesSetupPhaseTracker, *capturedSetupEvents) {
	t.Helper()
	captured := &capturedSetupEvents{events: make(map[string]clientEventRequest)}
	server := httptest.NewServer(captured.handler())
	t.Cleanup(server.Close)

	reporter := newSetupEventReporter(server.URL, &types.TaskAssignmentMessage{
		TaskID:  "task-123",
		EnvVars: map[string]string{warpAPIKeyEnv: "api-key"},
	})
	return newKubernetesSetupPhaseTracker(reporter), captured
}

func terminatedInitStatus(name string, started, finished time.Time, exitCode int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				StartedAt:  metav1.NewTime(started),
				FinishedAt: metav1.NewTime(finished),
				ExitCode:   exitCode,
			},
		},
	}
}

func TestKubernetesSetupPhaseTrackerReportsPhasesOnce(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: kubernetesSidecarInitPrefix + "0"},
				{Name: kubernetesSetupContainerName},
			},
			Containers: []corev1.Container{{Name: kubernetesTaskContainerName}},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(base.Add(5 * time.Second)),
				},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				terminatedInitStatus(kubernetesSidecarInitPrefix+"0", base.Add(6*time.Second), base.Add(18*time.Second), 0),
				terminatedInitStatus(kubernetesSetupContainerName, base.Add(18*time.Second), base.Add(25*time.Second), 0),
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: kubernetesTaskContainerName,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base.Add(30 * time.Second))},
					},
				},
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 6)

	wantLatencies := map[string]float64{
		SetupEventPodSchedule:         5000,  // creation -> scheduled
		setupEventSidecarImageWait(0): 1000,  // scheduled -> sidecar init start
		setupEventSidecarCopy(0):      12000, // sidecar init start -> finish
		SetupEventSidecarPrep:         12000, // sidecar init start -> finish
		SetupEventSetupCommand:        7000,  // setup init start -> finish
		SetupEventContainerStart:      5000,  // last init finish -> task running
	}
	for name, wantLatency := range wantLatencies {
		event, ok := captured.get(name)
		if !ok {
			t.Fatalf("missing event %s", name)
		}
		if event.Payload.LatencyMS != wantLatency {
			t.Errorf("%s latency_ms = %v, want %v", name, event.Payload.LatencyMS, wantLatency)
		}
		if event.Payload.IsError {
			t.Errorf("%s is_error = true, want false", name)
		}
	}

	// A repeated observation must not report the phases again.
	tracker.observePod(context.Background(), pod)
	time.Sleep(50 * time.Millisecond)
	if captured.count() != 6 {
		t.Errorf("event count after repeat observation = %d, want 6", captured.count())
	}
}

func TestKubernetesSetupPhaseTrackerReportsPerSidecarPhases(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: kubernetesSidecarInitPrefix + "0"},
				{Name: kubernetesSidecarInitPrefix + "1"},
				{Name: kubernetesSidecarInitPrefix + "2"},
			},
			Containers: []corev1.Container{{Name: kubernetesTaskContainerName}},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(base.Add(5 * time.Second)),
				},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{
					Name: kubernetesSidecarInitPrefix + "0",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base.Add(6 * time.Second))},
					},
				},
			},
		},
	}

	// A running copy sidecar already fixes the image wait; the copy is still open.
	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 2)
	if event, ok := captured.get(setupEventSidecarImageWait(0)); !ok || event.Payload.LatencyMS != 1000 {
		t.Fatalf("sidecar 0 image wait while running = %+v, %v; want 1000ms", event.Payload, ok)
	}
	if _, ok := captured.get(setupEventSidecarCopy(0)); ok {
		t.Fatal("copy reported for a sidecar that is still running")
	}

	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		terminatedInitStatus(kubernetesSidecarInitPrefix+"0", base.Add(6*time.Second), base.Add(8*time.Second), 0),
		terminatedInitStatus(kubernetesSidecarInitPrefix+"1", base.Add(14*time.Second), base.Add(16*time.Second), 0),
		terminatedInitStatus(kubernetesSidecarInitPrefix+"2", base.Add(22*time.Second), base.Add(24*time.Second), 0),
	}
	tracker.observePod(context.Background(), pod)
	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 8)

	wantLatencies := map[string]float64{
		SetupEventPodSchedule:         5000,
		setupEventSidecarImageWait(0): 1000,  // scheduled -> sidecar 0 start
		setupEventSidecarCopy(0):      2000,  // sidecar 0 start -> finish
		setupEventSidecarImageWait(1): 6000,  // sidecar 0 finish -> sidecar 1 start
		setupEventSidecarCopy(1):      2000,  // sidecar 1 start -> finish
		setupEventSidecarImageWait(2): 6000,  // sidecar 1 finish -> sidecar 2 start
		setupEventSidecarCopy(2):      2000,  // sidecar 2 start -> finish
		SetupEventSidecarPrep:         18000, // first sidecar start -> last finish
	}
	for name, wantLatency := range wantLatencies {
		event, ok := captured.get(name)
		if !ok {
			t.Fatalf("missing event %s", name)
		}
		if event.Payload.LatencyMS != wantLatency {
			t.Errorf("%s latency_ms = %v, want %v", name, event.Payload.LatencyMS, wantLatency)
		}
		if event.Payload.IsError {
			t.Errorf("%s is_error = true, want false", name)
		}
	}

	tracker.observePod(context.Background(), pod)
	time.Sleep(50 * time.Millisecond)
	if captured.count() != len(wantLatencies) {
		t.Errorf("event count after repeat observation = %d, want %d", captured.count(), len(wantLatencies))
	}
}

func TestKubernetesSetupPhaseTrackerPerSidecarPhasesFollowInitOrder(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	// Sidecar 0 is an image volume (no init container), so copy-sidecar-1 is the
	// first init container and its image wait anchors on PodScheduled. The setup
	// container is not a sidecar and gets no per-sidecar events.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: kubernetesSidecarInitPrefix + "1"},
				{Name: kubernetesSetupContainerName},
			},
			Containers: []corev1.Container{{Name: kubernetesTaskContainerName}},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(base.Add(2 * time.Second)),
				},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				terminatedInitStatus(kubernetesSidecarInitPrefix+"1", base.Add(9*time.Second), base.Add(11*time.Second), 0),
				terminatedInitStatus(kubernetesSetupContainerName, base.Add(15*time.Second), base.Add(20*time.Second), 0),
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 5)

	wantLatencies := map[string]float64{
		SetupEventPodSchedule:         2000,
		setupEventSidecarImageWait(1): 7000, // scheduled -> sidecar 1 start
		setupEventSidecarCopy(1):      2000,
		SetupEventSidecarPrep:         2000,
		SetupEventSetupCommand:        5000,
	}
	for name, wantLatency := range wantLatencies {
		event, ok := captured.get(name)
		if !ok {
			t.Fatalf("missing event %s", name)
		}
		if event.Payload.LatencyMS != wantLatency {
			t.Errorf("%s latency_ms = %v, want %v", name, event.Payload.LatencyMS, wantLatency)
		}
	}
	for _, name := range []string{setupEventSidecarImageWait(0), setupEventSidecarCopy(0)} {
		if _, ok := captured.get(name); ok {
			t.Errorf("unexpected event %s for a sidecar without a copy init container", name)
		}
	}
	if captured.count() != len(wantLatencies) {
		t.Errorf("event count = %d, want %d", captured.count(), len(wantLatencies))
	}
}

func TestKubernetesSetupPhaseTrackerImageVolumesMode(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	// Image-volumes mode: no init containers at all.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: kubernetesTaskContainerName}},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(base.Add(2 * time.Second)),
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: kubernetesTaskContainerName,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base.Add(20 * time.Second))},
					},
				},
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 2)

	if _, ok := captured.get(SetupEventSidecarPrep); ok {
		t.Error("unexpected sidecar prep event without copy init containers")
	}
	event, ok := captured.get(SetupEventContainerStart)
	if !ok {
		t.Fatal("missing container start event")
	}
	// Anchored to the scheduling time when no init containers ran.
	if event.Payload.LatencyMS != 18000 {
		t.Errorf("container start latency_ms = %v, want 18000", event.Payload.LatencyMS)
	}
}

func TestKubernetesSetupPhaseTrackerReportsSidecarFailure(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	// Two sidecar inits expected, the first one failed: report early with is_error.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: kubernetesSidecarInitPrefix + "0"},
				{Name: kubernetesSidecarInitPrefix + "1"},
			},
			Containers: []corev1.Container{{Name: kubernetesTaskContainerName}},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				terminatedInitStatus(kubernetesSidecarInitPrefix+"0", base.Add(time.Second), base.Add(4*time.Second), 1),
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 2)

	for _, name := range []string{SetupEventSidecarPrep, setupEventSidecarCopy(0)} {
		event, ok := captured.get(name)
		if !ok {
			t.Fatalf("missing event %s", name)
		}
		if !event.Payload.IsError {
			t.Errorf("%s is_error = false, want true", name)
		}
		if event.Payload.LatencyMS != 3000 {
			t.Errorf("%s latency_ms = %v, want 3000", name, event.Payload.LatencyMS)
		}
	}
	// Without a PodScheduled time there is no anchor for the image wait.
	if _, ok := captured.get(setupEventSidecarImageWait(0)); ok {
		t.Error("unexpected image wait event without a scheduling time")
	}
}

func TestKubernetesSetupPhaseTrackerWaitsForAllSidecars(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: kubernetesSidecarInitPrefix + "0"},
				{Name: kubernetesSidecarInitPrefix + "1"},
			},
			Containers: []corev1.Container{{Name: kubernetesTaskContainerName}},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				terminatedInitStatus(kubernetesSidecarInitPrefix+"0", base.Add(time.Second), base.Add(4*time.Second), 0),
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 1)
	if _, ok := captured.get(SetupEventSidecarPrep); ok {
		t.Fatal("aggregate sidecar prep reported with one of two sidecars terminated")
	}
	if _, ok := captured.get(setupEventSidecarCopy(0)); !ok {
		t.Fatal("missing per-sidecar copy event for the terminated sidecar")
	}

	pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses,
		terminatedInitStatus(kubernetesSidecarInitPrefix+"1", base.Add(6*time.Second), base.Add(9*time.Second), 0))
	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 4)

	wantLatencies := map[string]float64{
		SetupEventSidecarPrep:         8000, // sidecar 0 start -> sidecar 1 finish
		setupEventSidecarCopy(0):      3000,
		setupEventSidecarImageWait(1): 2000, // sidecar 0 finish -> sidecar 1 start
		setupEventSidecarCopy(1):      3000,
	}
	for name, wantLatency := range wantLatencies {
		event, ok := captured.get(name)
		if !ok {
			t.Fatalf("missing event %s", name)
		}
		if event.Payload.LatencyMS != wantLatency {
			t.Errorf("%s latency_ms = %v, want %v", name, event.Payload.LatencyMS, wantLatency)
		}
	}
}

func TestKubernetesSetupPhaseTrackerNilSafe(t *testing.T) {
	// A tracker with a nil reporter (reporting disabled) must be a no-op.
	tracker := newKubernetesSetupPhaseTracker(nil)
	tracker.observePod(context.Background(), &corev1.Pod{})

	var nilTracker *kubernetesSetupPhaseTracker
	nilTracker.observePod(context.Background(), &corev1.Pod{})
}

func TestKubernetesInitPreparationAndExecution(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	ctx, session, captured := newTaskTraceForTest(t, base)
	tracker := newKubernetesSetupPhaseTracker(nil)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec:       corev1.PodSpec{InitContainers: []corev1.Container{{Name: "verify"}}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(base.Add(2 * time.Second)),
		}}},
	}
	tracker.observePod(ctx, pod)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "verify", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base.Add(8 * time.Second))}},
	}}
	tracker.observePod(ctx, pod)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		terminatedInitStatus("verify", base.Add(8*time.Second), base.Add(20*time.Second), 0),
	}
	tracker.observePod(ctx, pod)
	tracker.observePod(ctx, pod)
	tracker.finish(ctx, nil)
	session.End("succeeded", nil)
	if err := session.ForceFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var root *tracepb.Span
	intervals := make(map[string]*tracepb.Span)
	for _, span := range captured() {
		if span.Name == "self_hosted_worker.task" {
			root = span
		}
		if span.Name == "self_hosted_worker.image_prepare" || span.Name == "self_hosted_worker.init_container" {
			if intervals[span.Name] != nil {
				t.Fatalf("duplicate init interval %s", span.Name)
			}
			intervals[span.Name] = span
		}
	}
	if root == nil || len(intervals) != 2 {
		t.Fatalf("missing root or init intervals: %v", intervals)
	}
	preparation, execution := intervals["self_hosted_worker.image_prepare"], intervals["self_hosted_worker.init_container"]
	if preparation.StartTimeUnixNano != uint64(base.Add(2*time.Second).UnixNano()) ||
		preparation.EndTimeUnixNano != uint64(base.Add(8*time.Second).UnixNano()) ||
		execution.StartTimeUnixNano != preparation.EndTimeUnixNano ||
		execution.EndTimeUnixNano != uint64(base.Add(20*time.Second).UnixNano()) {
		t.Fatal("init preparation and execution did not retain pod timestamp boundaries")
	}
	for _, span := range intervals {
		if !bytes.Equal(span.ParentSpanId, root.SpanId) {
			t.Fatal("init interval is not a task-root child")
		}
		found := false
		for _, attr := range span.Attributes {
			found = found || attr.Key == "k8s.container.name" && attr.Value.GetStringValue() == "verify"
		}
		if !found {
			t.Fatal("init interval does not identify its container")
		}
	}
}

func TestKubernetesSidecarCopyTraceIntervals(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	ctx, session, captured := newTaskTraceForTest(t, base)
	tracker := newKubernetesSetupPhaseTracker(nil)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{
			{Name: kubernetesSidecarInitPrefix + "0"},
			{Name: kubernetesSidecarInitPrefix + "1"},
		}},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(base.Add(2 * time.Second)),
			}},
			InitContainerStatuses: []corev1.ContainerStatus{
				terminatedInitStatus(kubernetesSidecarInitPrefix+"0", base.Add(4*time.Second), base.Add(6*time.Second), 0),
				terminatedInitStatus(kubernetesSidecarInitPrefix+"1", base.Add(12*time.Second), base.Add(14*time.Second), 0),
			},
		},
	}
	tracker.observePod(ctx, pod)
	tracker.finish(ctx, nil)
	session.End("succeeded", nil)
	if err := session.ForceFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	containerOf := func(span *tracepb.Span) string {
		for _, attr := range span.Attributes {
			if attr.Key == "k8s.container.name" {
				return attr.Value.GetStringValue()
			}
		}
		return ""
	}
	copies := make(map[string]*tracepb.Span)
	for _, span := range captured() {
		switch span.Name {
		case "self_hosted_worker.sidecar_copy":
			copies[containerOf(span)] = span
		case "self_hosted_worker.init_container":
			t.Fatalf("copy sidecar %s traced as a generic init container", containerOf(span))
		}
	}
	first, second := copies[kubernetesSidecarInitPrefix+"0"], copies[kubernetesSidecarInitPrefix+"1"]
	if first == nil || second == nil {
		t.Fatalf("missing sidecar copy intervals: %v", copies)
	}
	if first.StartTimeUnixNano != uint64(base.Add(4*time.Second).UnixNano()) ||
		first.EndTimeUnixNano != uint64(base.Add(6*time.Second).UnixNano()) ||
		second.StartTimeUnixNano != uint64(base.Add(12*time.Second).UnixNano()) ||
		second.EndTimeUnixNano != uint64(base.Add(14*time.Second).UnixNano()) {
		t.Fatal("sidecar copy intervals did not retain pod timestamp boundaries")
	}
}
