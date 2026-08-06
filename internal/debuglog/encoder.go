package debuglog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"
)

// Chunk is one bounded piece of provider output offered to a Sink.
type Chunk struct {
	Phase Phase
	// Stream must reflect what the provider actually reports. Backends that
	// cannot separate streams use StreamCombined or StreamUnknown.
	Stream Stream
	// Timestamp is the provider's timestamp; the zero value omits it.
	Timestamp time.Time
	// ObservedAt is when the worker saw this output. The zero value uses the
	// encoder's clock, which is correct for output read live from a provider;
	// a replayed capture supplies its own recorded observation time.
	ObservedAt time.Time
	Source     SourceIdentity
	Data       []byte
}

// Sink receives snapshot records from a backend's SnapshotTaskLogs. Backends
// hand it provider output and identity; framing, chunk bounds, transformation,
// encoding selection, sequencing, and truncation belong to the sink.
type Sink interface {
	// WriteChunk emits data, splitting it into records no larger than
	// MaxChunkBytes. Empty data emits nothing, so an empty provider stream
	// never produces a misleading zero-byte record.
	WriteChunk(chunk Chunk) error
	// WriteSourceError records that one source could not be read, using a
	// stable warning code rather than the provider's error text.
	WriteSourceError(phase Phase, stream Stream, source SourceIdentity, warningCode string) error
	// NoteOmittedBytes reports bytes an upstream capture already dropped so
	// the snapshot's truncation state stays truthful.
	NoteOmittedBytes(n int64)
}

// Encoder writes schema-v1 NDJSON into a bounded spool and finalizes it into
// one immutable snapshot file.
type Encoder struct {
	backend     string
	transformer ContentTransformer
	now         func() time.Time
	spool       *boundedSpool

	sequence        int64
	upstreamOmitted int64
	warnings        warningSet
}

// EncoderOptions configures a snapshot encoder.
type EncoderOptions struct {
	// Backend is the backend kind stamped on every record.
	Backend string
	// Transformer rewrites decoded message data before encoding.
	Transformer ContentTransformer
	// MaxBytes is the effective output bound for this snapshot.
	MaxBytes int64
	// CreateFile allocates one spool segment. The caller owns naming and mode.
	CreateFile func(suffix string) (*os.File, error)
	// Now supplies the worker observation clock; nil uses time.Now.
	Now func() time.Time
}

// NewEncoder creates a snapshot encoder backed by a bounded spool.
func NewEncoder(opts EncoderOptions) (*Encoder, error) {
	if opts.Transformer == nil {
		return nil, fmt.Errorf("debuglog: encoder requires a content transformer")
	}
	if opts.CreateFile == nil {
		return nil, fmt.Errorf("debuglog: encoder requires a file allocator")
	}
	spool, err := newBoundedSpool(opts.MaxBytes, opts.CreateFile)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Encoder{
		backend:     opts.Backend,
		transformer: opts.Transformer,
		now:         now,
		spool:       spool,
	}, nil
}

// WriteChunk implements Sink.
func (e *Encoder) WriteChunk(chunk Chunk) error {
	for _, part := range splitChunk(chunk.Data, MaxChunkBytes) {
		transformed, err := e.transformer.Transform(part)
		if err != nil {
			return fmt.Errorf("debuglog: content transform failed: %w", err)
		}
		if len(transformed) == 0 {
			continue
		}

		observedAt := chunk.ObservedAt
		if observedAt.IsZero() {
			observedAt = e.now()
		}

		e.sequence++
		record := dataRecord{
			SchemaVersion: SchemaVersion,
			Kind:          KindData,
			Sequence:      e.sequence,
			Backend:       e.backend,
			Phase:         string(chunk.Phase),
			Stream:        string(chunk.Stream),
			ObservedAt:    observedAt.UTC().Format(time.RFC3339Nano),
		}
		if !chunk.Timestamp.IsZero() {
			record.Timestamp = chunk.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		applySourceIdentity(&record, chunk.Source)

		if utf8.Valid(transformed) {
			record.Encoding = EncodingUTF8
			record.Data = string(transformed)
		} else {
			record.Encoding = EncodingBase64
			record.Data = base64.StdEncoding.EncodeToString(transformed)
		}

		if err := e.writeRecord(record); err != nil {
			return err
		}
	}
	return nil
}

// WriteSourceError implements Sink.
func (e *Encoder) WriteSourceError(phase Phase, stream Stream, source SourceIdentity, warningCode string) error {
	e.sequence++
	record := sourceErrorRecord{
		SchemaVersion:  SchemaVersion,
		Kind:           KindSourceError,
		Sequence:       e.sequence,
		Backend:        e.backend,
		Phase:          string(phase),
		Stream:         string(stream),
		ObservedAt:     e.now().UTC().Format(time.RFC3339Nano),
		WarningCode:    warningCode,
		ContainerID:    source.ContainerID,
		Namespace:      source.Namespace,
		Pod:            source.Pod,
		Container:      source.Container,
		ContainerType:  source.ContainerType,
		RestartAttempt: source.RestartAttempt,
		Previous:       source.Previous,
	}
	e.warnings.add(warningCode)
	return e.writeRecord(record)
}

// NoteOmittedBytes implements Sink.
func (e *Encoder) NoteOmittedBytes(n int64) {
	if n > 0 {
		e.upstreamOmitted += n
		e.warnings.add(WarningOutputDropped)
	}
}

// FinalizeResult describes the immutable snapshot an encoder produced.
type FinalizeResult struct {
	// Bytes is the exact size of the written snapshot.
	Bytes int64
	// Truncated reports whether the snapshot carries a truncation record.
	Truncated bool
	// Warnings are the deduplicated codes to report in the acknowledgement.
	Warnings []string
}

// Finalize streams the retained head, the truncation record when bytes were
// dropped, and the retained tail into out. The encoder's spool is released.
//
// The written object never exceeds the encoder's configured bound: the spool
// caps its segments against a budget that already reserves room for the
// truncation record.
func (e *Encoder) Finalize(out io.Writer) (FinalizeResult, error) {
	defer func() {
		_ = e.spool.Close()
	}()

	mark := e.spool.Watermark()
	segments := e.spool.segmentsAt(mark)

	omitted := mark.omitted + e.upstreamOmitted
	if mark.omitted > 0 {
		e.warnings.add(WarningOutputDropped)
	}

	var written int64
	// The head reader holds the earliest retained records; the truncation
	// record then names the gap before the retained tail segments.
	n, err := io.Copy(out, segments[0])
	written += n
	if err != nil {
		return FinalizeResult{}, err
	}

	truncated := false
	if omitted > 0 {
		line, marshalErr := marshalLine(truncationRecord{
			SchemaVersion:       SchemaVersion,
			Kind:                KindTruncation,
			Policy:              TruncationPolicyFirstLast,
			OmittedBytesAtLeast: omitted,
		})
		if marshalErr != nil {
			return FinalizeResult{}, marshalErr
		}
		// A budget too small to hold even this record retains nothing at all,
		// so the object stays empty and the request reports the capture as
		// unavailable rather than shipping an over-budget object.
		if written+int64(len(line)) <= e.spool.maxBytes {
			gap, writeErr := out.Write(line)
			written += int64(gap)
			if writeErr != nil {
				return FinalizeResult{}, writeErr
			}
			truncated = true
		}
	}

	for _, segment := range segments[1:] {
		n, err = io.Copy(out, segment)
		written += n
		if err != nil {
			return FinalizeResult{}, err
		}
	}

	if written > e.spool.maxBytes {
		return FinalizeResult{}, fmt.Errorf("debuglog: snapshot overran its %d byte bound", e.spool.maxBytes)
	}

	return FinalizeResult{
		Bytes:     written,
		Truncated: truncated,
		Warnings:  e.warnings.codes(),
	}, nil
}

// Close releases the encoder's spool without producing a snapshot.
func (e *Encoder) Close() error { return e.spool.Close() }

func (e *Encoder) writeRecord(record any) error {
	line, err := marshalLine(record)
	if err != nil {
		return err
	}
	return e.spool.WriteLine(line)
}

func marshalLine(record any) ([]byte, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("debuglog: failed to encode NDJSON record: %w", err)
	}
	return append(encoded, '\n'), nil
}

func applySourceIdentity(record *dataRecord, source SourceIdentity) {
	record.ContainerID = source.ContainerID
	record.Namespace = source.Namespace
	record.Pod = source.Pod
	record.Container = source.Container
	record.ContainerType = source.ContainerType
	record.RestartAttempt = source.RestartAttempt
	record.Previous = source.Previous
}

// splitChunk divides data into pieces no larger than limit, preferring to cut
// on a UTF-8 boundary so text output stays UTF-8 encodable instead of falling
// back to base64 at every chunk seam.
func splitChunk(data []byte, limit int) [][]byte {
	if len(data) == 0 {
		return nil
	}
	if len(data) <= limit {
		return [][]byte{data}
	}

	var parts [][]byte
	for len(data) > limit {
		cut := limit
		for back := 0; back < utf8.UTFMax && cut > 1; back++ {
			if utf8.RuneStart(data[cut]) {
				break
			}
			cut--
		}
		parts = append(parts, data[:cut])
		data = data[cut:]
	}
	if len(data) > 0 {
		parts = append(parts, data)
	}
	return parts
}
