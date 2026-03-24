# oz-agent-worker

Self-hosted worker for Oz cloud agents.

📖 **[Documentation](https://docs.warp.dev/agent-platform/cloud-agents/self-hosting)**

## Overview

`oz-agent-worker` is a daemon that connects to Oz via WebSocket to receive and execute cloud agent tasks on self-hosted infrastructure.

## Requirements

- Service account API key with team scope
- Network egress to warp-server
- One supported execution backend:
  - Docker daemon access for the Docker backend
  - Local `oz` CLI access plus a writable workspace root for the Direct backend
  - Kubernetes API access plus cluster credentials for the Kubernetes backend

## Usage

### Docker (Recommended)

The worker needs access to the Docker daemon to spawn task containers. Mount the host's Docker socket into the container:

```bash
docker run -v /var/run/docker.sock:/var/run/docker.sock \
  -e WARP_API_KEY="wk-abc123" \
  warpdotdev/oz-agent-worker --worker-id "my-worker"
```

> **Note:** Mounting the Docker socket gives the container access to the host's Docker daemon. This is required for the worker to create and manage task containers.

### Direct

The direct backend executes tasks directly on the host instead of inside Docker or Kubernetes. It requires the `oz` CLI to be available on `PATH` (or configured explicitly with `backend.direct.oz_path`) and stores per-task workspaces under `backend.direct.workspace_root` (default: `/var/lib/oz/workspaces`).

Example config:

```yaml
worker_id: "my-worker"
backend:
  direct:
    workspace_root: "/var/lib/oz/workspaces"
    oz_path: "/usr/local/bin/oz"
```

### Kubernetes

The Kubernetes backend creates one Job per task. Cluster selection is controlled by the Kubernetes client config:

- `backend.kubernetes.kubeconfig` points to an explicit kubeconfig file
- if `kubeconfig` is omitted, the worker uses in-cluster config when running inside Kubernetes
- otherwise it falls back to the default kubeconfig loading rules and uses the current context

Example config:

```yaml
worker_id: "my-worker"
backend:
  kubernetes:
    kubeconfig: "/path/to/kubeconfig"
    namespace: "agents"
    unschedulable_timeout: "2m"
    pod_template:
      nodeSelector:
        kubernetes.io/os: linux
      containers:
        - name: task
          resources:
            requests:
              cpu: "2"
              memory: 4Gi
```

Notes:

- `namespace` selects the namespace inside the chosen cluster; it does not choose the cluster itself, and defaults to `default` when omitted
- `unschedulable_timeout` controls how long a Pod may remain unschedulable before the task is failed early; it defaults to `30s`, and `0s` disables that fail-fast behavior
- `image_pull_policy` defaults to `IfNotPresent`
- the Kubernetes backend requires creating Pods with a root init container to materialize sidecars into `emptyDir` volumes
- the worker runs a short-lived startup preflight Job and waits for either preflight Pod creation or an early controller failure, so incompatible Pod Security or admission policy failures surface before the worker starts accepting tasks
- `preflight_image` defaults to `busybox:1.36`; set it if your cluster only allows pulling startup-preflight images from an internal or allowlisted registry
- `pod_template` accepts standard Kubernetes PodSpec YAML and is the recommended way to configure task pod scheduling, resources, and environment; when set, it replaces the legacy scheduling/resource fields
- when using `pod_template`, define a container named `task` if you want to customize the main task container directly; otherwise the worker appends its own `task` container to the PodSpec
- use `valueFrom.secretKeyRef` inside `pod_template` to inject Kubernetes Secret values into task container environment variables:

```yaml
pod_template:
  containers:
    - name: task
      env:
        - name: MY_SECRET
          valueFrom:
            secretKeyRef:
              name: my-k8s-secret
              key: secret-key
```

- legacy fields (`node_selector`, `tolerations`, `resources`, `service_account`, `image_pull_secret`, `termination_grace_period_seconds`) still work but cannot be combined with `pod_template`; when migrating, use the equivalent standard PodSpec fields inside `pod_template`

### Helm Chart

This repo includes a namespace-scoped Helm chart at `charts/oz-agent-worker`.

The chart deploys:

- a long-lived `Deployment` for `oz-agent-worker`
- a namespaced `ServiceAccount`
- a namespaced `Role` / `RoleBinding`
- a `ConfigMap` containing the worker config
- an optional `Secret` for `WARP_API_KEY` (or a reference to an existing `Secret`)

At runtime, the deployed worker connects outbound to Warp and creates one Kubernetes `Job` per task. The built-in Kubernetes Job controller then manages the task Pod lifecycle.

Recommended install flow:

```bash
kubectl create secret generic oz-agent-worker \
  --from-literal=WARP_API_KEY="wk-abc123" \
  --namespace agents

helm install oz-agent-worker ./charts/oz-agent-worker \
  --namespace agents \
  --create-namespace \
  --set worker.workerId=my-worker \
  --set image.tag=v1.2.3
```

The chart assumes the worker runs inside the target cluster and uses in-cluster Kubernetes auth by default. It does not create CRDs or cluster-scoped RBAC. Set `image.tag` explicitly for each install so the worker image is pinned instead of defaulting to `latest`.

The chart always deploys a single replica for a given `worker.workerId`. If you want multiple workers, deploy multiple releases with distinct worker IDs rather than scaling one release horizontally.

The chart defaults the long-lived worker `Deployment` to a non-root security context and conservative starting resource requests of `100m` CPU and `128Mi` memory. Tune `worker.resources` for your workload and cluster policy.

The Deployment includes a default `exec` liveness probe that checks the worker process is still running (`kill -0 1`). If the worker becomes unresponsive, Kubernetes will restart the pod after three consecutive failures. Override `worker.livenessProbe` in your values to use a custom probe (e.g. `httpGet` if you add a health endpoint), or set it to `null` to disable.

Recommended namespace-scoped permissions for the worker are:

- create, get, list, watch, delete `jobs`
- get, list, watch `pods`
- get `pods/log`
- list `events`

The worker Deployment's `ServiceAccount` is separate from the optional `backend.kubernetes.service_account` used by task Jobs and the startup preflight Job. The worker `Deployment` defaults to non-root, but the task namespace must still allow creating Jobs with a root init container, since sidecar materialization currently depends on that pattern. If your cluster restricts image sources for admission or policy reasons, set `kubernetesBackend.preflightImage` in the chart to an allowlisted image for the startup preflight Job.

### Go Install

```bash
go install github.com/warpdotdev/oz-agent-worker@latest
oz-agent-worker --api-key "wk-abc123" --worker-id "my-worker"
```

### Build from Source

```bash
git clone https://github.com/warpdotdev/oz-agent-worker.git
cd oz-agent-worker
go build -o oz-agent-worker
./oz-agent-worker --api-key "wk-abc123" --worker-id "my-worker"
```

## Environment Variables for Task Containers

Use `-e` / `--env` to pass environment variables into task containers:

```bash
# Explicit key=value
oz-agent-worker --api-key "wk-abc123" --worker-id "my-worker" -e MY_SECRET=hunter2

# Pass through from host environment
export MY_SECRET=hunter2
oz-agent-worker --api-key "wk-abc123" --worker-id "my-worker" -e MY_SECRET

# Multiple variables
oz-agent-worker --api-key "wk-abc123" --worker-id "my-worker" -e FOO=bar -e BAZ=qux
```

When using Docker to run the worker, note that `-e` flags for the worker itself (task containers) are passed as arguments, while `-e` flags for the worker container use Docker's syntax:

```bash
docker run -v /var/run/docker.sock:/var/run/docker.sock \
  -e WARP_API_KEY="wk-abc123" \
  warpdotdev/oz-agent-worker --worker-id "my-worker" -e MY_SECRET=hunter2
```

## Docker Connectivity

The worker automatically discovers the Docker daemon using standard Docker client mechanisms, in this order:

1. **`DOCKER_HOST`** environment variable (e.g., `unix:///var/run/docker.sock`, `tcp://localhost:2375`)
2. **Default socket location** (`/var/run/docker.sock` on Linux, `~/.docker/run/docker.sock` for rootless)
3. **Docker context** via `DOCKER_CONTEXT` environment variable
4. **Config file** (`~/.docker/config.json`) for context settings

Additional supported environment variables:
- `DOCKER_API_VERSION` - Specify Docker API version
- `DOCKER_CERT_PATH` - Path to TLS certificates
- `DOCKER_TLS_VERIFY` - Enable TLS verification

### Example: Remote Docker Daemon

```bash
export DOCKER_HOST="tcp://remote-host:2376"
export DOCKER_TLS_VERIFY=1
export DOCKER_CERT_PATH="/path/to/certs"
oz-agent-worker --api-key "wk-abc123" --worker-id "my-worker"
```

## License

Copyright © 2026 Warp
