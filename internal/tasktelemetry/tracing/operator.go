package tracing

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type operatorPipeline struct {
	processor sdktrace.SpanProcessor
	resource  *resource.Resource
}

var operator atomic.Pointer[operatorPipeline]

func InitOperator(ctx context.Context, res *resource.Resource) (func(context.Context) error, error) {
	exporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return nil, err
	}
	processor := sdktrace.NewBatchSpanProcessor(exporter)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor), sdktrace.WithResource(res))
	pipeline := &operatorPipeline{processor: processor, resource: res}
	operator.Store(pipeline)
	otel.SetTracerProvider(provider)
	return func(ctx context.Context) error {
		operator.CompareAndSwap(pipeline, nil)
		return provider.Shutdown(ctx)
	}, nil
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
