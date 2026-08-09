package worker

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
)

const (
	// dockerStreamHeaderLen is the length of the frame header the Docker
	// daemon prepends to each chunk of a non-TTY log stream.
	dockerStreamHeaderLen = 8
	// dockerStreamTypeStdout and dockerStreamTypeStderr are the stream
	// identifiers in that header's first byte.
	dockerStreamTypeStdout = 1
	dockerStreamTypeStderr = 2
	// dockerMaxFrameBytes bounds a single frame so a malformed header cannot
	// make the worker allocate an arbitrary buffer.
	dockerMaxFrameBytes = 4 << 20
)

// copyDockerLogStream demultiplexes Docker's framed log stream into snapshot
// chunks, preserving each frame's real stdout/stderr identity and the
// per-line provider timestamp. It never filters lines and never attributes
// output to the entrypoint or the client process: the container's stream is
// the only identity Docker reports.
func copyDockerLogStream(stream io.Reader, sink debuglog.Sink, source debuglog.SourceIdentity) error {
	reader := bufio.NewReader(stream)
	header := make([]byte, dockerStreamHeaderLen)

	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}

		streamID := debuglog.StreamCombined
		switch header[0] {
		case dockerStreamTypeStdout:
			streamID = debuglog.StreamStdout
		case dockerStreamTypeStderr:
			streamID = debuglog.StreamStderr
		}

		size := binary.BigEndian.Uint32(header[4:])
		if size == 0 {
			continue
		}
		if size > dockerMaxFrameBytes {
			// A frame this large means the header was not real framing (for
			// example a TTY-attached container). Report the remainder as
			// combined output rather than guessing at boundaries.
			return copyUnframedLogStream(io.MultiReader(bytes.NewReader(header), reader), sink, source)
		}

		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}

		if err := emitTimestampedLines(payload, streamID, source, sink); err != nil {
			return err
		}
	}
}

// copyUnframedLogStream handles a stream Docker did not multiplex. The output
// is truthfully labeled combined rather than split by guesswork.
func copyUnframedLogStream(stream io.Reader, sink debuglog.Sink, source debuglog.SourceIdentity) error {
	buf := make([]byte, debuglog.MaxChunkBytes)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			if emitErr := emitTimestampedLines(buf[:n], debuglog.StreamCombined, source, sink); emitErr != nil {
				return emitErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// emitTimestampedLines splits payload into lines and lifts each line's leading
// RFC3339 timestamp, which the Docker log API prepends when timestamps are
// requested, into the record's provider timestamp field.
func emitTimestampedLines(payload []byte, stream debuglog.Stream, source debuglog.SourceIdentity, sink debuglog.Sink) error {
	for len(payload) > 0 {
		line := payload
		if idx := bytes.IndexByte(payload, '\n'); idx >= 0 {
			line = payload[:idx+1]
			payload = payload[idx+1:]
		} else {
			payload = nil
		}

		timestamp, content := splitProviderTimestamp(line)
		if len(content) == 0 {
			continue
		}
		if err := sink.WriteChunk(debuglog.Chunk{
			Phase:     debuglog.PhaseContainer,
			Stream:    stream,
			Timestamp: timestamp,
			Source:    source,
			Data:      content,
		}); err != nil {
			return err
		}
	}
	return nil
}

// splitProviderTimestamp separates a provider-prefixed RFC3339 timestamp from
// the log content. A line without one keeps all of its bytes and reports a zero
// timestamp, so the record simply omits the provider timestamp field.
func splitProviderTimestamp(line []byte) (time.Time, []byte) {
	space := bytes.IndexByte(line, ' ')
	if space <= 0 {
		return time.Time{}, line
	}
	timestamp, err := time.Parse(time.RFC3339Nano, string(line[:space]))
	if err != nil {
		return time.Time{}, line
	}
	return timestamp, line[space+1:]
}
