package debuglog

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

var validationNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// goldenRequest is the protocol-v1 request fixture both sides of the contract
// must accept without field translation.
func goldenRequest() types.DebugArchiveLogsRequestedMessage {
	return types.DebugArchiveLogsRequestedMessage{
		ProtocolVersion: types.DebugArchiveProtocolVersion,
		RequestID:       "0f2f7c66-6f9d-4a05-9b4c-8d0f4f1e2a11",
		ArchiveID:       "3b3f6a1e-3f39-4a8d-9d7a-1f4c8f0a0b22",
		CollectionID:    "9c4f1a72-5d21-4a99-bb61-5b1d0f6a7c33",
		RunID:           "6c2c0f19-2c6f-4d1f-9f2a-0f6d1b3a4c44",
		ExecutionID:     "7d3d1f2a-3d7f-4e2f-8a3b-1f7e2c4b5d55",
		RequestedFormat: types.DebugArchiveFormatNDJSON,
		ExpiresAt:       validationNow.Add(MaxRequestLifetime),
		MaxBytes:        ProtocolCeilingBytes,
		ContentTransformer: types.ContentTransformerDescriptor{
			Kind:    TransformerKindNoop,
			Version: 1,
		},
		UploadTarget: types.UploadTarget{
			URL:     "https://storage.example.com/archives/candidate.ndjson?X-Signature=abc",
			Method:  http.MethodPut,
			Headers: map[string]string{"Content-Type": types.DebugArchiveFormatNDJSON},
		},
	}
}

func TestValidateRequestAcceptsGoldenFixture(t *testing.T) {
	request := goldenRequest()

	effective, err := ValidateRequest(&request, validationNow, ProtocolCeilingBytes)
	if err != nil {
		t.Fatalf("golden protocol-v1 request was rejected: %v", err)
	}
	if effective != ProtocolCeilingBytes {
		t.Fatalf("effective bound = %d, want %d", effective, ProtocolCeilingBytes)
	}
}

func TestValidateRequestUsesTheLowerOfRequestAndConfiguredBounds(t *testing.T) {
	request := goldenRequest()
	request.MaxBytes = 8 << 20

	t.Run("request bound is lower", func(t *testing.T) {
		effective, err := ValidateRequest(&request, validationNow, ProtocolCeilingBytes)
		if err != nil {
			t.Fatalf("ValidateRequest: %v", err)
		}
		if effective != 8<<20 {
			t.Fatalf("effective bound = %d, want %d", effective, 8<<20)
		}
	})

	t.Run("configured bound is lower", func(t *testing.T) {
		effective, err := ValidateRequest(&request, validationNow, 1<<20)
		if err != nil {
			t.Fatalf("ValidateRequest: %v", err)
		}
		if effective != 1<<20 {
			t.Fatalf("effective bound = %d, want %d", effective, 1<<20)
		}
	})
}

func TestValidateRequestIgnoresUnknownOptionalFields(t *testing.T) {
	// json.Unmarshal into the request DTO discards fields this worker does not
	// know, which is what lets a newer server add optional data safely.
	request := goldenRequest()
	if _, err := ValidateRequest(&request, validationNow, ProtocolCeilingBytes); err != nil {
		t.Fatalf("ValidateRequest: %v", err)
	}
}

func TestValidateRequestRejections(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*types.DebugArchiveLogsRequestedMessage)
		wantReason string
	}{
		{
			name:       "unsupported protocol version",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.ProtocolVersion = 2 },
			wantReason: types.DebugArchiveReasonUnsupportedProtocolVersion,
		},
		{
			name:       "missing request id",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.RequestID = "  " },
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name:       "missing execution id",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.ExecutionID = "" },
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name:       "identifier with control characters",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.ArchiveID = "abc\ndef" },
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name:       "unsupported format",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.RequestedFormat = "application/json" },
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name:       "already expired",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.ExpiresAt = validationNow.Add(-time.Second) },
			wantReason: types.DebugArchiveReasonRequestExpired,
		},
		{
			name: "expiry beyond the protocol lifetime",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.ExpiresAt = validationNow.Add(MaxRequestLifetime + time.Minute)
			},
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name:       "non-positive bound",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.MaxBytes = 0 },
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name:       "bound above the protocol ceiling",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.MaxBytes = ProtocolCeilingBytes + 1 },
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name: "unsupported content transformer",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.ContentTransformer = types.ContentTransformerDescriptor{Kind: "redact", Version: 1}
			},
			wantReason: types.DebugArchiveReasonUnsupportedContentTransformer,
		},
		{
			name: "unsupported transformer version",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.ContentTransformer = types.ContentTransformerDescriptor{Kind: TransformerKindNoop, Version: 2}
			},
			wantReason: types.DebugArchiveReasonUnsupportedContentTransformer,
		},
		{
			name:       "unsupported upload method",
			mutate:     func(r *types.DebugArchiveLogsRequestedMessage) { r.UploadTarget.Method = http.MethodPatch },
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name: "PUT target with multipart fields",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.UploadTarget.MultipartFields = map[string]string{"key": "value"}
			},
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name: "POST target without multipart fields",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.UploadTarget.Method = http.MethodPost
			},
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name: "plain HTTP target outside loopback",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.UploadTarget.URL = "http://storage.example.com/candidate.ndjson"
			},
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name: "non-HTTP scheme",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.UploadTarget.URL = "file:///etc/passwd"
			},
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
		{
			name: "header with control characters",
			mutate: func(r *types.DebugArchiveLogsRequestedMessage) {
				r.UploadTarget.Headers = map[string]string{"X-Bad": "a\rb"}
			},
			wantReason: types.DebugArchiveReasonInvalidRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := goldenRequest()
			tc.mutate(&request)

			_, err := ValidateRequest(&request, validationNow, ProtocolCeilingBytes)
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error = %v, want a *ValidationError", err)
			}
			if validation.ReasonCode != tc.wantReason {
				t.Fatalf("reason = %q, want %q", validation.ReasonCode, tc.wantReason)
			}
		})
	}
}

func TestValidateRequestAllowsLoopbackHTTPForLocalTesting(t *testing.T) {
	request := goldenRequest()
	request.UploadTarget.URL = "http://127.0.0.1:8080/candidate.ndjson"

	if _, err := ValidateRequest(&request, validationNow, ProtocolCeilingBytes); err != nil {
		t.Fatalf("a loopback HTTP target must be accepted for local testing: %v", err)
	}
}

func TestSanitizeMessageBoundsAndStripsControlCharacters(t *testing.T) {
	sanitized := SanitizeMessage("upload\nrejected\ttarget")
	if strings.ContainsAny(sanitized, "\n\t") {
		t.Fatalf("sanitized message retained control characters: %q", sanitized)
	}
	if sanitized != "upload rejected target" {
		t.Fatalf("sanitized = %q, want %q", sanitized, "upload rejected target")
	}

	long := SanitizeMessage(strings.Repeat("a", MaxAckMessageBytes*2))
	if len(long) != MaxAckMessageBytes {
		t.Fatalf("sanitized length = %d, want %d", len(long), MaxAckMessageBytes)
	}
}

func TestWarningSetDeduplicatesSortsAndCaps(t *testing.T) {
	var set warningSet
	set.add(WarningOutputDropped)
	set.add(WarningContainerLogsUnavailable)
	set.add(WarningOutputDropped)
	set.add("")

	codes := set.codes()
	if len(codes) != 2 {
		t.Fatalf("codes = %v, want two deduplicated entries", codes)
	}
	if codes[0] != WarningContainerLogsUnavailable || codes[1] != WarningOutputDropped {
		t.Fatalf("codes = %v, want them sorted", codes)
	}

	for i := 0; i < MaxWarningCodes*2; i++ {
		set.add(strings.Repeat("w", i+1))
	}
	if got := len(set.codes()); got != MaxWarningCodes {
		t.Fatalf("codes = %d, want the %d cap", got, MaxWarningCodes)
	}
}
