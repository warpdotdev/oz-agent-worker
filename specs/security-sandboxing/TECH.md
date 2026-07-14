# TECH: Security and Sandboxing for Autonomous Oz Agents

## Context

See `PRODUCT.md` for behavior. This spec covers the implementation changes needed to
deliver Docker network isolation and the documentation updates.

The only code change is adding a `network_mode` field to the Docker backend config and
threading it into `ContainerCreate`. All other deliverables are documentation.

Relevant code — `oz-agent-worker`:

- [`internal/config/config.go (40-43)`](../internal/config/config.go) — `DockerConfig`:
  `Volumes []string`, `Environment []EnvEntry`. The new `NetworkMode string` field lands
  here.
- [`internal/worker/docker.go (26-30)`](../internal/worker/docker.go) —
  `DockerBackendConfig`: mirrors the config struct as a runtime value; receives
  `NetworkMode string`.
- [`internal/worker/docker.go (139-142)`](../internal/worker/docker.go) —
  `ExecuteTask`: builds `container.HostConfig` with `Binds` and `Resources`. The
  `NetworkMode` field is set here.
- [`main.go`](../main.go) — reads config file and CLI flags; `DockerConfig` is
  converted into `DockerBackendConfig`.

## Proposed changes

### 1. Config struct (`internal/config/config.go`)

Add `NetworkMode` to `DockerConfig`:

```go path=null start=null
// DockerConfig holds Docker-backend-specific configuration.
type DockerConfig struct {
    Volumes     []string   `yaml:"volumes"`
    Environment []EnvEntry `yaml:"environment" validate:"dive"`
    // NetworkMode sets the Docker network the task container joins.
    // Accepts any value Docker's --network flag accepts: a named network
    // (e.g. "restricted-net"), "none" (no networking), "host", or empty
    // string (Docker's default bridge network).
    NetworkMode string `yaml:"network_mode"`
}
```

No validation tag is needed beyond the existing string zero-value semantics: empty string
means "use Docker default", which is the correct backward-compatible behavior.

### 2. Runtime config struct (`internal/worker/docker.go`)

Add `NetworkMode string` to `DockerBackendConfig`:

```go path=null start=null
// DockerBackendConfig holds configuration specific to the Docker backend.
type DockerBackendConfig struct {
    NoCleanup   bool
    Volumes     []string
    Env         map[string]string
    NetworkMode string // Docker network name, "none", "host", or "" (default bridge)
}
```

### 3. Wire from config to runtime (`main.go` or equivalent mapping)

The `DockerConfig` → `DockerBackendConfig` mapping already copies `Volumes` and
`Environment`. Add:

```go path=null start=null
NetworkMode: cfg.Backend.Docker.NetworkMode,
```

### 4. Apply in `ExecuteTask` (`internal/worker/docker.go`)

In the `HostConfig` construction inside `ExecuteTask`, set `NetworkMode` when the config
specifies one:

```go path=null start=null
hostConfig := &container.HostConfig{
    Binds:       binds,
    Resources:   dockerResourcesForShape(params.InstanceShape),
    NetworkMode: container.NetworkMode(b.config.NetworkMode),
}
```

`container.NetworkMode` is a `string` typedef already in
`github.com/moby/moby/api/types/container`. When the string is empty, Docker uses its
default bridge behavior — identical to today. No conditional is needed.

### 5. README: Security section

Add a new "Security and Sandboxing" section to `README.md` between the existing
"Environment Variables for Task Containers" and "Docker Connectivity" sections, covering:

- Isolation model per backend (Docker, Kubernetes, Direct).
- The `backend.docker.network_mode` config option with examples.
- The egress proxy pattern (env vars).
- Kubernetes NetworkPolicy guidance with the existing `pod_template` hook.
- Task pod labels (`oz-task-id`, `oz-worker-id`) available for NetworkPolicy selectors.
- A clear statement that the Direct backend provides no network isolation.

### 6. Skill reference update
(`warp-server/.agents/skills/self-hosted-worker-customer-guidance/references/secrets-and-security.md`)

Extend the "Network model" and "Practical hardening recommendations" sections to cover:

- `network_mode` for Docker.
- Egress proxy injection pattern.
- Kubernetes NetworkPolicy pattern with task pod labels.
- Note that `NO_PROXY` should exclude Warp control plane hostnames.

## Testing and validation

- **Unit: `TestDockerNetworkMode`** (`internal/worker/docker_test.go`) — assert that
  when `DockerBackendConfig.NetworkMode` is `"none"`, the built `HostConfig` carries
  `NetworkMode: "none"`; when empty, `NetworkMode` is `""` (Docker default). Use the
  existing mock Docker client pattern.
- **Config round-trip** (`internal/config/config_test.go`) — parse a YAML snippet with
  `backend.docker.network_mode: restricted-net` and assert the field is preserved; parse
  without the field and assert the zero value.
- **Manual smoke test (Docker backend):** Run a task with `network_mode: none` and
  confirm the agent container cannot reach external addresses (e.g. `curl https://example.com`
  fails); confirm the task still completes its internal work and the worker receives
  task-completed over its own WebSocket.
- Run `go test ./...` in `oz-agent-worker` and `helm template` to confirm no regressions.

## Risks and mitigations

- **`network_mode: host`** gives the task container full host network access, which is
  less restrictive than the default bridge. This is a valid user choice (e.g. for on-prem
  access) but should be documented clearly. *Mitigation:* README example shows `none` and
  a named network; explicitly notes that `host` expands rather than restricts access.
- **Proxy bypass by non-proxy-aware tools.** `HTTP_PROXY` / `HTTPS_PROXY` are honored by
  most Go, Python, and Node.js HTTP clients but not by raw TCP connections or DNS-based
  tools. A filtering proxy catches HTTP/HTTPS but not all protocols. *Mitigation:*
  Document the limitation explicitly so operators layer network-level controls
  (NetworkPolicy, iptables) if full protocol coverage is required.
- **Kubernetes NetworkPolicy is CNI-dependent.** Clusters running the default kubenet or
  flannel CNI may not enforce NetworkPolicy resources. *Mitigation:* README notes that
  NetworkPolicy enforcement requires a CNI plugin that implements it (Calico, Cilium,
  Weave, etc.).
- **`container.NetworkMode` is a string typedef.** An invalid network name is rejected by
  the Docker daemon at container creation time, not at config load time. The task will
  fail at the `ContainerCreate` step with a clear error message. *Mitigation:* document
  that the value is passed to Docker verbatim and any error surfaces as a task failure;
  no worker-side validation is added to preserve flexibility.

## Follow-ups

- Consider exposing `network_mode` as a per-runner override (parallel to how the Docker
  image is runner-scoped) so different runner configurations can apply different network
  policies without restarting the worker.
- Consider a `dns` config option for the Docker backend to allow pointing task containers
  at a split-horizon DNS resolver for internal service discovery.
- Evaluate Cilium Egress Gateway or similar CNI-level egress controls as a documented
  pattern for Kubernetes customers who need IP-level egress filtering without an HTTP
  proxy.
