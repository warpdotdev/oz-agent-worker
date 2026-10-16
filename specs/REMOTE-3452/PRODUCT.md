# Self-hosted Kubernetes network policies
## Summary
Enforce the server-resolved egress policy for each self-hosted Kubernetes execution. Refuse restricted executions when enforcement is unavailable. Preserve the existing behavior for executions without a policy.
- Work item: [REMOTE-3452](https://linear.app/warpdotdev/issue/REMOTE-3452/self-hosted-kubernetes-network-policy-support).
- Status: proposed specification, awaiting Ben Navetta's approval. This PR does not implement the feature.
- The companion [TECH.md](TECH.md) defines the implementation and validation criteria.
## Behavior
1. **Policy presence controls enforcement.** An absent or null resolved policy adds no worker restriction. A present policy with zero rules denies all policy-controlled egress. Do not treat an empty policy as absent. A policy declaration that the server combines with system allowances is not an empty resolved policy.
2. **The server owns policy meaning.** Enforce the resolved ALLOW rules supplied for this execution. Do not reload agent or Factory declarations in the worker. Do not add package repositories, arbitrary internal networks, wildcards, port restrictions, or new system hosts. Leave ingress unchanged. Preserve the server's existing feature gate and nil-policy behavior.
3. **Operators must opt in.** Network-policy support defaults to disabled. An operator may enable it only after the documented cluster prerequisites are met. The option declares that the operator has verified the cluster; it is not an automatic CNI security certification.
4. **Use the available enforcement engine.** Use Cilium's DNS-aware policy when its policy API is available. Otherwise, use native Kubernetes NetworkPolicy and resolve each allowed DNS name to IP addresses once before launch. Do not refresh that snapshot during the execution. Do not downgrade to native policy after a Cilium permission or application failure.
5. **Document the IP-based boundary.** Both modes grant network destinations, not HTTP Host or TLS identity. A permitted IP can serve more than one DNS name. The native snapshot can become stale during a run. The operator must align worker and task DNS views. Trusted DNS access is a necessary exception when hostname rules exist; it is not a general data-loss-prevention guarantee.
6. **Fail instead of weakening isolation.** Restricted executions fail with an actionable terminal error on unsupported workers, disabled support, invalid policy, DNS-resolution failure, missing permissions, or policy application failure. Docker, direct, and command backends do not support this feature. Never run the task unrestricted as a fallback. Capacity rejection and a disconnected worker retain their existing queue behavior.
7. **Upgrade the pool before enabling restricted assignments.** This change does not add capability negotiation or server-side worker-version filtering. Old workers ignore unknown JSON fields and can therefore ignore a policy. Operators must upgrade every worker in the target pool before the server sends policy-bearing assignments. Do not claim fail-closed compatibility with old workers or mixed old/new pools.
8. **Protect the entire execution.** Isolation applies before task-controlled code starts and covers setup, init containers, regular containers, sidecars, teardown, and debug-idle time. Worker control-plane traffic and node image pulls are outside the task-Pod network boundary. Do not restrict unrelated Pods or other executions.
9. **Trust the operator-managed cluster boundary.** Require an enforcing CNI, an already-enforced restricted-Pod default-deny baseline, trusted DNS, and no other policies that broaden restricted Pods' egress. Task credentials must not permit policy changes, Pod creation, or host escapes. Reject known bypass templates. Do not add per-execution connectivity probes.
10. **Keep isolation during uncertainty.** Worker shutdown, a lost watch, a logical run outcome, a cancellation acknowledgment, and main-container exit do not authorize policy deletion. Preserve isolation while any execution container may still run or its Job may automatically create another Pod.
11. **Separate execution cleanup from object retention.** Remove the policy after the execution is terminal and all its containers have stopped. With `--no-cleanup`, retain Job/Pod objects for inspection but still remove the policy after this stop condition. Deliberately restarting retained work requires a newly protected execution.
12. **Recover policy cleanup after disruption.** A replacement worker reconciles its pool's policies, including terminal executions that completed while the original worker was absent. Retry cleanup on API errors. Retain the policy when termination cannot be verified. Do not use a fixed retention timeout to remove isolation from potentially live work.
## Decisions from the interview
- Ben chose Cilium when available and one-time DNS-to-IP resolution otherwise. He accepted snapshot staleness for the expected run duration.
- Ben required terminal failure when the policy cannot be applied.
- Ben chose operator prerequisites and suggested an explicit “supports network policies” configuration option.
- Ben preferred removing policies after completion under `--no-cleanup`. Completion means terminal execution, not only a finished agent conversation.
- Ben explicitly excluded capability negotiation. A coordinated operator-managed upgrade is required instead.
## Out of scope
- Docker, direct, and custom-command policy enforcement.
- Changes to agent/Factory policy composition, inheritance, `extends`, or the system allowance list.
- Native DNS refresh, strict application-host enforcement, an egress proxy, or new wildcard/CIDR/port policy syntax.
- A full execution reattach/finalization service, CNI installation, or automatic cluster certification.
- Capability negotiation, worker-version dispatch filtering, or an equivalent compatibility handshake.
- Ingress controls, protection from a malicious cluster administrator, and rendered UI changes.
