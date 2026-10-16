package tasklogs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/tasktelemetry/otlp"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	maxRecordBytes  = 16 * 1024
	maxLineBytes    = 64 * 1024
	exportTimeout   = 5 * time.Second
	ShutdownTimeout = 5 * time.Second
)

type Reporter struct {
	provider  *sdklog.LoggerProvider
	logger    otellog.Logger
	redactor  redactor
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	closeOnce sync.Once
	writersMu sync.Mutex
	writers   []*Writer
}

func New(parent context.Context, serverRootURL, workerID, backend string, assignment *types.TaskAssignmentMessage) (*Reporter, error) {
	if assignment == nil || assignment.TelemetryCollection == nil || !assignment.TelemetryCollection.LoggingEnabled {
		return nil, nil
	}
	config := assignment.TelemetryCollection
	endpoint, err := otlp.Endpoint(config.Endpoint, "logs")
	if err != nil {
		return nil, errors.New("invalid task log collector endpoint")
	}
	if err := otlp.ValidateIdentityEndpoint(serverRootURL); err != nil {
		return nil, err
	}
	if assignment.EnvVars["WARP_API_KEY"] == "" || assignment.EnvVars["WARP_WORKLOAD_TOKEN"] == "" {
		return nil, errors.New("task log reporting requires task API key and workload token")
	}
	ctx, cancel := context.WithCancel(parent)
	r := &Reporter{ctx: ctx, cancel: cancel}
	r.AddEnv(assignment.EnvVars)
	client := otlp.Client(ctx, otlp.NewLogTokenSource(ctx, serverRootURL, assignment.TaskID, assignment.EnvVars))
	exporter, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(endpoint),
		otlploghttp.WithHTTPClient(client),
		otlploghttp.WithHeaders(map[string]string{}),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 500 * time.Millisecond,
			MaxInterval:     time.Second,
			MaxElapsedTime:  exportTimeout,
		}),
	)
	if err != nil {
		cancel()
		return nil, errors.New("could not initialize task log exporter")
	}
	r.provider = sdklog.NewLoggerProvider(
		sdklog.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "oz-agent-worker"),
			attribute.String("worker.id", workerID),
			attribute.String("worker.backend", backend),
			attribute.String("run_id", assignment.TaskID),
			attribute.String("execution_id", assignment.ExecutionID),
		)),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(privateExporter{Exporter: exporter},
			sdklog.WithExportInterval(5*time.Second),
			sdklog.WithMaxQueueSize(1024),
			sdklog.WithExportMaxBatchSize(128),
			sdklog.WithExportTimeout(exportTimeout),
		)),
	)
	r.logger = r.provider.Logger("oz-agent-worker.task")
	return r, nil
}

type privateExporter struct {
	sdklog.Exporter
}

func (e privateExporter) Export(ctx context.Context, records []sdklog.Record) error {
	// A collector can reflect Authorization: Bearer <token> in its response body.
	// OTel reports export errors globally, bypassing this task's credential redaction.
	if err := e.Exporter.Export(ctx, records); err != nil {
		return errors.New("task log export failed")
	}
	return nil
}

func (r *Reporter) Context() context.Context {
	return r.ctx
}

// AddEnv registers nonempty environment values for task-local credential redaction.
// Structured events match complete values; raw lines also match newline fragments.
func (r *Reporter) AddEnv(env map[string]string) {
	if r == nil {
		return
	}
	for _, value := range env {
		r.redactor.add(value)
	}
}

func (r *Reporter) AddEnvList(env []string) {
	if r == nil {
		return
	}
	for _, entry := range env {
		_, value, _ := strings.Cut(entry, "=")
		r.redactor.add(value)
	}
}

func (r *Reporter) Event(ctx context.Context, name string, start, finish time.Time, isError bool) {
	if r == nil {
		return
	}
	r.emit(ctx, otellog.SeverityInfo, name,
		attribute.String("event.name", name),
		attribute.String("start_ts", start.UTC().Format(time.RFC3339Nano)),
		attribute.String("finish_ts", finish.UTC().Format(time.RFC3339Nano)),
		attribute.Float64("latency_ms", float64(finish.Sub(start))/float64(time.Millisecond)),
		attribute.Bool("is_error", isError),
	)
}

func (r *Reporter) emit(ctx context.Context, severity otellog.Severity, message string, attrs ...attribute.KeyValue) {
	if r.closed.Load() {
		return
	}
	if len(message) > maxRecordBytes {
		message = message[:maxRecordBytes]
	}
	var record otellog.Record
	record.SetTimestamp(time.Now())
	record.SetSeverity(severity)
	record.SetSeverityText(strings.ToLower(severity.String()))
	record.SetBody(attribute.StringValue(message))
	record.AddAttributes(attrs...)
	r.logger.Emit(ctx, record)
}

func (r *Reporter) Shutdown(ctx context.Context) {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.cancel()
		r.writersMu.Lock()
		for _, writer := range r.writers {
			writer.close()
		}
		r.closed.Store(true)
		r.writers = nil
		r.writersMu.Unlock()
		// Export uses its own deadline, not the cancelled task context.
		_ = r.provider.Shutdown(ctx)
	})
}
