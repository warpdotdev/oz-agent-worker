# Self-hosted runner instance shape enforcement

## Summary
A runner's instance shape (`{ vcpus, memory_gb }`) — already a first-class field on the `Runner` data model and resolved server-side — must actually size the compute that a self-hosted `oz-agent-worker` provisions for a run. Today the worker receives and applies the runner's Docker image but ignores the instance shape, so a self-hosted run is never sized to the runner's requested SKU. This closes that gap: the resolved shape is transmitted over the dispatch WebSocket and applied by each containerized backend (Docker container resource limits, Kubernetes pod resource requests/limits). The direct backend has no container boundary and is intentionally unaffected.

## Problem
The `Runner` data model (REMOTE-1936) lets a team define compute config — image, instance shape, setup commands — and the server resolves the effective shape per run, capping it on Warp-hosted workers and leaving it unconstrained on self-hosted workers (the operator owns the compute). Warp-hosted backends (Namespace, Docker-sandbox) consume that resolved shape and size their instances. The self-hosted path does not: the shape never crosses the worker WebSocket, and none of the worker backends set CPU/memory on the container or pod. As a result, a self-hosted operator who sets a runner to "8 vCPU / 32 GB" gets whatever their backend defaults provide, with no relationship to the runner. The image, by contrast, is already resolved server-side and applied by the worker.

## Goals
- Transmit the run's resolved runner instance shape to the self-hosted worker over the existing dispatch WebSocket assignment message.
- Apply the shape in the Docker backend (container CPU/memory limits) and the Kubernetes backend (task container resource requests/limits, which become pod scheduling requirements).
- Preserve today's behavior exactly when a run has no explicit runner instance shape, and for any backend that cannot enforce one.
- Keep resolution and entitlement authority on the server; the worker applies what it is told without re-deriving runner precedence or caps.

## Non-goals
- Changing how the Docker image is resolved or transmitted — that already works end-to-end and is unchanged.
- Flipping setup-command execution from the environment to the runner — that remains deferred (REMOTE-1936) and is out of scope here.
- Sending the entire `RunnerConfig` (name, description, setup commands) over the wire. Only the resolved compute size that a backend can act on is transmitted.
- Instance-shape enforcement on the direct backend, which runs the agent as a host process with no container/cgroup boundary owned by the worker.
- Re-implementing the Warp-hosted plan/tenant cap on the worker. Capping stays server-side; self-hosted remains unconstrained by design (the operator owns the compute).
- Imposing the Warp tenant/plan default shape onto self-hosted compute. Only an explicitly configured runner shape is enforced.

## Behavior
### Transmission
1. When the server dispatches a run to a self-hosted worker and the run's resolved runner specifies an instance shape, the task assignment sent over the WebSocket carries that shape as a structured `{ vcpus, memory_gb }` value.
2. When the run's resolved runner has no instance shape (no runner referenced, or a runner with the shape unset), the assignment carries no instance shape, and behavior is byte-for-byte identical to today's assignment (the field is absent from the serialized payload).
3. The transmitted shape is the runner's explicitly configured shape, not the tenant/workspace default size. A self-hosted run with no explicit runner shape is never sized from the Warp plan default; it falls back to the operator's own backend configuration (invariants 2, 8, 11).
4. Only positive axes are meaningful. A shape value with a non-positive `vcpus` or `memory_gb` axis is treated as "unset" for that axis (invariant 10).

### Docker backend
5. When a task assignment carries an instance shape, the Docker backend creates the task container with a CPU limit derived from `vcpus` and a memory limit derived from `memory_gb`.
6. When the assignment carries no instance shape, the Docker backend creates the container with no shape-derived CPU or memory limit — identical to today.
7. A container that exceeds its memory limit is terminated by the Docker daemon (OOM); the worker already classifies and reports OOM-killed containers, and that reporting continues to apply to shape-limited containers.

### Kubernetes backend
8. When a task assignment carries an instance shape, the Kubernetes backend sets the task container's resource requests and limits from `vcpus` (CPU cores) and `memory_gb` (Gi), so the value participates in pod scheduling (node fit) and in-pod enforcement.
9. The runner shape, when present, takes precedence over any task-container resources declared in the operator's `pod_template`. When no shape is present, the `pod_template` resources (if any) are used unchanged, and when neither is present the task container has no resource requests/limits — identical to today.
10. Each axis is applied independently: a shape that sets only `vcpus` (or only `memory_gb`) sets only that resource and leaves the other to the `pod_template`/cluster default.

### Direct backend
11. The direct backend ignores any transmitted instance shape and runs the agent exactly as today. There is no container or worker-owned cgroup boundary to enforce a shape, and the operator owns host sizing.

### Compatibility and invariants
12. The shape field is additive and optional on the wire. An older worker that does not model the field ignores it and behaves as today; a newer worker that receives an assignment without the field applies no shape — so server and worker can roll out independently in either order with no run failures.
13. Instance-shape transmission and application are independent of harness and of which backend the operator runs: the same runner-backed task resolves to the same shape regardless of harness, and each backend applies it according to its own capabilities (invariants 5–11).
14. Enforcing a shape never changes which run is dispatched or to which worker; it only sizes the compute once the run reaches a containerized backend. A run is never failed solely because a shape could not be enforced (e.g. on the direct backend).
