package tracing

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/tasktelemetry/otlp"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const ShutdownTimeout = 5 * time.Second
const scope = "github.com/warpdotdev/oz-agent-worker"

type Session struct {
	mu         sync.Mutex
	provider   *sdktrace.TracerProvider
	processors []sdktrace.SpanProcessor
	root       trace.Span
	cancel     context.CancelFunc
	ended      bool
	closeOnce  sync.Once
}

// Start begins a worker-local task trace at receivedAt. A nil session means no
// destination is usable; a non-nil session can accompany a Warp initialization
// error when operator tracing remains available. The caller must End and
// Shutdown the session. A nil handle still permits assignment-driven export.
func (h *Handle) Start(ctx context.Context, server string, assignment *types.TaskAssignmentMessage, receivedAt time.Time) (context.Context, *Session, error) {
	pipelineCtx, cancel := context.WithCancel(context.Background())
	attrs := []attribute.KeyValue{attribute.String("service.name", "oz-agent-worker")}
	if h != nil {
		attrs = h.resource.Attributes()
	}
	attrs = append(attrs, attribute.String("warp.run_id", assignment.TaskID))
	if assignment.ExecutionID != "" {
		attrs = append(attrs, attribute.String("warp.execution_id", assignment.ExecutionID))
	}
	res := resource.NewSchemaless(attrs...)
	s := &Session{cancel: cancel}
	var initErr error
	if config := assignment.TelemetryCollection; config != nil && config.TracingEnabled {
		processor, err := newWarpProcessor(pipelineCtx, server, assignment)
		if err != nil {
			initErr = err
		} else {
			s.processors = append(s.processors, processor)
		}
	}
	if h != nil && h.processor != nil {
		s.processors = append(s.processors, &borrowedProcessor{SpanProcessor: h.processor})
	}
	if len(s.processors) == 0 {
		cancel()
		return ctx, nil, initErr
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	}
	for _, processor := range s.processors {
		opts = append(opts, sdktrace.WithSpanProcessor(processor))
	}
	s.provider = sdktrace.NewTracerProvider(opts...)
	ctx, s.root = s.provider.Tracer(scope).Start(ctx, "self_hosted_worker.task",
		trace.WithNewRoot(), trace.WithTimestamp(receivedAt),
		trace.WithAttributes(attrs...),
	)
	return ctx, s, initErr
}

func newWarpProcessor(ctx context.Context, server string, assignment *types.TaskAssignmentMessage) (sdktrace.SpanProcessor, error) {
	config, err := otlp.NewExportConfig(ctx, server, assignment)
	if err != nil {
		return nil, err
	}

	exporter, err := config.TraceExporter()
	if err != nil {
		return nil, err
	}
	return sdktrace.NewBatchSpanProcessor(exporter,
		sdktrace.WithMaxQueueSize(256),
		sdktrace.WithMaxExportBatchSize(64),
		sdktrace.WithBatchTimeout(5*time.Second),
		sdktrace.WithExportTimeout(otlp.ExportTimeout),
	), nil
}

// End closes only the task root; child-span creators must finish before it is called.
func (s *Session) End(outcome string, err error, attrs ...attribute.KeyValue) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	now := time.Now()
	s.root.SetAttributes(append(attrs, attribute.String("outcome", outcome))...)
	RecordError(s.root, err)
	switch outcome {
	case "failed":
		s.root.SetStatus(codes.Error, outcome)
	case "succeeded", "cancelled", "dispatched":
		s.root.SetStatus(codes.Ok, outcome)
	}
	s.root.End(trace.WithTimestamp(now))
}

// RecordError retains raw task failures on an existing span without ending it.
func RecordError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String("error.message", err.Error()))
		span.SetStatus(codes.Error, err.Error())
	}
}

func (s *Session) ForceFlush(ctx context.Context) error {
	if s == nil {
		return nil
	}
	results := make(chan error, len(s.processors))
	for _, processor := range s.processors {
		go func() { results <- processor.ForceFlush(ctx) }()
	}
	var err error
	for range s.processors {
		select {
		case result := <-results:
			err = errors.Join(err, result)
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	return err
}

func (s *Session) Shutdown(ctx context.Context) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
		defer cancel()
		stop := context.AfterFunc(ctx, s.cancel)
		defer stop()
		defer s.cancel()
		_ = s.ForceFlush(ctx)
		s.cancel()
		// TracerProvider skips processors when its context is already expired.
		// Stop each batcher explicitly so a timed-out flush cannot leave it alive.
		for _, processor := range s.processors {
			_ = processor.Shutdown(ctx)
		}
		_ = s.provider.Shutdown(ctx)
	})
}

// Tracer uses the context's provider so task routing never falls back to the
// process-global provider.
func Tracer(ctx context.Context) trace.Tracer {
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(scope)
}
