package debuglog

import (
	"errors"
	"strconv"

	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// PartialSnapshotError reports that a backend wrote valid data for some
// sources but could not read others. The coordinator uploads the bytes that
// were written and marks the capture partial rather than discarding them.
type PartialSnapshotError struct {
	WarningCodes []string
}

func (e *PartialSnapshotError) Error() string {
	return "debuglog: snapshot completed with partial provider data"
}

// SnapshotError is a backend snapshot failure that produced no usable data. Its
// reason code becomes the acknowledgement's, so it must stay in the bounded
// vocabulary and its detail must never carry provider text.
type SnapshotError struct {
	ReasonCode string
	Detail     string
}

func (e *SnapshotError) Error() string {
	return "debuglog: " + e.Detail + " (" + e.ReasonCode + ")"
}

// NewSnapshotError builds a typed snapshot failure.
func NewSnapshotError(reasonCode, detail string) error {
	return &SnapshotError{ReasonCode: reasonCode, Detail: detail}
}

// ErrBackendNotSupported is the typed error a backend without a log API
// returns from SnapshotLogs.
var ErrBackendNotSupported = &SnapshotError{
	ReasonCode: types.DebugArchiveReasonBackendNotSupported,
	Detail:     "backend does not expose execution logs",
}

// reasonForSnapshotError maps a backend error onto the acknowledgement's
// bounded reason vocabulary.
func reasonForSnapshotError(err error) string {
	var snapshotErr *SnapshotError
	if errors.As(err, &snapshotErr) {
		return snapshotErr.ReasonCode
	}
	return types.DebugArchiveReasonSnapshotFailed
}

func itoa(value int) string { return strconv.Itoa(value) }

func itoa64(value int64) string { return strconv.FormatInt(value, 10) }
