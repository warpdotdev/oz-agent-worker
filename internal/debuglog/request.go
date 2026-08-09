package debuglog

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

const (
	// ProtocolCeilingBytes is the largest snapshot the protocol permits for one
	// execution. A server request may lower it but never raise it.
	ProtocolCeilingBytes int64 = 64 << 20
	// MaxRequestLifetime bounds how far in the future a request's expiry may
	// sit, matching the server's 30-minute presigned target.
	MaxRequestLifetime = 30 * time.Minute
	// MaxAckMessageBytes caps the sanitized human-readable acknowledgement text.
	MaxAckMessageBytes = 256
	// MaxWarningCodes caps how many deduplicated warning codes one
	// acknowledgement carries.
	MaxWarningCodes = 16
)

// Warning codes re-exported so backends and the coordinator share one
// vocabulary with the wire contract.
const (
	WarningContainerLogsUnavailable   = types.DebugArchiveWarningContainerLogsUnavailable
	WarningPreviousLogsUnavailable    = types.DebugArchiveWarningPreviousLogsUnavailable
	WarningOutputDropped              = types.DebugArchiveWarningOutputDropped
	WarningProviderSnapshotIncomplete = types.DebugArchiveWarningProviderSnapshotIncomplete
)

// ValidationError reports a request this worker refuses, carrying the stable
// reason code the acknowledgement reports.
type ValidationError struct {
	ReasonCode string
	Detail     string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("debuglog: %s (%s)", e.Detail, e.ReasonCode)
}

func invalid(detail string) error {
	return &ValidationError{ReasonCode: types.DebugArchiveReasonInvalidRequest, Detail: detail}
}

// ValidateRequest checks every field the worker relies on before it touches a
// backend or the network. The effective output bound is the lower of the
// request's and the worker's configured ceilings.
//
// Callers must establish ownership first: a non-owning process stays silent
// even for a request that would fail validation.
func ValidateRequest(req *types.DebugArchiveLogsRequestedMessage, now time.Time, configuredCeiling int64) (effectiveMaxBytes int64, err error) {
	if req.ProtocolVersion != types.DebugArchiveProtocolVersion {
		return 0, &ValidationError{
			ReasonCode: types.DebugArchiveReasonUnsupportedProtocolVersion,
			Detail:     "unsupported protocol version",
		}
	}

	for name, value := range map[string]string{
		"request_id":    req.RequestID,
		"archive_id":    req.ArchiveID,
		"collection_id": req.CollectionID,
		"run_id":        req.RunID,
		"execution_id":  req.ExecutionID,
	} {
		if strings.TrimSpace(value) == "" {
			return 0, invalid("missing " + name)
		}
		if containsControlCharacters(value) {
			return 0, invalid(name + " contains control characters")
		}
	}

	if req.RequestedFormat != types.DebugArchiveFormatNDJSON {
		return 0, invalid("unsupported requested format")
	}

	if req.ExpiresAt.IsZero() {
		return 0, invalid("missing expires_at")
	}
	if !req.ExpiresAt.After(now) {
		return 0, &ValidationError{
			ReasonCode: types.DebugArchiveReasonRequestExpired,
			Detail:     "request already expired on receipt",
		}
	}
	if req.ExpiresAt.Sub(now) > MaxRequestLifetime {
		return 0, invalid("expires_at exceeds the protocol request lifetime")
	}

	if _, transformerErr := NewTransformer(req.ContentTransformer.Kind, req.ContentTransformer.Version); transformerErr != nil {
		return 0, &ValidationError{
			ReasonCode: types.DebugArchiveReasonUnsupportedContentTransformer,
			Detail:     "unsupported content transformer",
		}
	}

	if req.MaxBytes <= 0 {
		return 0, invalid("max_bytes must be positive")
	}
	if req.MaxBytes > ProtocolCeilingBytes {
		return 0, invalid("max_bytes exceeds the protocol ceiling")
	}
	effectiveMaxBytes = req.MaxBytes
	if configuredCeiling > 0 && configuredCeiling < effectiveMaxBytes {
		effectiveMaxBytes = configuredCeiling
	}

	if err := validateUploadTarget(req.UploadTarget); err != nil {
		return 0, err
	}

	return effectiveMaxBytes, nil
}

func validateUploadTarget(target types.UploadTarget) error {
	switch target.Method {
	case http.MethodPut:
		if len(target.MultipartFields) > 0 {
			return invalid("PUT target must not carry multipart fields")
		}
	case http.MethodPost:
		if len(target.MultipartFields) == 0 {
			return invalid("POST target requires multipart fields")
		}
	default:
		return invalid("upload method must be PUT or POST")
	}

	if containsControlCharacters(target.URL) {
		return invalid("upload target contains control characters")
	}
	parsed, err := url.Parse(target.URL)
	if err != nil {
		return invalid("upload target is not a valid URL")
	}
	if !isPermittedTargetURL(parsed) {
		return invalid("upload target must use HTTPS outside loopback")
	}

	for key, value := range target.Headers {
		if containsControlCharacters(key) || containsControlCharacters(value) {
			return invalid("upload target header contains control characters")
		}
	}
	for key, value := range target.MultipartFields {
		if containsControlCharacters(key) || containsControlCharacters(value) {
			return invalid("upload target field contains control characters")
		}
	}
	return nil
}

// isPermittedTargetURL allows HTTPS everywhere and plain HTTP only against
// loopback, so integration tests and local development can exercise the upload
// path without ever sending customer log bytes over an unencrypted network.
func isPermittedTargetURL(parsed *url.URL) bool {
	switch parsed.Scheme {
	case "https":
		return parsed.Host != ""
	case "http":
		return isLoopbackHost(parsed.Hostname())
	default:
		return false
	}
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return strings.HasPrefix(host, "127.")
	}
}

func containsControlCharacters(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// SanitizeMessage bounds and strips human-readable acknowledgement text. It
// never receives provider output, URLs, headers, local paths, or response
// bodies; this is the last line of defense against accidentally forwarding one.
func SanitizeMessage(message string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, message)
	cleaned = strings.Join(strings.Fields(cleaned), " ")

	if len(cleaned) <= MaxAckMessageBytes {
		return cleaned
	}
	truncated := cleaned[:MaxAckMessageBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

// warningSet deduplicates warning codes and caps them at MaxWarningCodes so an
// acknowledgement stays bounded regardless of how many sources degrade.
type warningSet struct {
	seen  map[string]struct{}
	order []string
}

func (w *warningSet) add(code string) {
	if code == "" || len(w.order) >= MaxWarningCodes {
		return
	}
	if w.seen == nil {
		w.seen = make(map[string]struct{}, MaxWarningCodes)
	}
	if _, ok := w.seen[code]; ok {
		return
	}
	w.seen[code] = struct{}{}
	w.order = append(w.order, code)
}

// codes returns the collected warnings sorted so acknowledgements for the same
// degradation are byte-identical across runs.
func (w *warningSet) codes() []string {
	out := make([]string, len(w.order))
	copy(out, w.order)
	sort.Strings(out)
	return out
}
