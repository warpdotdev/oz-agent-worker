# Worker task tracing

## Summary
Export worker task spans to Warp using assignment OTLP configuration, and to the operator's destination using the same instrumentation and IDs. No operator collector is required. This [REMOTE-3534](https://linear.app/warpdotdev/issue/REMOTE-3534/export-self-hosted-worker-task-traces-using-assignment-otlp) spec requires Ben Navetta's approval before implementation.

## Context
Source references use worker revision `266a4bea06605f46b3bef978dbd6298fcb24dade`:
- [`internal/types/messages.go:107-140`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/types/messages.go#L107-L140) already carries endpoint, tracing enablement, bootstrap token and expiry.
- [`internal/metrics/metrics.go:267-305`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/metrics/metrics.go#L267-L305) initializes the operator provider only for a nonempty, non-`none` exporter.
- [`internal/worker/worker.go:422-1030`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/worker.go#L422-L1030) contains receipt, admission and execution boundaries. Lifecycle events are not child spans.
- [`internal/tasklogs/reporter.go:42-139`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/tasklogs/reporter.go#L42-L139) and [`token.go:29-94`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/tasklogs/token.go#L29-L94) provide bounded HTTP export, credential-safe errors and refresh-token prior art.
The [server assignment gate](https://github.com/warpdotdev/warp-server/blob/5119a422570f9b1fd96d8a537006c4e5cbba3afd/logic/ai/ambient_agents/workers/selfhosted/websocket.go#L862-L913) already controls tracing independently from logging. No server or protobuf change is required.

## Product behavior
1. Warp export requires assignment `tracing_enabled=true`, a valid endpoint and usable task authentication. Operator exporter settings, including unset or `OTEL_TRACES_EXPORTER=none`, do not disable it. Logging enablement and `CollectTaskLogs` do not control tracing. Unusable assignment configuration leaves operator tracing intact.
2. Start `oz_worker.task` at the timestamp captured immediately after `ReadMessage` returns, before JSON decoding. Create it after decoding identifies the task, with that explicit start timestamp. Each assignment gets a fresh worker-local trace. Do not propagate a new parent into the server or sandbox.
3. End the root after backend return, synchronous cleanup, task-log flush and terminal-message enqueue attempt. Include configured agent idle-on-complete time, including the default 45 minutes. Do not wait for server acknowledgement or later asynchronous cancellation retries. Command success ends at dispatch finalization, not remote completion.
4. Trace rejected and duplicate assignments as short roots with bounded outcomes and no execution children. Malformed messages produce no task trace. Different execution IDs for the same run produce separate roots. WebSocket reconnect does not end a running task trace.
5. On graceful shutdown, end any remaining observation as `interrupted` when the existing worker shutdown wait ends. Preserve running Kubernetes Jobs and dispatched tasks under current rules. Do not report remote success or failure from that interruption. Abrupt process death can lose spans.
6. End observed setup/execution/cleanup children on failure, cancellation or interruption. Keep execution semantics, cleanup policy, logging and duration metrics unchanged. Root `outcome` values are `succeeded`, `failed`, `cancelled`, `dispatched`, `rejected`, `duplicate` and `interrupted`. Set OTel status to Error for failure, Ok for success/cancellation/dispatch, and Unset otherwise. Use existing failure-reason enums.

## Technical design
### One instrumentation path, independent queues
- Add `internal/tasktraces` to own trace sessions and helpers. Have the trace portion of `metrics.Init` delegate operator initialization to this package. Keep the current operator exporter selection, resource configuration and process-global tracer behavior.
- Create one task-scoped SDK provider per assignment when either destination is enabled. Attach its root to the task context. Child helpers obtain the tracer from the active span's provider, not the global provider. Instrument each phase once.
- Fan out centrally through processors: register the task's owned Warp batch processor and a non-owning adapter to the process-owned operator batch processor. Both receive the same ended spans, with identical trace IDs, span IDs and parentage.
- The adapter forwards `OnStart`/`OnEnd`. Its `Shutdown` disables that adapter only; its `ForceFlush` is a no-op. The process owns operator flushing/shutdown. A task must never close another task's exporter.
- Retain current root sampling semantics: fresh roots are sampled, and children follow their parent. Do not add sampler-environment support in this change. Build the Warp branch even when operator initialization fails.
- OTel SDK v1.45.0 supports this design: `BatchSpanProcessor.OnEnd` enqueues without blocking by default and serializes exporter calls. Do not share a raw `SpanExporter` across task batchers; its interface does not require concurrency safety.

### Phase coverage and finalization
- In `worker.go`, pass receipt time through `handleMessage`. Store the trace session with active task state. Close it once on every return path and shutdown. All phase spans are root children with prefix `oz_worker.`.
- Common phase: `claim` covers admission and claim enqueue.
- [`docker.go:124-272`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/docker.go#L124-L272): `image_prepare`, `sidecar_prepare`, `container_start` (create/start), `execution` (wait) and `cleanup` (synchronous removal).
- [`direct.go:162-319`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/direct.go#L162-L319): `workspace_prepare`, optional `setup`, `execution` (agent process) and `cleanup` (teardown/removal).
- [`kubernetes.go:189-522`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/kubernetes.go#L189-L522): `job_create`, `pod_schedule`, observed `sidecar_prepare`/`setup`/`container_start`, `execution` (post-start observation until backend terminal result) and `cleanup` (synchronous stop/delete request). Omit unobserved remote teardown hooks.
- [`command.go:97-137`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/command.go#L97-L137): `dispatch_prepare` and `dispatch`. Omit remote execution and later cancellation.
- Reuse `setup_events.go` and `kubernetes_setup_events.go` timestamps independently from log/event reporters. Preserve observed Kubernetes timestamps; skip incomplete historical intervals. Live spans end at the observation cutoff.
- End the root before trace-export flushing, so exporter latency does not extend task duration. Finalization must be idempotent, preserve context values through detached cleanup, and reject late spans after session closure.

### Authentication, privacy and bounds
- Extract task-identity HTTP/token helpers from `tasklogs` into `internal/tasktelemetry` without changing logging. Seed tracing auth from a nonempty, unexpired bootstrap token. Refresh within one minute of expiry using task `WARP_API_KEY` and `WARP_WORKLOAD_TOKEN`; also use refresh credentials if bootstrap is unusable. Without valid auth, drop Warp exports. Never use the worker's API key.
- Use OTLP/HTTP protobuf at assignment endpoint plus `/v1/traces`. Set endpoint, headers, protocol and transport explicitly. Do not inherit operator OTLP destinations, headers or TLS overrides. Keep routing/auth task-local; never mutate process environment or global providers per task.
- Validate collector and identity URLs as HTTP(S), without userinfo, query or fragment. Allow HTTP for local deployments. Reject redirects that change scheme or host, including HTTPS downgrade. Send bearer tokens only to the validated collector and task API credentials only to the configured identity endpoint.
- Allow attributes only for service name/version, worker ID/backend, `task.id`, `run_id`, optional `execution_id`, bounded phase/outcome/failure reason and numeric exit code. Remove title, image and raw errors from task instrumentation. Exclude prompts, commands, paths, environment values, tokens and collector responses. Preserve operator resources locally; allowlist Warp resources to service name/version, worker ID/backend and run/execution IDs.
- Replace Warp exporter/auth errors with fixed messages before they reach global SDK error handling. Collector errors can reflect credentials. Telemetry failures must not change task results or prevent export to the other destination.
- Give each Warp queue 256 spans, batches of 64, a 5-second interval and a 5-second export/retry budget. Drop on overflow; never block instrumentation. Use a pipeline-lifetime context independent of task cancellation.
- After ending the root, give task export shutdown at most 5 seconds. At the deadline, cancel its pipeline context to stop in-flight export/auth work and release task credentials and buffers. During worker shutdown, close sessions concurrently under one shared 5-second telemetry deadline, not 5 seconds per task. Keep operator ownership process-scoped.

## Decisions
- **Processor fanout, not synchronous exporter fanout:** both keep identical IDs and centralized routing. Independent batchers prevent destination stalls and unsafe shared-exporter calls. This implements the requester's wrapper/fanout intent.
- **Task-scoped providers, not a global trace-to-credential registry:** sessions make credential lifetime explicit. They cost one bounded Warp queue per traced assignment. A registry would require eviction and late-span routing rules.
- **Existing worker boundary, not initial-turn completion:** Ben confirmed “Yes keep the worker boundary.” This includes idle and synchronous finalization without adding a completion signal or changing execution semantics.
- **No operator collector dependency:** the requester rejected it. Warp export remains server-gated and independent of operator settings.

## Assumptions
- Technical default for approval: export all eligible worker task traces with the bounds above. This change does not introduce a sampling or worker-tracing configuration surface.

## Out of scope
- Durable traces across worker restarts, remote completion after dispatch or preserved shutdown, distributed parent propagation and new correlation identifiers.
- New logging/metrics behavior, policy controls, UI work, live-cluster deployment or computer-use verification.

## Validation criteria
These are required implementation checks, not results claimed by this spec-only PR. Use SDK span recorders, fake backends/Kubernetes clients and `httptest` collector/identity servers.
- `TestTaskTraceEnablement`: assignment absent/disabled/invalid/valid × operator unset/none/configured/initialization-error × logging on/off. Assert exact enabled destinations and no fallback to operator credentials or endpoint.
- `TestTaskTraceDualExport`: both destinations receive identical IDs and parentage exactly once. Close one task, then export another. Assert the operator exporter stays open and process shutdown closes it once.
- `TestTaskTraceConcurrentIsolation`: interleave two tasks with distinct endpoints/tokens; assert no cross-routing, credential/resource leakage or data races.
- `TestTaskTraceWorkerBoundary`: assert root start equals pre-decode receipt time, claim is a child, idle and synchronous finalization are included, and trace flushing is excluded. Cover success, failure, rejection, duplicate, follow-up execution, cancellation, enqueue failure and reconnect.
- `TestTaskTraceBackendPhases`: cover each backend's observable phases, failure/cancellation closure, disabled cleanup, absent optional setup, Kubernetes status timestamps and Command dispatch cutoff. Assert no orphan or post-cutoff children.
- `TestTaskTraceAuthAndPrivacy`: cover bootstrap use, expiry refresh, missing/invalid bootstrap, refresh failure, redirect rejection, operator-environment isolation and hostile collector errors containing a sentinel secret. Assert only allowlisted Warp data leaves the worker.
- `TestTaskTraceBoundedShutdown`: block each collector independently; assert the other still exports, queue overflow does not block task work, task flush returns within 5 seconds, and shutdown uses one shared deadline. Assert interrupted Kubernetes traces do not delete preserved Jobs and late finalizers do not export twice.
- Run `go test -race ./internal/tasktraces ./internal/tasktelemetry ./internal/tasklogs ./internal/worker`, `go test ./...`, `go vet ./...`, `go build -v ./...`, `golangci-lint run` and the repository's `gofmt -s -l .` check.
