// Package metrics owns the OpenTelemetry metrics pipeline for the worker.
//
// The exporter is selected at runtime by the operator via the standard
// OpenTelemetry environment variables (OTEL_METRICS_EXPORTER and friends),
// using the go.opentelemetry.io/contrib/exporters/autoexport package. The
// supported values are:
//
//   - prometheus: starts an in-process HTTP server on
//     ${OTEL_EXPORTER_PROMETHEUS_HOST}:${OTEL_EXPORTER_PROMETHEUS_PORT}
//     (defaults to localhost:9464) serving the /metrics endpoint.
//   - otlp:       pushes metrics over OTLP (http/protobuf by default;
//     controlled by OTEL_EXPORTER_OTLP_PROTOCOL and the standard
//     OTLP endpoint environment variables).
//   - console:    writes metrics to stdout.
//   - none:       disables metrics export entirely.
//
// When OTEL_METRICS_EXPORTER is unset, autoexport defaults to OTLP. To make
// metrics fully opt-in we treat the unset case as "off" and only call into
// autoexport when the variable is set, so existing deployments are unaffected.
//
// All instruments are package-level singletons. Helpers are safe to call
// before Init runs or when metrics are disabled: they fall back to no-op
// instruments backed by the OpenTelemetry global no-op MeterProvider, so
// worker code can call the helpers unconditionally.
package metrics

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// scopeName is the instrumentation-scope name used for all worker metrics.
// Prometheus translates the scope into the otel_scope_name label, so making
// this stable is part of the public contract for downstream dashboards.
const scopeName = "github.com/warpdotdev/oz-agent-worker"

// Config is the input for Init.
type Config struct {
	// WorkerID is the operator-supplied identifier for this worker.
	WorkerID string
	// Backend is the resolved backend type ("docker", "direct", "kubernetes").
	Backend string
	// Version is the build version of the worker binary; may be empty.
	Version string
}

// instruments holds the full set of synchronous OTel instruments created from
// a single MeterProvider. Helpers read this struct atomically so that Init can
// hot-swap from the no-op set to the SDK-backed set without locking.
type instruments struct {
	connected          metric.Int64Gauge
	tasksActive        metric.Int64UpDownCounter
	tasksMaxConcurrent metric.Int64Gauge
	tasksClaimed       metric.Int64Counter
	tasksRejected      metric.Int64Counter
	tasksCompleted     metric.Int64Counter
	taskDuration       metric.Float64Histogram
	wsReconnects       metric.Int64Counter
	workerInfo         metric.Int64Gauge
}

// activeInstruments is the current instrument set. It always points to a
// valid (possibly no-op) instruments value so callers never need to nil-check.
var activeInstruments atomic.Pointer[instruments]

func init() {
	// Seed the package with no-op instruments backed by the OTel no-op
	// MeterProvider so helpers work even if Init is never called.
	noopMeter := metricnoop.NewMeterProvider().Meter(scopeName)
	noopSet, err := buildInstruments(noopMeter)
	if err != nil {
		// The no-op meter can't fail in practice; fall back to a zero-value
		// instruments struct rather than panicking from package init.
		noopSet = &instruments{}
	}
	activeInstruments.Store(noopSet)
}

// Init wires up the metrics pipeline based on the OTEL_METRICS_EXPORTER
// environment variable. When the variable is unset or set to "none", Init is
// a no-op and returns a no-op shutdown function. Otherwise it constructs an
// SDK MeterProvider with a worker-scoped resource and replaces the package
// instruments with SDK-backed versions.
//
// Init must only be called once. The returned shutdown function flushes and
// stops the exporter; it is safe to call after Init returns an error.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	exporter := os.Getenv("OTEL_METRICS_EXPORTER")
	switch exporter {
	case "", "none":
		// Metrics disabled: keep the no-op instruments installed by init().
		return noop, nil
	}

	reader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return noop, err
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		// We have a reader but failed to build a resource; close the reader
		// and surface the error.
		return noop, errors.Join(err, reader.Shutdown(ctx))
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	meter := provider.Meter(scopeName)
	set, err := buildInstruments(meter)
	if err != nil {
		return noop, errors.Join(err, provider.Shutdown(ctx))
	}
	activeInstruments.Store(set)

	return provider.Shutdown, nil
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName("oz-agent-worker"),
	}
	if cfg.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.Version))
	}
	if cfg.WorkerID != "" {
		attrs = append(attrs, attribute.String("worker.id", cfg.WorkerID))
	}
	if cfg.Backend != "" {
		attrs = append(attrs, attribute.String("worker.backend", cfg.Backend))
	}
	return resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
	)
}

func buildInstruments(m metric.Meter) (*instruments, error) {
	connected, err := m.Int64Gauge(
		"oz_worker_connected",
		metric.WithDescription("1 while the worker has an active WebSocket connection to warp-server, 0 otherwise."),
	)
	if err != nil {
		return nil, err
	}
	tasksActive, err := m.Int64UpDownCounter(
		"oz_worker_tasks_active",
		metric.WithDescription("Number of tasks the worker is currently executing."),
	)
	if err != nil {
		return nil, err
	}
	tasksMaxConcurrent, err := m.Int64Gauge(
		"oz_worker_tasks_max_concurrent",
		metric.WithDescription("Configured upper bound on concurrent tasks for this worker. 0 means unlimited."),
	)
	if err != nil {
		return nil, err
	}
	tasksClaimed, err := m.Int64Counter(
		"oz_worker_tasks_claimed_total",
		metric.WithDescription("Total tasks the worker has claimed since process start."),
	)
	if err != nil {
		return nil, err
	}
	tasksRejected, err := m.Int64Counter(
		"oz_worker_tasks_rejected_total",
		metric.WithDescription("Total tasks the worker has rejected since process start."),
	)
	if err != nil {
		return nil, err
	}
	tasksCompleted, err := m.Int64Counter(
		"oz_worker_tasks_completed_total",
		metric.WithDescription("Total tasks the worker has finished, labeled by terminal result."),
	)
	if err != nil {
		return nil, err
	}
	taskDuration, err := m.Float64Histogram(
		"oz_worker_task_duration_seconds",
		metric.WithDescription("Wall-clock duration of task execution on the worker, labeled by terminal result."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	wsReconnects, err := m.Int64Counter(
		"oz_worker_websocket_reconnects_total",
		metric.WithDescription("Total WebSocket reconnect attempts since process start."),
	)
	if err != nil {
		return nil, err
	}
	workerInfo, err := m.Int64Gauge(
		"oz_worker_info",
		metric.WithDescription("Constant 1 with build/runtime metadata as labels."),
	)
	if err != nil {
		return nil, err
	}
	return &instruments{
		connected:          connected,
		tasksActive:        tasksActive,
		tasksMaxConcurrent: tasksMaxConcurrent,
		tasksClaimed:       tasksClaimed,
		tasksRejected:      tasksRejected,
		tasksCompleted:     tasksCompleted,
		taskDuration:       taskDuration,
		wsReconnects:       wsReconnects,
		workerInfo:         workerInfo,
	}, nil
}

func current() *instruments {
	return activeInstruments.Load()
}

// SetConnected records the worker's WebSocket connection state.
func SetConnected(connected bool) {
	val := int64(0)
	if connected {
		val = 1
	}
	current().connected.Record(context.Background(), val)
}

// IncTasksActive marks that one more task has begun executing on this worker.
func IncTasksActive() {
	current().tasksActive.Add(context.Background(), 1)
}

// DecTasksActive marks that one task has finished executing on this worker.
func DecTasksActive() {
	current().tasksActive.Add(context.Background(), -1)
}

// SetMaxConcurrent records the configured concurrency limit. 0 means
// unlimited; expose the configured value as-is so dashboards can decide how
// to render saturation.
func SetMaxConcurrent(n int) {
	current().tasksMaxConcurrent.Record(context.Background(), int64(n))
}

// RecordTaskClaim records a successful claim (worker has accepted a task).
func RecordTaskClaim() {
	current().tasksClaimed.Add(context.Background(), 1)
}

// RecordTaskRejected records a task that the worker rejected, e.g. because
// it was at the configured concurrency limit. The reason label is intended
// to be a small bounded enum.
func RecordTaskRejected(reason string) {
	current().tasksRejected.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("reason", reason)),
	)
}

// TaskResult is the bounded enum of terminal task outcomes the worker
// reports. Keeping this list small avoids label-cardinality blowups.
type TaskResult string

const (
	TaskResultSucceeded TaskResult = "succeeded"
	TaskResultFailed    TaskResult = "failed"
)

// RecordTaskCompleted records a completed task, regardless of outcome, along
// with its wall-clock duration on this worker.
func RecordTaskCompleted(result TaskResult, duration time.Duration) {
	attrs := metric.WithAttributes(attribute.String("result", string(result)))
	current().tasksCompleted.Add(context.Background(), 1, attrs)
	current().taskDuration.Record(context.Background(), duration.Seconds(), attrs)
}

// RecordWebsocketReconnect records a reconnect attempt against warp-server.
// The reason label is intended to be a small bounded enum
// (e.g. "dial_failed", "remote_close").
func RecordWebsocketReconnect(reason string) {
	current().wsReconnects.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("reason", reason)),
	)
}

// SetWorkerInfo emits a constant gauge with build metadata. It is intended
// to be called once, immediately after Init.
func SetWorkerInfo(version, backend, workerID string) {
	current().workerInfo.Record(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("version", version),
			attribute.String("backend", backend),
			attribute.String("worker_id", workerID),
		),
	)
}
