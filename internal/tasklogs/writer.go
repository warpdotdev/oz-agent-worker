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
	mu       sync.Mutex
	reporter *Reporter
	ctx      context.Context
	source   string
	line     []byte
	discard  bool
	closed   bool
}

func (r *Reporter) Writer(ctx context.Context, source string) *Writer {
	writer := &Writer{reporter: r, ctx: ctx, source: source}
	r.writersMu.Lock()
	defer r.writersMu.Unlock()
	if r.closed.Load() || len(r.writers) >= 128 {
		writer.closed = true
	} else {
		r.writers = append(r.writers, writer)
	}
	return writer
}

func (w *Writer) Write(p []byte) (int, error) {
	n := len(p)
	if w.reporter == nil {
		return n, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return n, nil
	}
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
func (w *Writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flush()
	w.closed = true
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

// Setup output must not be exported until the environment file's credentials are registered.
func (r *Reporter) BufferedOutput(ctx context.Context, source string, local io.Writer) (io.Writer, func(bool)) {
	if r == nil {
		return local, func(bool) {}
	}
	buffer := &deferredOutput{}
	return io.MultiWriter(local, buffer), func(environmentKnown bool) {
		buffer.mu.Lock()
		defer buffer.mu.Unlock()
		if buffer.closed {
			return
		}
		buffer.closed = true
		if environmentKnown && !buffer.overflow {
			writer := r.Writer(ctx, source)
			_, _ = writer.Write(buffer.bytes)
			writer.Flush()
		}
		buffer.bytes = nil
	}
}

type deferredOutput struct {
	mu       sync.Mutex
	bytes    []byte
	overflow bool
	closed   bool
}

func (b *deferredOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed && !b.overflow {
		if len(p) > maxLineBytes-len(b.bytes) {
			b.bytes = nil
			b.overflow = true
		} else {
			b.bytes = append(b.bytes, p...)
		}
	}
	return len(p), nil
}
