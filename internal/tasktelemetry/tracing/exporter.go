package tracing

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/tasktelemetry/otlp"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func newWarpProcessor(ctx context.Context, server string, assignment *types.TaskAssignmentMessage, res *resource.Resource) (sdktrace.SpanProcessor, error) {
	endpoint, err := otlp.Endpoint(assignment.TelemetryCollection.Endpoint, "traces")
	if err != nil {
		return nil, err
	}
	source, err := otlp.NewTraceTokenSource(ctx, server, assignment)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithHTTPClient(otlp.Client(ctx, source)),
		otlptracehttp.WithHeaders(map[string]string{}),
		otlptracehttp.WithTimeout(otlp.ExportTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled: true, InitialInterval: 500 * time.Millisecond,
			MaxInterval: time.Second, MaxElapsedTime: otlp.ExportTimeout,
		}),
	)
	if err != nil {
		return nil, errors.New("could not initialize task trace exporter")
	}
	return sdktrace.NewBatchSpanProcessor(&privateExporter{exporter: exporter, ctx: ctx, resource: res},
		sdktrace.WithMaxQueueSize(256),
		sdktrace.WithMaxExportBatchSize(64),
		sdktrace.WithBatchTimeout(5*time.Second),
		sdktrace.WithExportTimeout(otlp.ExportTimeout),
	), nil
}

type privateExporter struct {
	mu       sync.Mutex
	exporter sdktrace.SpanExporter
	ctx      context.Context
	resource *resource.Resource
}

type warpSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s warpSpan) Resource() *resource.Resource { return s.resource }

func (e *privateExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	exporter := e.exporter
	e.mu.Unlock()
	if exporter == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	defer cancel()
	filtered := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, span := range spans {
		filtered[i] = warpSpan{ReadOnlySpan: span, resource: e.resource}
	}
	// Collector responses may reflect bearer credentials into SDK-global errors.
	if err := exporter.ExportSpans(ctx, filtered); err != nil {
		return errors.New("task trace export failed")
	}
	return nil
}

func (e *privateExporter) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	exporter := e.exporter
	e.exporter = nil
	e.mu.Unlock()
	if exporter != nil {
		if err := exporter.Shutdown(ctx); err != nil {
			return errors.New("task trace exporter shutdown failed")
		}
	}
	return nil
}
