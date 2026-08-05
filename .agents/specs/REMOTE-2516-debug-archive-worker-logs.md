# REMOTE-2516: Self-hosted worker debug-archive logs

## Document status
This is the `warpdotdev/oz-agent-worker` companion to the debug-archive product and technical contract in [`warpdotdev/warp-server#13839`](https://github.com/warpdotdev/warp-server/pull/13839). The server spec owns archive creation, authorization, storage, trigger policy, Temporal orchestration, and the server half of the WebSocket protocol. This document owns the worker half: proving execution ownership, capturing Docker/Kubernetes/direct-backend logs, retaining a bounded terminal snapshot, uploading to a server-supplied target, and acknowledging the result.

Both documents are required to implement REMOTE-2516. If their wire contracts differ, implementation must reconcile the specs before either PR is promoted; neither side should silently infer a different protocol.

## Summary
Warp staff, run creators, and owning-team admins need a HAR-like debug archive for cloud-agent runs. For self-hosted executions, the logs live inside infrastructure controlled by `oz-agent-worker`. `warp-server` cannot query a customer's Docker daemon, Kubernetes cluster, or direct child process, and the current worker removes its task ownership record and often destroys the backend resource as soon as execution ends.

V1 adds a request-driven worker protocol. `warp-server` broadcasts a versioned `debug_archive_logs_requested` message, with one immutable presigned upload target, to every connected process using the assigned worker ID. Each process checks an exact `(run_id, execution_id)` active/recent ownership registry. Only the process that executed the assignment snapshots logs, uploads bounded NDJSON directly to the target, and sends `debug_archive_logs_uploaded`. Other processes report `not_owner`. Missing logs, old workers, disconnected workers, upload failures, and expired local spools make only the self-hosted log source partial; they never fail or pause the cloud-agent run.

V1 supports snapshots during an active run and a bounded final snapshot after terminal execution. It does not continuously append to one object. The design leaves room for a future mode that uploads immutable, monotonically numbered chunks while a run is active.

## Product contract

### Users and outcomes
The direct consumer is the debug-archive collector in `warp-server`; human authorization and archive download are server responsibilities.

For a supported self-hosted execution:
- An active collection captures logs observed up to a consistent snapshot watermark without interrupting the agent.
- A collection triggered immediately after an execution failure can still retrieve a bounded final snapshot after the container, Job, process, or workspace has been cleaned up.
- A later collection can replace the logical archive source with a newer snapshot.
- The archive manifest can distinguish uploaded, unavailable, unsupported, not-owned, failed, and truncated worker-log outcomes.

### Supported backends
V1 captures:
- Docker container stdout and stderr for the exact execution container.
- Kubernetes logs for every init container and regular container in every pod belonging to the exact execution Job, including current and best-effort previous logs after a restart.
- Direct-backend stdout and stderr for worker-managed setup, agent, and teardown processes.

The command backend dispatches into an opaque operator-owned runtime and has no backend log API. A worker that owns a command-backend assignment responds `unavailable` with `backend_not_supported`; it must not claim that dispatch-command stdout is the remote agent's execution log.

### Partial behavior
Log collection is best effort:
- A request is independent from task execution and never blocks assignment, process progress, terminal reporting, teardown, or worker reconnection.
- A failure to initialize or write the local spool never rejects or fails the assigned task.
- An inability to capture or upload returns a sanitized acknowledgement when possible. If the worker is disconnected or too old to understand the message, the server times out.
- The server publishes the remainder of the archive with a partial/stale self-hosted-log source according to the server spec.

### Retention and bounds
V1 defaults to:
- 64 MiB maximum retained/output bytes per execution.
- First-half/last-half preservation with an explicit NDJSON truncation record when the source exceeds its bound.
- 30 minutes of post-terminal local retention.
- 1 GiB total worker spool budget.
- Two concurrent upload handlers per worker process.

The values are operator-configurable except that a server request may lower, but never raise, the worker's per-execution 64-MiB ceiling. Expired and successfully uploaded terminal spools are eligible for deletion. Active task capture is never terminated because of spool pressure; it degrades to a smaller truncated snapshot or an unavailable source.

The local spool is process/host scoped. The default does not promise recovery after worker process, host, or Kubernetes worker-pod replacement. Kubernetes installations may mount persistent storage in a later release, but cross-process ownership recovery is outside V1.

### Version skew
- An old worker ignores the unknown request; the server times out and records the source as unavailable.
- A new worker accepts protocol version 1 and ignores unknown optional JSON fields.
- A new worker receiving an unsupported protocol version sends `failed` with reason `unsupported_protocol_version`.
- A server that does not send archive requests does not change worker behavior.
- Protocol handling is disabled only by the absence of the new server message; no worker rollout may make ordinary task execution depend on archive availability.

### Security and privacy
Worker logs may contain prompts, source code, identifiers, and secrets emitted by a process. V1 uploads the captured bytes without an archive-specific redaction pass, matching the server spec. The worker:
- keeps spool directories mode `0700` and files mode `0600`;
- never logs log content, presigned URLs, signed headers, multipart fields, or upload response bodies;
- sends bytes only to the exact authenticated-server-supplied target;
- permits HTTPS targets, with plain HTTP allowed only for loopback integration tests/local development;
- disables HTTP redirects so a signed target cannot redirect bytes elsewhere;
- deletes terminal bytes after a successful upload/replay metadata is durable or after retention expiry.

The archive-wide `ContentTransformer` boundary in `warp-server` remains the future redaction point. This worker protocol must preserve enough structured source metadata for that transformer to evolve without another backend capture redesign.

## Current state
- `internal/types/messages.go` defines only assignment, lifecycle, and cancellation message DTOs. Unknown server messages are logged and ignored.
- `internal/worker/worker.go:323` decodes WebSocket messages synchronously on the read loop. Archive work cannot run inline there without blocking heartbeats, cancellation, or later assignments.
- `internal/worker/worker.go:62` tracks active tasks only by run/task ID. `executeTask` removes the record immediately after terminal reporting unless the command backend spawned the task.
- `internal/worker/backend.go:38` has execution, cancellation, shutdown, and shutdown-preservation methods, but no log snapshot contract.
- `internal/worker/docker.go:99` knows the exact container ID only inside `ExecuteTask`; a defer force-removes that container before the worker reports terminal state. Existing `getContainerLogs` reads the whole multiplexed stream into memory and does not preserve stdout/stderr identity.
- `internal/worker/kubernetes.go:406` deletes successful Jobs immediately. Existing diagnostic collection reads at most 1 MiB per container into memory, omits timestamps/stream metadata, and is not exposed outside failure logging.
- `internal/worker/direct.go:233` connects agent stdout/stderr directly to the worker process and removes the per-task workspace after execution. No task-scoped log bytes survive.
- `internal/config/config.go` has no spool configuration. The Helm Deployment mounts only its configuration and therefore has no explicit bounded spool volume.

These constraints require changes in both the shared worker lifecycle and all three supported backends; adding only a WebSocket message would race terminal cleanup and fail the primary `ANY_FAILURE` use case.

## Technical design

### Wire protocol
Add the following v1 message types to `internal/types/messages.go`.

Server to worker:

```json
{
  "type": "debug_archive_logs_requested",
  "data": {
    "protocol_version": 1,
    "request_id": "uuid",
    "archive_id": "uuid",
    "collection_id": "uuid",
    "run_id": "uuid",
    "execution_id": "uuid",
    "requested_format": "application/x-ndjson",
    "expires_at": "RFC3339 timestamp",
    "max_bytes": 67108864,
    "upload_target": {
      "url": "https://...",
      "method": "PUT or POST",
      "headers": {"provider-header": "value"},
      "multipart_fields": {"provider-field": "value"}
    }
  }
}
```

Worker to server:

```json
{
  "type": "debug_archive_logs_uploaded",
  "data": {
    "protocol_version": 1,
    "request_id": "uuid",
    "archive_id": "uuid",
    "collection_id": "uuid",
    "run_id": "uuid",
    "execution_id": "uuid",
    "outcome": "uploaded",
    "backend_kind": "docker",
    "bytes": 1234,
    "crc32c": "base64",
    "sha256": "hex",
    "truncated": false,
    "capture_status": "complete",
    "warning_codes": [],
    "reason_code": "",
    "message": ""
  }
}
```

Allowed outcomes are `uploaded`, `unavailable`, `not_owner`, and `failed`. Uploaded outcomes set `capture_status` to `complete` or `partial`; partial capture includes at most 16 deduplicated stable `warning_codes` such as `container_logs_unavailable`, `previous_logs_unavailable`, `output_dropped`, or `provider_snapshot_incomplete`. Non-upload outcomes omit byte/checksum/capture fields and use stable bounded reason codes:
- `unsupported_protocol_version`
- `invalid_request`
- `request_expired`
- `backend_not_supported`
- `resource_not_ready`
- `resource_not_found`
- `spool_expired`
- `spool_exhausted`
- `snapshot_failed`
- `upload_rejected`
- `upload_expired`
- `upload_failed`
- `worker_shutting_down`
- `request_capacity_exhausted`

Human-readable `message` is sanitized and capped at 256 UTF-8 bytes. It never includes provider output, a URL, request headers/fields, a local path, or an HTTP response body.

Required request validation occurs before ownership/upload work:
- protocol version is 1;
- identifiers and requested format are non-empty and valid;
- `expires_at` is in the future;
- `max_bytes` is positive and no larger than the protocol's 64-MiB ceiling; the effective output bound is the minimum of the request and configured ceilings;
- method is `PUT` or `POST`;
- PUT has no multipart fields, while POST has the fields supplied by the server;
- target/header/field values contain no control characters;
- target scheme satisfies the HTTPS/loopback rule.

### Ownership and request state
Replace the single-purpose active-task map with a `TaskRegistry` that exposes exact compound-key lookup while retaining existing cancellation behavior:
- `active[(run_id, execution_id)]` stores task context/cancel state, backend kind, normalized backend resource identity, and its `TaskLogCapture`.
- `recent[(run_id, execution_id)]` stores terminal snapshot metadata, final outcome time, expiry, and the immutable local spool handle.
- The existing run-ID cancellation lookup remains available because cancellation messages carry only `task_id`.

At assignment, the worker creates the active registry entry and bounded capture before starting the backend. If capture initialization fails, it records a non-fatal capture status and continues the task.

Immediately after `Backend.ExecuteTask` returns—and before `task_failed`, `task_completed`, or `task_cancelled` is enqueued—the worker finalizes the log capture and atomically moves ownership from active to recent. Docker/Kubernetes final capture happens inside backend execution before their resource-cleanup defer runs. This ordering guarantees that an `ANY_FAILURE` server request triggered by the terminal message sees the recent entry rather than racing its creation.

Every connected process with the same worker ID receives the server broadcast. A process checks both IDs:
- Exact active/recent match: it is the owner and handles the request.
- Run exists with a different execution, or neither exact key exists: return `not_owner`.
- Exact command-backend match: return `unavailable/backend_not_supported`.

A request cache of at most 1,024 entries keyed by `request_id` makes duplicate delivery idempotent:
- one in-flight goroutine owns snapshot/upload;
- concurrent duplicates attach to that result;
- a completed duplicate replays the same acknowledgement without re-uploading;
- reuse of one request ID with different immutable fields returns `failed/invalid_request`.

Completed request metadata remains until the later of request expiry and recent-entry expiry. It contains IDs, outcome, size/checksums, capture status/warnings, and reason only—not the target. Expired entries are removed first when the cache reaches its limit. If no expired entry is available, a new request returns `failed/request_capacity_exhausted`; task execution and existing request handlers continue.

### Asynchronous request coordinator
`handleMessage` parses and validates the envelope, then hands archive requests to a `DebugLogCoordinator` goroutine. It never performs provider reads or network uploads on the WebSocket read loop.

The coordinator:
1. Performs ownership and idempotency checks.
2. Acquires the global upload semaphore and per-execution snapshot mutex.
3. Re-checks request expiry and ownership.
4. Creates a request-scoped bounded snapshot.
5. For active Docker/Kubernetes executions, calls the live backend snapshot method. For active direct executions, takes a consistent watermark copy of the existing spool. For recent executions, copies the immutable final spool.
6. Closes and verifies the local request snapshot.
7. Uploads it to the supplied target while computing CRC32C and SHA-256.
8. Enqueues one acknowledgement through the existing single WebSocket writer.
9. Records the completed request result and releases resources.

Queueing never extends `expires_at`. A request that waits past expiry reports `upload_expired` without contacting the target. Worker shutdown cancels in-flight requests and does not delay ordinary shutdown beyond the existing bounded backend shutdown.

### Backend contract
Extend `Backend` with:

```go
SnapshotTaskLogs(ctx context.Context, taskID, executionID string, writer io.Writer) error
```

The method writes protocol-v1 NDJSON records and returns typed errors that the coordinator maps to stable reason codes. `PartialSnapshotError` carries bounded warning codes after valid sibling data has been written; the coordinator uploads those bytes with `capture_status=partial` rather than discarding them. Other errors produce a non-upload outcome when no valid snapshot exists. The method must:
- scope provider lookup to both exact IDs and this worker/backend;
- stream rather than return log bytes;
- honor cancellation and the supplied bounded writer;
- make no task lifecycle changes;
- be safe to call while `ExecuteTask` is running;
- never remove a provider resource;
- emit deterministic source ordering where the provider has multiple streams/containers.

Add a task-scoped `LogCapture` handle to `TaskParams`. Backends write their terminal/fallback data into this handle before cleanup. The command backend implements `SnapshotTaskLogs` by returning the typed unsupported error.
Terminal capture after a cancelled execution uses a fresh, bounded `context.WithoutCancel` context no longer than `BackendShutdownTimeout`; otherwise Docker/Kubernetes cancellation would make their provider log calls fail before cleanup. The final-capture deadline cannot extend the worker's existing bounded shutdown contract.

### NDJSON format
Every output line is valid UTF-8 JSON with `schema_version: 1` and `kind`.

Data records contain:
- `kind: "data"`
- monotonically increasing `sequence` within the snapshot
- `backend: "docker" | "kubernetes" | "direct"`
- `phase` (`setup`, `agent`, `teardown`, or `container`)
- `stream` (`stdout`, `stderr`, `combined`, or `unknown`)
- provider timestamp when supplied, plus worker `observed_at`
- `encoding: "utf8" | "base64"`
- `data`
- optional Docker `container_id`
- optional Kubernetes `namespace`, `pod`, `container`, `container_type`, `restart_attempt`, and `previous`

Data is framed into bounded chunks no larger than 32 KiB before JSON encoding. Valid UTF-8 is stored directly; other bytes are base64. Empty streams do not produce placeholder records.

A readable sibling plus an unreadable backend stream emits a `kind: "source_error"` record containing only safe source identity and one stable warning code. It never embeds the provider error string. The same warning appears in acknowledgement `warning_codes`, allowing `warp-server` to mark the source partial without parsing arbitrary provider text.

When bytes are dropped, output includes one valid record:

```json
{"schema_version":1,"kind":"truncation","policy":"first_last","omitted_bytes_at_least":123}
```

The first and last portions each end on an NDJSON record boundary. Final sequence numbers describe emitted order, not original provider byte offsets. The acknowledgement's `truncated` value agrees with the record.

### Bounded log store
Add an internal secure disk-backed `TaskLogStore` independent of task workspaces:
- Defaults to `${TMPDIR}/oz-agent-worker/debug-logs`.
- Creates the root with `0700` and regular files with `0600`; rejects symlinks and path-derived task IDs.
- Keys all files with internal random/digested names rather than raw IDs.
- Uses two bounded first/last segments per execution. Fixed maximum record chunks keep tail indexing bounded in memory.
- Supports an atomic snapshot watermark while writers continue, so active direct execution can be copied without closing its spool.
- Tracks actual bytes, truncation, capture errors, active/recent state, and expiry.
- Uses a global budget. Cleanup order is acknowledged/expired entries, then oldest terminal entries. Active entries are not deleted; when no capacity remains, their effective retained window shrinks or capture becomes unavailable and records a pressure reason.
- Runs startup cleanup for orphan files from a previous unclean process. V1 does not reconstruct ownership from those files, so they are deleted rather than exposed.
- Runs periodic expiry cleanup and best-effort shutdown cleanup.

Add top-level YAML configuration:

```yaml
debug_log_spool:
  directory: ""
  retention: "30m"
  max_total_bytes: 1073741824
  max_execution_bytes: 67108864
  max_concurrent_uploads: 2
```

Invalid duration, non-positive limits, a per-execution limit over 64 MiB, a global limit below the per-execution limit, or an unwritable configured directory fails worker startup. Runtime spool exhaustion remains non-fatal to tasks.

The Helm chart:
- mounts a dedicated `emptyDir` at `/var/lib/oz-agent-worker/debug-logs`;
- defaults `sizeLimit` to 1 GiB;
- writes the matching spool config;
- allows the volume configuration and limits to be overridden;
- documents that the default `emptyDir` is lost when the worker pod is replaced.

### Docker adapter
`DockerBackend` adds a mutex-protected exact `(task_id, execution_id) -> container_id` registry:
- Register after container creation and before start.
- Keep it through the final snapshot.
- Remove it only after the final snapshot attempt and container cleanup.

`SnapshotTaskLogs`:
- resolves the exact registered container;
- calls Docker `ContainerLogs` with stdout, stderr, and timestamps enabled and no time-range filter;
- demultiplexes Docker's stream framing so each record has correct stdout/stderr identity;
- emits chunked NDJSON directly to the supplied writer.

`ExecuteTask` always attempts one final snapshot into `TaskLogCapture` after the container stops and before `ContainerRemove`, regardless of success, failure, cancellation, or worker log level. It replaces the existing unbounded `io.ReadAll` diagnostic path. Snapshot failure is recorded but does not change the task result or skip cleanup.

### Kubernetes adapter
`SnapshotTaskLogs`:
- lists pods using the execution, task, and configured worker hash labels and verifies returned labels;
- sorts pods deterministically by name;
- visits init containers and regular containers in declared order;
- requests all available logs with timestamps and no time-range filter;
- attempts `Previous: true` before current logs for a container whose restart count is non-zero;
- tags every output record with pod/container identity and whether it is previous/current;
- treats a vanished pod, unavailable previous stream, or one unreadable container as a typed per-snapshot partial error while retaining readable sibling streams.

The adapter must not use the existing 1-MiB `collectPodLogs` helper for archive collection. It streams through the shared aggregate 64-MiB bound.

Before a successful Job is deleted, and before returning any terminal success/failure/cancellation whose pods are still readable, `ExecuteTask` captures a final snapshot into `TaskLogCapture`. A final-capture error cannot change Job outcome, terminal reporting, preservation-on-shutdown, or cleanup policy. Failed Jobs may remain for their existing TTL, but the local final spool is still created so collection does not depend on that TTL.

The chart's existing `get pods/log` RBAC is sufficient; implementation must not broaden it beyond the namespace or add secret-read permissions.

If the Kubernetes worker process is disrupted while a preserved task Job continues, the replacement process does not inherit the old process's ownership/spool in V1. A later request is partial unless a future reattach protocol reconstructs exact ownership. This limitation is explicit and does not weaken the existing preserve-on-shutdown behavior.

### Direct adapter
Create one task capture before setup. Replace worker-global output assignment with phase-aware multi-writers:
- Setup stdout/stderr goes to the worker console and `phase=setup` capture.
- Agent stdout/stderr goes to the worker console and `phase=agent` capture.
- Teardown stdout/stderr goes to the worker console and `phase=teardown` capture.
- Capture mechanics: the archive branch of each multiwriter is a bounded non-blocking queue, not a synchronous disk write. `Write` copies a bounded chunk into the queue and immediately reports the original byte count; when the queue is full it drops archive bytes, marks `output_dropped`, and leaves the existing console sink/child process behavior unchanged. A per-task background encoder drains the queue into `TaskLogStore`. Terminal finalization drains it under a bounded deadline before workspace cleanup. `SnapshotTaskLogs` takes a consistent watermark copy of already-drained plus queued task data without closing the active capture. No task workspace file content is stored.

### Upload client
The upload client accepts the provider-neutral target from the authenticated server:
- PUT sends the snapshot as the request body with supplied headers.
- POST builds a streaming multipart form with supplied fields and one file part; it never loads the file into memory.
- Redirects are disabled.
- The request body is capped at the lower of request `max_bytes`, configured per-execution maximum, and actual snapshot size.
- Each attempt reopens the immutable local snapshot, allowing replay.
- Network errors, HTTP 408/429, and 5xx responses retry with bounded exponential backoff only before `expires_at`.
- Other 4xx responses are terminal `upload_rejected`.
- Only a 2xx response is `uploaded`.
- Response bodies are drained/closed under a small cap but never logged or returned in acknowledgement text.

CRC32C and SHA-256 are computed over the exact uploaded file bytes. Byte count and digests are included in the acknowledgement so `warp-server` can compare them with object-store attributes before signaling its Temporal workflow.

After a successful terminal-spool upload, the spool may be removed; completed request metadata remains so an identical duplicate request can replay the acknowledgement without the bytes. Active captures remain until execution terminal. A new request ID after the terminal spool was removed may truthfully return `spool_expired`.

### Immutable snapshots and future incremental collection
V1 does not add append semantics to hosted storage and does not keep a long-lived upload stream:
- Each request creates one immutable bounded snapshot and uploads one complete object.
- The server's later archive generation replaces the logical log source.
- Active `ALWAYS` collection is supported by requesting snapshots after assignment and later lifecycle events.

For future continuous collection, `TaskLogStore` and the NDJSON sequence field permit sealed immutable chunks. A future protocol can request/upload chunk keys plus a sequence watermark and checksums. It must not require mutating an existing GCS/S3 object. No continuous chunk scheduler, server chunk manifest, or upload bandwidth policy is in V1.

### Observability
Add bounded-cardinality metrics:
- log collection requests by outcome, backend, and active/recent ownership;
- snapshot duration and bytes buckets;
- spool current bytes and configured limit;
- spool entries by active/recent state;
- truncation and spool-pressure counts;
- upload duration, retry, and result counts;
- request queue depth/in-flight count.

Structured logs may include request ID, run ID, execution ID, backend, outcome, bytes, truncation, and stable reason. They never include captured data, local spool filenames, target coordinates, signed URL/query, signed headers/form fields, checksum input, or response bodies.

### Rollout and compatibility
1. Land server schema/status/manual collection without claiming self-hosted completeness.
2. Land this worker implementation and publish a pinned immutable worker image.
3. Validate old-worker timeout and new-worker v1 upload against staging.
4. Validate active and post-failure capture for Docker, Kubernetes, and direct.
5. Enable creator/team-admin access and automatic policies only after the coordinated server/worker metrics show bounded duration, disk, and upload behavior.

`warp-server` remains tolerant of old worker versions indefinitely. Documentation must state the minimum worker version required for self-hosted logs in debug archives.

## Design alternatives

### Server pulls from customer infrastructure
The server cannot safely reach an arbitrary customer Docker daemon or Kubernetes API and has no direct-process handle. Worker-pushed bytes to a narrow presigned target preserve customer network boundaries and avoid sending object-store credentials to the worker.

### Send logs through the WebSocket
WebSocket transfer would burden the regular server, interfere with control messages, duplicate flow control, and risk loading large logs into memory. Direct upload keeps the control channel metadata-only and lets object-store limits enforce a hard boundary.

### First worker connection wins
Multiple worker processes can use one worker ID. Selecting the first connection could leak the wrong execution or return misleading emptiness. Broadcasting and exact active/recent ownership makes only the process that executed the assignment authoritative.

### Keep backend resources instead of spooling
Retaining containers/Jobs/workspaces would delay cleanup, consume customer resources, and still fail for direct output or worker restarts. A short bounded worker-owned spool decouples archive availability from backend cleanup.

### Capture continuously for every task
Continuous remote upload would maximize recovery but adds bandwidth, credentials, chunk publication, and policy complexity even when no archive is requested. V1 captures locally within fixed bounds and uploads on request. Immutable chunk streaming remains a compatible follow-up.

### Append to one object
GCS/S3 and the server's existing provider-neutral presigned-target contract do not offer one portable append primitive. Immutable snapshots avoid partial object visibility, retry ambiguity, and provider-specific behavior.

### Keep only the last 64 MiB
Tail-only capture often loses setup/image/initialization failures. First/last retention preserves both early context and the terminal failure while keeping a strict bound, at the cost of an explicit gap.

### Memory buffering
64 MiB per task multiplied by concurrency can exhaust the long-lived worker. Secure disk-backed head/tail storage keeps memory bounded and permits retryable upload bodies.

### One generic stdout hook for every backend
Direct execution can be teed, but Docker and Kubernetes already own provider-native logs with stream/container metadata. A shared `SnapshotTaskLogs` contract plus backend-specific adapters preserves this context and handles resources that outlive the local process.

## Risks and mitigations
- Log bytes contain secrets: keep storage private, never log payload/targets, use exact presigned destinations, and rely on the server transformer boundary for future redaction.
- Disk pressure affects the worker: strict per-execution/global bounds, secure dedicated spool, terminal eviction, pressure metrics, and task-non-fatal degradation.
- Terminal request races cleanup: every backend finalizes before resource cleanup, and the worker moves ownership to recent before terminal reporting.
- Multi-instance worker collision: exact compound ownership, broadcast, `not_owner`, server identity/assignment verification, and idempotent request IDs.
- WebSocket read starvation: provider/upload work runs asynchronously with a semaphore.
- Signed URL expiry: immutable local snapshot, replayable attempts, bounded retry before expiry, and explicit expired outcome.
- Target abuse: authenticated control channel, method/scheme/header validation, no redirects, and no target logging.
- Kubernetes worker replacement loses ownership: explicit V1 limitation; the archive remains partial and a future Job reattach protocol can recover it.
- Protocol drift across repositories: explicit companion links, checked-in golden v1 fixtures on both sides, and coordinated staging tests before rollout.
- Direct capture changes subprocess behavior: multiwriters keep the existing console sink, spool writes are non-blocking/best-effort, and exit status remains authoritative.
- Backend snapshot errors alter task result: capture errors are isolated and never replace execution errors, status, or cleanup.

## Validation criteria

### Wire contract and parsing
- WVC-001: A golden protocol-v1 request fixture is accepted by both this worker and the server implementation from `warpdotdev/warp-server#13839` without field translation.
- WVC-002: Golden complete and partial `uploaded` acknowledgement fixtures produced by the worker are accepted by the server and preserve every identity, byte, checksum, truncation, capture status/warning, backend, and outcome field.
- WVC-003: Unknown optional request fields are ignored; a missing required field, invalid ID/format/method/control character, non-positive/oversized bound, or expired request yields `failed` with a stable sanitized reason and no target request.
- WVC-004: Unsupported protocol versions produce `failed/unsupported_protocol_version`; an old-worker fixture that ignores the new message still yields the server-spec timeout/partial behavior.
- WVC-005: No acknowledgement or worker log contains a target URL, signed query/header/form field, local path, captured byte, or response body.

### Ownership and idempotency
- WVC-006: Among two connected processes with one worker ID, only the process with the exact active `(run_id, execution_id)` uploads; the other reports `not_owner`.
- WVC-007: A process that owns another execution of the same run cannot upload for the requested execution.
- WVC-008: An exact recent entry remains authoritative after terminal execution until upload/expiry.
- WVC-009: A duplicate `request_id` with identical content causes at most one snapshot/upload and replays the same acknowledgement.
- WVC-010: Reusing a request ID with different archive, collection, run, execution, target, expiry, or bound is rejected.
- WVC-011: Requests run off the WebSocket read loop; while one provider read/upload is blocked, heartbeat, cancellation, and another assignment message are still processed.
- WVC-012: The upload semaphore limits total concurrent handlers and the per-execution mutex prevents overlapping provider snapshots without blocking unrelated executions.

### Terminal ordering and partial behavior
- WVC-013: For success, process failure, timeout/cancellation, and backend setup failure, finalization/move-to-recent occurs before terminal lifecycle message enqueue.
- WVC-014: A simulated `ANY_FAILURE` request arriving immediately after `task_failed` retrieves the recent snapshot without racing task-map deletion.
- WVC-015: Spool initialization, write, finalization, snapshot, and upload failures never change task claim, execution exit result, terminal message, teardown, or worker reconnect behavior.
- WVC-016: No owning entry yields `not_owner`; an owning entry with no captured bytes yields a classified `unavailable` result rather than an empty uploaded object.
- WVC-017: A command-backend owner reports `unavailable/backend_not_supported`, while a different command-worker process reports `not_owner`.

### NDJSON and truncation
- WVC-018: Docker, Kubernetes, and direct fixtures emit only valid schema-v1 NDJSON; every data record has bounded data, encoding, backend, phase, stream, sequence, and observed-time fields, and safe `source_error` records contain no raw provider error.
- WVC-019: Invalid UTF-8/binary output round-trips through base64 records without corrupting the NDJSON stream.
- WVC-020: Output below the bound is byte-complete after record decoding and reports `truncated=false`.
- WVC-021: Output above the bound preserves valid first/last record sets, contains one truncation record with an omitted-byte lower bound, stays within the request/configured limit, and reports `truncated=true`.
- WVC-022: Empty provider streams do not create misleading zero-byte data records.
- WVC-023: CRC32C, SHA-256, and byte count match the exact complete NDJSON uploaded, including its truncation record.

### Secure bounded store
- WVC-024: A new store creates its root as `0700`, files as `0600`, uses non-user-derived names, and rejects symlink/path traversal fixtures.
- WVC-025: Each execution cannot retain more than the configured/requested ceiling, and all active, recent, and request-snapshot files together cannot grow beyond the configured global budget.
- WVC-026: Under global pressure, acknowledged/expired and oldest terminal entries are evicted before active entries; active tasks continue and receive a truthful truncated/unavailable capture state.
- WVC-027: A consistent active snapshot uses a fixed watermark while later writes continue into the task spool.
- WVC-028: Terminal entries expire at the configured time, successful-upload entries become eligible for deletion, and duplicate-request acknowledgement metadata survives long enough to replay.
- WVC-029: Startup removes unowned orphan spool files without reconstructing or exposing their data.
- WVC-030: Invalid spool config or an unwritable configured directory fails startup with no credential/log content in the error.

### Docker
- WVC-031: Exact task/execution ownership resolves exactly one container; a mismatched execution cannot read another container.
- WVC-032: Active Docker snapshot calls the container-log API with stdout, stderr, timestamps, and no time-range filter, and demultiplexes stream identity into NDJSON.
- WVC-033: Success, non-zero exit, OOM, context cancellation, and wait failure each attempt final capture before container removal.
- WVC-034: A container removed before a live request yields a classified unavailable error, while its finalized recent spool remains uploadable.
- WVC-035: Archive capture replaces the existing unbounded `io.ReadAll` diagnostic path and passes a large-log test without memory scaling with output size.

### Kubernetes
- WVC-036: Pod selection requires exact execution, task, and worker hash labels; pods with only a colliding/mismatched label set are excluded.
- WVC-037: Snapshot ordering is pod name, then declared init containers, then declared regular containers, with identity on every record.
- WVC-038: Every readable current stream is captured with provider timestamps and no time filter; a restarted container also attempts and labels previous logs.
- WVC-039: One missing/unreadable container does not discard readable siblings, emits a safe source-error record, and uploads with `capture_status=partial` plus the matching bounded warning code.
- WVC-040: Successful Job cleanup happens only after final snapshot; failure/cancellation also attempts a final snapshot when pods remain readable.
- WVC-041: Final-capture failure does not change existing Job deletion, failed-Job TTL retention, task result, or Kubernetes preserve-on-worker-shutdown semantics.
- WVC-042: Helm/RBAC rendering still grants only namespace-scoped Job/pod/event operations plus `get pods/log`; no secret-read or cluster-scoped permission is added.
- WVC-043: Worker-pod replacement with a preserved Job is documented/tested as loss of process-local recent ownership in V1, not a false successful upload by the replacement.

### Direct
- WVC-044: Setup, agent, and teardown stdout/stderr continue to reach the worker console and are separately labeled in the task capture.
- WVC-045: One task's direct output cannot appear in another concurrent task's snapshot.
- WVC-046: Active direct snapshot returns bytes only through its watermark while the process continues and later output remains available to a later snapshot.
- WVC-047: Direct terminal capture remains available after per-task workspace cleanup and stores no workspace file content.
- WVC-048: A full/failed archive queue drops bytes and records `output_dropped` while its `Write` returns the child byte count promptly; a spool write failure or slow archive request cannot block the subprocess pipe or alter its exit code.

### Upload behavior
- WVC-049: A PUT fixture sends exactly the bounded snapshot with supplied safe headers, no redirect following, and matching digests.
- WVC-050: A POST fixture streams supplied multipart fields plus one file part without loading the snapshot into memory.
- WVC-051: Network error, 408, 429, and 5xx fixtures retry with bounded backoff before expiry; non-retryable 4xx fails immediately; no retry starts after expiry.
- WVC-052: Only 2xx produces `uploaded`; server-side object-attribute verification using the reported byte/checksum values passes for the staged object, and the server marks partial capture/truncation truthfully from acknowledgement metadata.
- WVC-053: HTTP redirects are rejected without forwarding signed headers or body to the redirect destination.
- WVC-054: Upload memory remains bounded for a 64-MiB fixture and each retry reopens the same immutable local snapshot.
- WVC-055: After a successful upload and local spool removal, an identical duplicate request replays the cached acknowledgement without another network request.

### Active, recent, and version-skew scenarios
- WVC-056: An active Docker, Kubernetes, and direct execution each satisfies an `ALWAYS`/manual request while continuing to run.
- WVC-057: A later request ID after execution end uploads a newer terminal snapshot that includes output after the active watermark.
- WVC-058: A request before the backend resource exists reports `resource_not_ready` and does not interfere with later snapshots.
- WVC-059: A disconnected, shutting-down, expired-spool, unsupported backend, and old-worker scenario each maps to the server's partial/stale behavior and never blocks archive publication.
- WVC-060: No V1 path appends to an existing object; every request uploads one complete immutable object.

### Configuration, operations, and verification
- WVC-061: File config defaults/overrides/validation cover directory, retention, global bytes, execution bytes, and concurrent uploads with strict unknown-field parsing.
- WVC-062: Helm lint/template proves the dedicated spool volume, mount, size limit, and generated config are consistent; an override fixture renders custom bounds.
- WVC-063: README/operator docs describe the sensitive-data implication, defaults, disk sizing, ephemeral pod-replacement limitation, supported backends, and minimum server/worker compatibility.
- WVC-064: Metrics cover request/result, ownership, snapshot, bytes, truncation, spool pressure/usage, upload/retry, and queue state without run/execution/request IDs as metric labels.
- WVC-065: Structured-log tests or capture assertions prove no log bytes, target material, upload response body, or local spool path is emitted.
- WVC-066: Unit tests use fake/injected Docker, Kubernetes-log, filesystem, clock, WebSocket, and HTTP dependencies; no test requires customer infrastructure.
- WVC-067: Coordinated staging validation uses server-generated GCS and S3-style targets, verifies server `GetAttrs`, and covers multi-instance ownership plus old-worker timeout.
- WVC-068: `gofmt -s`, `go vet ./...`, `golangci-lint run`, `go test ./...`, `go build -v ./...`, `helm lint`, and `helm template` pass before the implementation PR is promoted.
- WVC-069: This change has no rendered UI; computer-use visual verification is not applicable.

## Cross-repository completion
Implementation is complete only when this spec's WVC-001/WVC-002/WVC-052/WVC-067 interoperate with the server-side VC-041, VC-076 through VC-082, and VC-087 in `warpdotdev/warp-server#13839`. A worker-only test double or server-only fake is not sufficient evidence for the final protocol gate.
