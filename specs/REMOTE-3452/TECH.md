# Self-hosted Kubernetes network policy enforcement
## Summary
Implement the behaviors in [PRODUCT.md](PRODUCT.md) across the worker and warp-server. This is a spec-only PR. Approval authorizes later implementation, not deployment or a feature-flag change.
## Context
Inspected source commits:
- Worker: `266a4bea06605f46b3bef978dbd6298fcb24dade`.
- Server: `e8ce83577401d37056ab85122b5b63c459ec0b25`.
- Protocol repository: `30d54b549cf87930f3f071b38ade82bf13305ba2`. Self-hosted assignments use mirrored JSON, not protobuf. No warp-proto-apis change is required.
Current data flow and implementation anchors:
- [Server `dispatch/worker.go:33-38`](https://github.com/warpdotdev/warp-server/blob/e8ce83577401d37056ab85122b5b63c459ec0b25/logic/ai/ambient_agents/dispatch/worker.go#L33-L38) carries `SandboxRequirements.NetworkPolicy`. [Dispatcher `:1862-1875`](https://github.com/warpdotdev/warp-server/blob/e8ce83577401d37056ab85122b5b63c459ec0b25/logic/ai/ambient_agents/dispatcher.go#L1862-L1875) resolves it under `WorkerNetworkPoliciesEnabled`.
- [Server `networking/network_policy.go:28-116`](https://github.com/warpdotdev/warp-server/blob/e8ce83577401d37056ab85122b5b63c459ec0b25/logic/ai/ambient_agents/networking/network_policy.go#L28-L116) already implements REMOTE-3510: declaration union, deny subtraction, system allowances, and nil when neither scope declares a policy.
- [Server `model/types/network_policy.go:11-48`](https://github.com/warpdotdev/warp-server/blob/e8ce83577401d37056ab85122b5b63c459ec0b25/model/types/network_policy.go#L11-L48) defines the resolved rules. [Host validation](https://github.com/warpdotdev/warp-server/blob/e8ce83577401d37056ab85122b5b63c459ec0b25/util/network_hosts.go#L28-L60) accepts normalized literal DNS names and IPv4/IPv6 addresses, not URLs, wildcard hosts, or CIDRs.
- [Server `selfhosted/worker.go:148-198`](https://github.com/warpdotdev/warp-server/blob/e8ce83577401d37056ab85122b5b63c459ec0b25/logic/ai/ambient_agents/workers/selfhosted/worker.go#L148-L198) receives requirements but does not forward the policy. Assignment construction is in [server `websocket.go:838-850`](https://github.com/warpdotdev/warp-server/blob/e8ce83577401d37056ab85122b5b63c459ec0b25/logic/ai/ambient_agents/workers/selfhosted/websocket.go#L838-L850).
- [Worker `internal/types/messages.go:120-149`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/types/messages.go#L120-L149) mirrors the assignment. [Worker `worker.go:660-734`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/worker.go#L660-L734) claims before backend provisioning. `prepareTaskParams` transfers assignment fields to `TaskParams`.
- JSON decoding does not reject unknown fields. An old worker can silently ignore an added policy field. Ben explicitly excluded capability negotiation; coordinated upgrade is a prerequisite, not a guarantee supplied by this protocol.
- [Worker `kubernetes.go:360-419`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/kubernetes.go#L360-L419) creates Jobs, retains failures, and cleans successful Jobs. [Cancellation and shutdown `:571-626`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/kubernetes.go#L571-L626) distinguish deleting a Job from preserving it across worker disruption.
- [Worker `kubernetes_api.go:133-248`](https://github.com/warpdotdev/oz-agent-worker/blob/266a4bea06605f46b3bef978dbd6298fcb24dade/internal/worker/kubernetes_api.go#L133-L248) requests asynchronous stop and background deletion. Neither operation confirms that Pods have stopped.
- Config wiring spans `internal/config/config.go`, `main.go:197-309`, `internal/worker/kubernetes.go:93-126`, and Helm `values.yaml` / `templates/configmap.yaml`. The chart's `templates/role.yaml` has no policy permissions.
## Proposed changes
### 1. Operator configuration and supported boundary
- Add `backend.kubernetes.supports_network_policies: false`. Wire it through `mergeConfig` to `KubernetesBackendConfig.SupportsNetworkPolicies`. Add Helm `kubernetesBackend.supportsNetworkPolicies: false`.
- This is a file/Helm option. Do not add a global CLI boolean that can imply support on Docker, direct, or command backends.
- Add a `network_policy_dns` block under `backend.kubernetes`, with Helm equivalent `kubernetesBackend.networkPolicyDNS`:
  - `server_ips`: explicit resolver IPv4/IPv6 addresses; default empty.
  - `namespace`: DNS Pod namespace; default `kube-system`.
  - `pod_labels`: DNS Pod labels; default `{"k8s-app": "kube-dns"}`.
- When support is enabled, require nonempty, valid resolver addresses and nonempty namespace/labels. These describe the cluster's trusted DNS service. Use them for native DNS grants and Cilium DNS endpoint selection.
- The native resolver sends lookups to the configured resolver addresses, with bounded UDP/TCP resolution. The task's final DNS configuration must use the same trusted DNS view. Document how to obtain the cluster DNS Service addresses.
- V1 targets ordinary Pod-network tasks with cluster DNS. NodeLocal DNS, custom DNS forwarding, service meshes, and secondary interfaces require operator validation of equivalent enforcement; the worker must not synthesize broad network grants to accommodate them.
- Before enabling support, the operator must install and verify a persistent native egress default-deny policy selecting the reserved label `oz-network-restricted=true`. Ship an example manifest and installation instructions in the worker README. Do not create this baseline moments before each task.
- The operator must verify that the CNI has handled the baseline before enabling support. Kubernetes exposes no portable policy-enforced readiness status. A Helm success or successful policy POST does not satisfy this prerequisite.
- The baseline must not select unrestricted task Pods. Other native, Cilium, or administrative policies must not widen restricted Pods' egress. Admission must preserve the worker's selector labels and the task's network boundary.
- Task identities must not create Pods, modify labels/policies, access a privileged daemon, or escape to host networking. The task ServiceAccount remains separate from the worker's policy-management ServiceAccount.
- Reject restricted templates with `hostNetwork: true`, a privileged container, or added `NET_ADMIN`/`SYS_ADMIN` capabilities. Inspect regular and init containers in the final merged template. Root sidecar-copy init containers remain supported; root UID alone is not privileged mode.
- Do not run connectivity probes per task. Real-CNI tests validate the implementation; production enforcement still relies on the operator's attestation.
### 2. Wire contract and admission
- Add `network_policy,omitempty` to both `TaskAssignmentMessage` types. The server field is `*types.NetworkPolicy`; the worker defines equivalent policy/rule structs in `internal/types/messages.go`.
- Preserve `rules`, `hosts`, `action`, and `description`. `action` is the JSON string `ALLOW`. Descriptions do not change enforcement.
- Omit the policy for nil requirements. Preserve a non-nil empty policy as an object. Accept both empty and null `rules` arrays as zero rules; an absent/null policy remains unrestricted.
- Pass the existing requirements pointer through `SelfHostedWorker.Process`, `SendTaskAndWaitForClaim`, and `sendTaskToWorker`. Do not independently resolve declarations in the assignment builder. Transfer it through `prepareTaskParams` to `TaskParams.NetworkPolicy`.
- Do not add advertised capabilities, new handshake headers, worker-version routing, or a server-side opt-in lookup. Leave connection selection unchanged.
- Every worker in a policy-bearing pool must run the upgraded worker. Its local opt-in determines whether it can apply the received policy. A disabled or unsupported upgraded worker rejects restricted assignments; the server cannot enforce this check on old workers.
- Preserve existing disconnected-worker availability/claim-timeout behavior and capacity requeue behavior.
- Validate the received policy before claiming or provisioning: known action, nonempty hosts per rule, and the server's canonical literal-host grammar. Do not silently skip bad rules, invalid addresses, or unknown actions.
- Extend `TaskRejectedMessage` with optional `execution_id` and `code`. New worker rejections echo the assignment execution ID. Codes `network_policy_unsupported` and `network_policy_invalid` are terminal; absent code retains legacy capacity behavior.
- Convert those two rejection codes to non-retryable user errors in the server claim path. Require a matching pending execution and selected connection for coded policy rejections. A late rejection must not finalize a successor execution.
- A disabled/unsupported upgraded worker receiving a policy rejects it through this coded path without invoking its backend. Old workers cannot supply this protection; document the coordinated rollout requirement.
- Keep the current early `task_claimed` timing for accepted work. Actual policy API calls happen after claim, so DNS/API latency does not consume the existing ten-second claim window.
- After claim, report enforcement failures through `task_failed`, not `task_rejected`. Add bounded failure reason `network_policy` to worker metrics and the server's mirrored failure classification.
- Classify this reason as `FAILED`, with automatic retries and new-execution retries disabled. The current generic self-hosted user-error constructor marks failures retryable; special-case this reason rather than inheriting that behavior.
### 3. Provider selection and translation
- Add a small provider boundary in `internal/worker/kubernetes_network_policy.go`: discover provider, compile policy, create policy, list owned policies, and delete by captured UID. Keep the native typed client and a Cilium dynamic client behind this boundary.
- Discover `cilium.io/v2` / `ciliumnetworkpolicies` with API discovery when support is enabled. Use a dynamic client for that resource; do not add a full Cilium agent dependency.
- A definite missing group/resource selects native `networking.k8s.io/v1` / `networkpolicies`. A discovery timeout, authorization error, or partial discovery failure is not evidence of absence.
- If the Cilium API exists, select Cilium. A Cilium create/get/list/delete failure must not trigger native fallback. A CRD's existence is not proof that Cilium enforcement is active; the operator attests that prerequisite.
- Discover the provider for each accepted restricted assignment before resource creation. Initialization failure reports terminal `network_policy` failure. Unrestricted assignments do not require policy discovery or new policy permissions.
- Explicitly set `policyTypes: [Egress]` for native policies. For Cilium, set `enableDefaultDeny.egress: true` and omit ingress rules/default-deny changes.
- Use the per-launch reserved selector described below, never an empty selector or the worker's entire pool.
- For native policies, compile literal IPv4 and IPv6 addresses to exact `/32` and `/128` `ipBlock` destinations. Resolve hostname rules once using an injectable resolver. Include all returned A/AAAA addresses; deduplicate and sort them.
- Any required hostname lookup error or zero-address answer fails the execution before its Job is resumed. Do not emit a partial allow list after resolution failure.
- For Cilium, compile literal addresses to exact `toCIDR` destinations and hostname strings to `toFQDNs.matchName`. Do not use match patterns or widen to `world`, `cluster`, or `host` entities.
- Host rules have no port constraint. Do not restrict task traffic to HTTPS merely because a system host usually uses port 443.
- If hostname rules exist, add DNS UDP/TCP port 53 access only to the configured DNS service:
  - Native: separate exact resolver-IP peers and a combined DNS namespace/Pod selector. This covers Service-IP and endpoint policy evaluation without allowing all Pods or all namespaces.
  - Cilium: `toEndpoints` matching the configured DNS namespace/labels, with port 53 DNS-proxy rules for the allowed query names.
- With zero resolved rules, emit no DNS exception and no allow rule. Never serialize an empty peer/rule object that means allow-all.
- Do not resolve Cilium hostname rules to worker-side IP snapshots. Cilium manages DNS-to-IP tracking. Treat CNAME chains according to Cilium's documented DNS handling; do not add unrelated hostname grants.
- Cluster Service DNAT and Cilium-managed endpoint identity can deny traffic that an IP rule names. Document these provider limits. Never solve reachability by granting an entire cluster/network. Acceptance tests use controlled external destinations and the declared DNS service.
### 4. Launch ordering and failure safety
- Keep nil-policy launches on their existing path. The following state machine applies only to restricted launches.
- Allocate a fresh UUID token for each created Job. Add worker-owned labels `oz-network-restricted=true` and `oz-network-policy-id=<token>` to its Job and Pod template. Operator extra labels must not override these values.
- Create the Job with `suspend: true`, `backoffLimit: 0`, and `restartPolicy: Never`. Its metadata records launch state `prepared`. Capture its UID before policy creation. A suspended Job starts no init, setup, or task containers.
- Compile and create one provider policy in the task namespace. Select its token label. Store worker/task/execution identity, provider, Job name, and full Job UID in policy labels/annotations.
- Check the API response matches the intended selector and rules. Admission mutation that broadens the policy fails the launch. Unknown timeouts are ambiguous: re-read by name and verify identity/content before proceeding.
- Resume only with a UID/resourceVersion-fenced update that changes `prepared` to `launched` and clears suspension. A conflicting or replaced Job is not safe to resume.
- The persistent, already-enforced baseline supplies startup isolation while the per-execution allow policy propagates. Do not substitute a sleep or successful API POST for that prerequisite.
- Give restricted provisioning a two-minute context budget. On failure, leave the Job suspended or request fenced expiration with `parallelism: 0` and `activeDeadlineSeconds: 1`. Do not launch task code to report a worker error.
- Treat `AlreadyExists` and ambiguous Job-create responses as identity-sensitive outcomes. Never replace another Job/policy or attach a new allowance to an existing live execution. Fail the attempted launch unless the response proves ownership of its own creation.
- Never delete the policy in a generic `ExecuteTask` defer. Cleanup is a separate state check. Failure or cancellation can return before Kubernetes has terminated containers.
### 5. Policy cleanup and reconciliation
- Implement policy-only reconciliation at worker startup and every 30 seconds while support is enabled. Scope lists to the configured namespace and worker-pool hash. It does not need server secrets or full run reattachment.
- Do not make the policy a Job-owned garbage-collection dependent. Background Job deletion could garbage-collect the policy while Pods still terminate. Explicit reconciliation controls deletion order.
- A policy is eligible for deletion only when its captured Job cannot automatically launch more Pods and all of that Job's Pods are terminal or absent.
- Re-read the Job by name and compare the recorded full UID. For an existing matching Job, require a terminal `Complete` or `Failed` condition. A nonterminal Job with zero current Pods is not stopped.
- List Pods by captured Job controller UID and verify owner references. Require terminal Pod phase and no observed running/waiting regular, init, or native-sidecar container. Keep the policy for `Pending`, `Running`, `Unknown`, or unconfirmed termination.
- For a deleted or replaced Job, require all Pods belonging to the old UID to be terminal or absent before deleting its policy. A new Job with the same name has a different token and must remain unaffected.
- On normal success or failure, reconcile immediately after terminal observations. With `NoCleanup`, retain Job/Pod objects but still delete eligible policies. Keep failed-Job retention and TTL behavior unchanged.
- On cancellation, request Job deletion as today but retain the policy during background deletion and Pod grace periods. Policy reconciliation continues independently after the task context ends.
- On worker shutdown, preserve live Jobs and policies. A replacement worker uses their persisted metadata to clean terminal policies. If no replacement runs, safe leaks can remain until an operator performs verified cleanup.
- Detect abandoned `prepared` Jobs older than five minutes. Fenced updates atomically set launch state `aborted`, zero parallelism, and the one-second deadline before clearing suspension. A delayed provisioner must refuse to resume an `aborted` Job.
- This abort path covers crashes between suspended Job creation, policy creation, and resume. Do not expire `launched` Jobs merely because the original worker disappeared.
- Retry cleanup with existing Kubernetes API backoff and a detached bounded cleanup context. Use UID preconditions on policy deletes. `NotFound` is success; permission errors, stale observations, and API outages retain isolation and emit diagnostics.
- Do not infer physical termination from force deletion or a partitioned node. Cluster fencing and privileged operator actions remain within the operator trust boundary.
### 6. Helm, documentation, and rollout
- When the opt-in is enabled, add a namespaced Role rule for `networking.k8s.io` `networkpolicies`: `create`, `get`, `list`, `delete`.
- Also grant `cilium.io` `ciliumnetworkpolicies` with those verbs. An RBAC rule for a missing resource is harmless; Helm must render without the CRD installed. Do not install a Cilium CRD or ClusterRole.
- Existing Job patch rights support suspend/resume/expiration. Existing Pod get/list/watch rights support termination checks. The API discovery behavior uses the cluster's standard discovery permissions; surface denial rather than granting cluster administration.
- Update `templates/configmap.yaml`, values, config validation, and merge tests together. Render disabled/enabled configurations and `rbac.create=false`.
- Document the operator baseline manifest, trusted DNS configuration, additive policies, supported CNI behavior, bypass restrictions, and the fact that node image pulls are not Pod egress.
- Publish the upgraded worker/chart first. With policy-bearing dispatch disabled for the target pool, upgrade every connected replica to an immutable supported image, verify cluster prerequisites, and explicitly set the local opt-in.
- Only then deploy or enable the assignment-plumbing change that sends non-nil policies. Coordinate with the existing server feature gate and policy configuration; do not add a new negotiation mechanism or assume the global gate is pool-scoped.
- If old and new worker replicas coexist while policy-bearing dispatch is enabled, old replicas can execute unrestricted. This is an unsupported, insecure rollout state, not a case the new server detects. Rollback requires stopping policy-bearing assignment before reintroducing old binaries.
- Preserve the existing `WorkerNetworkPoliciesEnabled` gate. Turning it off intentionally restores nil-policy dispatch for new executions; it is not a secure workaround for unsupported workers.
- Drain existing restricted Jobs and policies before removing policy RBAC or disabling their reconciliation. Never remove live policy objects as a rollback shortcut.
## Decisions and alternatives
- **Cilium first, native snapshot second — Ben's choice.** Cilium tracks DNS changes but requires its API/CNI. Native policy works with more CNIs but freezes A/AAAA answers and shares IP-level allowances. Rejecting all non-Cilium clusters and building a proxy were not chosen.
- **No native refresh — Ben's choice.** It reduces controller complexity for short-lived runs. Long/debug-idle executions can lose reachability or retain stale allowed IPs; the spec does not hide that trade-off.
- **Operator attestation — Ben's choice.** Disabled-by-default opt-in makes the prerequisite explicit. Per-run probes add latency and can confuse an unreachable target with correct enforcement; they do not prove universal isolation.
- **No capability negotiation — Ben's explicit scope limit.** A handshake could protect mixed-version pools, but is not part of this change. Coordinated upgrade avoids that protocol expansion and leaves old-worker safety dependent on the rollout prerequisite.
- **Terminal enforcement failures — Ben's choice.** Existing capacity requeue semantics cannot represent permanent incompatibility. Coded preclaim rejection and non-retryable postclaim failure preserve this distinction.
- **Execution-bound cleanup — Ben's preference, clarified.** Retained Job objects do not require retained policies after all execution stops. Deleting on agent conversation completion is unsafe for idle processes and sidecars.
- **Suspended Job and explicit reconciliation — technical design.** They provide a durable identity before policy creation and preserve isolation during termination. Automatic Job owner-reference cleanup is smaller but has a background-deletion race.
## Assumptions
- **Assumption:** The operator can provide resolver addresses and DNS Pod selectors for one trusted cluster-DNS view. Additional DNS topologies require validation; this does not authorize a catch-all DNS grant.
- **Assumption:** Provider initialization can use standard API discovery. Missing discovery permission is a failure, not permission to downgrade.
- **Assumption:** Operators can hold policy-bearing dispatch until all replicas in a target pool are upgraded. The opt-in cannot compensate for an old replica that ignores the field.
- **Assumption:** Full task reattachment is unnecessary for policy cleanup. Pool-scoped policy/Job/Pod metadata is sufficient to reclaim confirmed terminal resources.
- **Assumption:** The two-minute provisioning budget, five-minute orphan-preparation cutoff, and 30-second sweep interval are initial internal constants, not new product settings.
- No unanswered interview decisions remain. These implementation assumptions remain visible for spec review.
## Out of scope
Apply the product non-goals. Do not change server policy resolution, introduce a new policy language, promise strict hostname/DLP isolation, or implement general-purpose sandbox hardening. Do not add capability negotiation, CNI health certification, a per-node probe service, or a full worker execution-recovery protocol.
## Validation criteria
These are required implementation checks, not results claimed by this spec-only PR. New test names below are proposed acceptance-test names.
### Worker tests
- `TestNetworkPolicyAssignmentJSON`: nil/null/absent versus `{}`/empty/null rules; ALLOW wire string; IPv4/IPv6/hostname fields; description preservation. Covers product behaviors 1–2 and 7.
- `TestNetworkPolicyConfigAndBackendAdmission`: disabled default, enabled/config validation, non-Kubernetes backends, and the local opt-in check. Assert no new handshake/header or server worker-version filter is introduced. Covers 3, 6–7, 9.
- `TestNetworkPolicyPreclaimRejection`: unsupported/invalid policy sends execution-scoped coded rejection, makes no backend/resource call, and does not consume one-shot acceptance or leak a concurrency slot. Covers 6–7.
- `TestKubernetesNetworkPolicyProviderSelection`: present Cilium, definitely absent API, discovery denied/partial/timeout, and Cilium permission/create failure. Only definite API absence uses native. Covers 4 and 6.
- `TestNativeNetworkPolicyTranslation`: multiple A/AAAA results, canonical exact CIDRs, duplicate hosts, DNS TCP/UDP peers, lookup failure/no answers, and no refresh. Covers 1–2, 4–5.
- `TestCiliumNetworkPolicyTranslation`: exact names, literal CIDRs, restricted DNS query rules, no ingress changes, no broad entity grants, and no unintended port restrictions on host grants. Empty policy must deny all without DNS. Covers 1–2, 4–5.
- `TestKubernetesRestrictedLaunchOrdering`: suspended Job, unique reserved labels, policy-create failure, admission mutation, ambiguous API responses, UID conflicts, resume fencing, deadline, and abort/resume race. Assert no task Pod is launched on failure. Covers 6 and 8–9.
- `TestKubernetesNetworkPolicyCleanup`: success, failure, `NoCleanup`, lingering regular/native sidecars, Pending/Unknown Pods, background cancellation, watch loss, worker shutdown/restart, TTL-deleted Jobs, name reuse, concurrent sweepers, and forbidden deletion. Assert policy deletion never precedes the stop predicate. Covers 10–12.
- Run `go test ./...`, `go vet ./...`, `go build ./...`, and the repository CI's formatting/lint checks.
### Server tests
- Extend `selfhosted/websocket_test.go` and `worker_test.go` for exact requirements passthrough, nil/empty preservation, and unchanged connection selection/unrestricted dispatch.
- Test coded preclaim rejection fencing by execution and selected connection. A late message cannot reject a successor. Capacity and legacy uncoded rejections still requeue.
- Add dispatcher and Temporal offer tests that assert `FAILED` and non-retryability after an upgraded worker's coded policy rejection. Disconnected/capacity cases retain existing behavior.
- Test `network_policy` postclaim failure as a non-retryable user failure. Verify it does not inherit the generic constructor's retry flag.
- Run `go test ./logic/ai/ambient_agents/workers/selfhosted`, `go test ./logic/ai/ambient_agents/networking`, and `go test ./logic/ai/ambient_agents` in warp-server.
### Helm and live Kubernetes tests
- Run `helm lint ./charts/oz-agent-worker --set worker.workerId=ci-worker --set image.tag=ci`.
- Run `helm template oz-agent-worker ./charts/oz-agent-worker --namespace agents --set worker.workerId=ci-worker --set image.tag=ci` for support disabled, support enabled with DNS settings, and externally managed RBAC. Assert disabled support adds no policy RBAC and enabled support grants no cluster-scoped permissions.
- Add an integration-tagged `TestKubernetesNetworkPolicyWithCNI` using the existing `OZ_K8S_TEST_KUBECONFIG` disposable-cluster convention. Run `go test -tags=integration ./internal/worker -run 'Test(KubernetesNetworkPolicyWithCNI|JobExpirationWithController)' -count=1`.
- Run it separately against an enforcing Cilium cluster and an enforcing native-policy cluster without the Cilium API. Record cluster/CNI versions and provider selection. Fake Kubernetes clients cannot prove enforcement.
- Use controlled external allowed/forbidden endpoints with distinct IPv4 addresses and, on dual-stack clusters, distinct IPv6 addresses. Use controlled DNS with multiple records. Assert allowed reachability and blocked direct-IP/hostname destinations from setup and task containers.
- Verify the empty resolved policy blocks DNS and external egress. Verify a nil policy does not acquire restricted labels or a per-task policy.
- Remove policy-create permission and assert terminal failure before task code runs. Restore it and confirm a new execution succeeds. Verify a present Cilium API with denied Cilium rights does not use native fallback.
- Exercise cancellation with a slow sidecar shutdown, `--no-cleanup`, worker termination before Job completion, and replacement-worker reconciliation. Assert the policy persists while any container runs, then disappears after terminal execution while retained Job/Pod objects remain.
- The production opt-in is an operator attestation; these tests do not claim that an API-only check can detect a non-enforcing production CNI. No UI/computer-use verification is required.
- Verify the rollout instructions against a mixed-version fixture: an old JSON decoder ignores the new field. Record this as the known compatibility limit, not a passing security scenario. Require evidence that all target replicas use the upgraded image before enabling policy-bearing dispatch.
## References
- [Kubernetes NetworkPolicy semantics, asynchronous handling, and host-network limitations](https://kubernetes.io/docs/concepts/services-networking/network-policies/).
- [Cilium DNS-based egress policies](https://docs.cilium.io/en/stable/security/dns/).
- [Job terminal conditions and Kubernetes version differences](https://kubernetes.io/docs/concepts/workloads/controllers/job/#terminal-job-conditions).
- [Native sidecar lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/).
