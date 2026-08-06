package metrics

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Debug-archive metrics deliberately carry no run, execution, or request
// identifier as a label. Those identifiers belong on task span events, where
// they cannot blow up metric cardinality.

// Ownership states reported alongside a debug-archive request outcome.
const (
	DebugArchiveOwnershipActive       = "active"
	DebugArchiveOwnershipCleanupGrace = "cleanup_grace"
)

// debugArchiveInstruments holds the debug-archive metric set. It is built and
// swapped alongside the main instrument set.
type debugArchiveInstruments struct {
	requests          metric.Int64Counter
	snapshotDuration  metric.Float64Histogram
	snapshotBytes     metric.Int64Histogram
	uploads           metric.Int64Counter
	uploadDuration    metric.Float64Histogram
	captureBytes      metric.Int64Gauge
	cleanupGraceCount metric.Int64Gauge
	cleanupResults    metric.Int64Counter
	requestsInFlight  metric.Int64UpDownCounter
	truncations       metric.Int64Counter
}

func buildDebugArchiveInstruments(m metric.Meter) (*debugArchiveInstruments, error) {
	requests, err := m.Int64Counter(
		"oz_worker_debug_archive_requests_total",
		metric.WithDescription("Debug-archive log requests this worker owned, labeled by backend, outcome, and stable reason."),
	)
	if err != nil {
		return nil, err
	}
	snapshotDuration, err := m.Float64Histogram(
		"oz_worker_debug_archive_snapshot_duration_seconds",
		metric.WithDescription("Wall-clock duration of a debug-archive log snapshot, labeled by backend."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	snapshotBytes, err := m.Int64Histogram(
		"oz_worker_debug_archive_snapshot_bytes",
		metric.WithDescription("Size of the NDJSON object a debug-archive snapshot produced, labeled by backend."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}
	uploads, err := m.Int64Counter(
		"oz_worker_debug_archive_uploads_total",
		metric.WithDescription("Debug-archive snapshot uploads, labeled by result."),
	)
	if err != nil {
		return nil, err
	}
	uploadDuration, err := m.Float64Histogram(
		"oz_worker_debug_archive_upload_duration_seconds",
		metric.WithDescription("Wall-clock duration of a debug-archive snapshot upload, labeled by result."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	captureBytes, err := m.Int64Gauge(
		"oz_worker_debug_archive_capture_bytes",
		metric.WithDescription("Bytes currently reserved by direct-execution captures and request snapshots."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}
	cleanupGraceCount, err := m.Int64Gauge(
		"oz_worker_cleanup_grace_entries",
		metric.WithDescription("Executions this worker is retaining through their idle-on-complete cleanup grace."),
	)
	if err != nil {
		return nil, err
	}
	cleanupResults, err := m.Int64Counter(
		"oz_worker_cleanup_grace_results_total",
		metric.WithDescription("Backend resource cleanups performed at cleanup-grace expiry, labeled by backend and result."),
	)
	if err != nil {
		return nil, err
	}
	requestsInFlight, err := m.Int64UpDownCounter(
		"oz_worker_debug_archive_requests_in_flight",
		metric.WithDescription("Debug-archive log requests currently being snapshotted or uploaded."),
	)
	if err != nil {
		return nil, err
	}
	truncations, err := m.Int64Counter(
		"oz_worker_debug_archive_truncations_total",
		metric.WithDescription("Debug-archive snapshots that dropped bytes to stay within their bound, labeled by backend."),
	)
	if err != nil {
		return nil, err
	}

	return &debugArchiveInstruments{
		requests:          requests,
		snapshotDuration:  snapshotDuration,
		snapshotBytes:     snapshotBytes,
		uploads:           uploads,
		uploadDuration:    uploadDuration,
		captureBytes:      captureBytes,
		cleanupGraceCount: cleanupGraceCount,
		cleanupResults:    cleanupResults,
		requestsInFlight:  requestsInFlight,
		truncations:       truncations,
	}, nil
}

// RecordDebugArchiveRequest records one owned request's terminal outcome.
func RecordDebugArchiveRequest(backend, ownership, outcome, reasonCode string) {
	current().debugArchive.requests.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("backend", backend),
			attribute.String("ownership", ownership),
			attribute.String("outcome", outcome),
			attribute.String("reason", reasonCode),
		),
	)
}

// RecordDebugArchiveSnapshot records the cost of producing one snapshot.
func RecordDebugArchiveSnapshot(backend string, duration time.Duration, bytes int64) {
	attrs := metric.WithAttributes(attribute.String("backend", backend))
	current().debugArchive.snapshotDuration.Record(context.Background(), duration.Seconds(), attrs)
	current().debugArchive.snapshotBytes.Record(context.Background(), bytes, attrs)
}

// RecordDebugArchiveTruncation records a snapshot that had to drop bytes.
func RecordDebugArchiveTruncation(backend string) {
	current().debugArchive.truncations.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("backend", backend)),
	)
}

// RecordDebugArchiveUpload records one upload attempt's terminal result.
func RecordDebugArchiveUpload(result string, duration time.Duration) {
	attrs := metric.WithAttributes(attribute.String("result", result))
	current().debugArchive.uploads.Add(context.Background(), 1, attrs)
	current().debugArchive.uploadDuration.Record(context.Background(), duration.Seconds(), attrs)
}

// SetDebugArchiveCaptureBytes records the disk budget currently committed to
// captures and request snapshots.
func SetDebugArchiveCaptureBytes(bytes int64) {
	current().debugArchive.captureBytes.Record(context.Background(), bytes)
}

// SetCleanupGraceEntries records how many executions are being retained for
// post-terminal log retrieval.
func SetCleanupGraceEntries(count int) {
	current().debugArchive.cleanupGraceCount.Record(context.Background(), int64(count))
}

// RecordCleanupGraceResult records a backend resource cleanup performed when an
// execution's cleanup grace expired.
func RecordCleanupGraceResult(backend, result string) {
	current().debugArchive.cleanupResults.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("backend", backend),
			attribute.String("result", result),
		),
	)
}

// IncDebugArchiveRequestsInFlight marks a request as entering snapshot/upload.
func IncDebugArchiveRequestsInFlight() {
	current().debugArchive.requestsInFlight.Add(context.Background(), 1)
}

// DecDebugArchiveRequestsInFlight marks a request as leaving snapshot/upload.
func DecDebugArchiveRequestsInFlight() {
	current().debugArchive.requestsInFlight.Add(context.Background(), -1)
}
