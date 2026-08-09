# Security and Sandboxing for Autonomous Oz Agents

## Summary

Enterprise customers running self-hosted Oz workers need clarity on the security and
sandboxing guarantees Oz provides out-of-the-box, and reliable mechanisms to restrict
what autonomous agents can reach on their internal infrastructure. Today the Docker and
Kubernetes backends provide process/filesystem isolation but expose no native controls
for network egress or filtering. This spec documents the current isolation boundary,
identifies enterprise gaps, and introduces two practical additions: a configurable Docker
network mode so task containers can be attached to a restricted or isolated network, and
first-class documentation of egress proxy configuration for both backends.

## Background: What Oz Provides Today

### Warp-hosted cloud agents (Namespace.so sandboxes)

Each cloud agent run executes inside an ephemeral Namespace.so microVM. The sandbox
provides strong OS-level isolation: the agent cannot escape the VM boundary, cannot
address other tenant workloads, and cannot reach customer internal infrastructure unless
the customer explicitly exposes an endpoint. This model offers robust out-of-the-box
isolation but provides no mechanism for the agent to reach internal infrastructure at all,
which is why enterprise customers need self-hosted workers.

### Self-hosted workers — Docker backend

- **Process isolation:** Each task runs inside a distinct Docker container (one container
  per task). The container lifecycle is tied to the task; containers are removed after
  completion by default.
- **Filesystem isolation:** The container has its own ephemeral filesystem. Only volumes
  explicitly mounted by the operator are accessible.
- **Resource isolation:** CPU and memory limits are applied when a runner instance shape
  is configured (REMOTE-1936 / merged). Without a shape the container runs unconstrained.
- **Network isolation (current gap):** By default the container is attached to Docker's
  default bridge network. This gives the agent unrestricted outbound connectivity from the
  worker host, including access to internal infrastructure reachable from that host. No
  egress filtering is applied by the worker; no allowlist or denylist of destinations is
  enforced.

### Self-hosted workers — Kubernetes backend

- **Process isolation:** Each task runs as a Kubernetes Job (one pod per task). Pod and
  container boundaries provide OS-level isolation.
- **Filesystem isolation:** Ephemeral `emptyDir` volumes; operator-mounted volumes only.
- **Namespace isolation:** The worker deploys task pods in a configured namespace. The
  operator can separate task pods from other workloads via Kubernetes RBAC and namespace
  boundaries.
- **Network isolation (operator-controlled):** Kubernetes NetworkPolicy resources can be
  applied at the namespace level to restrict what task pods can reach. The worker's
  `pod_template` field already accepts arbitrary PodSpec YAML, so operators can add pod
  labels that a NetworkPolicy selector can match. NetworkPolicy enforcement depends on the
  cluster's CNI plugin (Calico, Cilium, etc.). No NetworkPolicy is created by the worker
  itself; the operator configures this entirely in their cluster.
- **Security contexts:** The Helm chart applies hardened defaults to the long-lived worker
  Deployment pod: `runAsNonRoot`, `seccompProfile: RuntimeDefault`, dropped capabilities.
  Task pods inherit defaults from the cluster unless the operator's `pod_template`
  overrides them.

### Self-hosted workers — Direct backend

- **No container boundary:** Tasks run as a subprocess of the worker process on the host.
  All host network access is available. No sandboxing is provided beyond process
  isolation.
- **Direct backend is not appropriate for restricted-access scenarios** without the
  operator layering their own OS-level controls (firewall rules, seccomp, etc.).

## Problem

An IMC enterprise evaluator asked whether Oz provides out-of-the-box network-level
security controls — network policies, filtering proxies — so autonomous agents cannot
freely reach internal infrastructure. The honest answer today is: we provide strong
process/filesystem isolation but not network-level egress filtering. Enterprise customers
must implement their own controls (Kubernetes NetworkPolicy, egress firewall rules, or a
filtering proxy). They need:

1. **Clarity** on what is and is not provided today, so they can design their controls
   correctly.
2. **A supported, documented egress proxy pattern** so agents can route all outbound
   traffic through a filtering proxy the customer operates.
3. **Docker network isolation** — an explicit hook to attach task containers to a
   restricted Docker network (instead of the default bridge) without requiring the
   operator to patch entrypoints or iptables rules manually.

## Goals

- Document the current isolation boundary clearly in the README and the
  self-hosted-worker customer guidance skill.
- Add a `network_mode` field to the Docker backend configuration so operators can specify
  the Docker network name (or `none`) that task containers join. This is the practical
  hook for connecting containers to a restricted network managed by the operator.
- Document the egress proxy pattern: passing `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`
  to task containers via the existing environment variable injection mechanism.
- Document the Kubernetes NetworkPolicy pattern using the existing `pod_template` hook.

## Non-goals

- Warp does not implement a built-in filtering proxy, firewall ruleset, or content
  inspection layer. These belong to customer infrastructure.
- Warp-hosted sandbox network restrictions are out of scope; customers who need to reach
  internal infra already use self-hosted workers.
- No Oz server changes are required; all changes are in `oz-agent-worker`.
- The Direct backend is out of scope for network isolation; it has no container boundary
  the worker can configure.

## Behavior

### Docker network mode

1. The Docker backend config accepts an optional `network_mode` string. When set, the
   task container's `HostConfig.NetworkMode` is set to that value.
2. Accepted values are any value Docker's `--network` flag accepts: a named network
   (e.g. `restricted-net`, `host`, `none`), or the empty string (default bridge).
3. When `network_mode` is absent or empty, behavior is identical to today (Docker's
   default bridge network).
4. Setting `network_mode: none` disables all networking for the task container. The
   agent runtime can still reach the Warp control plane through the worker's own network
   path; task-internal tool calls that require internet access will fail, which is the
   intended behavior for maximum isolation.
5. Operators who want egress-filtered access (rather than no access) create a Docker
   network with appropriate iptables or firewall rules and set `network_mode` to that
   network's name. The worker does not manage the network itself.

### Egress proxy

6. Task containers respect the standard `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`
   environment variables. Operators inject these via the existing `backend.docker.environment`
   config entries or Kubernetes `pod_template` `env` entries.
7. A filtering proxy the operator operates receives all outbound HTTP/HTTPS traffic from
   the agent and can enforce an allowlist or denylist of destinations.
8. `NO_PROXY` should include the Warp control plane endpoints (`oz.warp.dev`,
   `app.warp.dev`) so the worker's own WebSocket and the agent's task-status calls do
   not route through the proxy.
9. The worker does not validate or enforce proxy settings; responsibility lies with the
   operator. This is documented explicitly so customers do not assume the worker
   intercepts non-proxy-aware traffic.

### Kubernetes NetworkPolicy

10. Task pods are labeled with `oz-task-id` and `oz-worker-id` by the worker. Operators
    can write NetworkPolicy resources that select on these labels to restrict task pod
    egress to a set of allowed destinations.
11. No NetworkPolicy is created by the worker. Operators are responsible for creating and
    maintaining NetworkPolicy resources that match their trust requirements.
12. The `pod_template` mechanism already supports adding arbitrary pod metadata (labels,
    annotations) that NetworkPolicy selectors and admission controllers can consume.
    Operators can also add pod-level security settings (securityContext, seccompProfile,
    capabilities) via `pod_template`.

### Documentation

13. The README security section documents:
    - The isolation model per backend (Docker, Kubernetes, Direct).
    - The `network_mode` option for Docker.
    - The egress proxy pattern.
    - The Kubernetes NetworkPolicy guidance.
    - An explicit statement that the Direct backend provides no network isolation.
14. The self-hosted-worker customer guidance skill's `secrets-and-security.md` reference
    is updated to match.

## Why not build a built-in egress filter?

A built-in filtering layer in the worker (e.g. a sidecar transparent proxy enforced by
iptables) would require elevated capabilities (`NET_ADMIN`), is hard to keep correct
across Linux kernel versions, and duplicates tools that enterprise operators already run
(Zscaler, Squid, Cilium Egress Gateway). The proxy pattern and Kubernetes NetworkPolicy
are lower-risk, more auditable, and compose with existing enterprise controls. We document
how to use them with Oz rather than reimplementing them.
