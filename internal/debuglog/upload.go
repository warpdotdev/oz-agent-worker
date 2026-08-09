package debuglog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

const (
	// uploadFilePartName is the multipart field name for the snapshot object.
	// Presigned POST policies conventionally sign the file part last, under
	// this name.
	uploadFilePartName = "file"
	// uploadFileName is the filename reported in the multipart file part.
	uploadFileName = "worker-logs.ndjson"

	uploadInitialBackoff = 500 * time.Millisecond
	uploadMaxBackoff     = 15 * time.Second
	uploadBackoffRate    = 2.0
	// uploadResponseDrainLimit bounds how much of a response body is read
	// before the connection is reused. The bytes are discarded, never logged.
	uploadResponseDrainLimit = 4 << 10
)

// UploadError reports a terminal upload failure with the stable reason code
// the acknowledgement carries. It deliberately excludes the target, the signed
// query, request headers, and the response body.
type UploadError struct {
	ReasonCode string
	Detail     string
}

func (e *UploadError) Error() string {
	return fmt.Sprintf("debuglog: %s (%s)", e.Detail, e.ReasonCode)
}

// Uploader sends a finalized snapshot to the server-supplied target.
type Uploader struct {
	client *http.Client
	now    func() time.Time
	// sleep waits between retries; tests substitute it to avoid real delays.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewUploader builds an uploader whose HTTP client refuses redirects, so a
// signed target can never bounce the snapshot, its headers, or its body to
// another host.
func NewUploader(client *http.Client, now func() time.Time) *Uploader {
	if client == nil {
		client = &http.Client{Timeout: MaxRequestLifetime}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if now == nil {
		now = time.Now
	}
	return &Uploader{client: client, now: now, sleep: sleepContext}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Upload sends the snapshot, retrying transient failures with bounded
// exponential backoff until expiresAt. Every attempt reopens the immutable
// local object so a retry is byte-identical.
func (u *Uploader) Upload(ctx context.Context, target types.UploadTarget, snapshot *Snapshot, expiresAt time.Time) error {
	backoff := uploadInitialBackoff
	var lastDetail string

	for {
		if !u.now().Before(expiresAt) {
			return &UploadError{
				ReasonCode: types.DebugArchiveReasonUploadExpired,
				Detail:     "upload target expired before the snapshot was accepted",
			}
		}

		status, err := u.attempt(ctx, target, snapshot)
		var terminal *UploadError
		switch {
		case err == nil && status >= 200 && status < 300:
			return nil
		case err == nil && isRetryableStatus(status):
			lastDetail = fmt.Sprintf("upload target returned a retryable status (%d)", status)
		case err == nil:
			return &UploadError{
				ReasonCode: types.DebugArchiveReasonUploadRejected,
				Detail:     fmt.Sprintf("upload target rejected the snapshot (%d)", status),
			}
		case errors.As(err, &terminal):
			// The attempt already classified this as unrecoverable, such as a
			// target that tried to redirect the snapshot elsewhere.
			return terminal
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return &UploadError{
				ReasonCode: types.DebugArchiveReasonWorkerShuttingDown,
				Detail:     "upload cancelled before completion",
			}
		default:
			lastDetail = "upload transport error"
		}

		// Waiting past the deadline is pointless: the target is already
		// unusable, so report expiry instead of burning another attempt.
		if !u.now().Add(backoff).Before(expiresAt) {
			return &UploadError{
				ReasonCode: types.DebugArchiveReasonUploadFailed,
				Detail:     lastDetail,
			}
		}
		if err := u.sleep(ctx, backoff); err != nil {
			return &UploadError{
				ReasonCode: types.DebugArchiveReasonWorkerShuttingDown,
				Detail:     "upload cancelled before completion",
			}
		}
		backoff = min(time.Duration(float64(backoff)*uploadBackoffRate), uploadMaxBackoff)
	}
}

func isRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// attempt performs one upload and returns the response status. The response
// body is drained under a small cap and discarded without being read into an
// error or acknowledgement.
func (u *Uploader) attempt(ctx context.Context, target types.UploadTarget, snapshot *Snapshot) (int, error) {
	req, err := u.buildRequest(ctx, target, snapshot)
	if err != nil {
		return 0, err
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, uploadResponseDrainLimit))
		_ = resp.Body.Close()
	}()

	// CheckRedirect surfaces redirects as ordinary responses, so a 3xx here
	// means the target tried to send the snapshot elsewhere. It never gets a
	// second request carrying the signed headers or body.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return 0, &UploadError{
			ReasonCode: types.DebugArchiveReasonUploadRejected,
			Detail:     "upload target attempted a redirect",
		}
	}
	return resp.StatusCode, nil
}

func (u *Uploader) buildRequest(ctx context.Context, target types.UploadTarget, snapshot *Snapshot) (*http.Request, error) {
	if target.Method == http.MethodPost {
		return u.buildMultipartRequest(ctx, target, snapshot)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.URL, io.NopCloser(snapshot.Open()))
	if err != nil {
		return nil, err
	}
	req.ContentLength = snapshot.Bytes()
	applyTargetHeaders(req, target.Headers)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", types.DebugArchiveFormatNDJSON)
	}
	return req, nil
}

// buildMultipartRequest streams the form so the snapshot is never held in
// memory. The file part is written last, matching presigned POST policies.
func (u *Uploader) buildMultipartRequest(ctx context.Context, target types.UploadTarget, snapshot *Snapshot) (*http.Request, error) {
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)

	go func() {
		err := func() error {
			for _, field := range sortedPairs(target.MultipartFields) {
				if err := form.WriteField(field.key, field.value); err != nil {
					return err
				}
			}
			part, err := form.CreateFormFile(uploadFilePartName, uploadFileName)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, snapshot.Open()); err != nil {
				return err
			}
			return form.Close()
		}()
		_ = writer.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, reader)
	if err != nil {
		_ = reader.Close()
		return nil, err
	}
	applyTargetHeaders(req, target.Headers)
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req, nil
}

func applyTargetHeaders(req *http.Request, headers map[string]string) {
	for _, header := range sortedPairs(headers) {
		req.Header.Set(header.key, header.value)
	}
}

type keyValue struct {
	key   string
	value string
}

// sortedPairs orders a target's headers and fields by key so repeated attempts
// build byte-identical requests instead of depending on map iteration order.
func sortedPairs(values map[string]string) []keyValue {
	pairs := make([]keyValue, 0, len(values))
	for key, value := range values {
		pairs = append(pairs, keyValue{key: key, value: value})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	return pairs
}
