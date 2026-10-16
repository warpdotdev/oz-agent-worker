package tracing

import (
	"context"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Handle struct {
	processor sdktrace.SpanProcessor
	resource  *resource.Resource
	provider  *sdktrace.TracerProvider
}

func New(ctx context.Context, workerID, backend, version string) (*Handle, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", "oz-agent-worker"),
		attribute.String("worker.id", workerID),
		attribute.String("worker.backend", backend),
	}
	if version != "" {
		attrs = append(attrs, attribute.String("service.version", version))
	}
	h := &Handle{resource: resource.NewSchemaless(attrs...)}
	if !shouldInitTraces() {
		return h, nil
	}
	exporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return h, err
	}
	h.processor = sdktrace.NewBatchSpanProcessor(exporter)
	h.provider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.processor), sdktrace.WithResource(h.resource))
	otel.SetTracerProvider(h.provider)
	return h, nil
}

func shouldInitTraces() bool {
	exporter := strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))
	return exporter != "" && !strings.EqualFold(exporter, "none")
}

func (h *Handle) Shutdown(ctx context.Context) error {
	if h == nil || h.provider == nil {
		return nil
	}
	return h.provider.Shutdown(ctx)
}

// Each task borrows the operator processor, but only the process owns its shutdown.
type borrowedProcessor struct {
	sdktrace.SpanProcessor
	closed atomic.Bool
}

func (p *borrowedProcessor) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	if !p.closed.Load() {
		p.SpanProcessor.OnStart(ctx, span)
	}
}

func (p *borrowedProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if !p.closed.Load() {
		p.SpanProcessor.OnEnd(span)
	}
}

func (p *borrowedProcessor) ForceFlush(ctx context.Context) error {
	if p.closed.Load() {
		return nil
	}
	return p.SpanProcessor.ForceFlush(ctx)
}

func (p *borrowedProcessor) Shutdown(context.Context) error {
	p.closed.Store(true)
	return nil
}
