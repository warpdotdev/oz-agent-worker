package tracing

import (
	"context"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/tasktelemetry/otlp"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

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
