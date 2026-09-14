# TECH: Kubernetes failure details for self-hosted agent runs

## Context
Kubernetes task pods can terminate with an exit code such as 143 while the self-hosted worker reports only a generic container-exit failure. Exit code 143 is consistent with the common `128 + SIGTERM` convention but does not identify why the process stopped. Kubernetes may also expose Job conditions, Pod conditions, container termination state, and short-lived Events that make the failure diagnosable.

The worker already observes most of this data:
- [`internal/worker/kubernetes.go (373-460) @ 4803ee7`](https://github.com/warpdotdev/oz-agent-worker/blob/4803ee76bffb44de29344d9eea0bec3254f4cab4/internal/worker/kubernetes.go#L373-L460) watches Jobs and Pods and performs a 30-second safety poll.
- [`internal/worker/kubernetes.go (970-1035) @ 4803ee7`](https://github.com/warpdotdev/oz-agent-worker/blob/4803ee76bffb44de29344d9eea0bec3254f4cab4/internal/worker/kubernetes.go#L970-L1035) classifies Pod and container failures. It already lists Pod Events for selected failure paths.
- [`internal/worker/backend.go (174-225) @ 4803ee7`](https://github.com/warpdotdev/oz-agent-worker/blob/4803ee76bffb44de29344d9eea0bec3254f4cab4/internal/worker/backend.go#L174-L225) carries only failure reason, exit code, and a human-readable error from a backend to the worker.
- [`internal/types/messages.go (75-90) @ 4803ee7`](https://github.com/warpdotdev/oz-agent-worker/blob/4803ee76bffb44de29344d9eea0bec3254f4cab4/internal/types/messages.go#L75-L90) sends only task ID, message, failure reason, and exit code in `task_failed`.

The server currently turns that message into terminal task state without preserving backend-specific structured evidence:
- [`workers/selfhosted/websocket.go (174-190) @ e5db184`](https://github.com/warpdotdev/warp-server/blob/e5db184d839491f17a24d26e7a679f45483b1fff/logic/ai/ambient_agents/workers/selfhosted/websocket.go#L174-L190) defines the server copy of `TaskFailedMessage`.
- [`workers/selfhosted/websocket.go (798-899) @ e5db184`](https://github.com/warpdotdev/warp-server/blob/e5db184d839491f17a24d26e7a679f45483b1fff/logic/ai/ambient_agents/workers/selfhosted/websocket.go#L798-L899) handles terminal messages and finalizes the active execution.
- [`workers/selfhosted/worker.go (31-34) @ e5db184`](https://github.com/warpdotdev/warp-server/blob/e5db184d839491f17a24d26e7a679f45483b1fff/logic/ai/ambient_agents/workers/selfhosted/worker.go#L31-L34) stores the workload-token hash in self-hosted `WorkerData`.
- [`model/ai_run_executions.go (293-299) @ e5db184`](https://github.com/warpdotdev/warp-server/blob/e5db184d839491f17a24d26e7a679f45483b1fff/model/ai_run_executions.go#L293-L299) currently replaces the entire `worker_data` value when updating it.

## Proposed changes
### Worker collection
Add optional backend-specific details to `TaskFailure`. The Kubernetes backend will populate `failure_details.kubernetes` for all Kubernetes failures where useful Job, Pod, container, condition, or Event evidence is available.

The Pod watch is the primary collection point because it already receives the full Pod status before failed Jobs are TTL-cleaned. Job-terminal handling and the safety poll remain fallback collection points. Event listing is best-effort and must never prevent failure reporting.

The worker will use a typed Kubernetes details structure with an explicit field allowlist. It will identify the failing container status by container name and kind, never by array position. It will not serialize a complete Pod, Job, or Event object. Event lookup will use the Pod or Job UID. Kubernetes returns complete Event objects, so the worker necessarily receives Event messages transiently inside the customer cluster; it will immediately project only approved metadata and will not inspect, log, retain, transmit, or persist Event messages. It will not parse arbitrary Event messages into authoritative causes in this version.

### Wire protocol
Extend the worker and server copies of `TaskFailedMessage` with:
- `execution_id`, matching the string field already sent in task assignments.
- optional `failure_details`.

The worker owns a typed `failure_details.kubernetes` schema. The server receives `failure_details` as `json.RawMessage`, preserving fields from newer workers without needing a cross-backend server schema. Both fields are additive and optional, so older peers continue to work.

Add `execution_id` to `TaskCompletedMessage` for protocol consistency, but do not use it for server-side execution selection in this work. Exact execution affinity is deferred.

### Server persistence
When `task_failed` contains `failure_details` of at most 32 KiB, store that JSON value under `failure_details` in self-hosted `AIRunExecution.WorkerData`, as a sibling of `workload_token_hash`. The `jsonb` column may normalize byte-level formatting, but it must preserve the complete semantic value, including unknown fields.

Add a specialized datastore operation that atomically updates only the `failure_details` JSON key. Do not read, decode, and replace the complete `WorkerData` object, because that could lose concurrent updates or the workload-token hash.

Until the server uses `execution_id`, persist against the same active execution selected by the existing terminal-message flow. Persistence is best-effort: oversized details, lookup failure, or database failure are logged, and task finalization continues.

This work does not change failure classification, user-facing formatting, WebSocket acknowledgement/retry behavior, or worker reattachment.

## Kubernetes field decisions
The following list records every Kubernetes field considered during design and the decision to include or omit it based on diagnostic value and customer-data risk.

### Proposed to include
- Schema version.
- Observation source: Pod watch, Job watch, or safety poll.
- Observation timestamp.
- Job name and UID. The Job is created by the worker.
- Job completion/failure condition type, status, and reason.
- Pod name and UID. The Pod is created by the Job controlled by the worker.
- Pod phase and top-level reason.
- Pod deletion timestamp.
- Pod condition type, status, reason, and transition timestamp, including `DisruptionTarget` when present.
- Failing container kind: regular container, init container, or sidecar.
- Failing container name. Worker-created containers have known names, but a failing container inherited from `podTemplate` may have a customer-defined name.
- Container state: waiting or terminated.
- Waiting reason.
- Termination reason, raw Kubernetes exit code, normalized worker exit code, and signal when Kubernetes supplies one. The normalized value uses `128 + signal` when a signal is present and otherwise equals the raw exit code.
- Container start and finish timestamps when supplied.
- Event metadata: type, reason, count, first observed timestamp, and last observed timestamp.
- A `truncated` marker if the worker omits records or fields to stay within bounds.

These fields are mostly bounded machine-shaped observations. Kubernetes reason strings are not a strict enum for custom components, so they must still be length-limited and treated as untrusted text.

### Omit: broader infrastructure identifiers
- Namespace name.
- Node name.
- Container runtime ID.
- Event reporting controller, component, and source host.

These identifiers extend beyond the execution objects created by the worker or have little value without customer-side node and runtime logs. They can disclose internal project names, cluster topology, cloud providers, installed controllers, or runtime details. Namespace is already present in some current user-facing failure messages, but this work will not duplicate it into structured `WorkerData`.

### Omit: free-form messages
- Job condition message.
- Pod top-level status message.
- Pod condition message.
- Container waiting message.
- Container termination message.
- Event message or note.

These have high diagnostic value but are arbitrary text. They may contain registry or service URLs, Secret/ConfigMap/PVC names, volume or cloud resource identifiers, admission-policy output, probe details, command output, application text, or accidentally logged secrets. Some Pod, container, and `FailedMount` Event messages already reach Warp inside the current user-facing task failure, but this work will not duplicate or expand that data in structured `failure_details`. Event listing necessarily receives messages in memory because Kubernetes does not provide field projection; the worker will not inspect, log, retain, parse, transmit, or persist them for these diagnostics.

### Considered but not proposed
- Full Pod or Job JSON.
- Pod spec, owner references, finalizers, priority, restart policy, resource requests, and termination grace period.
- Image names and registry references.
- Volume, PVC, Secret, and ConfigMap names.
- Pod IP, host IP, and other network information.
- All normal, init, and ephemeral container statuses rather than only the failing container.
- Container logs, previous-container logs, or termination-log file contents.
- Node conditions and Node Events.
- Kubelet, container-runtime, journal, kernel OOM, or audit logs.

These can help with deep incident response but are unnecessary for the first diagnostic snapshot, require broader access or substantially more data, and can contain customer code, output, credentials, or infrastructure details.

## Testing and validation
Worker tests:
- Table-driven details-builder tests cover representative container termination, waiting/init-container failure, Job deadline, Pod eviction/condition, and partially populated status inputs.
- Current `state.terminated` data is captured for the worker's `restartPolicy: Never` task pods; `lastState` is not mistaken for the current termination.
- Raw and normalized exit codes remain distinguishable when Kubernetes supplies a signal separately from an exit code.
- Event projection retains only approved metadata and cannot serialize message, reporting-controller, component, or source-host fields.
- Missing status fields and Event-list errors do not prevent `task_failed`.
- `execution_id` and `failure_details` serialize compatibly and are omitted when absent.

Server tests:
- `TaskFailedMessage` accepts `failure_details` as `json.RawMessage`, and unknown nested fields survive persistence with semantic JSON equality.
- Atomic merge preserves `workload_token_hash` and unrelated `WorkerData` keys.
- Payloads at the 32 KiB limit persist; oversized payloads are skipped without blocking finalization.
- One representative datastore failure proves diagnostic persistence does not block finalization.
- Terminal messages without the new fields retain existing behavior.

Do not commit exhaustive tests for every Kubernetes reason, exact human-readable message snapshots, log output, or every possible datastore error. Those would mostly lock in incidental implementation details rather than the contract. During implementation, use focused tests and fake-client inspection as confidence-builders, then run `go test ./...` in `oz-agent-worker`; run focused self-hosted WebSocket and execution-store tests, then `./script/presubmit`, in `warp-server`.

## Parallelization
Parallel implementation is not proposed. The worker collection, duplicated wire contract, active-execution lookup, and atomic persistence behavior are small but tightly coupled across the two repositories. Implement the server receiver and persistence first, then the worker sender; the additive optional fields keep either rollout order compatible, but server-first avoids silently dropping diagnostics from upgraded workers.

If pull requests are requested, use one `david/` branch and draft PR per repository. Keep this spec with the worker implementation and link the server PR from both PR descriptions.

## Risks and mitigations
- Sensitive customer data could cross the self-hosting boundary. Mitigation: explicit allowlist, resolved field policy above, no full Kubernetes objects, free-form messages, or logs, bounded strings, and a 32 KiB server limit.
- Kubernetes status and Events are eventually consistent and may already be deleted. Mitigation: capture immediately, treat collection as best-effort, and preserve `unknown` instead of overclaiming a cause.
- Active-execution lookup can associate a late report with a newer execution. Mitigation: include `execution_id` now and defer using it explicitly; do not imply that this work fixes the existing race.
- Diagnostic persistence could interfere with terminal state. Mitigation: log persistence failures and always continue existing finalization.

## Follow-ups
- Use terminal-message `execution_id` to select and validate the exact execution.
- Consider a customer-controlled option for transmitting selected free-form Kubernetes messages.
- Add narrowly scoped, evidence-labelled Event classifiers only if metadata proves insufficient.
- Address terminal-message acknowledgement/retry and Kubernetes Job reattachment separately.
