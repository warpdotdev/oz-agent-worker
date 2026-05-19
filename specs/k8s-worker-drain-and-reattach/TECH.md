# Context
Kubernetes self-hosted workers run as a long-lived `oz-agent-worker` Deployment while each Oz run executes as a Kubernetes Job. This is the right execution boundary for clusters that use Karpenter, but the current worker shutdown path still treats the worker process as the owner of active task lifecycles.
Relevant code:
- `internal/worker/worker.go:58` stores active task cancel functions in `Worker.activeTasks`.
- `internal/worker/worker.go:367` derives each task context from the worker context before calling the backend.
- `internal/worker/worker.go:625` cancels every active task when the process receives SIGTERM.
- `internal/worker/kubernetes.go:123` creates a Job per task and then watches Job and Pod state.
- `internal/worker/kubernetes.go:285` deletes Jobs in a defer when the task context is cancelled or cleanup is enabled.
- `internal/worker/kubernetes.go:399` deletes all Jobs labeled for the worker during Kubernetes backend shutdown.
- `internal/worker/backend.go:38` defines the backend interface used by the shared worker lifecycle.
- `charts/oz-agent-worker/templates/deployment.yaml:15` deploys the worker as a single-replica Deployment for a given worker ID.
With this behavior, a normal Kubernetes pod termination caused by Karpenter consolidation sends SIGTERM to the worker, the worker cancels active task contexts, and the Kubernetes backend deletes the task Jobs. The task pod may have been perfectly healthy, but the worker relocation kills the run anyway.
# Proposed changes
Implement the smallest durable fix by making Kubernetes backend tasks survive worker shutdown.
Add a backend capability that lets the shared worker know whether task execution should be detached during process shutdown. Docker and direct backends keep the current behavior because their child process/container lifetimes are owned by the worker host. The Kubernetes backend opts into preservation because the durable unit is the Kubernetes Job.
Worker lifecycle changes:
- Introduce an optional backend interface such as `PreservesTasksOnShutdown() bool`.
- In `Worker.Shutdown`, skip cancelling `activeTasks` when the backend opts into preservation.
- Still cancel the worker context and close the WebSocket so the process exits cleanly and the control plane stops assigning new work to that connection.
Kubernetes backend changes:
- Have `KubernetesBackend` opt into task preservation on shutdown.
- Change the `ExecuteTask` cancellation branch so context cancellation stops local watching and returns a cancellation error without deleting the Job when the cancellation is caused by worker shutdown.
- Change `KubernetesBackend.Shutdown` to avoid deleting all worker-labeled Jobs. Its previous cleanup behavior is unsafe for drainable workers because it destroys unrelated in-flight Jobs on pod termination.
- Preserve existing cleanup after terminal Job completion when `cleanup=true`; completed/failed Jobs should still be deleted by the worker that observes the terminal state.
- Set `ttlSecondsAfterFinished` on task Jobs when cleanup is enabled so preserved Jobs that finish while no worker is watching are eventually garbage-collected by Kubernetes.
- Wrap the Kubernetes task command so the task Pod itself calls `harness-support report-shutdown` after the agent process exits. This lets a preserved Job finalize its active execution through the task-scoped workload token even when the original worker WebSocket is gone.
Helm/documentation changes:
- Add a chart value for `worker.terminationGracePeriodSeconds`, defaulting to a short bounded value, so operators can make SIGTERM handling explicit without relying on `do-not-disrupt`.
- Add a chart value for `kubernetesBackend.ttlSecondsAfterFinished`, defaulting to one day, as the fallback cleanup window for Jobs that outlive their original worker pod.
- Document that worker pod disruption no longer intentionally deletes active task Jobs, while task pod/node disruption can still interrupt the live run.
This MVP does not fully reattach a replacement worker to already-running Jobs and send final `task_completed` / `task_failed` messages. It prevents the immediate destructive behavior, which is the highest-leverage first fix. The task-side `report-shutdown` hook should finalize the execution on normal agent process exit even if the original worker is gone, but it is still not a full reconciliation mechanism: if the task Pod cannot reach Warp, the workload token expires, or the Pod dies before the hook runs, the control-plane execution can remain open until a replacement reconciles it or server-side stale-task cleanup fires. That delayed finalization can block handoff to a follow-up run even though the Kubernetes Pod was preserved. A follow-up should persist/reconcile active Kubernetes Jobs so a new worker can resume monitoring and finalize abandoned Jobs instead of relying on the agent runtime and stale-task timeout.
# Cleanup behavior
Changing shutdown to preserve Jobs must not mean Jobs accumulate forever.
There are three cleanup paths:
- Normal observed completion: if the worker that created the Job is still watching when the Job completes or fails, the existing `cleanup=true` behavior still deletes the terminal Job immediately.
- Worker-disrupted completion: if the worker pod terminates first and the Job later completes while no worker is watching, `ttlSecondsAfterFinished` lets the Kubernetes TTL controller garbage-collect the terminal Job. The Helm chart defaults this fallback to one day via `kubernetesBackend.ttlSecondsAfterFinished`, and the worker omits the TTL when `cleanup=false` so debugging behavior remains opt-in.
- Reattach/reconcile follow-up: the durable fix should let a replacement worker reattach to preserved Jobs, report terminal task state, and delete or TTL-clean reconciled Jobs idempotently.
The remaining caveat is Jobs that never reach a terminal state. `ttlSecondsAfterFinished` only starts after completion or failure, so truly stuck running Jobs still need either an operator-configured `activeDeadlineSeconds`, a future reconciliation loop that can classify stale Jobs, or explicit cancellation semantics.
# Zero-loss Karpenter disruption scope
There are two different disruption cases behind “zero loss,” and they have different implementation costs.
## Worker pod relocated, task pod remains running
This is the customer’s immediate scenario when Karpenter moves the long-lived worker pod but leaves the task Job/Pod alone. The MVP in this PR is the destructive-behavior fix: worker termination no longer cancels the active task context or deletes the task Job.
To make this fully zero-loss instead of “do not kill the Job,” the next increment is reattach/reconcile:
- Persist the Kubernetes Job name, namespace, worker ID, task ID, and current backend state in worker/task execution data when creating a Job.
- On worker startup, list Jobs labeled with the worker ID/task hash labels and match them to open tasks in the control plane.
- Recreate local watches for matched Jobs and Pods, then send `task_completed` or `task_failed` when a preserved Job reaches a terminal state.
- Make finalization idempotent so both the old worker (if it exits slowly) and the replacement worker can safely race to report the same terminal outcome.
- Add cleanup for orphaned terminal Jobs after successful reconciliation.
Estimated effort: roughly 2-4 engineering days if the control-plane APIs already expose the needed open-task lookup/update hooks; closer to 1 week if we need to add worker-data persistence or a new reattach handshake.
This PR includes a fallback `ttlSecondsAfterFinished` so completed Jobs do not accumulate indefinitely before reattach/reconcile exists. That TTL only applies after the Job reaches a terminal state; it does not replace reattach/finalization and does not stop truly stuck running Jobs without `activeDeadlineSeconds`.
## Task pod or task node evicted
This is a larger project. If Karpenter evicts the actual task pod, the live process, PTY/session stream, and ephemeral workspace state are gone unless we make the agent runtime resumable.
Zero loss for task-pod eviction would require:
- Durable workspace storage, such as a per-task PVC or snapshot/checkpoint mechanism, rather than only pod-local `emptyDir`.
- Agent/session checkpointing so the replacement pod can resume from a durable conversation/run state without replaying unsafe side effects.
- A retry/resume protocol between worker, control plane, and Oz CLI that distinguishes “worker observer moved” from “task process died but can be resumed.”
- Task pod disruption policy knobs, such as a PDB or Karpenter `do-not-disrupt`/expiry guidance on task pods only, to reduce eviction frequency while preserving node rotation.
- Clear customer-facing semantics for what is guaranteed: worker-pod relocation can be lossless; task-pod eviction is resumable only after durable workspace/session support lands.
Estimated effort: at least 2-4 weeks for a production-ready resumable path, and potentially more depending on how much durable PTY/session/workspace support already exists in Oz. Until then, the safest product behavior is to preserve Jobs across worker relocation and report task-pod eviction as retryable/interrupted with a specific reason.
# Testing and validation
Unit tests:
- Add a worker shutdown test proving Kubernetes-style preserving backends are not actively cancelled during `Worker.Shutdown`.
- Add a Kubernetes backend test proving `Shutdown` does not delete worker-labeled Jobs.
- Add a Kubernetes backend test proving `ExecuteTask` does not delete a Job when its context is cancelled before terminal Job completion.
- Keep existing completion cleanup tests passing so terminal Jobs are still cleaned up when `cleanup=true`.
Validation commands:
- `go test ./internal/worker`
- `go test ./...` if focused tests pass and runtime is acceptable.
- `helm template` against `charts/oz-agent-worker` with required image tag and worker ID to confirm the new chart value renders.
# Parallelization
Parallel sub-agents are not proposed. The change is tightly scoped to one repo and a small set of coupled files (`worker.go`, `kubernetes.go`, Kubernetes tests, and Helm chart templates). Splitting this across agents would create merge conflicts and add coordination overhead beyond the implementation cost.
# Risks and mitigations
- Risk: preserving Jobs without reattach can leave a task row/execution in an open state if the task-side shutdown report does not run successfully and no worker observes the terminal Job. This can delay handoff to a subsequent run until server-side stale-task cleanup or manual intervention. Mitigation: have the Kubernetes wrapper call `harness-support report-shutdown` from inside the task Pod, rely on the existing stale-task timeout only as a safety net, and prioritize replacement-worker reattach/reconcile before positioning this as seamless handoff.
- Risk: shutdown-triggered context cancellation is indistinguishable from explicit task cancellation inside `KubernetesBackend.ExecuteTask`. Mitigation: preserve Jobs only for worker-level shutdown, while explicit task cancellation should remain destructive in a follow-up by threading cancellation reason through the backend.
- Risk: completed Jobs may accumulate if the worker is repeatedly disrupted. Mitigation: keep terminal cleanup when a worker observes completion and add reattach/reconcile cleanup in the follow-up.
# Follow-ups
- Persist Kubernetes Job identity in worker data / execution data.
- On worker startup, list open Jobs for the worker ID and reattach watches for tasks still open in the control plane.
- Add an explicit draining state/message in the worker WebSocket protocol so the control plane can distinguish a healthy idle worker from one that is terminating.
- Add customer-facing Karpenter guidance for worker Deployment disruption vs task pod disruption.
- Design durable task-pod resume semantics before promising zero loss when Karpenter evicts the task pod itself.
