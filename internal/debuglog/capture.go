package debuglog

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// captureQueueDepth bounds how many chunks may await the background encoder.
// A full queue drops archive bytes rather than back-pressuring the subprocess
// pipe that feeds it.
const captureQueueDepth = 256

// captureRecord is the internal on-disk framing for captured output. It stores
// what the direct backend observed; the request's content transformer is
// applied later, when a snapshot re-encodes these records into NDJSON.
type captureRecord struct {
	Phase      string `json:"p"`
	Stream     string `json:"s"`
	ObservedAt string `json:"t"`
	Data       string `json:"d"`
}

// TaskLogCapture is a bounded, secure, disk-backed copy of one direct
// execution's stdout and stderr. Writers never block on it: a full queue drops
// archive bytes and marks the capture partial while the task's console output
// and process exit stay unaffected.
type TaskLogCapture struct {
	store    *Store
	spool    *boundedSpool
	reserved int64
	now      func() time.Time

	queue chan captureRecord
	done  chan struct{}

	dropped atomic.Bool
	failed  atomic.Bool
	bytes   atomic.Int64
	// pending counts records accepted but not yet written to disk. An empty
	// queue is not enough for finalization: the encoder may have taken the
	// last record and not written it yet.
	pending atomic.Int64

	closeOnce sync.Once
}

// NewTaskLogCapture allocates a bounded capture against the store's budget.
func (s *Store) NewTaskLogCapture(now func() time.Time) (*TaskLogCapture, error) {
	limit := s.config.MaxExecutionBytes
	if err := s.reserve(limit); err != nil {
		return nil, err
	}

	spool, err := newBoundedSpool(limit, func(suffix string) (*os.File, error) {
		return s.createFile("capture", suffix)
	})
	if err != nil {
		s.release(limit)
		return nil, err
	}

	if now == nil {
		now = time.Now
	}
	capture := &TaskLogCapture{
		store:    s,
		spool:    spool,
		reserved: limit,
		now:      now,
		queue:    make(chan captureRecord, captureQueueDepth),
		done:     make(chan struct{}),
	}
	go capture.drain()
	return capture, nil
}

// Writer returns an io.Writer that copies output into the capture under the
// given phase and stream. Write always reports the full input length and never
// returns an error, so a capture problem can never alter the child process's
// view of its own pipe.
func (c *TaskLogCapture) Writer(phase Phase, stream Stream) io.Writer {
	return &captureWriter{capture: c, phase: phase, stream: stream}
}

type captureWriter struct {
	capture *TaskLogCapture
	phase   Phase
	stream  Stream
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.capture.offer(w.phase, w.stream, p)
	return len(p), nil
}

// offer enqueues a bounded copy of p, dropping it when the queue is full.
func (c *TaskLogCapture) offer(phase Phase, stream Stream, p []byte) {
	if len(p) == 0 {
		return
	}
	for _, part := range splitChunk(p, MaxChunkBytes) {
		// The caller owns p's backing array and may reuse it before the
		// encoder drains the queue, so the chunk is copied here.
		record := captureRecord{
			Phase:      string(phase),
			Stream:     string(stream),
			ObservedAt: c.now().UTC().Format(time.RFC3339Nano),
			Data:       base64.StdEncoding.EncodeToString(part),
		}
		c.pending.Add(1)
		select {
		case c.queue <- record:
		default:
			c.pending.Add(-1)
			c.dropped.Store(true)
		}
	}
}

func (c *TaskLogCapture) drain() {
	defer close(c.done)
	for record := range c.queue {
		c.writeRecord(record)
		c.pending.Add(-1)
	}
}

func (c *TaskLogCapture) writeRecord(record captureRecord) {
	line, err := json.Marshal(record)
	if err != nil {
		c.failed.Store(true)
		return
	}
	line = append(line, '\n')
	if err := c.spool.WriteLine(line); err != nil {
		if !errors.Is(err, ErrSpoolClosed) {
			c.failed.Store(true)
		}
		return
	}
	c.bytes.Add(int64(len(line)))
}

// Finalize drains queued output under a bounded deadline so a terminal
// snapshot sees the execution's last bytes. The capture stays readable
// afterwards; only Close releases it.
func (c *TaskLogCapture) Finalize(deadline time.Duration) {
	timer := time.NewTimer(deadline)
	defer timer.Stop()

	for {
		if c.pending.Load() == 0 {
			return
		}
		select {
		case <-timer.C:
			// Output still in flight past the deadline is reported as dropped
			// so the snapshot is truthfully marked partial.
			c.dropped.Store(true)
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// Bytes reports how many capture bytes have been written to disk.
func (c *TaskLogCapture) Bytes() int64 { return c.bytes.Load() }

// SnapshotTo replays the capture's contents at a fixed watermark into sink
// while writers keep appending. Output after the watermark stays available to
// a later snapshot.
func (c *TaskLogCapture) SnapshotTo(sink Sink) error {
	if c.dropped.Load() || c.failed.Load() {
		// The exact dropped byte count is unknown because the queue discards
		// chunks without accounting them; report a nonzero lower bound so the
		// snapshot is truthfully marked truncated.
		sink.NoteOmittedBytes(1)
	}

	mark := c.spool.Watermark()
	if mark.omitted > 0 {
		sink.NoteOmittedBytes(mark.omitted)
	}

	for _, segment := range c.spool.segmentsAt(mark) {
		if err := replayCaptureSegment(segment, sink); err != nil {
			return err
		}
	}
	return nil
}

func replayCaptureSegment(segment io.Reader, sink Sink) error {
	decoder := json.NewDecoder(segment)
	for {
		var record captureRecord
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			// A rotated segment can begin mid-line; the remainder of this
			// segment is unparseable, and the truncation record already
			// accounts for the gap.
			return nil
		}

		data, err := base64.StdEncoding.DecodeString(record.Data)
		if err != nil {
			continue
		}
		chunk := Chunk{
			Phase:  Phase(record.Phase),
			Stream: Stream(record.Stream),
			Data:   data,
		}
		if observed, parseErr := time.Parse(time.RFC3339Nano, record.ObservedAt); parseErr == nil {
			chunk.ObservedAt = observed
		}
		if err := sink.WriteChunk(chunk); err != nil {
			return err
		}
	}
}

// Close stops the encoder, deletes the capture's bytes, and returns its share
// of the disk budget. It is idempotent.
func (c *TaskLogCapture) Close() {
	c.closeOnce.Do(func() {
		close(c.queue)
		<-c.done
		_ = c.spool.Close()
		c.store.release(c.reserved)
	})
}
