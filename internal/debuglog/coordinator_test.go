package debuglog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// fakeOwnership answers exactly the executions a test says this process ran.
type fakeOwnership struct {
	mu    sync.Mutex
	owned map[executionKey]Ownership
}

func newFakeOwnership() *fakeOwnership {
	return &fakeOwnership{owned: make(map[executionKey]Ownership)}
}

func (o *fakeOwnership) own(runID, executionID string, ownership Ownership) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.owned[executionKey{runID: runID, executionID: executionID}] = ownership
}

func (o *fakeOwnership) release(runID, executionID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.owned, executionKey{runID: runID, executionID: executionID})
}

func (o *fakeOwnership) LookupExecution(runID, executionID string) (Ownership, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ownership, ok := o.owned[executionKey{runID: runID, executionID: executionID}]
	return ownership, ok
}

// fakeSource writes canned output into whatever sink it is handed.
type fakeSource struct {
	calls atomic.Int32
	// output, when non-empty, is emitted as one chunk.
	output string
	// err, when non-nil, is returned after any output is written.
	err error
	// block, when non-nil, gates the snapshot so a test can hold a slot.
	block chan struct{}
	// started reports the execution ID of each snapshot as it begins, so a
	// test can wait for a specific request to hold the upload slot instead of
	// racing the coordinator's goroutines.
	started chan string
}

func (s *fakeSource) SnapshotLogs(_ context.Context, _, executionID string, sink Sink) error {
	s.calls.Add(1)
	if s.started != nil {
		s.started <- executionID
	}
	if s.block != nil {
		<-s.block
	}
	if s.output != "" {
		if err := sink.WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: []byte(s.output)}); err != nil {
			return err
		}
	}
	return s.err
}

// awaitStart blocks until a snapshot for executionID has begun.
func (s *fakeSource) awaitStart(t *testing.T, executionID string) {
	t.Helper()
	select {
	case got := <-s.started:
		if got != executionID {
			t.Fatalf("snapshot started for %q, want %q", got, executionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for the snapshot of %q to start", executionID)
	}
}

// recordingSender captures the acknowledgements the coordinator enqueues.
type recordingSender struct {
	mu   sync.Mutex
	acks []*types.DebugArchiveLogsUploadedMessage
	sent chan struct{}
}

func newRecordingSender() *recordingSender {
	return &recordingSender{sent: make(chan struct{}, 16)}
}

func (s *recordingSender) SendDebugArchiveAck(ack *types.DebugArchiveLogsUploadedMessage) error {
	s.mu.Lock()
	s.acks = append(s.acks, ack)
	s.mu.Unlock()
	s.sent <- struct{}{}
	return nil
}

func (s *recordingSender) await(t *testing.T) *types.DebugArchiveLogsUploadedMessage {
	t.Helper()
	select {
	case <-s.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an acknowledgement")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acks[len(s.acks)-1]
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.acks)
}

type coordinatorFixture struct {
	coordinator *Coordinator
	ownership   *fakeOwnership
	source      *fakeSource
	sender      *recordingSender
	uploads     *atomic.Int32
	targetURL   string
}

func newCoordinatorFixture(t *testing.T, source *fakeSource, mutate func(*Config)) *coordinatorFixture {
	t.Helper()

	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		uploads.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	ownership := newFakeOwnership()
	sender := newRecordingSender()
	store := newTestStore(t, mutate)

	uploader := NewUploader(&http.Client{Timeout: 5 * time.Second}, time.Now)
	uploader.sleep = func(context.Context, time.Duration) error { return nil }

	return &coordinatorFixture{
		coordinator: NewCoordinator(CoordinatorOptions{
			Ownership: ownership,
			Source:    source,
			Sender:    sender,
			Store:     store,
			Uploader:  uploader,
		}),
		ownership: ownership,
		source:    source,
		sender:    sender,
		uploads:   &uploads,
		targetURL: server.URL,
	}
}

func (f *coordinatorFixture) request() types.DebugArchiveLogsRequestedMessage {
	request := goldenRequest()
	request.ExpiresAt = time.Now().Add(MaxRequestLifetime)
	request.MaxBytes = 1 << 15
	request.UploadTarget.URL = f.targetURL
	return request
}

func TestCoordinatorNonOwnerStaysSilent(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "should not be read"}, nil)
	request := fixture.request()

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	if fixture.sender.count() != 0 {
		t.Fatalf("a non-owning process sent %d acknowledgements, want 0", fixture.sender.count())
	}
	if fixture.source.calls.Load() != 0 {
		t.Fatal("a non-owning process read the provider")
	}
	if fixture.uploads.Load() != 0 {
		t.Fatal("a non-owning process contacted the upload target")
	}
}

func TestCoordinatorProcessOwningAnotherExecutionOfTheSameRunStaysSilent(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "other execution output"}, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, "a-different-execution", Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	if fixture.sender.count() != 0 {
		t.Fatalf("owning another execution of the same run produced %d acknowledgements, want 0", fixture.sender.count())
	}
	if fixture.uploads.Load() != 0 {
		t.Fatal("owning another execution of the same run uploaded a snapshot")
	}
}

func TestCoordinatorOwnerUploadsAndAcknowledges(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "captured output"}, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	if ack.Outcome != types.DebugArchiveOutcomeUploaded {
		t.Fatalf("outcome = %q (%s), want %q", ack.Outcome, ack.ReasonCode, types.DebugArchiveOutcomeUploaded)
	}
	if ack.ProtocolVersion != types.DebugArchiveProtocolVersion {
		t.Errorf("protocol version = %d, want %d", ack.ProtocolVersion, types.DebugArchiveProtocolVersion)
	}
	for field, got := range map[string]string{
		"request_id":    ack.RequestID,
		"archive_id":    ack.ArchiveID,
		"collection_id": ack.CollectionID,
		"run_id":        ack.RunID,
		"execution_id":  ack.ExecutionID,
	} {
		if got == "" {
			t.Errorf("%s is empty; every identity field must round-trip", field)
		}
	}
	if ack.BackendKind != BackendDocker {
		t.Errorf("backend_kind = %q, want %q", ack.BackendKind, BackendDocker)
	}
	if ack.Bytes == 0 || ack.CRC32C == "" || ack.SHA256 == "" {
		t.Errorf("expected byte count and both digests, got bytes=%d crc32c=%q sha256=%q", ack.Bytes, ack.CRC32C, ack.SHA256)
	}
	if ack.Truncated {
		t.Error("a snapshot below its bound must report truncated=false")
	}
	if ack.CaptureStatus != types.DebugArchiveCaptureComplete {
		t.Errorf("capture_status = %q, want %q", ack.CaptureStatus, types.DebugArchiveCaptureComplete)
	}
	if ack.ContentTransformerVersion != 1 {
		t.Errorf("content_transformer_version = %d, want 1", ack.ContentTransformerVersion)
	}
	if len(ack.WarningCodes) != 0 {
		t.Errorf("warning_codes = %v, want none", ack.WarningCodes)
	}
	if fixture.uploads.Load() != 1 {
		t.Errorf("uploads = %d, want 1", fixture.uploads.Load())
	}
}

func TestCoordinatorAcknowledgementNeverLeaksTargetOrContent(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "super secret log line"}, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	for _, forbidden := range []string{fixture.targetURL, "super secret log line", "/tmp/"} {
		if strings.Contains(ack.Message, forbidden) {
			t.Fatalf("acknowledgement message leaked %q: %q", forbidden, ack.Message)
		}
	}
}

func TestCoordinatorPartialSnapshotUploadsWithWarnings(t *testing.T) {
	source := &fakeSource{
		output: "readable sibling output",
		err:    &PartialSnapshotError{WarningCodes: []string{WarningContainerLogsUnavailable}},
	}
	fixture := newCoordinatorFixture(t, source, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendKubernetes})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	if ack.Outcome != types.DebugArchiveOutcomeUploaded {
		t.Fatalf("outcome = %q, want the readable siblings to still upload", ack.Outcome)
	}
	if ack.CaptureStatus != types.DebugArchiveCapturePartial {
		t.Fatalf("capture_status = %q, want %q", ack.CaptureStatus, types.DebugArchiveCapturePartial)
	}
	if len(ack.WarningCodes) != 1 || ack.WarningCodes[0] != WarningContainerLogsUnavailable {
		t.Fatalf("warning_codes = %v, want [%s]", ack.WarningCodes, WarningContainerLogsUnavailable)
	}
}

func TestCoordinatorOwningEntryWithNoBytesIsUnavailable(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{}, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	if ack.Outcome != types.DebugArchiveOutcomeUnavailable {
		t.Fatalf("outcome = %q, want %q", ack.Outcome, types.DebugArchiveOutcomeUnavailable)
	}
	if ack.ReasonCode != types.DebugArchiveReasonCaptureUnavailable {
		t.Fatalf("reason = %q, want %q", ack.ReasonCode, types.DebugArchiveReasonCaptureUnavailable)
	}
	if fixture.uploads.Load() != 0 {
		t.Fatal("an empty capture must not upload a zero-byte object")
	}
}

func TestCoordinatorCommandBackendReportsUnsupported(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{}, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendCommand})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	if ack.Outcome != types.DebugArchiveOutcomeUnavailable {
		t.Fatalf("outcome = %q, want %q", ack.Outcome, types.DebugArchiveOutcomeUnavailable)
	}
	if ack.ReasonCode != types.DebugArchiveReasonBackendNotSupported {
		t.Fatalf("reason = %q, want %q", ack.ReasonCode, types.DebugArchiveReasonBackendNotSupported)
	}
	if fixture.source.calls.Load() != 0 {
		t.Fatal("an unsupported backend must not be asked for logs")
	}
}

func TestCoordinatorRejectsUnsupportedProtocolVersion(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "x"}, nil)
	request := fixture.request()
	request.ProtocolVersion = 99
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	if ack.Outcome != types.DebugArchiveOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", ack.Outcome, types.DebugArchiveOutcomeFailed)
	}
	if ack.ReasonCode != types.DebugArchiveReasonUnsupportedProtocolVersion {
		t.Fatalf("reason = %q, want %q", ack.ReasonCode, types.DebugArchiveReasonUnsupportedProtocolVersion)
	}
	if fixture.uploads.Load() != 0 {
		t.Fatal("an unsupported protocol version must not contact the target")
	}
}

func TestCoordinatorUnsupportedTransformerUploadsNothing(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "x"}, nil)
	request := fixture.request()
	request.ContentTransformer = types.ContentTransformerDescriptor{Kind: "redact", Version: 3}
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	if ack.ReasonCode != types.DebugArchiveReasonUnsupportedContentTransformer {
		t.Fatalf("reason = %q, want %q", ack.ReasonCode, types.DebugArchiveReasonUnsupportedContentTransformer)
	}
	if fixture.uploads.Load() != 0 {
		t.Fatal("an unsupported transformer must never upload untransformed data")
	}
}

func TestCoordinatorDuplicateRequestReplaysOneAcknowledgement(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "captured output"}, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()
	first := fixture.sender.await(t)

	duplicate := request
	fixture.coordinator.Handle(context.Background(), &duplicate)
	fixture.coordinator.Wait()
	replayed := fixture.sender.await(t)

	if fixture.source.calls.Load() != 1 {
		t.Fatalf("provider reads = %d, want exactly one", fixture.source.calls.Load())
	}
	if fixture.uploads.Load() != 1 {
		t.Fatalf("uploads = %d, want exactly one", fixture.uploads.Load())
	}
	if replayed.SHA256 != first.SHA256 || replayed.Bytes != first.Bytes || replayed.Outcome != first.Outcome {
		t.Fatalf("replayed acknowledgement %+v differs from the original %+v", replayed, first)
	}
}

func TestCoordinatorRejectsReusedRequestIDWithDifferentContent(t *testing.T) {
	fixture := newCoordinatorFixture(t, &fakeSource{output: "captured output"}, nil)
	request := fixture.request()
	fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})

	fixture.coordinator.Handle(context.Background(), &request)
	fixture.coordinator.Wait()
	fixture.sender.await(t)

	conflicting := request
	conflicting.ArchiveID = "a-different-archive"
	fixture.coordinator.Handle(context.Background(), &conflicting)
	fixture.coordinator.Wait()

	ack := fixture.sender.await(t)
	if ack.Outcome != types.DebugArchiveOutcomeFailed {
		t.Fatalf("outcome = %q, want %q", ack.Outcome, types.DebugArchiveOutcomeFailed)
	}
	if ack.ReasonCode != types.DebugArchiveReasonInvalidRequest {
		t.Fatalf("reason = %q, want %q", ack.ReasonCode, types.DebugArchiveReasonInvalidRequest)
	}
	if fixture.uploads.Load() != 1 {
		t.Fatalf("uploads = %d, want the conflicting request to upload nothing", fixture.uploads.Load())
	}
}

func TestCoordinatorReportsExpiryWhenQueuedPastTheDeadline(t *testing.T) {
	blocked := make(chan struct{})
	source := &fakeSource{output: "captured output", block: blocked, started: make(chan string, 4)}
	fixture := newCoordinatorFixture(t, source, func(c *Config) { c.MaxConcurrentUploads = 1 })

	first := fixture.request()
	fixture.ownership.own(first.RunID, first.ExecutionID, Ownership{BackendKind: BackendDocker})
	fixture.coordinator.Handle(context.Background(), &first)
	source.awaitStart(t, first.ExecutionID)

	// A second request for a different execution has to wait for the only
	// upload slot, and its deadline lapses while it does.
	second := fixture.request()
	second.RequestID = "second-request"
	second.ExecutionID = "second-execution"
	second.ExpiresAt = time.Now().Add(150 * time.Millisecond)
	fixture.ownership.own(second.RunID, second.ExecutionID, Ownership{BackendKind: BackendDocker})
	fixture.coordinator.Handle(context.Background(), &second)

	time.Sleep(300 * time.Millisecond)
	close(blocked)
	fixture.coordinator.Wait()

	var expired *types.DebugArchiveLogsUploadedMessage
	fixture.sender.mu.Lock()
	for _, ack := range fixture.sender.acks {
		if ack.RequestID == "second-request" {
			expired = ack
		}
	}
	fixture.sender.mu.Unlock()

	if expired == nil {
		t.Fatal("the queued request produced no acknowledgement")
	}
	if expired.ReasonCode != types.DebugArchiveReasonUploadExpired {
		t.Fatalf("reason = %q, want %q", expired.ReasonCode, types.DebugArchiveReasonUploadExpired)
	}
}

func TestCoordinatorReportsCleanupGraceExpiryWhenOwnershipLapses(t *testing.T) {
	blocked := make(chan struct{})
	source := &fakeSource{output: "captured output", block: blocked, started: make(chan string, 4)}
	fixture := newCoordinatorFixture(t, source, func(c *Config) { c.MaxConcurrentUploads = 1 })

	holding := fixture.request()
	fixture.ownership.own(holding.RunID, holding.ExecutionID, Ownership{BackendKind: BackendDocker})
	fixture.coordinator.Handle(context.Background(), &holding)
	source.awaitStart(t, holding.ExecutionID)

	lapsing := fixture.request()
	lapsing.RequestID = "lapsing-request"
	lapsing.ExecutionID = "lapsing-execution"
	fixture.ownership.own(lapsing.RunID, lapsing.ExecutionID, Ownership{BackendKind: BackendDocker, InCleanupGrace: true})
	fixture.coordinator.Handle(context.Background(), &lapsing)

	// The grace deadline passes while the request waits for a slot.
	time.Sleep(100 * time.Millisecond)
	fixture.ownership.release(lapsing.RunID, lapsing.ExecutionID)
	close(blocked)
	fixture.coordinator.Wait()

	var lapsed *types.DebugArchiveLogsUploadedMessage
	fixture.sender.mu.Lock()
	for _, ack := range fixture.sender.acks {
		if ack.RequestID == "lapsing-request" {
			lapsed = ack
		}
	}
	fixture.sender.mu.Unlock()

	if lapsed == nil {
		t.Fatal("the lapsing request produced no acknowledgement")
	}
	if lapsed.ReasonCode != types.DebugArchiveReasonCleanupGraceExpired {
		t.Fatalf("reason = %q, want %q", lapsed.ReasonCode, types.DebugArchiveReasonCleanupGraceExpired)
	}
}

func TestCoordinatorSemaphoreBoundsConcurrentHandlers(t *testing.T) {
	blocked := make(chan struct{})
	source := &fakeSource{output: "captured output", block: blocked, started: make(chan string, 8)}
	fixture := newCoordinatorFixture(t, source, func(c *Config) { c.MaxConcurrentUploads = 1 })

	for i := 0; i < 3; i++ {
		request := fixture.request()
		request.RequestID = "request-" + string(rune('a'+i))
		request.ExecutionID = "execution-" + string(rune('a'+i))
		fixture.ownership.own(request.RunID, request.ExecutionID, Ownership{BackendKind: BackendDocker})
		fixture.coordinator.Handle(context.Background(), &request)
	}

	time.Sleep(200 * time.Millisecond)
	if got := source.calls.Load(); got != 1 {
		t.Fatalf("concurrent provider reads = %d, want the configured limit of 1", got)
	}

	close(blocked)
	fixture.coordinator.Wait()
	if got := source.calls.Load(); got != 3 {
		t.Fatalf("total provider reads = %d, want 3 once the slot frees", got)
	}
}
