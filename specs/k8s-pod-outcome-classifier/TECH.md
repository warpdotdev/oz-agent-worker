# TECH: Kubernetes pod outcome classifier

## Context
The past week's substantive change in `oz-agent-worker` was [`3382ab6`](https://github.com/warpdotdev/oz-agent-worker/commit/3382ab67b65dde105b37a7b7039a51d657ff3e2d), which fixed a Kubernetes backend false failure by ignoring non-zero exit codes from native sidecar init containers (`restartPolicy: Always`). That fix is correct, but it exposes an architectural issue: Kubernetes outcome policy is still encoded as ad hoc conditionals inside pod inspection rather than as an explicit model of what can and cannot determine task failure.

Current code at `3382ab67b65dde105b37a7b7039a51d657ff3e2d`:
- [`internal/worker/kubernetes.go:123`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/internal/worker/kubernetes.go#L123) — `ExecuteTask` creates one Kubernetes Job per Oz task, then races Job watch events, Pod watch events, and a safety poll.
- [`internal/worker/kubernetes.go:477-509`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/internal/worker/kubernetes.go#L477-L509) — `handleJobState` treats Job completion as the authoritative success signal, and only asks pods for diagnostics when the Job has failed.
- [`internal/worker/kubernetes.go:929-1012`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/internal/worker/kubernetes.go#L929-L1012) — `inspectPodFailure` combines several responsibilities: native sidecar semantics, init-container setup failures, task-container failures, unschedulable detection, event lookups, pod eviction, and fallback failed-pod classification.
- [`internal/worker/kubernetes.go:1014-1027`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/internal/worker/kubernetes.go#L1014-L1027) — `restartableInitContainerNames` identifies Kubernetes native sidecars so their exit codes can be excluded from task-failure decisions.
- [`internal/worker/kubernetes.go:1029-1044`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/internal/worker/kubernetes.go#L1029-L1044) — `collectPodLogs` reads all init-container and regular-container logs when a failure is detected, which is useful diagnostically but makes failure handling depend on broad pod traversal.
- [`internal/worker/kubernetes_test.go:266-376`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/internal/worker/kubernetes_test.go#L266-L376) — regression coverage for restartable sidecars verifies exit-code suppression, image pull failures, and task-container failure attribution.
- [`internal/worker/kubernetes_test.go:378-484`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/internal/worker/kubernetes_test.go#L378-L484) — end-to-end fake-client regression test for the race where a pod wind-down event arrives before JobComplete.
- [`README.md:160-163`](https://github.com/warpdotdev/oz-agent-worker/blob/3382ab67b65dde105b37a7b7039a51d657ff3e2d/README.md#L160-L163) — documents that `restartPolicy: Always` init containers are service sidecars whose exit codes do not determine Job outcome.

The improvement opportunity is to make Kubernetes task outcome classification a first-class boundary. The worker should have one small layer that translates Kubernetes Job/Pod/container state into worker task outcomes, and the watch loop should consume that layer instead of knowing every Kubernetes exception inline.

## Proposed changes
Introduce an internal Kubernetes outcome classifier that separates pure state classification from API-backed diagnostics and log collection.

### 1. Add explicit pod outcome types
Add unexported types near the Kubernetes backend code, either in `internal/worker/kubernetes.go` or a new `internal/worker/kubernetes_outcome.go` if the file split improves readability:

```go path=null start=null
type kubernetesPodOutcomeKind int

const (
    kubernetesPodOutcomeRunning kubernetesPodOutcomeKind = iota
    kubernetesPodOutcomeFailure
)

type kubernetesPodOutcome struct {
    kind kubernetesPodOutcomeKind
    failure error
}
```

Keep the first iteration deliberately narrow: the classifier only needs `running` and `failure` because success remains Job-authoritative. Do not add a pod-success path unless a later reconciliation feature needs it; pod phase `Succeeded` can still be non-terminal from the worker's perspective until the Job controller reports `JobComplete`.

### 2. Split pure classification from event lookups
Refactor `inspectPodFailure` into two layers:
- `classifyPodFailure(pod *corev1.Pod) error` handles state that is already present on the pod: eviction reason/message, init-container terminations, init-container waiting reasons, task-container terminations, task-container waiting reasons, unschedulable conditions when the configured timeout says to fail, and `PodFailed` fallback.
- `inspectPodFailure(ctx, pod)` remains the API-aware wrapper that calls `classifyPodFailure` first, then lists events only when the pod is pending/failed and no pod-local failure was already identified.

This preserves current behavior while shrinking the surface where Kubernetes API calls can be introduced accidentally. The classifier should accept `restartableInitContainerNames(pod)` as an explicit helper result or keep that helper private inside classification; either is fine as long as native sidecar behavior is centralized.

### 3. Represent native sidecar policy as a named predicate
Replace the direct `!restartableSidecars[status.Name]` conditional with a predicate whose name encodes Kubernetes semantics:

```go path=null start=null
func initContainerExitDeterminesPodOutcome(pod *corev1.Pod, status corev1.ContainerStatus) bool
```

Implementation should return false for init containers declared with `restartPolicy: Always`, and true for ordinary init containers. Waiting-state failures for sidecars should still count, because an image-pull/admission/startup failure prevents the service sidecar from becoming available and can block the task container.

This makes the recently added exception reviewable as a Kubernetes policy rule instead of a one-off map check.

### 4. Keep watch-loop ownership narrow
`ExecuteTask` should continue to:
- Treat `handleJobState` as the terminal success/failure authority.
- Use pod watch events only for early failure detection and diagnostics.
- Use the safety ticker to poll both Job and Pod state.

The proposed classifier is not a behavior change. It is a boundary change that makes the current behavior easier to extend for future reattach/reconcile work from `specs/k8s-worker-drain-and-reattach/TECH.md`, where a replacement worker will need to classify already-running or terminal Jobs without duplicating the current watch-loop conditionals.

### 5. Optional file organization
If `kubernetes.go` remains easy to navigate after the refactor, keep the helper types in that file. If it keeps growing, move pod outcome logic to `kubernetes_outcome.go` and tests to `kubernetes_outcome_test.go`. Avoid creating a public package: this is backend-internal policy, not a reusable API.

## Testing and validation
Use the existing sidecar regression tests as the behavioral guardrail, then add smaller table-driven tests around the extracted classifier.

Unit coverage:
- `TestClassifyPodFailureIgnoresRestartableSidecarExitCodes` covers the same native sidecar cases currently in `TestInspectPodFailureIgnoresRestartableSidecarExitCodes`: sidecar SIGTERM exit is ignored, ordinary init-container exit fails, task-container exit still fails, and sidecar waiting/image-pull failure still fails.
- `TestInitContainerExitDeterminesPodOutcome` table-tests ordinary init containers, restartable init containers, missing specs, and nil restart policies.
- Existing `TestExecuteTaskSucceedsWhenSidecarExitsBeforeJobComplete` remains the end-to-end watch-loop regression so the refactor cannot reintroduce the pod-before-JobComplete race.
- Existing unschedulable, eviction, volume mount, and container diagnostic tests should continue to call `inspectPodFailure` because they exercise the API-aware wrapper and metrics classification.

Validation commands:
- `go test ./internal/worker`
- `go test ./...`

No product spec exists for this architecture cleanup. Validation should therefore reference the behavior already documented in `README.md`: restartable native sidecar exit codes do not fail tasks; ordinary init-container and task-container failures do.

## Parallelization
Parallel sub-agents are not proposed for the implementation. The useful change is a small refactor in one backend file plus adjacent unit tests, and the work is tightly coupled around preserving existing failure semantics. Splitting it across agents would increase the chance of semantic drift or merge conflicts.

If this becomes part of the larger Kubernetes reattach/reconcile follow-up, parallelization becomes useful after the classifier lands:
- `k8s-classifier` local agent on branch `oz/k8s-outcome-classifier` owns `internal/worker/kubernetes_outcome.go` and classifier tests.
- `k8s-reattach` local agent on branch `oz/k8s-reattach-reconcile` owns startup reconciliation and Job watch reattachment, using the classifier as an input.
- The main integrator merges classifier first, then reattach, with one combined PR if the reattach work depends on classifier internals.

For this spec's immediate scope, implement sequentially in the existing checkout.

## Risks and mitigations
- Risk: a refactor changes failure timing or metrics reasons without intending to. Mitigation: keep the existing `inspectPodFailure` tests intact during extraction, and add table tests before modifying watch-loop code.
- Risk: the classifier name suggests it can determine pod success, leading future code to treat `PodSucceeded` as terminal before JobComplete. Mitigation: model only `running` and `failure` in the first iteration and document that success remains Job-authoritative.
- Risk: event-backed failures such as volume mount errors blur the pure/API-aware boundary. Mitigation: keep event lookup in `inspectPodFailure` and name tests accordingly so pure classification stays deterministic.

## Follow-ups
- Use the classifier from the future Kubernetes reattach/reconcile loop so preserved Jobs from worker disruption and actively watched Jobs share one failure policy.
- Add metrics or structured debug logging for ignored restartable-sidecar terminations if operators need visibility into noisy service shutdowns without failing the task.
- Consider promoting outcome classification into a dedicated package only if a second Kubernetes execution path needs it; until then, keep it unexported and backend-local.
