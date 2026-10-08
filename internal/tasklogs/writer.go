package tasklogs

import (
	"bytes"
	"context"
	"io"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

// Writer buffers one task-output stream across arbitrary write boundaries.
// It emits complete lines, redacts registered environment values, and drops lines
// over 64 KiB in full so partial credentials cannot survive truncation.
type Writer struct {
	mu       sync.Mutex
	reporter *Reporter
	ctx      context.Context
	attrs    []attribute.KeyValue
	line     []byte
	discard  bool
	closed   bool
}

// Writer labels each record with source and flushes pending bytes at reporter shutdown.
func (r *Reporter) Writer(ctx context.Context, source string, attrs ...attribute.KeyValue) *Writer {
	recordAttrs := make([]attribute.KeyValue, len(attrs)+1)
	copy(recordAttrs, attrs)
	recordAttrs[len(attrs)] = attribute.String("log.source", source)
	writer := &Writer{reporter: r, ctx: ctx, attrs: recordAttrs}
	r.writersMu.Lock()
	defer r.writersMu.Unlock()
	if r.closed.Load() || len(r.writers) >= 128 {
		writer.closed = true
	} else {
		r.writers = append(r.writers, writer)
	}
	return writer
}

// Write accepts all bytes without propagating collector failures to the producer.
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

// Flush emits the final unterminated line unless it exceeded the line limit.
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
		w.reporter.emit(w.ctx, otellog.SeverityInfo,
			w.reporter.redactor.redactLine(string(w.line)),
			w.attrs...,
		)
	}
	w.line = w.line[:0]
	w.discard = false
}

// Output copies original bytes locally and redacted lines to OTLP.
// The returned function flushes a final unterminated line when the producer exits.
// A nil reporter leaves local output unchanged.
func (r *Reporter) Output(ctx context.Context, source string, local io.Writer) (io.Writer, func()) {
	if r == nil {
		return local, func() {}
	}
	writer := r.Writer(ctx, source)
	return io.MultiWriter(local, writer), writer.Flush
}
