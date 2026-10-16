package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/tasktelemetry/tracing"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestTaskTraceBoundedShutdown(t *testing.T) {
	var mu sync.Mutex
	var spans []*tracepb.Span
	operatorCollector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		var payload collector.ExportTraceServiceRequest
		if err := proto.Unmarshal(data, &payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, resource := range payload.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				spans = append(spans, scope.Spans...)
			}
		}
	}))
	defer operatorCollector.Close()
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", operatorCollector.URL+"/v1/traces")
	previous := otel.GetTracerProvider()
	shutdown, err := tracing.InitOperator(t.Context(), resource.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer otel.SetTracerProvider(previous)
	defer func() { _ = shutdown(t.Context()) }()

	release := make(chan struct{})
	blockedCollector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-release
	}))
	defer blockedCollector.Close()
	defer close(release)
	client := fake.NewSimpleClientset()
	podWatches := make(chan *watch.FakeWatcher, 2)
	client.PrependWatchReactor("jobs", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, watch.NewFake(), nil
	})
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		watcher := watch.NewFake()
		podWatches <- watcher
		return true, watcher, nil
	})
	backend := &KubernetesBackend{clientset: client, config: KubernetesBackendConfig{Namespace: "agents", SetupCommand: "setup"}}
	w := &Worker{
		ctx: t.Context(), backend: backend, config: Config{BackendType: "kubernetes"},
		activeTasks: make(map[taskExecution]activeTask), outbound: newOutboundQueue(8),
	}
	for _, run := range []string{"alpha", "beta"} {
		w.handleTaskAssignment(&types.TaskAssignmentMessage{
			TaskID: run, ExecutionID: "execution-" + run, Task: &types.Task{ID: run},
			TelemetryCollection: &types.TelemetryCollectionConfig{
				Endpoint: blockedCollector.URL, TracingEnabled: true,
				BootstrapToken: &types.TelemetryBootstrapToken{Token: "token-" + run, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano)},
			},
		}, time.Now())
		select {
		case watcher := <-podWatches:
			now := time.Now()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: run, CreationTimestamp: metav1.NewTime(now.Add(-time.Second))},
				Spec:       corev1.PodSpec{InitContainers: []corev1.Container{{Name: kubernetesSetupContainerName}}},
				Status: corev1.PodStatus{
					Conditions:            []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}},
					InitContainerStatuses: []corev1.ContainerStatus{{Name: kubernetesSetupContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now)}}}},
				},
			}
			watcher.Add(pod)
			watcher.Modify(pod)
		case <-time.After(time.Second):
			t.Fatal("task did not reach the Kubernetes pod watch")
		}
	}
	start := time.Now()
	w.shutdownTasks()
	if time.Since(start) > 6*time.Second {
		t.Fatal("shutdown used a separate timeout per task")
	}
	w.tasksMutex.Lock()
	var cancels []context.CancelFunc
	for _, task := range w.activeTasks {
		if task.ctx.Err() != nil {
			t.Error("trace interruption cancelled a preserved execution")
		}
		cancels = append(cancels, task.cancel)
	}
	w.tasksMutex.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	w.taskWG.Wait()
	backend.Shutdown(t.Context())
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "jobs" && (action.GetVerb() == "delete" || action.GetVerb() == "update") {
			t.Fatal("trace finalization stopped a preserved Job")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	roots, live := 0, 0
	for _, span := range spans {
		if span.Name == "self_hosted_worker.task" {
			roots++
		}
		if span.Name == "self_hosted_worker.setup" {
			live++
		}
		if span.Name == "self_hosted_worker.task" || span.Name == "self_hosted_worker.setup" {
			outcome := ""
			for _, attr := range span.Attributes {
				if attr.Key == "outcome" {
					outcome = attr.Value.GetStringValue()
				}
			}
			if outcome != "interrupted" {
				t.Error("remaining observation was not interrupted")
			}
		}
	}
	if roots != 2 || live != 2 {
		t.Fatalf("late finalizers lost or duplicated spans: roots=%d live=%d", roots, live)
	}
}
