package tasklogs

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"golang.org/x/oauth2"
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

func New(serverRootURL, workerID, backend string, assignment *types.TaskAssignmentMessage) (*Reporter, error) {
	if assignment == nil || assignment.TelemetryCollection == nil || !assignment.TelemetryCollection.LoggingEnabled {
		return nil, nil
	}
	config := assignment.TelemetryCollection
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("invalid task log collector endpoint")
	}
	server, err := url.Parse(serverRootURL)
	if err != nil || server.Host == "" || (server.Scheme != "https" && server.Scheme != "http") || server.User != nil || server.RawQuery != "" || server.Fragment != "" {
		return nil, errors.New("invalid task identity endpoint")
	}
	if assignment.EnvVars["WARP_API_KEY"] == "" || assignment.EnvVars["WARP_WORKLOAD_TOKEN"] == "" {
		return nil, errors.New("task log reporting requires task API key and workload token")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/logs"
	ctx, cancel := context.WithCancel(context.Background())
	r := &Reporter{ctx: ctx, cancel: cancel}
	r.AddEnv(assignment.EnvVars)
	base := &http.Client{
		Timeout:       exportTimeout,
		CheckRedirect: noRedirect,
	}
	client := oauth2.NewClient(context.WithValue(ctx, oauth2.HTTPClient, base),
		newTokenSource(serverRootURL, assignment.TaskID, assignment.EnvVars))
	exporter, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(endpoint.String()),
		otlploghttp.WithHTTPClient(client),
		otlploghttp.WithHeaders(map[string]string{}),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}),
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

func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type privateExporter struct {
	sdklog.Exporter
}

func (e privateExporter) Export(ctx context.Context, records []sdklog.Record) error {
	// Collector response bodies reach OTel's global error handler without task redaction.
	if err := e.Exporter.Export(ctx, records); err != nil {
		return errors.New("task log export failed")
	}
	return nil
}

func (r *Reporter) Context() context.Context {
	return r.ctx
}

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

func (r *Reporter) log(ctx context.Context, level, message string) {
	message = r.redactor.redact(message)
	severity := otellog.SeverityInfo
	switch level {
	case "trace":
		severity = otellog.SeverityTrace
	case "debug":
		severity = otellog.SeverityDebug
	case "warn":
		severity = otellog.SeverityWarn
	case "error":
		severity = otellog.SeverityError
	case "fatal", "panic":
		severity = otellog.SeverityFatal
	}
	r.emit(ctx, severity, level, message)
}

func (r *Reporter) Event(ctx context.Context, name string, start, finish time.Time, isError bool) {
	if r == nil {
		return
	}
	r.emit(ctx, otellog.SeverityInfo, "info", name,
		attribute.String("event.name", name),
		attribute.String("start_ts", start.UTC().Format(time.RFC3339Nano)),
		attribute.String("finish_ts", finish.UTC().Format(time.RFC3339Nano)),
		attribute.Float64("latency_ms", float64(finish.Sub(start))/float64(time.Millisecond)),
		attribute.Bool("is_error", isError),
	)
}

func (r *Reporter) emit(ctx context.Context, severity otellog.Severity, level, message string, attrs ...attribute.KeyValue) {
	if r.closed.Load() {
		return
	}
	if len(message) > maxRecordBytes {
		message = message[:maxRecordBytes]
	}
	var record otellog.Record
	record.SetTimestamp(time.Now())
	record.SetSeverity(severity)
	record.SetSeverityText(level)
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
