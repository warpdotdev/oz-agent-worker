package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
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
				{Name: "copy-sidecar-0"},
				{Name: "setup"},
			},
			Containers: []corev1.Container{{Name: "task"}},
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
				terminatedInitStatus("copy-sidecar-0", base.Add(6*time.Second), base.Add(18*time.Second), 0),
				terminatedInitStatus("setup", base.Add(18*time.Second), base.Add(25*time.Second), 0),
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "task",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(base.Add(30 * time.Second))},
					},
				},
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 4)

	wantLatencies := map[string]float64{
		SetupEventPodSchedule:    5000,  // creation -> scheduled
		SetupEventSidecarPrep:    12000, // sidecar init start -> finish
		SetupEventSetupCommand:   7000,  // setup init start -> finish
		SetupEventContainerStart: 5000,  // last init finish -> task running
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
	if captured.count() != 4 {
		t.Errorf("event count after repeat observation = %d, want 4", captured.count())
	}
}

func TestKubernetesSetupPhaseTrackerImageVolumesMode(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	// Image-volumes mode: no init containers at all.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "task"}},
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
					Name: "task",
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
				{Name: "copy-sidecar-0"},
				{Name: "copy-sidecar-1"},
			},
			Containers: []corev1.Container{{Name: "task"}},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				terminatedInitStatus("copy-sidecar-0", base.Add(time.Second), base.Add(4*time.Second), 1),
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 1)

	event, ok := captured.get(SetupEventSidecarPrep)
	if !ok {
		t.Fatal("missing sidecar prep event")
	}
	if !event.Payload.IsError {
		t.Error("is_error = false, want true")
	}
	if event.Payload.LatencyMS != 3000 {
		t.Errorf("latency_ms = %v, want 3000", event.Payload.LatencyMS)
	}
}

func TestKubernetesSetupPhaseTrackerWaitsForAllSidecars(t *testing.T) {
	tracker, captured := newTrackerForTest(t)
	base := time.Date(2026, 8, 12, 15, 35, 0, 0, time.UTC)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "copy-sidecar-0"},
				{Name: "copy-sidecar-1"},
			},
			Containers: []corev1.Container{{Name: "task"}},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				terminatedInitStatus("copy-sidecar-0", base.Add(time.Second), base.Add(4*time.Second), 0),
			},
		},
	}

	tracker.observePod(context.Background(), pod)
	time.Sleep(50 * time.Millisecond)
	if captured.count() != 0 {
		t.Fatalf("event count with one of two sidecars terminated = %d, want 0", captured.count())
	}

	pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses,
		terminatedInitStatus("copy-sidecar-1", base.Add(4*time.Second), base.Add(9*time.Second), 0))
	tracker.observePod(context.Background(), pod)
	captured.waitForEvents(t, 1)

	event, ok := captured.get(SetupEventSidecarPrep)
	if !ok {
		t.Fatal("missing sidecar prep event")
	}
	if event.Payload.LatencyMS != 8000 {
		t.Errorf("latency_ms = %v, want 8000", event.Payload.LatencyMS)
	}
}

func TestKubernetesSetupPhaseTrackerNilSafe(t *testing.T) {
	// A tracker with a nil reporter (reporting disabled) must be a no-op.
	tracker := newKubernetesSetupPhaseTracker(nil)
	tracker.observePod(context.Background(), &corev1.Pod{})

	var nilTracker *kubernetesSetupPhaseTracker
	nilTracker.observePod(context.Background(), &corev1.Pod{})
}
