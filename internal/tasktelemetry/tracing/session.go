package tracing

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const ShutdownTimeout = 5*time.Second
const scope = "github.com/warpdotdev/oz-agent-worker"

type sessionKey struct{}

type Session struct {
	mu sync.Mutex
	provider *sdktrace.TracerProvider
	processors []sdktrace.SpanProcessor
	root trace.Span
	ctx context.Context
	cancel context.CancelFunc
	ended bool
	phases map[*Phase]struct{}
	closeOnce sync.Once
}

func Start(ctx context.Context, server, workerID, backend string, assignment *types.TaskAssignmentMessage, receivedAt time.Time) (context.Context, *Session, error) {
	pipelineCtx, cancel := context.WithCancel(context.Background())
	attrs := []attribute.KeyValue{
		attribute.String("service.name", "oz-agent-worker"),
		attribute.String("worker.id", workerID),
		attribute.String("worker.backend", backend),
		attribute.String("run_id", assignment.TaskID),
	}
	if assignment.ExecutionID != "" {
		attrs = append(attrs, attribute.String("execution_id", assignment.ExecutionID))
	}
	op := operator.Load()
	var opResource *resource.Resource
	if op != nil {
		opResource = op.resource
		if version, ok := opResource.Set().Value("service.version"); ok {
			attrs = append(attrs, attribute.KeyValue{Key: "service.version", Value: version})
		}
	}
	warpResource := resource.NewSchemaless(attrs...)
	res, _ := resource.Merge(opResource, warpResource)
	s := &Session{cancel: cancel, phases: make(map[*Phase]struct{})}
	var initErr error
	if config := assignment.TelemetryCollection; config != nil && config.TracingEnabled {
		processor, err := newWarpProcessor(pipelineCtx, server, assignment, warpResource)
		if err != nil {
			initErr = err
		} else {
			s.processors = append(s.processors, processor)
		}
	}
	if op != nil {
		s.processors = append(s.processors, &borrowedProcessor{SpanProcessor: op.processor})
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
		trace.WithAttributes(append(attrs, attribute.String("task.id", assignment.TaskID))...),
	)
	s.ctx = context.WithValue(ctx, sessionKey{}, s)
	return s.ctx, s, initErr
}

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
	for phase := range s.phases {
		phase.span.SetAttributes(attribute.String("outcome", outcome))
		recordResult(phase.span, err)
		phase.span.End(trace.WithTimestamp(now))
	}
	s.phases = nil
	s.root.SetAttributes(append(attrs, attribute.String("outcome", outcome))...)
	recordResult(s.root, err)
	switch outcome {
	case "failed":
		s.root.SetStatus(codes.Error, outcome)
	case "succeeded", "cancelled", "dispatched":
		s.root.SetStatus(codes.Ok, outcome)
	}
	s.root.End(trace.WithTimestamp(now))
}

func recordResult(span trace.Span, err error) {
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

type Phase struct {
	session *Session
	span trace.Span
}

func StartPhase(ctx context.Context, name string) *Phase {
	return StartPhaseAt(ctx, name, time.Now())
}

func StartPhaseAt(ctx context.Context, name string, start time.Time) *Phase {
	s, _ := ctx.Value(sessionKey{}).(*Session)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil
	}
	_, span := trace.SpanFromContext(s.ctx).TracerProvider().Tracer(scope).Start(s.ctx, "self_hosted_worker."+name,
		trace.WithTimestamp(start), trace.WithAttributes(attribute.String("phase", name)))
	p := &Phase{session: s, span: span}
	s.phases[p] = struct{}{}
	return p
}

func (p *Phase) End(err error) {
	p.EndAt(time.Now(), err)
}

func (p *Phase) EndAt(finish time.Time, err error) {
	if p == nil {
		return
	}
	s := p.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, active := s.phases[p]; !active {
		return
	}
	delete(s.phases, p)
	recordResult(p.span, err)
	p.span.End(trace.WithTimestamp(finish))
}
