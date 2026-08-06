package debuglog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fileAllocator hands the spool real files under a test-owned directory.
func fileAllocator(t *testing.T) func(string) (*os.File, error) {
	t.Helper()
	dir := t.TempDir()
	var index int
	return func(suffix string) (*os.File, error) {
		index++
		path := filepath.Join(dir, suffix+"-"+time.Now().Format("150405.000000000")+"-"+string(rune('a'+index)))
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	}
}

func newTestEncoder(t *testing.T, maxBytes int64, transformer ContentTransformer) *Encoder {
	t.Helper()
	if transformer == nil {
		transformer = noopTransformer{}
	}
	encoder, err := NewEncoder(EncoderOptions{
		Backend:     BackendDocker,
		Transformer: transformer,
		MaxBytes:    maxBytes,
		CreateFile:  fileAllocator(t),
		Now:         func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	return encoder
}

func decodeRecords(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("snapshot line is not valid JSON (%q): %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestEncoderEmitsSchemaV1DataRecords(t *testing.T) {
	encoder := newTestEncoder(t, 1<<20, nil)
	restarts := int32(2)

	if err := encoder.WriteChunk(Chunk{
		Phase:     PhaseContainer,
		Stream:    StreamStderr,
		Timestamp: time.Unix(1690000000, 0).UTC(),
		Source: SourceIdentity{
			Namespace:      "agents",
			Pod:            "oz-task-run-exec-abcd",
			Container:      "task",
			ContainerType:  "regular",
			RestartAttempt: &restarts,
			Previous:       true,
		},
		Data: []byte("hello world"),
	}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if result.Truncated {
		t.Error("a snapshot below its bound must not be marked truncated")
	}

	records := decodeRecords(t, out.Bytes())
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	record := records[0]

	for field, want := range map[string]any{
		"schema_version":  float64(SchemaVersion),
		"kind":            KindData,
		"sequence":        float64(1),
		"backend":         BackendDocker,
		"phase":           string(PhaseContainer),
		"stream":          string(StreamStderr),
		"encoding":        EncodingUTF8,
		"data":            "hello world",
		"namespace":       "agents",
		"pod":             "oz-task-run-exec-abcd",
		"container":       "task",
		"container_type":  "regular",
		"restart_attempt": float64(2),
		"previous":        true,
	} {
		if record[field] != want {
			t.Errorf("%s = %v, want %v", field, record[field], want)
		}
	}
	if record["observed_at"] == "" || record["timestamp"] == "" {
		t.Errorf("expected both provider and observed timestamps, got %v", record)
	}
}

func TestEncoderOmitsProviderTimestampWhenAbsent(t *testing.T) {
	encoder := newTestEncoder(t, 1<<20, nil)
	if err := encoder.WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: []byte("x")}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	if _, err := encoder.Finalize(&out); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if _, present := decodeRecords(t, out.Bytes())[0]["timestamp"]; present {
		t.Error("a chunk with no provider timestamp must not report one")
	}
}

func TestEncoderEmptyChunkProducesNoRecord(t *testing.T) {
	encoder := newTestEncoder(t, 1<<20, nil)
	if err := encoder.WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if result.Bytes != 0 {
		t.Fatalf("empty stream produced %d bytes, want 0", result.Bytes)
	}
}

func TestEncoderBase64EncodesInvalidUTF8(t *testing.T) {
	encoder := newTestEncoder(t, 1<<20, nil)
	binary := []byte{0x00, 0xff, 0xfe, 0x41}
	if err := encoder.WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: binary}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	if _, err := encoder.Finalize(&out); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	record := decodeRecords(t, out.Bytes())[0]
	if record["encoding"] != EncodingBase64 {
		t.Fatalf("encoding = %v, want %v", record["encoding"], EncodingBase64)
	}
	decoded, err := base64.StdEncoding.DecodeString(record["data"].(string))
	if err != nil {
		t.Fatalf("base64 payload did not decode: %v", err)
	}
	if !bytes.Equal(decoded, binary) {
		t.Fatalf("round-tripped %v, want %v", decoded, binary)
	}
}

func TestEncoderSplitsChunksAtMaxChunkBytes(t *testing.T) {
	encoder := newTestEncoder(t, 8<<20, nil)
	payload := bytes.Repeat([]byte("a"), MaxChunkBytes*2+7)
	if err := encoder.WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: payload}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	if _, err := encoder.Finalize(&out); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	records := decodeRecords(t, out.Bytes())
	if len(records) != 3 {
		t.Fatalf("record count = %d, want 3", len(records))
	}
	var reassembled strings.Builder
	for i, record := range records {
		if got := int(record["sequence"].(float64)); got != i+1 {
			t.Errorf("sequence = %d, want %d", got, i+1)
		}
		data := record["data"].(string)
		if len(data) > MaxChunkBytes {
			t.Errorf("record %d carries %d bytes, above the %d chunk bound", i, len(data), MaxChunkBytes)
		}
		reassembled.WriteString(data)
	}
	if reassembled.String() != string(payload) {
		t.Error("decoded records did not reassemble into the original payload")
	}
}

func TestEncoderTruncatesWithRecordBoundedFirstLastPolicy(t *testing.T) {
	// A tight bound forces the spool to rotate, dropping the middle of the
	// stream while keeping whole records at both ends.
	encoder := newTestEncoder(t, 4096, nil)
	const lines = 400
	for i := 0; i < lines; i++ {
		if err := encoder.WriteChunk(Chunk{
			Phase:  PhaseAgent,
			Stream: StreamStdout,
			Data:   []byte(strings.Repeat("x", 40)),
		}); err != nil {
			t.Fatalf("WriteChunk: %v", err)
		}
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !result.Truncated {
		t.Fatal("expected the snapshot to be marked truncated")
	}
	if result.Bytes > 4096 {
		t.Fatalf("snapshot is %d bytes, above its %d bound", result.Bytes, 4096)
	}

	records := decodeRecords(t, out.Bytes())
	truncations := 0
	var truncationIndex int
	for i, record := range records {
		if record["kind"] == KindTruncation {
			truncations++
			truncationIndex = i
			if record["policy"] != TruncationPolicyFirstLast {
				t.Errorf("policy = %v, want %v", record["policy"], TruncationPolicyFirstLast)
			}
			if omitted, _ := record["omitted_bytes_at_least"].(float64); omitted <= 0 {
				t.Errorf("omitted_bytes_at_least = %v, want a positive lower bound", record["omitted_bytes_at_least"])
			}
		}
	}
	if truncations != 1 {
		t.Fatalf("truncation record count = %d, want 1", truncations)
	}
	if truncationIndex == 0 || truncationIndex == len(records)-1 {
		t.Fatalf("truncation record at index %d: expected records on both sides", truncationIndex)
	}

	first := records[0]["sequence"].(float64)
	last := records[len(records)-1]["sequence"].(float64)
	if first != 1 {
		t.Errorf("first retained sequence = %v, want 1", first)
	}
	if last != lines {
		t.Errorf("last retained sequence = %v, want %d", last, lines)
	}
}

func TestEncoderSourceErrorCarriesOnlyWarningCode(t *testing.T) {
	encoder := newTestEncoder(t, 1<<20, nil)
	source := SourceIdentity{Pod: "pod-1", Container: "task"}
	if err := encoder.WriteSourceError(PhaseContainer, StreamCombined, source, WarningContainerLogsUnavailable); err != nil {
		t.Fatalf("WriteSourceError: %v", err)
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	record := decodeRecords(t, out.Bytes())[0]
	if record["kind"] != KindSourceError {
		t.Fatalf("kind = %v, want %v", record["kind"], KindSourceError)
	}
	if record["warning_code"] != WarningContainerLogsUnavailable {
		t.Fatalf("warning_code = %v, want %v", record["warning_code"], WarningContainerLogsUnavailable)
	}
	if _, present := record["data"]; present {
		t.Error("a source-error record must not carry provider data")
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != WarningContainerLogsUnavailable {
		t.Fatalf("warnings = %v, want [%s]", result.Warnings, WarningContainerLogsUnavailable)
	}
}

// upperTransformer proves the transformer touches only decoded message data.
type upperTransformer struct{}

func (upperTransformer) Kind() string { return "test-upper" }

func (upperTransformer) Version() int { return 7 }

func (upperTransformer) Transform(data []byte) ([]byte, error) {
	return bytes.ToUpper(data), nil
}

func TestEncoderTransformsOnlyMessageData(t *testing.T) {
	provider := time.Unix(1690000000, 0).UTC()
	source := SourceIdentity{Pod: "pod-a", Container: "task"}

	encodeWith := func(transformer ContentTransformer) map[string]any {
		encoder := newTestEncoder(t, 1<<20, transformer)
		if err := encoder.WriteChunk(Chunk{
			Phase:     PhaseContainer,
			Stream:    StreamStdout,
			Timestamp: provider,
			Source:    source,
			Data:      []byte("secret value"),
		}); err != nil {
			t.Fatalf("WriteChunk: %v", err)
		}
		var out bytes.Buffer
		if _, err := encoder.Finalize(&out); err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		return decodeRecords(t, out.Bytes())[0]
	}

	preserved := encodeWith(noopTransformer{})
	transformed := encodeWith(upperTransformer{})

	if preserved["data"] != "secret value" {
		t.Fatalf("the no-op transformer changed data: %v", preserved["data"])
	}
	if transformed["data"] != "SECRET VALUE" {
		t.Fatalf("data = %v, want the transformed value", transformed["data"])
	}
	for _, field := range []string{"schema_version", "kind", "sequence", "backend", "phase", "stream", "timestamp", "observed_at", "pod", "container"} {
		if preserved[field] != transformed[field] {
			t.Errorf("transformer changed structural field %s: %v vs %v", field, preserved[field], transformed[field])
		}
	}
}

func TestEncoderReportsUpstreamOmittedBytesAsTruncation(t *testing.T) {
	encoder := newTestEncoder(t, 1<<20, nil)
	encoder.NoteOmittedBytes(4096)
	if err := encoder.WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: []byte("tail")}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if !result.Truncated {
		t.Fatal("upstream omission must mark the snapshot truncated")
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != WarningOutputDropped {
		t.Fatalf("warnings = %v, want [%s]", result.Warnings, WarningOutputDropped)
	}
}

// TestEncoderNeverExceedsItsBound sweeps bounds from pathologically small to
// comfortably large. The request's max_bytes is a hard ceiling on the object
// the worker uploads, so no combination of record size, tail overshoot, and
// truncation metadata may push the finalized snapshot past it.
func TestEncoderNeverExceedsItsBound(t *testing.T) {
	bounds := []int64{
		1, 2, 16, 64,
		maxTruncationLineBytes - 1,
		maxTruncationLineBytes,
		maxTruncationLineBytes + 1,
		256, 512, 1024, 4096, 65536,
	}
	payloads := map[string][]byte{
		"tiny":  []byte("x"),
		"line":  bytes.Repeat([]byte("y"), 200),
		"chunk": bytes.Repeat([]byte("z"), MaxChunkBytes),
	}

	for _, bound := range bounds {
		for name, payload := range payloads {
			t.Run(fmt.Sprintf("bound=%d/%s", bound, name), func(t *testing.T) {
				encoder := newTestEncoder(t, bound, nil)
				for i := 0; i < 50; i++ {
					if err := encoder.WriteChunk(Chunk{
						Phase:  PhaseAgent,
						Stream: StreamStdout,
						Data:   payload,
					}); err != nil {
						t.Fatalf("WriteChunk: %v", err)
					}
				}

				var out bytes.Buffer
				result, err := encoder.Finalize(&out)
				if err != nil {
					t.Fatalf("Finalize: %v", err)
				}

				if result.Bytes > bound {
					t.Fatalf("snapshot is %d bytes, above its %d bound", result.Bytes, bound)
				}
				if int64(out.Len()) != result.Bytes {
					t.Fatalf("wrote %d bytes but reported %d", out.Len(), result.Bytes)
				}
				// Whatever survives must still be parseable NDJSON.
				decodeRecords(t, out.Bytes())
			})
		}
	}
}

func TestEncoderKeepsWholeRecordsAndReportsTheGapAtATightBound(t *testing.T) {
	// Room for the reserved truncation record plus a handful of data records,
	// so the snapshot must retain some output at both ends and name the gap.
	bound := maxTruncationLineBytes + 1200
	encoder := newTestEncoder(t, bound, nil)
	for i := 0; i < 200; i++ {
		if err := encoder.WriteChunk(Chunk{
			Phase:  PhaseAgent,
			Stream: StreamStdout,
			Data:   []byte(strings.Repeat("q", 40)),
		}); err != nil {
			t.Fatalf("WriteChunk: %v", err)
		}
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if result.Bytes > bound {
		t.Fatalf("snapshot is %d bytes, above its %d bound", result.Bytes, bound)
	}
	if !result.Truncated {
		t.Fatal("expected the snapshot to be marked truncated")
	}

	records := decodeRecords(t, out.Bytes())
	truncations := 0
	for _, record := range records {
		if record["kind"] == KindTruncation {
			truncations++
		}
	}
	if truncations != 1 {
		t.Fatalf("truncation record count = %d, want exactly 1", truncations)
	}
	if records[0]["kind"] != KindData {
		t.Fatalf("first record kind = %v, want the earliest data retained", records[0]["kind"])
	}
	if records[len(records)-1]["kind"] != KindData {
		t.Fatalf("last record kind = %v, want the newest data retained", records[len(records)-1]["kind"])
	}
}

func TestEncoderYieldsAnEmptyObjectWhenTheBoundCannotHoldARecord(t *testing.T) {
	// A bound this small cannot hold even the truncation record. Emitting
	// nothing keeps the ceiling hard; the coordinator turns an empty snapshot
	// into a classified unavailable outcome rather than uploading it.
	encoder := newTestEncoder(t, 4, nil)
	if err := encoder.WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: []byte("dropped")}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if result.Bytes != 0 {
		t.Fatalf("snapshot is %d bytes, want an empty object", result.Bytes)
	}
	if result.Truncated {
		t.Fatal("truncated must not be reported without a truncation record")
	}
}

func TestEncoderReportsARecordTooLargeForItsBoundAsOmitted(t *testing.T) {
	// A single record wider than a tail segment cannot be retained without
	// overrunning the bound, so it is dropped and accounted for rather than
	// written in full.
	bound := maxTruncationLineBytes + 400
	encoder := newTestEncoder(t, bound, nil)
	if err := encoder.WriteChunk(Chunk{
		Phase:  PhaseAgent,
		Stream: StreamStdout,
		Data:   bytes.Repeat([]byte("w"), 4096),
	}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}

	var out bytes.Buffer
	result, err := encoder.Finalize(&out)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if result.Bytes > bound {
		t.Fatalf("snapshot is %d bytes, above its %d bound", result.Bytes, bound)
	}
	if !result.Truncated {
		t.Fatal("dropping an oversized record must mark the snapshot truncated")
	}

	records := decodeRecords(t, out.Bytes())
	if len(records) != 1 || records[0]["kind"] != KindTruncation {
		t.Fatalf("records = %v, want only the truncation record", records)
	}
	if omitted, _ := records[0]["omitted_bytes_at_least"].(float64); omitted <= 0 {
		t.Fatalf("omitted_bytes_at_least = %v, want the dropped record accounted for", records[0]["omitted_bytes_at_least"])
	}
}
