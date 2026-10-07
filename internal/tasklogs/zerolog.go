package tasklogs

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog"
)

// ZerologWriter accepts zerolog JSON events and exports their redacted messages and severity.
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
	if len(p) > maxLineBytes || w.reporter.closed.Load() {
		return len(p), nil
	}
	var event struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(p, &event) == nil {
		w.reporter.log(w.ctx, level.String(), event.Message)
	}
	return len(p), nil
}
