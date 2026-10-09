package tasklogs

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/rs/zerolog"
	otellog "go.opentelemetry.io/otel/log"
)

// ZerologWriter exports the full redacted JSON payload as the body, retaining event metadata.
func (r *Reporter) ZerologWriter(ctx context.Context) zerolog.LevelWriter {
	return zerologWriter{reporter: r, ctx: ctx}
}

type zerologWriter struct {
	reporter *Reporter
	ctx      context.Context
}

func (w zerologWriter) Write(p []byte) (int, error) {
	return w.WriteLevel(zerolog.InfoLevel, p)
}

func (w zerologWriter) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	if len(p) > maxLineBytes || w.reporter.closed.Load() || !json.Valid(p) {
		return len(p), nil
	}
	var event any
	decoder := json.NewDecoder(bytes.NewReader(p))
	decoder.UseNumber()
	if decoder.Decode(&event) != nil {
		return len(p), nil
	}
	// Normalize escaping to match registered JSON credential variants without rounding numbers.
	if body, err := json.Marshal(event); err == nil {
		w.reporter.emit(w.ctx, zerologSeverity(level), w.reporter.redactor.redact(string(body)))
	}
	return len(p), nil
}

func zerologSeverity(level zerolog.Level) otellog.Severity {
	switch level {
	case zerolog.TraceLevel:
		return otellog.SeverityTrace
	case zerolog.DebugLevel:
		return otellog.SeverityDebug
	case zerolog.WarnLevel:
		return otellog.SeverityWarn
	case zerolog.ErrorLevel:
		return otellog.SeverityError
	case zerolog.FatalLevel, zerolog.PanicLevel:
		return otellog.SeverityFatal
	default:
		return otellog.SeverityInfo
	}
}
