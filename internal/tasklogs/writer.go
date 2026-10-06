package tasklogs

import (
	"bytes"
	"context"
	"io"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

type Writer struct {
	mu sync.Mutex
	reporter *Reporter
	ctx context.Context
	source string
	line []byte
	discard bool
}

func (r *Reporter) Writer(ctx context.Context, source string) *Writer {
	return &Writer{reporter: r, ctx: ctx, source: source}
}

func (w *Writer) Write(p []byte) (int, error) {
	n := len(p)
	if w.reporter == nil {
		return n, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if len(w.line)+len(part) > maxLineBytes {
			// Dropping the whole line avoids exporting a truncated credential.
			w.discard = true
			w.line = nil
		}
		if !w.discard {
			w.line = append(w.line, part...)
		}
		if end < 0 {
			break
		}
		w.flush()
		p = p[end+1:]
	}
	return n, nil
}

func (w *Writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flush()
}

func (w *Writer) flush() {
	if !w.discard && len(w.line) > 0 {
		w.reporter.emit(w.ctx, otellog.SeverityInfo, "info",
			w.reporter.redactor.redact(string(w.line)),
			attribute.String("log.source", w.source),
		)
	}
	w.line = w.line[:0]
	w.discard = false
}

func (r *Reporter) Output(ctx context.Context, source string, local io.Writer) (io.Writer, func()) {
	if r == nil {
		return local, func() {}
	}
	writer := r.Writer(ctx, source)
	return io.MultiWriter(local, writer), writer.Flush
}
