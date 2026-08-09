package debuglog

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

func readAll(snapshot *Snapshot) ([]byte, error) {
	return io.ReadAll(snapshot.Open())
}

func newTestSnapshot(t *testing.T, payload string) *Snapshot {
	t.Helper()
	store := newTestStore(t, nil)
	snapshot, err := store.NewSnapshot(BackendDirect, noopTransformer{}, 1<<15)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	t.Cleanup(snapshot.Close)

	if err := snapshot.Sink().WriteChunk(Chunk{Phase: PhaseAgent, Stream: StreamStdout, Data: []byte(payload)}); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if err := snapshot.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return snapshot
}

// newTestUploader returns an uploader whose retry backoff is instant, so the
// retry policy is exercised without slowing the suite.
func newTestUploader(now func() time.Time) *Uploader {
	uploader := NewUploader(&http.Client{Timeout: 5 * time.Second}, now)
	uploader.sleep = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}
	return uploader
}

func TestUploadPutSendsExactlyTheSnapshotWithSuppliedHeaders(t *testing.T) {
	snapshot := newTestSnapshot(t, "put payload")
	want, err := readAll(snapshot)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}

	var gotBody []byte
	var gotHeader, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeader = r.Header.Get("X-Provider-Token")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	uploader := newTestUploader(time.Now)
	target := types.UploadTarget{
		URL:     server.URL,
		Method:  http.MethodPut,
		Headers: map[string]string{"X-Provider-Token": "signed"},
	}
	if err := uploader.Upload(context.Background(), target, snapshot, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotHeader != "signed" {
		t.Errorf("X-Provider-Token = %q, want %q", gotHeader, "signed")
	}
	if string(gotBody) != string(want) {
		t.Errorf("uploaded body did not match the snapshot bytes")
	}
}

func TestUploadPostStreamsMultipartFieldsAndOneFilePart(t *testing.T) {
	snapshot := newTestSnapshot(t, "post payload")
	want, err := readAll(snapshot)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}

	gotFields := map[string]string{}
	var gotFile []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if parseErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, partErr := reader.NextPart()
			if partErr != nil {
				break
			}
			body, _ := io.ReadAll(part)
			if part.FileName() != "" {
				gotFile = body
			} else {
				gotFields[part.FormName()] = string(body)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	uploader := newTestUploader(time.Now)
	target := types.UploadTarget{
		URL:             server.URL,
		Method:          http.MethodPost,
		MultipartFields: map[string]string{"key": "archives/candidate.ndjson", "policy": "signed-policy"},
	}
	if err := uploader.Upload(context.Background(), target, snapshot, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if gotFields["key"] != "archives/candidate.ndjson" || gotFields["policy"] != "signed-policy" {
		t.Errorf("multipart fields = %v, want the supplied provider fields", gotFields)
	}
	if string(gotFile) != string(want) {
		t.Error("multipart file part did not match the snapshot bytes")
	}
}

func TestUploadRetriesTransientStatusesThenSucceeds(t *testing.T) {
	snapshot := newTestSnapshot(t, "retry payload")

	var attempts atomic.Int32
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		switch attempts.Add(1) {
		case 1:
			w.WriteHeader(http.StatusRequestTimeout)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		case 3:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	uploader := NewUploader(&http.Client{Timeout: 5 * time.Second}, time.Now)
	uploader.sleep = func(context.Context, time.Duration) error { return nil }

	target := types.UploadTarget{URL: server.URL, Method: http.MethodPut}
	if err := uploader.Upload(context.Background(), target, snapshot, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if attempts.Load() != 4 {
		t.Fatalf("attempts = %d, want 4", attempts.Load())
	}
	for i, body := range bodies {
		if body != bodies[0] {
			t.Fatalf("attempt %d replayed different bytes than the first attempt", i)
		}
	}
}

func TestUploadTreatsOther4xxAsTerminalRejection(t *testing.T) {
	snapshot := newTestSnapshot(t, "rejected payload")

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	uploader := newTestUploader(time.Now)
	target := types.UploadTarget{URL: server.URL, Method: http.MethodPut}
	err := uploader.Upload(context.Background(), target, snapshot, time.Now().Add(time.Minute))

	var uploadErr *UploadError
	if !errors.As(err, &uploadErr) {
		t.Fatalf("error = %v, want an *UploadError", err)
	}
	if uploadErr.ReasonCode != types.DebugArchiveReasonUploadRejected {
		t.Fatalf("reason = %q, want %q", uploadErr.ReasonCode, types.DebugArchiveReasonUploadRejected)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want no retry after a terminal rejection", attempts.Load())
	}
}

func TestUploadRejectsRedirectsWithoutForwardingSignedMaterial(t *testing.T) {
	snapshot := newTestSnapshot(t, "redirect payload")

	var redirectTargetHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	uploader := newTestUploader(time.Now)
	target := types.UploadTarget{
		URL:     server.URL,
		Method:  http.MethodPut,
		Headers: map[string]string{"X-Provider-Token": "signed"},
	}
	err := uploader.Upload(context.Background(), target, snapshot, time.Now().Add(time.Minute))

	var uploadErr *UploadError
	if !errors.As(err, &uploadErr) {
		t.Fatalf("error = %v, want an *UploadError", err)
	}
	if uploadErr.ReasonCode != types.DebugArchiveReasonUploadRejected {
		t.Fatalf("reason = %q, want %q", uploadErr.ReasonCode, types.DebugArchiveReasonUploadRejected)
	}
	if redirectTargetHits.Load() != 0 {
		t.Fatal("the redirect destination received the snapshot")
	}
	if strings.Contains(uploadErr.Detail, server.URL) {
		t.Fatalf("upload error leaked the target URL: %q", uploadErr.Detail)
	}
}

func TestUploadDoesNotStartAfterExpiry(t *testing.T) {
	snapshot := newTestSnapshot(t, "expired payload")

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	now := time.Now()
	uploader := newTestUploader(func() time.Time { return now })
	target := types.UploadTarget{URL: server.URL, Method: http.MethodPut}
	err := uploader.Upload(context.Background(), target, snapshot, now.Add(-time.Second))

	var uploadErr *UploadError
	if !errors.As(err, &uploadErr) {
		t.Fatalf("error = %v, want an *UploadError", err)
	}
	if uploadErr.ReasonCode != types.DebugArchiveReasonUploadExpired {
		t.Fatalf("reason = %q, want %q", uploadErr.ReasonCode, types.DebugArchiveReasonUploadExpired)
	}
	if attempts.Load() != 0 {
		t.Fatal("an expired request must not contact the target")
	}
}

func TestUploadDoesNotRetryPastExpiry(t *testing.T) {
	snapshot := newTestSnapshot(t, "late payload")

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	now := time.Now()
	uploader := newTestUploader(func() time.Time { return now })
	target := types.UploadTarget{URL: server.URL, Method: http.MethodPut}
	// The deadline is closer than the first backoff, so no retry may start.
	err := uploader.Upload(context.Background(), target, snapshot, now.Add(uploadInitialBackoff/2))

	var uploadErr *UploadError
	if !errors.As(err, &uploadErr) {
		t.Fatalf("error = %v, want an *UploadError", err)
	}
	if uploadErr.ReasonCode != types.DebugArchiveReasonUploadFailed {
		t.Fatalf("reason = %q, want %q", uploadErr.ReasonCode, types.DebugArchiveReasonUploadFailed)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want exactly one before the deadline", attempts.Load())
	}
}

func TestUploadRetriesTransportErrors(t *testing.T) {
	snapshot := newTestSnapshot(t, "transport payload")

	// A closed server produces a connection error rather than a status.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	var sleeps atomic.Int32
	uploader := NewUploader(&http.Client{Timeout: time.Second}, time.Now)
	uploader.sleep = func(context.Context, time.Duration) error {
		if sleeps.Add(1) >= 2 {
			return context.Canceled
		}
		return nil
	}

	target := types.UploadTarget{URL: url, Method: http.MethodPut}
	err := uploader.Upload(context.Background(), target, snapshot, time.Now().Add(time.Minute))
	if err == nil {
		t.Fatal("expected an upload failure against a closed target")
	}
	if sleeps.Load() < 2 {
		t.Fatalf("backoff attempts = %d, want the transport error to be retried", sleeps.Load())
	}
}
