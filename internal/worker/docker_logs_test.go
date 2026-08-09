package worker

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/debuglog"
)

// dockerFrame builds one frame of the Docker daemon's multiplexed log stream.
func dockerFrame(streamType byte, payload string) []byte {
	header := make([]byte, dockerStreamHeaderLen)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	return append(header, payload...)
}

// captureSink records the chunks a backend writes without touching disk.
type captureSink struct {
	chunks   []debuglog.Chunk
	warnings []string
	omitted  int64
}

func (s *captureSink) WriteChunk(chunk debuglog.Chunk) error {
	copied := chunk
	copied.Data = append([]byte(nil), chunk.Data...)
	s.chunks = append(s.chunks, copied)
	return nil
}

func (s *captureSink) WriteSourceError(_ debuglog.Phase, _ debuglog.Stream, _ debuglog.SourceIdentity, warningCode string) error {
	s.warnings = append(s.warnings, warningCode)
	return nil
}

func (s *captureSink) NoteOmittedBytes(n int64) { s.omitted += n }

func TestCopyDockerLogStreamPreservesStreamIdentityAndTimestamps(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		dockerFrame(dockerStreamTypeStdout, "2026-01-02T03:04:05.000000000Z entrypoint starting\n"),
		dockerFrame(dockerStreamTypeStderr, "2026-01-02T03:04:06.000000000Z client warning\n"),
		dockerFrame(dockerStreamTypeStdout, "2026-01-02T03:04:07.000000000Z client done\n"),
	}, nil))

	sink := &captureSink{}
	source := debuglog.SourceIdentity{ContainerID: "container-abc"}
	if err := copyDockerLogStream(stream, sink, source); err != nil {
		t.Fatalf("copyDockerLogStream: %v", err)
	}

	if len(sink.chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3", len(sink.chunks))
	}
	want := []struct {
		stream debuglog.Stream
		data   string
	}{
		{debuglog.StreamStdout, "entrypoint starting\n"},
		{debuglog.StreamStderr, "client warning\n"},
		{debuglog.StreamStdout, "client done\n"},
	}
	for i, expected := range want {
		got := sink.chunks[i]
		if got.Stream != expected.stream {
			t.Errorf("chunk %d stream = %q, want %q", i, got.Stream, expected.stream)
		}
		if string(got.Data) != expected.data {
			t.Errorf("chunk %d data = %q, want %q", i, got.Data, expected.data)
		}
		if got.Phase != debuglog.PhaseContainer {
			t.Errorf("chunk %d phase = %q, want %q", i, got.Phase, debuglog.PhaseContainer)
		}
		if got.Source.ContainerID != "container-abc" {
			t.Errorf("chunk %d container = %q, want container-abc", i, got.Source.ContainerID)
		}
		if got.Timestamp.IsZero() {
			t.Errorf("chunk %d lost the provider timestamp", i)
		}
	}
	if !sink.chunks[0].Timestamp.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("first timestamp = %v, want the provider value", sink.chunks[0].Timestamp)
	}
}

func TestCopyDockerLogStreamKeepsUntimestampedLinesIntact(t *testing.T) {
	stream := bytes.NewReader(dockerFrame(dockerStreamTypeStdout, "plain line with no timestamp\n"))

	sink := &captureSink{}
	if err := copyDockerLogStream(stream, sink, debuglog.SourceIdentity{}); err != nil {
		t.Fatalf("copyDockerLogStream: %v", err)
	}

	if len(sink.chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(sink.chunks))
	}
	if string(sink.chunks[0].Data) != "plain line with no timestamp\n" {
		t.Fatalf("data = %q, want the whole line retained", sink.chunks[0].Data)
	}
	if !sink.chunks[0].Timestamp.IsZero() {
		t.Fatal("a line without a provider timestamp must not report one")
	}
}

func TestCopyDockerLogStreamDoesNotFilterOrAttributeLines(t *testing.T) {
	// Both the entrypoint and the client it starts write to the same container
	// streams. Every line must survive, with no invented process identity.
	stream := bytes.NewReader(bytes.Join([][]byte{
		dockerFrame(dockerStreamTypeStdout, "[entrypoint] preparing workspace\n"),
		dockerFrame(dockerStreamTypeStdout, "[oz] agent turn 1\n"),
		dockerFrame(dockerStreamTypeStderr, "[oz] agent warning\n"),
		dockerFrame(dockerStreamTypeStderr, "[entrypoint] teardown\n"),
	}, nil))

	sink := &captureSink{}
	if err := copyDockerLogStream(stream, sink, debuglog.SourceIdentity{}); err != nil {
		t.Fatalf("copyDockerLogStream: %v", err)
	}

	if len(sink.chunks) != 4 {
		t.Fatalf("chunk count = %d, want every line retained", len(sink.chunks))
	}
	for i, chunk := range sink.chunks {
		if chunk.Phase != debuglog.PhaseContainer {
			t.Errorf("chunk %d phase = %q, want the provider's own %q", i, chunk.Phase, debuglog.PhaseContainer)
		}
	}
}

func TestCopyDockerLogStreamHandlesAnUnframedStream(t *testing.T) {
	// A TTY-attached container returns raw bytes with no frame header. The
	// output is reported as combined rather than split by guesswork.
	stream := strings.NewReader(strings.Repeat("tty output line\n", 4))

	sink := &captureSink{}
	if err := copyUnframedLogStream(stream, sink, debuglog.SourceIdentity{}); err != nil {
		t.Fatalf("copyUnframedLogStream: %v", err)
	}

	if len(sink.chunks) == 0 {
		t.Fatal("expected the unframed output to be captured")
	}
	for i, chunk := range sink.chunks {
		if chunk.Stream != debuglog.StreamCombined {
			t.Errorf("chunk %d stream = %q, want %q", i, chunk.Stream, debuglog.StreamCombined)
		}
	}
}

func TestCopyDockerLogStreamStopsCleanlyOnATruncatedFrame(t *testing.T) {
	// A stream cut mid-frame must end the snapshot rather than error out and
	// discard everything already read.
	complete := dockerFrame(dockerStreamTypeStdout, "first line\n")
	partial := dockerFrame(dockerStreamTypeStdout, "second line\n")[:6]

	sink := &captureSink{}
	if err := copyDockerLogStream(bytes.NewReader(append(complete, partial...)), sink, debuglog.SourceIdentity{}); err != nil {
		t.Fatalf("copyDockerLogStream: %v", err)
	}
	if len(sink.chunks) != 1 {
		t.Fatalf("chunk count = %d, want the complete frame retained", len(sink.chunks))
	}
}

// countingReader reports how much of a stream was pulled into memory at once.
type countingReader struct {
	remaining int
	frame     []byte
	offset    int
	maxRead   int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 && r.offset >= len(r.frame) {
		return 0, io.EOF
	}
	if r.offset >= len(r.frame) {
		r.offset = 0
		r.remaining--
		if r.remaining < 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, r.frame[r.offset:])
	r.offset += n
	if n > r.maxRead {
		r.maxRead = n
	}
	return n, nil
}

func TestCopyDockerLogStreamMemoryDoesNotScaleWithOutputSize(t *testing.T) {
	// The archive path streams rather than reading the whole container log
	// into memory, so a very large log is bounded by the chunk size.
	frame := dockerFrame(dockerStreamTypeStdout, strings.Repeat("x", 4096)+"\n")
	reader := &countingReader{remaining: 4096, frame: frame}

	sink := &captureSink{}
	if err := copyDockerLogStream(reader, sink, debuglog.SourceIdentity{}); err != nil {
		t.Fatalf("copyDockerLogStream: %v", err)
	}

	total := 0
	for _, chunk := range sink.chunks {
		if len(chunk.Data) > debuglog.MaxChunkBytes {
			t.Fatalf("a chunk carried %d bytes, above the %d bound", len(chunk.Data), debuglog.MaxChunkBytes)
		}
		total += len(chunk.Data)
	}
	if total < 4096 {
		t.Fatalf("captured %d bytes, want the large log to be streamed through", total)
	}
}
