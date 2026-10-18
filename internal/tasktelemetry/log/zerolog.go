package tasklogs

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

// ZerologWriter exports the message as the body and other JSON fields as attributes.
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
	var event map[string]any
	decoder := json.NewDecoder(bytes.NewReader(p))
	decoder.UseNumber()
	if decoder.Decode(&event) != nil {
		return len(p), nil
	}
	// Normalize JSON escaping before matching credential variants in keys and values.
	// Redact the whole payload so the body and nested attributes share one output budget.
	payload, err := json.Marshal(event)
	if err != nil {
		return len(p), nil
	}
	redacted := w.reporter.redactor.redact(string(payload))
	event = nil
	decoder = json.NewDecoder(strings.NewReader(redacted))
	decoder.UseNumber()
	if decoder.Decode(&event) != nil {
		w.reporter.emit(w.ctx, zerologSeverity(level), "[REDACTED]")
		return len(p), nil
	}
	message, ok := event[zerolog.MessageFieldName].(string)
	if ok {
		delete(event, zerolog.MessageFieldName)
	}
	w.reporter.emit(w.ctx, zerologSeverity(level), message, jsonValue(event).AsMap()...)
	return len(p), nil
}

func jsonValue(value any) attribute.Value {
	switch value := value.(type) {
	case string:
		return attribute.StringValue(value)
	case bool:
		return attribute.BoolValue(value)
	case json.Number:
		if number, err := value.Int64(); err == nil {
			return attribute.Int64Value(number)
		}
		if number, err := value.Float64(); err == nil && strconv.FormatFloat(number, 'g', -1, 64) == value.String() {
			return attribute.Float64Value(number)
		}
		// OTLP has no arbitrary-precision number; retain the literal rather than round it.
		return attribute.StringValue(value.String())
	case []any:
		items := make([]attribute.Value, len(value))
		for i, item := range value {
			items[i] = jsonValue(item)
		}
		return attribute.SliceValue(items...)
	case map[string]any:
		fields := make([]attribute.KeyValue, 0, len(value))
		for key, item := range value {
			fields = append(fields, attribute.KeyValue{Key: attribute.Key(key), Value: jsonValue(item)})
		}
		return attribute.MapValue(fields...)
	default:
		return attribute.Value{}
	}
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
