# TECH: Self-hosted runner instance shape enforcement

## Context
The `Runner` data model (REMOTE-1936, merged in `warp-server`) introduced `RunnerInstanceShape{ Vcpus, MemoryGb }` and server-side resolution of the effective compute for a run. The dispatcher resolves the shape and hands it to each worker; Warp-hosted backends apply it, but the self-hosted path drops it on the floor and no worker backend sizes its container/pod. See `PRODUCT.md` for behavior. This spec covers the cross-repo glue to transmit the resolved shape to `oz-agent-worker` and apply it per backend.

The self-hosted dispatch wire is a hand-duplicated JSON struct, **not** protobuf: the server marshals [`selfhosted.TaskAssignmentMessage`](https://github.com/warpdotdev/warp-server/blob/19782ff8fca12e6265d8dd88d3e02749adac204b/logic/ai/ambient_agents/workers/selfhosted/websocket.go#L105-L119) and the worker unmarshals its own copy, [`types.TaskAssignmentMessage`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/types/messages.go#L34-L52). Any new field must be added identically to both.

Relevant code — `warp-server` @ `19782ff`:
- [`model/types/runner.go (10-25)`](https://github.com/warpdotdev/warp-server/blob/19782ff8fca12e6265d8dd88d3e02749adac204b/model/types/runner.go#L10-L25) — `RunnerConfig` + `RunnerInstanceShape` (json: `vcpus`, `memory_gb`).
- [`logic/ai/ambient_agents/dispatcher.go (1267-1301)`](https://github.com/warpdotdev/warp-server/blob/19782ff8fca12e6265d8dd88d3e02749adac204b/logic/ai/ambient_agents/dispatcher.go#L1267-L1301) — `resolveSandboxRequirements`: resolves the shape, but falls back to the tenant default when the runner has none, and self-hosted is unconstrained.
- [`workers/selfhosted/websocket.go (423-560)`](https://github.com/warpdotdev/warp-server/blob/19782ff8fca12e6265d8dd88d3e02749adac204b/logic/ai/ambient_agents/workers/selfhosted/websocket.go#L423-L560) — `sendTaskToWorker` builds the assignment; already resolves the image via `common.ResolveDockerImageForTask`. Symmetric place to resolve the shape.
- [`logic/runners.go` `ResolveRunnerConfigForTask`](https://github.com/warpdotdev/warp-server/blob/19782ff8fca12e6265d8dd88d3e02749adac204b/logic/runners.go) — returns the resolved `*RunnerConfig` (precedence + missing-runner fall-through) without applying the tenant default.
- [`workers/namespace/namespace.go (470-481)`](https://github.com/warpdotdev/warp-server/blob/19782ff8fca12e6265d8dd88d3e02749adac204b/logic/ai/ambient_agents/workers/namespace/namespace.go#L470-L481) and [`workers/dockersandbox/dockersandbox.go (258-296)`](https://github.com/warpdotdev/warp-server/blob/19782ff8fca12e6265d8dd88d3e02749adac204b/logic/ai/ambient_agents/workers/dockersandbox/dockersandbox.go#L258-L296) — reference implementations of shape application (Warp-hosted).

Relevant code — `oz-agent-worker` @ `6e63dce`:
- [`internal/types/messages.go (34-52)`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/types/messages.go#L34-L52) — worker `TaskAssignmentMessage` (carries `docker_image`, no shape).
- [`internal/worker/backend.go (13-36)`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/worker/backend.go#L13-L36) — `TaskParams`, the backend-agnostic struct.
- [`internal/worker/worker.go (435-506)`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/worker/worker.go#L435-L506) — `prepareTaskParams` maps the assignment to `TaskParams`.
- [`internal/worker/docker.go (128-146)`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/worker/docker.go#L128-L146) — `ContainerCreate` with `HostConfig{Binds}` and no `Resources`.
- [`internal/worker/kubernetes.go (235-249)`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/worker/kubernetes.go#L235-L249) — task container build (no `Resources`); [`buildTaskPodSpec (497-529)`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/worker/kubernetes.go#L497-L529) overlays worker fields onto the `pod_template` task container.
- [`internal/worker/direct.go (130-246)`](https://github.com/warpdotdev/oz-agent-worker/blob/6e63dcea048c228cf06a99128f1d74596cc35556/internal/worker/direct.go#L130-L246) — host-process backend, no container.

## Proposed changes
### 1. Wire type (both repos, identical JSON)
Add an optional, pointer-typed shape to both `TaskAssignmentMessage` copies so it is omitted when unset and ignored by older peers:
```go path=null start=null
// matches model/types.RunnerInstanceShape json tags
type InstanceShape struct {
    Vcpus    int `json:"vcpus"`
    MemoryGb int `json:"memory_gb"`
}
// on TaskAssignmentMessage:
InstanceShape *InstanceShape `json:"instance_shape,omitempty"`
```
Server copy lives in `workers/selfhosted/websocket.go`; worker copy in `internal/types/messages.go`.

### 2. Server: populate the shape (`sendTaskToWorker`)
Resolve the runner shape symmetrically to the existing image resolution and set it **only when explicitly configured**, deliberately not threading `requirements.InstanceShape` (which carries the tenant default and would impose plan sizing on operator compute — PRODUCT invariant 3):
```go path=null start=null
if cfg, err := logic.ResolveRunnerConfigForTask(ctx, db, m.datastores, task); err != nil {
    log.Warnf(ctx, "runner shape resolve failed for task %s; sending no shape: %v", task.ID, err)
} else if cfg != nil && cfg.InstanceShape != nil &&
    (cfg.InstanceShape.Vcpus > 0 || cfg.InstanceShape.MemoryGb > 0) {
    assignment.InstanceShape = &InstanceShape{Vcpus: cfg.InstanceShape.Vcpus, MemoryGb: cfg.InstanceShape.MemoryGb}
}
```
Self-hosted is unconstrained server-side, so the resolved runner shape equals what should be applied; no clamp is needed on this path. A resolve error is non-fatal (send no shape) — consistent with the run-never-blocked-by-runner posture (REMOTE-1936 invariant 16).

### 3. Worker: thread into `TaskParams`
Add `InstanceShape *types.InstanceShape` to `TaskParams` and copy `assignment.InstanceShape` in `prepareTaskParams`. Backends read `params.InstanceShape`; `nil` means "no shape", preserving current behavior.

### 4. Docker backend
When `params.InstanceShape != nil`, set `hostConfig.Resources` before `ContainerCreate`, per positive axis (PRODUCT 5, 6, 10):
- `NanoCPUs = int64(vcpus) * 1_000_000_000`
- `Memory = int64(memoryGb) << 30` (bytes), with `MemorySwap` pinned to the same value so `memory_gb` is a hard ceiling (no extra swap) regardless of host swap configuration, matching the Kubernetes memory limit.

Docker enforces these as ceilings (no request concept). OOM handling already exists (`containerWasOOMKilled`, PRODUCT 7).

### 5. Kubernetes backend
When `params.InstanceShape != nil`, set the `task` container's `Resources` requests and limits per positive axis (PRODUCT 8, 10):
- CPU: `resource.NewQuantity(int64(vcpus), resource.DecimalSI)`
- Memory: `resource.NewQuantity(int64(memoryGb)<<30, resource.BinarySI)` (Gi)

Apply in `buildTaskPodSpec` so the runner shape overrides `pod_template` task-container resources when present, and leaves them untouched when absent (PRODUCT 9). `k8s.io/apimachinery/pkg/api/resource` is already imported. Setting requests == limits pins the task container to the requested size (predictable provisioning mirroring the SKU); this is the chosen default (see Risks for the requests-only alternative). Only the task container is sized — the `setup`/`copy-sidecar` init containers are not — so the pod is not strictly Guaranteed QoS, but the task container's requests still drive scheduling (node fit) and its limits drive enforcement.

### 6. Direct backend
No change beyond accepting the new `TaskParams` field. The shape is ignored (PRODUCT 11).

### 7. Helm / docs
No new chart value is required — the shape is dynamic per-run. Update `charts/oz-agent-worker/values.yaml` `kubernetesBackend.podTemplate` comment and the README to note that a runner instance shape overrides `pod_template` task-container `resources` per run, and that the direct backend does not enforce shapes.

## Testing and validation
- **Server unit (`workers/selfhosted/websocket_test.go`)**: extend the `sendTaskToWorker` table — runner with shape ⇒ `assignment.InstanceShape` populated (PRODUCT 1); no runner / runner without shape ⇒ `instance_shape` absent from marshaled JSON via the existing `map[string]json.RawMessage` omitempty check (PRODUCT 2, 12); confirm the tenant default is never emitted (PRODUCT 3).
- **Worker unit (`internal/worker/docker_test.go`, `kubernetes_test.go`)**: shape present ⇒ Docker `HostConfig.Resources.NanoCPUs`/`Memory` set (PRODUCT 5); k8s task container requests/limits set and overriding a `pod_template` resources block (PRODUCT 8, 9); shape absent ⇒ no resources set on either (PRODUCT 6, 9); single-axis shape sets only that axis (PRODUCT 10). The k8s suite already constructs backends with fake clientsets; assert on the built `*batchv1.Job` pod spec.
- **Worker unit (`prepareTaskParams`)**: `assignment.InstanceShape` round-trips into `TaskParams.InstanceShape` (PRODUCT 3 threading).
- **End-to-end (self-hosted, oz-local + oz-agent-worker)**: dispatch one runner at two shapes through the Docker backend and confirm each container boots with the requested CPU/memory limits; repeat against the Kubernetes backend and confirm the task pod is created with matching requests/limits and schedules. Mirrors REMOTE-1936 `TECH.md` "Self-hosted end-to-end" validation.
- **Compatibility**: old worker + new server (extra field ignored) and new worker + old server (nil shape ⇒ no limits) both run unchanged (PRODUCT 12).
- Run `go test ./...` in `oz-agent-worker`; `./script/presubmit` in `warp-server`; `helm template` to confirm the chart still renders.

## Parallelization
Two-repo change, but tightly coupled by an identical wire contract, so it is sequenced rather than fanned out. Land the additive wire field + server populate (`warp-server`) and the worker field + backend application (`oz-agent-worker`) as two PRs; because the field is optional both can merge in either order without breaking dispatch (PRODUCT 12). Within `oz-agent-worker` the Docker and Kubernetes backend edits touch separate files and could be split across local sub-agents on separate worktree branches (`oz/runner-shape-docker`, `oz/runner-shape-k8s`), but the wire/`TaskParams` change they both depend on is small and shared, so the coordination overhead outweighs the wall-clock savings — implement sequentially in one branch per repo.

## Risks and mitigations
- **Kubernetes requests == limits vs operator policy.** Setting requests == limits can collide with a cluster `LimitRange`/`ResourceQuota` or make pods unschedulable on small nodes. *Mitigation:* the shape only applies when a runner explicitly sets one (opt-in); document the `pod_template` override semantics; if operators need burstable task pods, a follow-up can expose a requests-vs-limits policy. The requests-only alternative was considered and rejected as the default because it does not actually bound memory (no hard limit), diverging from Warp-hosted behavior.
- **Docker OOM / CPU throttling surprise.** A too-small `memory_gb` will OOM-kill the task. *Mitigation:* existing OOM classification reports it clearly; self-hosted operators own the shape values.
- **Rollout ordering.** *Mitigation:* additive `omitempty` pointer field; neither side hard-requires the other (PRODUCT 12), validated by compatibility tests.
- **Wire drift between the two hand-copied structs.** *Mitigation:* identical JSON tags matching `RunnerInstanceShape`; assert the exact `instance_shape` JSON shape in both repos' tests.

## Follow-ups
- Optional chart/runner control over Kubernetes QoS (requests-only vs requests==limits) if operators need burstable task pods.
- Surface shape-vs-cluster incompatibility (e.g. unschedulable due to no node fit) as a clearer task failure reason, building on the existing unschedulable detection.
- Revisit transmitting richer runner config (e.g. setup commands) only if/when the deferred setup-command execution flip (REMOTE-1936) lands.
