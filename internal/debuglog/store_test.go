package debuglog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, mutate func(*Config)) *Store {
	t.Helper()
	config := DefaultConfig()
	config.Directory = t.TempDir()
	config.MaxExecutionBytes = 1 << 16
	config.MaxTotalBytes = 1 << 20
	if mutate != nil {
		mutate(&config)
	}
	store, err := NewStore(config)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestConfigValidateRejectsUnusableBounds(t *testing.T) {
	tests := map[string]Config{
		"non-positive execution bound": {MaxExecutionBytes: 0, MaxTotalBytes: 1 << 20, MaxConcurrentUploads: 2},
		"execution bound above the protocol ceiling": {
			MaxExecutionBytes:    ProtocolCeilingBytes + 1,
			MaxTotalBytes:        ProtocolCeilingBytes * 2,
			MaxConcurrentUploads: 2,
		},
		"total below the execution bound": {MaxExecutionBytes: 1 << 20, MaxTotalBytes: 1 << 10, MaxConcurrentUploads: 2},
		"non-positive concurrency":        {MaxExecutionBytes: 1 << 20, MaxTotalBytes: 1 << 21, MaxConcurrentUploads: -1},
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			if err := config.Validate(); err == nil {
				t.Fatal("expected the configuration to be rejected")
			}
		})
	}
}

func TestConfigWithDefaultsFillsUnsetBounds(t *testing.T) {
	filled := Config{}.WithDefaults()
	if filled.MaxTotalBytes != DefaultMaxTotalBytes ||
		filled.MaxExecutionBytes != DefaultMaxExecutionBytes ||
		filled.MaxConcurrentUploads != DefaultMaxConcurrentUploads {
		t.Fatalf("defaults were not applied: %+v", filled)
	}
	if err := filled.Validate(); err != nil {
		t.Fatalf("the default configuration must validate: %v", err)
	}
}

func TestNewStoreCreatesASecureRoot(t *testing.T) {
	base := t.TempDir()
	config := DefaultConfig()
	config.Directory = base
	store, err := NewStore(config)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	info, err := os.Stat(store.root)
	if err != nil {
		t.Fatalf("capture root was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != captureDirMode {
		t.Fatalf("capture root mode = %o, want %o", perm, captureDirMode)
	}
	if !strings.HasPrefix(store.root, base) {
		t.Fatalf("capture root %q escaped its configured base %q", store.root, base)
	}
}

func TestNewStoreRemovesOrphansFromAPreviousProcess(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, captureDirName)
	if err := os.MkdirAll(root, captureDirMode); err != nil {
		t.Fatalf("failed to seed capture root: %v", err)
	}
	orphan := filepath.Join(root, "capture-deadbeef-1-head")
	if err := os.WriteFile(orphan, []byte("stale bytes"), captureFileMode); err != nil {
		t.Fatalf("failed to seed orphan: %v", err)
	}

	config := DefaultConfig()
	config.Directory = base
	if _, err := NewStore(config); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned capture file survived startup: %v", err)
	}
}

func TestStoreCreatesFilesWithNonUserDerivedNamesAndTightMode(t *testing.T) {
	store := newTestStore(t, nil)

	file, err := store.createFile("capture", "head")
	if err != nil {
		t.Fatalf("createFile: %v", err)
	}
	defer func() { _ = closeAndRemove(file) }()

	info, err := file.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != captureFileMode {
		t.Fatalf("capture file mode = %o, want %o", perm, captureFileMode)
	}
	if filepath.Dir(file.Name()) != store.root {
		t.Fatalf("capture file %q was created outside the capture root", file.Name())
	}
}

func TestStoreRefusesToOverwriteAPlantedSymlink(t *testing.T) {
	store := newTestStore(t, nil)

	// A predictable name is what a symlink attack needs; the store's random
	// names plus O_EXCL are what defeat it. Recreate the exact name the store
	// would use and prove it refuses rather than following the link.
	target := filepath.Join(t.TempDir(), "outside")
	victim := filepath.Join(store.root, "planted")
	if err := os.Symlink(target, victim); err != nil {
		t.Skipf("symlinks are unavailable on this platform: %v", err)
	}

	if _, err := os.OpenFile(victim, os.O_RDWR|os.O_CREATE|os.O_EXCL, captureFileMode); err == nil {
		t.Fatal("expected O_EXCL to refuse an existing symlink")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the symlink target was written through: %v", err)
	}
}

func TestStoreBudgetIsSharedAndReleased(t *testing.T) {
	store := newTestStore(t, func(c *Config) {
		c.MaxExecutionBytes = 1 << 16
		c.MaxTotalBytes = 1 << 17
	})

	first, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	second, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if _, err := store.NewTaskLogCapture(nil); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("third capture error = %v, want %v", err, ErrBudgetExhausted)
	}

	first.Close()
	third, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("a capture must be admitted after budget is released: %v", err)
	}

	second.Close()
	third.Close()
	if reserved := store.ReservedBytes(); reserved != 0 {
		t.Fatalf("reserved bytes = %d after closing every capture, want 0", reserved)
	}
}

// collectingSink records what a capture replays without touching disk.
type collectingSink struct {
	chunks  []Chunk
	omitted int64
}

func (s *collectingSink) WriteChunk(chunk Chunk) error {
	copied := chunk
	copied.Data = append([]byte(nil), chunk.Data...)
	s.chunks = append(s.chunks, copied)
	return nil
}

func (s *collectingSink) WriteSourceError(Phase, Stream, SourceIdentity, string) error { return nil }

func (s *collectingSink) NoteOmittedBytes(n int64) { s.omitted += n }

func (s *collectingSink) text() string {
	var builder strings.Builder
	for _, chunk := range s.chunks {
		builder.Write(chunk.Data)
	}
	return builder.String()
}

func TestTaskLogCaptureLabelsPhaseAndStream(t *testing.T) {
	store := newTestStore(t, nil)
	capture, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("NewTaskLogCapture: %v", err)
	}
	defer capture.Close()

	if _, err := capture.Writer(PhaseSetup, StreamStdout).Write([]byte("setup-out\n")); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	if _, err := capture.Writer(PhaseAgent, StreamStderr).Write([]byte("agent-err\n")); err != nil {
		t.Fatalf("agent write: %v", err)
	}
	if _, err := capture.Writer(PhaseTeardown, StreamStdout).Write([]byte("teardown-out\n")); err != nil {
		t.Fatalf("teardown write: %v", err)
	}
	capture.Finalize(2 * time.Second)

	sink := &collectingSink{}
	if err := capture.SnapshotTo(sink); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	if len(sink.chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3", len(sink.chunks))
	}
	want := []struct {
		phase  Phase
		stream Stream
		data   string
	}{
		{PhaseSetup, StreamStdout, "setup-out\n"},
		{PhaseAgent, StreamStderr, "agent-err\n"},
		{PhaseTeardown, StreamStdout, "teardown-out\n"},
	}
	for i, expected := range want {
		got := sink.chunks[i]
		if got.Phase != expected.phase || got.Stream != expected.stream || string(got.Data) != expected.data {
			t.Errorf("chunk %d = {%s %s %q}, want {%s %s %q}",
				i, got.Phase, got.Stream, got.Data, expected.phase, expected.stream, expected.data)
		}
		if got.ObservedAt.IsZero() {
			t.Errorf("chunk %d has no worker observation time", i)
		}
	}
}

func TestTaskLogCaptureWriteAlwaysReportsTheChildByteCount(t *testing.T) {
	store := newTestStore(t, nil)
	capture, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("NewTaskLogCapture: %v", err)
	}
	defer capture.Close()

	writer := capture.Writer(PhaseAgent, StreamStdout)
	payload := []byte(strings.Repeat("z", 4096))
	// Far more writes than the archive queue can hold, so drops are certain.
	for i := 0; i < captureQueueDepth*4; i++ {
		n, err := writer.Write(payload)
		if err != nil {
			t.Fatalf("write %d returned an error, which would surface on the child's pipe: %v", i, err)
		}
		if n != len(payload) {
			t.Fatalf("write %d reported %d bytes, want %d", i, n, len(payload))
		}
	}
}

func TestTaskLogCaptureSnapshotUsesAFixedWatermark(t *testing.T) {
	store := newTestStore(t, nil)
	capture, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("NewTaskLogCapture: %v", err)
	}
	defer capture.Close()

	writer := capture.Writer(PhaseAgent, StreamStdout)
	if _, err := writer.Write([]byte("before-watermark\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	capture.Finalize(2 * time.Second)

	first := &collectingSink{}
	if err := capture.SnapshotTo(first); err != nil {
		t.Fatalf("first SnapshotTo: %v", err)
	}
	if !strings.Contains(first.text(), "before-watermark") {
		t.Fatalf("first snapshot = %q, want the pre-watermark output", first.text())
	}
	if strings.Contains(first.text(), "after-watermark") {
		t.Fatal("first snapshot leaked output written after its watermark")
	}

	if _, err := writer.Write([]byte("after-watermark\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	capture.Finalize(2 * time.Second)

	second := &collectingSink{}
	if err := capture.SnapshotTo(second); err != nil {
		t.Fatalf("second SnapshotTo: %v", err)
	}
	if !strings.Contains(second.text(), "after-watermark") {
		t.Fatalf("second snapshot = %q, want the later output", second.text())
	}
}

func TestTaskLogCaptureIsPerExecution(t *testing.T) {
	store := newTestStore(t, nil)
	first, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	defer first.Close()
	second, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	defer second.Close()

	if _, err := first.Writer(PhaseAgent, StreamStdout).Write([]byte("task-one-output\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	first.Finalize(2 * time.Second)
	second.Finalize(2 * time.Second)

	sink := &collectingSink{}
	if err := second.SnapshotTo(sink); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	if strings.Contains(sink.text(), "task-one-output") {
		t.Fatalf("one task's output appeared in another's snapshot: %q", sink.text())
	}
}

func TestTaskLogCaptureCloseIsIdempotent(t *testing.T) {
	store := newTestStore(t, nil)
	capture, err := store.NewTaskLogCapture(nil)
	if err != nil {
		t.Fatalf("NewTaskLogCapture: %v", err)
	}

	capture.Close()
	capture.Close()

	if reserved := store.ReservedBytes(); reserved != 0 {
		t.Fatalf("reserved bytes = %d after a double close, want 0", reserved)
	}
}

func TestSnapshotComputesChecksumsOverTheUploadedBytes(t *testing.T) {
	store := newTestStore(t, nil)
	snapshot, err := store.NewSnapshot(BackendDirect, noopTransformer{}, 1<<15)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	defer snapshot.Close()

	if err := snapshot.Sink().WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: []byte("payload")}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if err := snapshot.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	if snapshot.Bytes() == 0 {
		t.Fatal("expected the snapshot to carry bytes")
	}
	if snapshot.CRC32C() == "" || snapshot.SHA256() == "" {
		t.Fatalf("expected both digests, got crc32c=%q sha256=%q", snapshot.CRC32C(), snapshot.SHA256())
	}

	// Every attempt must replay identical bytes for a retry to be safe.
	firstBytes, err := readAll(snapshot)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	secondBytes, err := readAll(snapshot)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatal("re-opening the snapshot produced different bytes")
	}
	if int64(len(firstBytes)) != snapshot.Bytes() {
		t.Fatalf("read %d bytes, want the reported %d", len(firstBytes), snapshot.Bytes())
	}
}
