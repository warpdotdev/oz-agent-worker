package metrics

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// withTestReader installs an SDK MeterProvider backed by a manual.Reader so
// tests can collect emitted metrics deterministically. It restores the
// previous instrument set when the test finishes.
func withTestReader(t *testing.T, cfg Config) *sdkmetric.ManualReader {
	t.Helper()

	prev := activeInstruments.Load()
	t.Cleanup(func() {
		activeInstruments.Store(prev)
	})

	reader := sdkmetric.NewManualReader()
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			attribute.String("worker.id", cfg.WorkerID),
			attribute.String("worker.backend", cfg.Backend),
		),
	)
	if err != nil {
		t.Fatalf("build resource: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	set, err := buildInstruments(provider.Meter(scopeName))
	if err != nil {
		t.Fatalf("build instruments: %v", err)
	}
	activeInstruments.Store(set)
	return reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not emitted; got %d scopes", name, len(rm.ScopeMetrics))
	return metricdata.Metrics{}
}

// TestHelpersSafeBeforeInit ensures the helpers do not panic when Init has not
// been called yet (i.e. when only the package init() noop instruments are
// installed). This guards the contract that worker code can call helpers
// unconditionally.
func TestHelpersSafeBeforeInit(t *testing.T) {
	// Don't install a test reader -- exercise the noop instruments.
	SetConnected(true)
	SetConnected(false)
	IncTasksActive()
	DecTasksActive()
	SetMaxConcurrent(4)
	RecordTaskClaim()
	RecordTaskRejected("at_capacity")
	RecordTaskCompleted(TaskResultSucceeded, 250*time.Millisecond)
	RecordTaskCompleted(TaskResultFailed, 1*time.Second)
	RecordWebsocketReconnect("dial_failed")
	SetWorkerInfo("v0.0.0", "docker", "test")
}

func TestRecordTaskCompletedEmitsCounterAndHistogram(t *testing.T) {
	reader := withTestReader(t, Config{WorkerID: "w1", Backend: "docker"})

	RecordTaskCompleted(TaskResultSucceeded, 750*time.Millisecond)
	RecordTaskCompleted(TaskResultSucceeded, 1500*time.Millisecond)
	RecordTaskCompleted(TaskResultFailed, 200*time.Millisecond)

	rm := collect(t, reader)

	completed := findMetric(t, rm, "oz_worker_tasks_completed_total")
	sum, ok := completed.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("expected Sum[int64], got %T", completed.Data)
	}
	bySuccess := map[string]int64{}
	for _, dp := range sum.DataPoints {
		v, _ := dp.Attributes.Value("result")
		bySuccess[v.AsString()] = dp.Value
	}
	if got := bySuccess["succeeded"]; got != 2 {
		t.Errorf("succeeded count = %d, want 2", got)
	}
	if got := bySuccess["failed"]; got != 1 {
		t.Errorf("failed count = %d, want 1", got)
	}

	hist := findMetric(t, rm, "oz_worker_task_duration_seconds")
	if _, ok := hist.Data.(metricdata.Histogram[float64]); !ok {
		t.Fatalf("expected Histogram[float64], got %T", hist.Data)
	}
}

func TestTasksActiveTracksUpDown(t *testing.T) {
	reader := withTestReader(t, Config{WorkerID: "w1", Backend: "docker"})

	IncTasksActive()
	IncTasksActive()
	IncTasksActive()
	DecTasksActive()

	rm := collect(t, reader)

	active := findMetric(t, rm, "oz_worker_tasks_active")
	sum, ok := active.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("expected Sum[int64] for UpDownCounter, got %T", active.Data)
	}
	if len(sum.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(sum.DataPoints))
	}
	if got := sum.DataPoints[0].Value; got != 2 {
		t.Errorf("active = %d, want 2", got)
	}
}

func TestSetConnectedEmitsGauge(t *testing.T) {
	reader := withTestReader(t, Config{WorkerID: "w1", Backend: "docker"})

	SetConnected(true)
	rm := collect(t, reader)

	connected := findMetric(t, rm, "oz_worker_connected")
	g, ok := connected.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("expected Gauge[int64], got %T", connected.Data)
	}
	if len(g.DataPoints) != 1 || g.DataPoints[0].Value != 1 {
		t.Errorf("connected gauge = %+v, want value 1", g.DataPoints)
	}

	SetConnected(false)
	rm = collect(t, reader)
	connected = findMetric(t, rm, "oz_worker_connected")
	g = connected.Data.(metricdata.Gauge[int64])
	if g.DataPoints[0].Value != 0 {
		t.Errorf("connected gauge after disconnect = %d, want 0", g.DataPoints[0].Value)
	}
}

func TestRecordTaskRejectedTagsReason(t *testing.T) {
	reader := withTestReader(t, Config{WorkerID: "w1", Backend: "docker"})

	RecordTaskRejected("at_capacity")
	RecordTaskRejected("at_capacity")
	RecordTaskRejected("not_ready")

	rm := collect(t, reader)
	rejected := findMetric(t, rm, "oz_worker_tasks_rejected_total")
	sum := rejected.Data.(metricdata.Sum[int64])

	byReason := map[string]int64{}
	for _, dp := range sum.DataPoints {
		v, _ := dp.Attributes.Value("reason")
		byReason[v.AsString()] = dp.Value
	}
	if got := byReason["at_capacity"]; got != 2 {
		t.Errorf("at_capacity count = %d, want 2", got)
	}
	if got := byReason["not_ready"]; got != 1 {
		t.Errorf("not_ready count = %d, want 1", got)
	}
}

func TestInitDisabledByDefault(t *testing.T) {
	t.Setenv("OTEL_METRICS_EXPORTER", "")
	shutdown, err := Init(context.Background(), Config{WorkerID: "w1", Backend: "docker"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("Init returned nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestInitNoneIsDisabled(t *testing.T) {
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	shutdown, err := Init(context.Background(), Config{WorkerID: "w1", Backend: "docker"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestNewResourceIncludesWorkerAttrs(t *testing.T) {
	res, err := newResource(context.Background(), Config{
		WorkerID: "alpha",
		Backend:  "kubernetes",
		Version:  "v1.2.3",
	})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	want := map[string]string{
		"service.name":    "oz-agent-worker",
		"service.version": "v1.2.3",
		"worker.id":       "alpha",
		"worker.backend":  "kubernetes",
	}
	for k, v := range want {
		got, ok := res.Set().Value(attribute.Key(k))
		if !ok {
			t.Errorf("resource missing attribute %q", k)
			continue
		}
		if got.AsString() != v {
			t.Errorf("resource %q = %q, want %q", k, got.AsString(), v)
		}
	}
}
