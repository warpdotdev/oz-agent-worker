# Task telemetry sanitized diagnostics
## Context
The past week added assignment-scoped task telemetry export and then changed log export failures to include the original OTLP error in worker logs. The relevant recent commits are `93971be` and `bc5bd37`.
The current code intentionally separates task-local telemetry from operator/process telemetry, but the log exporter now has a diagnostic path that can carry arbitrary collector response bodies into process-global logs:
- [`internal/tasktelemetry/otlp/exporter.go:23 @ bc5bd37`](https://github.com/warpdotdev/oz-agent-worker/blob/bc5bd37f3159fc9c5a79c30e346dbcddeb2aadec/internal/tasktelemetry/otlp/exporter.go#L23-L43) builds task-scoped exporters with assignment endpoint and credentials.
- [`internal/tasktelemetry/otlp/exporter.go:69 @ bc5bd37`](https://github.com/warpdotdev/oz-agent-worker/blob/bc5bd37f3159fc9c5a79c30e346dbcddeb2aadec/internal/tasktelemetry/otlp/exporter.go#L69-L82) initializes the OTLP log exporter with the task-owned HTTP client.
- [`internal/tasktelemetry/otlp/exporter.go:106 @ bc5bd37`](https://github.com/warpdotdev/oz-agent-worker/blob/bc5bd37f3159fc9c5a79c30e346dbcddeb2aadec/internal/tasktelemetry/otlp/exporter.go#L106-L117) logs `Err(err)` through the process-global zerolog logger when log export fails.
- [`internal/tasktelemetry/otlp/exporter_test.go:25 @ bc5bd37`](https://github.com/warpdotdev/oz-agent-worker/blob/bc5bd37f3159fc9c5a79c30e346dbcddeb2aadec/internal/tasktelemetry/otlp/exporter_test.go#L25-L82) asserts that a collector HTTP 403 response body is preserved in worker logs and does not reach the task logger.
- [`specs/REMOTE-3534/TECH.md:48 @ bc5bd37`](https://github.com/warpdotdev/oz-agent-worker/blob/bc5bd37f3159fc9c5a79c30e346dbcddeb2aadec/specs/REMOTE-3534/TECH.md#L48-L50) already states that collector responses may reflect bearer credentials into SDK-global errors.
The immediate privacy problem is not that worker logs receive a diagnostic; they should. The problem is that the architecture has no shared, enforceable diagnostic contract for task telemetry exporter errors. Trace export sanitizes by returning fixed messages, while log export currently preserves the provider error string. Future telemetry signals or retry paths could repeat the same mistake unless sanitization is expressed as a reusable boundary.
## Proposed changes
Introduce a shared sanitized diagnostic policy inside `internal/tasktelemetry/otlp` and require every task telemetry exporter wrapper to use it before errors reach task logs, worker logs, SDK-global handlers, or callers.
1. Add a small diagnostic module, for example `internal/tasktelemetry/otlp/diagnostics.go`, with:
   - `type Signal string` values for `traces` and `logs`.
   - `type ExportDiagnostic struct { Signal Signal; Category string; StatusCode int; Retryable bool; RequestID string }`.
   - `SanitizeExportError(signal Signal, err error) (ExportDiagnostic, error)` that returns a fixed public error such as `task log export failed` or `task trace export failed` plus bounded metadata.
   - Optional extraction of safe fields from known HTTP/OTLP error types: HTTP status code, retryability/category, and collector request ID headers if accessible. Do not include response bodies, Authorization headers, endpoint URLs with user info, token values, task environment values, or raw nested error strings.
2. Refactor both exporter wrappers to use the shared sanitizer:
   - `traceExporter.ExportSpans` should keep returning the fixed trace failure error, but should pass through the same sanitizer so behavior remains consistent if structured worker diagnostics are later added.
   - `logExporter.Export` should log only sanitized fields through the worker logger and return the fixed public error. The process-global logger is acceptable only after sanitization; the task context logger should still not receive exporter diagnostics.
3. Update tests to make the privacy invariant explicit:
   - Replace `TestLogExportErrorStaysInWorkerLog` with a test that sends a collector response containing a sentinel Authorization token and verifies no worker log field contains that sentinel or arbitrary response body text.
   - Add a trace-export counterpart so both signals enforce the same contract.
   - Keep an assertion that a diagnostic event is emitted with safe status/category fields so operators can still debug routing, authentication class, and collector availability.
4. Document the contract near the code and keep `specs/REMOTE-3534/TECH.md` aligned:
   - exporter wrappers may expose fixed errors and safe metadata,
   - raw collector responses are never diagnostic output,
   - telemetry export failure never changes task outcome or blocks the other destination.
This is intentionally a small architecture hardening step rather than a broad telemetry redesign. It preserves the separation between task-local redaction and process diagnostics while making the diagnostic boundary explicit and testable.
## Testing and validation
- Add unit tests under `internal/tasktelemetry/otlp` for sanitizer behavior with:
  - HTTP 403 response body echoing `Authorization: Bearer sentinel-token`,
  - HTTP 5xx response body with arbitrary text,
  - network errors without HTTP status,
  - nil or unknown errors.
- Update log exporter tests to assert worker logs contain the fixed message and safe status/category fields, but not the sentinel, response body, token, or endpoint.
- Add trace exporter tests with the same hostile collector response to prevent signal-specific drift.
- Preserve `TestHTTPSExport` coverage so TLS isolation and task-owned clients remain unchanged.
- Run `gofmt` on changed Go files and targeted tests:
  - `go test ./internal/tasktelemetry/otlp`
  - `go test ./internal/tasktelemetry/...`
- For a full pre-merge check, run the repository's existing Go validation set from `specs/REMOTE-3534/TECH.md`: `go test ./...`, `go vet ./...`, `go build -v ./...`, and `golangci-lint run` when available.
## Parallelization
Parallel sub-agents are not necessary for the fix itself because the implementation should be small and tightly coupled to one package. If this hardening expands into broader telemetry policy work, use this split:
- `sanitizer`: local agent in worktree `../oz-agent-worker-telemetry-sanitizer` on branch `agent/telemetry-sanitizer`; owns `internal/tasktelemetry/otlp/diagnostics.go` and signal wrapper changes.
- `privacy-tests`: local agent in worktree `../oz-agent-worker-telemetry-privacy-tests` on branch `agent/telemetry-privacy-tests`; starts after the sanitizer API is sketched and owns hostile collector tests for logs/traces.
- `validation`: local agent in worktree `../oz-agent-worker-telemetry-validation` on branch `agent/telemetry-validation`; runs targeted and full Go validation after integration.
For the likely scoped fix, a single PR branch is preferable to avoid coordination overhead.
## Risks and mitigations
- Over-sanitizing could make collector issues hard to diagnose. Preserve bounded metadata such as signal, status code, retryability/category, and request ID while dropping raw bodies and nested strings.
- OTel exporter error types may not expose structured status cleanly. Treat unknown errors as `category=unknown` and prefer privacy over fidelity.
- Tests that currently assert raw diagnostic preservation must be inverted carefully so they still prove diagnostics stay out of task logs and now also prove worker logs are sanitized.
## Follow-ups
- Consider adding a small package-level comment or README for task telemetry privacy boundaries so future signal exporters use the sanitizer by default.
- Audit any new task telemetry destinations for the same rule: raw provider or collector errors may be returned only after sanitization.
