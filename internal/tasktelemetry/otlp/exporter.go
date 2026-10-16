package otlp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type ExportConfig struct {
	ctx      context.Context
	endpoint *url.URL
	client   *http.Client
}

func NewExportConfig(ctx context.Context, serverRootURL string, assignment *types.TaskAssignmentMessage) (*ExportConfig, error) {
	if assignment == nil || assignment.TelemetryCollection == nil {
		return nil, errors.New("task telemetry configuration unavailable")
	}
	endpoint, err := parseEndpoint(assignment.TelemetryCollection.Endpoint)
	if err != nil {
		return nil, err
	}
	source, err := NewTokenSource(ctx, BootstrapToken(assignment), RefreshFromAssignment(serverRootURL, assignment))
	if err != nil {
		return nil, err
	}
	return &ExportConfig{ctx: ctx, endpoint: endpoint, client: Client(ctx, source)}, nil
}

func (c *ExportConfig) TraceExporter() (sdktrace.SpanExporter, error) {
	exporter, err := otlptracehttp.New(c.ctx,
		otlptracehttp.WithEndpointURL(signalEndpoint(c.endpoint, "traces")),
		otlptracehttp.WithHTTPClient(c.client),
		otlptracehttp.WithTLSClientConfig(nil),
		otlptracehttp.WithHeaders(map[string]string{}),
		otlptracehttp.WithTimeout(ExportTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled: true, InitialInterval: 500 * time.Millisecond,
			MaxInterval: time.Second, MaxElapsedTime: ExportTimeout,
		}),
	)
	if err != nil {
		return nil, errors.New("could not initialize task trace exporter")
	}
	return traceExporter{SpanExporter: exporter, ctx: c.ctx}, nil
}

func (c *ExportConfig) LogExporter() (sdklog.Exporter, error) {
	exporter, err := otlploghttp.New(c.ctx,
		otlploghttp.WithEndpointURL(signalEndpoint(c.endpoint, "logs")),
		otlploghttp.WithHTTPClient(c.client),
		otlploghttp.WithTLSClientConfig(nil),
		otlploghttp.WithHeaders(map[string]string{}),
		otlploghttp.WithTimeout(ExportTimeout),
		otlploghttp.WithRetry(otlploghttp.RetryConfig{
			Enabled: true, InitialInterval: 500 * time.Millisecond,
			MaxInterval: time.Second, MaxElapsedTime: ExportTimeout,
		}),
	)
	if err != nil {
		return nil, errors.New("could not initialize task log exporter")
	}
	return logExporter{Exporter: exporter}, nil
}

type traceExporter struct {
	sdktrace.SpanExporter
	ctx context.Context
}

func (e traceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	defer cancel()
	// Collector responses may reflect bearer credentials into SDK-global errors.
	if err := e.SpanExporter.ExportSpans(ctx, spans); err != nil {
		return errors.New("task trace export failed")
	}
	return nil
}

func (e traceExporter) Shutdown(ctx context.Context) error {
	if err := e.SpanExporter.Shutdown(ctx); err != nil {
		return errors.New("task trace exporter shutdown failed")
	}
	return nil
}

type logExporter struct {
	sdklog.Exporter
}

func (e logExporter) Export(ctx context.Context, records []sdklog.Record) error {
	// OTel reports exporter errors outside the task-local log redactor.
	if err := e.Exporter.Export(ctx, records); err != nil {
		return errors.New("task log export failed")
	}
	return nil
}
