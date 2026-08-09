package debuglog

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/warpdotdev/oz-agent-worker/internal/log"
	"github.com/warpdotdev/oz-agent-worker/internal/metrics"
	"github.com/warpdotdev/oz-agent-worker/internal/types"
)

// maxCachedRequests bounds the idempotency cache.
const maxCachedRequests = 1024

// snapshotFinalizeDeadline bounds how long a terminal direct capture may keep
// draining before a snapshot reads it.
const snapshotFinalizeDeadline = 2 * time.Second

// Ownership describes this process's claim over one exact (run, execution)
// pair. Only the process that executed the assignment produces one.
type Ownership struct {
	// BackendKind is the backend that executed the assignment.
	BackendKind string
	// InCleanupGrace reports whether the execution has already reported
	// terminal state and is being retained for its cleanup grace.
	InCleanupGrace bool
	// Capture is the direct backend's bounded output capture, if any.
	Capture *TaskLogCapture
}

// state names the ownership phase this request was served from, for metrics.
func (o Ownership) state() string {
	if o.InCleanupGrace {
		return metrics.DebugArchiveOwnershipCleanupGrace
	}
	return metrics.DebugArchiveOwnershipActive
}

// OwnershipLookup resolves an exact (run, execution) pair to this process's
// claim. A false result means this process did not execute the assignment and
// must stay silent.
type OwnershipLookup interface {
	LookupExecution(runID, executionID string) (Ownership, bool)
}

// SnapshotSource writes an execution's provider logs into a sink. It is
// implemented by the worker's backends.
type SnapshotSource interface {
	SnapshotLogs(ctx context.Context, runID, executionID string, sink Sink) error
}

// Sender enqueues an acknowledgement through the worker's single WebSocket
// writer.
type Sender interface {
	SendDebugArchiveAck(ack *types.DebugArchiveLogsUploadedMessage) error
}

// Coordinator runs debug-archive log requests off the WebSocket read loop, so
// a slow provider read or upload can never delay heartbeats, cancellations, or
// later assignments.
type Coordinator struct {
	ownership OwnershipLookup
	source    SnapshotSource
	sender    Sender
	store     *Store
	uploader  *Uploader
	now       func() time.Time

	uploadSlots chan struct{}

	mu       sync.Mutex
	cache    map[string]*cacheEntry
	perExec  map[executionKey]*sync.Mutex
	inflight sync.WaitGroup
}

type executionKey struct {
	runID       string
	executionID string
}

// cacheEntry makes duplicate delivery of one request_id idempotent. It records
// only identifiers and result metadata, never the upload target.
type cacheEntry struct {
	fingerprint string
	retainUntil time.Time

	done chan struct{}
	ack  *types.DebugArchiveLogsUploadedMessage
}

// CoordinatorOptions configures a Coordinator.
type CoordinatorOptions struct {
	Ownership OwnershipLookup
	Source    SnapshotSource
	Sender    Sender
	Store     *Store
	Uploader  *Uploader
	// Now supplies the coordinator's clock; nil uses time.Now.
	Now func() time.Time
}

// NewCoordinator builds a coordinator bounded by the store's configured upload
// concurrency.
func NewCoordinator(opts CoordinatorOptions) *Coordinator {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	uploader := opts.Uploader
	if uploader == nil {
		uploader = NewUploader(nil, now)
	}
	return &Coordinator{
		ownership:   opts.Ownership,
		source:      opts.Source,
		sender:      opts.Sender,
		store:       opts.Store,
		uploader:    uploader,
		now:         now,
		uploadSlots: make(chan struct{}, opts.Store.Config().MaxConcurrentUploads),
		cache:       make(map[string]*cacheEntry),
		perExec:     make(map[executionKey]*sync.Mutex),
	}
}

// Handle dispatches a request to a background goroutine and returns
// immediately. The caller is the WebSocket read loop.
func (c *Coordinator) Handle(ctx context.Context, req *types.DebugArchiveLogsRequestedMessage) {
	c.inflight.Add(1)
	go func() {
		defer c.inflight.Done()
		c.process(ctx, req)
	}()
}

// Wait blocks until in-flight requests finish. Worker shutdown cancels their
// context first, so this does not extend shutdown beyond the bounded backend
// stop.
func (c *Coordinator) Wait() { c.inflight.Wait() }

func (c *Coordinator) process(ctx context.Context, req *types.DebugArchiveLogsRequestedMessage) {
	// Ownership is checked before validation: a process that did not execute
	// this assignment must produce no acknowledgement, log, ID-bearing metric,
	// or cache entry, even for a request it would otherwise reject.
	owner, owns := c.ownership.LookupExecution(req.RunID, req.ExecutionID)
	if !owns {
		return
	}

	effectiveMaxBytes, err := ValidateRequest(req, c.now(), c.store.Config().MaxExecutionBytes)
	if err != nil {
		var validation *ValidationError
		if errors.As(err, &validation) {
			c.respond(ctx, req, owner, failure(validation.ReasonCode, validation.Detail))
			return
		}
		c.respond(ctx, req, owner, failure(types.DebugArchiveReasonInvalidRequest, "request rejected"))
		return
	}

	entry, replay := c.admit(req, owner)
	if replay != nil {
		// A duplicate of a request already handled: replay the recorded
		// acknowledgement without touching the provider or the network.
		c.send(ctx, owner, replay)
		return
	}
	if entry == nil {
		c.respond(ctx, req, owner, failure(
			types.DebugArchiveReasonRequestCapacityExhausted,
			"worker request cache is full",
		))
		return
	}

	metrics.IncDebugArchiveRequestsInFlight()
	ack := c.collect(ctx, req, owner, effectiveMaxBytes)
	metrics.DecDebugArchiveRequestsInFlight()

	c.finish(entry, req, ack)
	c.send(ctx, owner, ack)
}

// collect performs the snapshot and upload for a request this process owns.
func (c *Coordinator) collect(
	ctx context.Context,
	req *types.DebugArchiveLogsRequestedMessage,
	owner Ownership,
	maxBytes int64,
) *types.DebugArchiveLogsUploadedMessage {
	if owner.BackendKind == BackendCommand {
		return c.ack(req, owner, failure(
			types.DebugArchiveReasonBackendNotSupported,
			"the command backend dispatches into an opaque runtime with no log API",
		))
	}

	select {
	case c.uploadSlots <- struct{}{}:
		defer func() { <-c.uploadSlots }()
	case <-ctx.Done():
		return c.ack(req, owner, failure(types.DebugArchiveReasonWorkerShuttingDown, "worker is shutting down"))
	}

	execMutex := c.executionMutex(req.RunID, req.ExecutionID)
	execMutex.Lock()
	defer execMutex.Unlock()

	// Queueing never extends the request's deadline, and ownership can lapse
	// while a slot is held, so both are re-checked before any provider work.
	if !c.now().Before(req.ExpiresAt) {
		return c.ack(req, owner, failure(types.DebugArchiveReasonUploadExpired, "request expired while queued"))
	}
	current, owns := c.ownership.LookupExecution(req.RunID, req.ExecutionID)
	if !owns {
		return c.ack(req, owner, failure(types.DebugArchiveReasonCleanupGraceExpired, "cleanup grace expired while queued"))
	}
	owner = current

	transformer, err := NewTransformer(req.ContentTransformer.Kind, req.ContentTransformer.Version)
	if err != nil {
		return c.ack(req, owner, failure(types.DebugArchiveReasonUnsupportedContentTransformer, "unsupported content transformer"))
	}

	snapshot, err := c.store.NewSnapshot(owner.BackendKind, transformer, maxBytes)
	if err != nil {
		if errors.Is(err, ErrBudgetExhausted) {
			return c.ack(req, owner, failure(types.DebugArchiveReasonRequestCapacityExhausted, "worker capture disk budget exhausted"))
		}
		return c.ack(req, owner, failure(types.DebugArchiveReasonSnapshotFailed, "failed to allocate a snapshot"))
	}
	defer snapshot.Close()

	if owner.Capture != nil && owner.InCleanupGrace {
		owner.Capture.Finalize(snapshotFinalizeDeadline)
	}

	start := c.now()
	snapshotErr := c.source.SnapshotLogs(ctx, req.RunID, req.ExecutionID, snapshot.Sink())
	var partial *PartialSnapshotError
	switch {
	case snapshotErr == nil:
	case errors.As(snapshotErr, &partial):
		// A partial provider read still produced valid sibling data; it is
		// uploaded with capture_status=partial rather than discarded.
	default:
		return c.ack(req, owner, failure(reasonForSnapshotError(snapshotErr), "log snapshot failed"))
	}

	if err := snapshot.Finalize(); err != nil {
		return c.ack(req, owner, failure(types.DebugArchiveReasonSnapshotFailed, "failed to finalize the snapshot"))
	}
	metrics.RecordDebugArchiveSnapshot(owner.BackendKind, c.now().Sub(start), snapshot.Bytes())
	if snapshot.Truncated() {
		metrics.RecordDebugArchiveTruncation(owner.BackendKind)
	}

	if snapshot.Bytes() == 0 {
		return c.ack(req, owner, failure(types.DebugArchiveReasonCaptureUnavailable, "no log bytes were available for this execution"))
	}

	uploadStart := c.now()
	if err := c.uploader.Upload(ctx, req.UploadTarget, snapshot, req.ExpiresAt); err != nil {
		metrics.RecordDebugArchiveUpload("failed", c.now().Sub(uploadStart))
		var uploadErr *UploadError
		if errors.As(err, &uploadErr) {
			return c.ack(req, owner, failure(uploadErr.ReasonCode, uploadErr.Detail))
		}
		return c.ack(req, owner, failure(types.DebugArchiveReasonUploadFailed, "upload failed"))
	}
	metrics.RecordDebugArchiveUpload("uploaded", c.now().Sub(uploadStart))

	warnings := combineWarnings(snapshot.Warnings(), partial)
	captureStatus := types.DebugArchiveCaptureComplete
	if len(warnings) > 0 {
		captureStatus = types.DebugArchiveCapturePartial
	}

	ack := c.ack(req, owner, result{outcome: types.DebugArchiveOutcomeUploaded})
	ack.Bytes = snapshot.Bytes()
	ack.CRC32C = snapshot.CRC32C()
	ack.SHA256 = snapshot.SHA256()
	ack.Truncated = snapshot.Truncated()
	ack.ContentTransformerVersion = transformer.Version()
	ack.CaptureStatus = captureStatus
	ack.WarningCodes = warnings
	return ack
}

// admit reserves the request's cache slot. It returns a replayable
// acknowledgement for a duplicate, or a nil entry when the cache is full.
func (c *Coordinator) admit(req *types.DebugArchiveLogsRequestedMessage, owner Ownership) (*cacheEntry, *types.DebugArchiveLogsUploadedMessage) {
	fingerprint := requestFingerprint(req)

	c.mu.Lock()
	existing, ok := c.cache[req.RequestID]
	if ok {
		c.mu.Unlock()
		if existing.fingerprint != fingerprint {
			// The same request ID with different immutable content is a
			// server-side error, not a retry; honoring it would upload a
			// second object under one identity.
			return nil, c.ack(req, owner, failure(
				types.DebugArchiveReasonInvalidRequest,
				"request id was reused with different content",
			))
		}
		<-existing.done
		return nil, existing.ack
	}

	if len(c.cache) >= maxCachedRequests {
		c.evictExpiredLocked()
		if len(c.cache) >= maxCachedRequests {
			c.mu.Unlock()
			return nil, nil
		}
	}

	entry := &cacheEntry{fingerprint: fingerprint, done: make(chan struct{})}
	c.cache[req.RequestID] = entry
	c.mu.Unlock()
	return entry, nil
}

// finish records a completed request's result and publishes it to any
// duplicate waiting on the same request ID.
func (c *Coordinator) finish(entry *cacheEntry, req *types.DebugArchiveLogsRequestedMessage, ack *types.DebugArchiveLogsUploadedMessage) {
	c.mu.Lock()
	entry.ack = ack
	entry.retainUntil = req.ExpiresAt
	c.mu.Unlock()
	close(entry.done)
}

// evictExpiredLocked drops entries whose request has expired, so a long-lived
// worker reclaims cache space before refusing new requests.
func (c *Coordinator) evictExpiredLocked() {
	now := c.now()
	for id, entry := range c.cache {
		select {
		case <-entry.done:
		default:
			continue
		}
		if !entry.retainUntil.IsZero() && now.After(entry.retainUntil) {
			delete(c.cache, id)
		}
	}
}

func (c *Coordinator) executionMutex(runID, executionID string) *sync.Mutex {
	key := executionKey{runID: runID, executionID: executionID}

	c.mu.Lock()
	defer c.mu.Unlock()
	if mutex, ok := c.perExec[key]; ok {
		return mutex
	}
	mutex := &sync.Mutex{}
	c.perExec[key] = mutex
	return mutex
}

// ForgetExecution drops the per-execution snapshot mutex once an execution's
// cleanup grace has expired.
func (c *Coordinator) ForgetExecution(runID, executionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.perExec, executionKey{runID: runID, executionID: executionID})
}

// result is the outcome-shaped part of an acknowledgement.
type result struct {
	outcome    string
	reasonCode string
	message    string
}

func failure(reasonCode, message string) result {
	outcome := types.DebugArchiveOutcomeFailed
	switch reasonCode {
	case types.DebugArchiveReasonBackendNotSupported,
		types.DebugArchiveReasonCaptureUnavailable,
		types.DebugArchiveReasonCleanupGraceExpired,
		types.DebugArchiveReasonResourceNotFound,
		types.DebugArchiveReasonResourceNotReady:
		outcome = types.DebugArchiveOutcomeUnavailable
	}
	return result{outcome: outcome, reasonCode: reasonCode, message: message}
}

func (c *Coordinator) ack(req *types.DebugArchiveLogsRequestedMessage, owner Ownership, res result) *types.DebugArchiveLogsUploadedMessage {
	return &types.DebugArchiveLogsUploadedMessage{
		ProtocolVersion: types.DebugArchiveProtocolVersion,
		RequestID:       req.RequestID,
		ArchiveID:       req.ArchiveID,
		CollectionID:    req.CollectionID,
		RunID:           req.RunID,
		ExecutionID:     req.ExecutionID,
		Outcome:         res.outcome,
		BackendKind:     owner.BackendKind,
		WarningCodes:    []string{},
		ReasonCode:      res.reasonCode,
		Message:         SanitizeMessage(res.message),
	}
}

func (c *Coordinator) respond(ctx context.Context, req *types.DebugArchiveLogsRequestedMessage, owner Ownership, res result) {
	c.send(ctx, owner, c.ack(req, owner, res))
}

func (c *Coordinator) send(ctx context.Context, owner Ownership, ack *types.DebugArchiveLogsUploadedMessage) {
	metrics.RecordDebugArchiveRequest(ack.BackendKind, owner.state(), ack.Outcome, ack.ReasonCode)
	if err := c.sender.SendDebugArchiveAck(ack); err != nil {
		log.Warnf(ctx, "Failed to send debug archive acknowledgement: %v", err)
	}
}

// requestFingerprint captures the immutable fields a retry must repeat. It
// hashes nothing secret into a log line: the value stays in memory.
func requestFingerprint(req *types.DebugArchiveLogsRequestedMessage) string {
	return req.ArchiveID + "|" + req.CollectionID + "|" + req.RunID + "|" + req.ExecutionID + "|" +
		req.RequestedFormat + "|" + req.ExpiresAt.UTC().Format(time.RFC3339Nano) + "|" +
		req.ContentTransformer.Kind + "|" + itoa(req.ContentTransformer.Version) + "|" +
		itoa64(req.MaxBytes) + "|" + req.UploadTarget.Method + "|" + req.UploadTarget.URL
}

func combineWarnings(encoderWarnings []string, partial *PartialSnapshotError) []string {
	set := warningSet{}
	for _, code := range encoderWarnings {
		set.add(code)
	}
	if partial != nil {
		for _, code := range partial.WarningCodes {
			set.add(code)
		}
	}
	return set.codes()
}
