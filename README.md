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
    default_image: "my-registry.io/dev-image:latest"
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

- `default_image` sets the Docker image for task Jobs when no Warp environment is configured on the run; this lets you skip creating a Warp environment entirely if all your tasks use the same base image (precedence: Warp environment image > `default_image` > `ubuntu:22.04`)
- `namespace` selects the namespace inside the chosen cluster; it does not choose the cluster itself, and defaults to `default` when omitted
- `unschedulable_timeout` controls how long a Pod may remain unschedulable before the task is failed early; it defaults to `30s`, and `0s` disables that fail-fast behavior
- `image_pull_policy` defaults to `IfNotPresent`
- `sidecar_image` overrides the warp-agent sidecar image reference sent by the server (e.g. `docker.io/warpdotdev/warp-agent:latest`); set this when cluster nodes cannot pull directly from Docker Hub and must use an internal registry mirror or pull-through cache instead. This only affects the warp-agent sidecar (mounted at `/agent`), not any additional sidecars. When using this override, you are responsible for keeping your mirror in sync with `docker.io/warpdotdev/warp-agent` — the server normally sends the correct version-matched image per task, so a stale mirror may cause version incompatibility
- by default, the Kubernetes backend materializes sidecars with root init containers into `emptyDir` volumes, matching the existing behavior
- set `use_image_volumes: true` to opt into native image volumes for sidecars; in that mode, sidecar mounts are read-only and Kubernetes/runtime support for the built-in `ImageVolume` Pod volume source is required
- Kubernetes `1.35+` is the recommended and tested target for `use_image_volumes: true`; Kubernetes `1.33`-`1.34` may work if `ImageVolume` is enabled and the container runtime supports image volumes
- the worker runs a short-lived startup preflight Job for the configured sidecar-loading mode and waits for either preflight success or an early controller, mount, or admission failure, so incompatible cluster/runtime policy failures surface before the worker starts accepting tasks
- `preflight_image` defaults to `busybox:1.36`; set it if your cluster only allows pulling startup-preflight images from an internal or allowlisted registry
- `pod_template` accepts standard Kubernetes PodSpec YAML and is the declarative way to configure task pod scheduling, service accounts, image pull secrets, resources, and environment
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

The worker Deployment's `ServiceAccount` is separate from the task Job `serviceAccountName` you may set inside `backend.kubernetes.pod_template` / `kubernetesBackend.podTemplate`. The worker `Deployment` defaults to non-root. By default, task Jobs still materialize sidecars with root init containers; set `kubernetesBackend.useImageVolumes=true` to opt into native image volumes instead. Kubernetes `1.35+` is the recommended and tested target for that opt-in path, while Kubernetes `1.33`-`1.34` may work if `ImageVolume` is enabled and the container runtime supports image volumes. If your cluster restricts image sources for admission or policy reasons, set `kubernetesBackend.preflightImage` in the chart to an allowlisted image for the startup preflight Job, and configure task `imagePullSecrets` inside `podTemplate` when needed.

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

When configuring the Kubernetes backend via YAML or Helm, declarative task-container env belongs in `backend.kubernetes.pod_template` / `kubernetesBackend.podTemplate` rather than a separate top-level Kubernetes env list. The `-e` / `--env` flags remain available as backend-agnostic runtime overrides.

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

## Monitoring

The worker can export metrics over OpenTelemetry. Exporter selection is
driven by the standard
[OpenTelemetry environment variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/),
implemented via
[`go.opentelemetry.io/contrib/exporters/autoexport`](https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/exporters/autoexport).
When `OTEL_METRICS_EXPORTER` is unset (or set to `none`), the worker emits no
metrics, matching the pre-existing behavior.

### Quick start with Prometheus

```bash
export OTEL_METRICS_EXPORTER=prometheus
export OTEL_EXPORTER_PROMETHEUS_HOST=0.0.0.0
export OTEL_EXPORTER_PROMETHEUS_PORT=9464
oz-agent-worker --api-key "$WARP_API_KEY" --worker-id "my-worker"

# In another shell:
curl -s localhost:9464/metrics | grep oz_worker_
```

### Quick start with OTLP

```bash
export OTEL_METRICS_EXPORTER=otlp
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability.svc:4318
oz-agent-worker --api-key "$WARP_API_KEY" --worker-id "my-worker"
```

### Helm

```bash
helm install oz-agent-worker ./charts/oz-agent-worker \
  --namespace agents --create-namespace \
  --set worker.workerId=my-worker \
  --set image.tag=v1.2.3 \
  --set metrics.enabled=true
```

With `metrics.enabled=true` and the default `metrics.exporter=prometheus`, the
chart adds:

- a `containerPort: metrics` (default 9464) on the worker Deployment
- the `OTEL_METRICS_EXPORTER`, `OTEL_EXPORTER_PROMETHEUS_HOST`, and
  `OTEL_EXPORTER_PROMETHEUS_PORT` environment variables
- a namespace-scoped `Service` named `<release>-oz-agent-worker-metrics` with
  `prometheus.io/scrape` annotations
- optionally a `PodMonitor` (`metrics.podMonitor.create=true`) for clusters
  using the Prometheus Operator

For OTLP push instead, set `metrics.exporter=otlp` and forward the relevant
endpoint variables via `metrics.extraEnv`:

```yaml
metrics:
  enabled: true
  exporter: otlp
  extraEnv:
    - name: OTEL_EXPORTER_OTLP_ENDPOINT
      value: http://otel-collector.observability.svc:4318
```

### Metric catalog

All metrics carry the resource attributes `service.name=oz-agent-worker`,
`service.version`, `worker.id`, and `worker.backend`, so each worker process
shows up as a distinct series.

- `oz_worker_connected` (gauge): `1` while the worker has an active WebSocket
  connection to warp-server, `0` otherwise. Aggregate to count connected
  workers: `sum(oz_worker_connected)`.
- `oz_worker_tasks_active` (gauge / UpDownCounter): tasks currently executing
  on this worker. To count workers running ≥1 task:
  `count(oz_worker_tasks_active > 0)`.
- `oz_worker_tasks_max_concurrent` (gauge): configured concurrency limit
  (`0` means unlimited).
- `oz_worker_tasks_claimed_total` (counter): total tasks accepted by the
  worker since process start.
- `oz_worker_tasks_rejected_total{reason}` (counter): tasks the worker
  declined, e.g. `reason="at_capacity"`.
- `oz_worker_tasks_completed_total{result}` (counter): completed tasks
  labeled `result="succeeded"` or `result="failed"`. Success rate over 5m:
  `sum(rate(oz_worker_tasks_completed_total{result="succeeded"}[5m])) /
   sum(rate(oz_worker_tasks_completed_total[5m]))`.
- `oz_worker_task_duration_seconds{result}` (histogram): wall-clock task
  duration on the worker. p95: `histogram_quantile(0.95,
   sum by (le) (rate(oz_worker_task_duration_seconds_bucket[5m])))`.
- `oz_worker_websocket_reconnects_total{reason}` (counter): reconnect
  attempts; spikes indicate flapping workers.
- `oz_worker_info{version,backend,worker_id}` (gauge, value `1`): build and
  runtime metadata, useful for joining other series by labels.

### Sample dashboards / alerts

Direct mappings for the questions enterprise operators most commonly ask:

- **Workers available:** `sum(oz_worker_connected)`
- **Workers active (running ≥1 task):**
  `count(oz_worker_tasks_active > 0)`
- **Saturation:**
  `sum(oz_worker_tasks_active) /
   sum(oz_worker_tasks_max_concurrent > 0)`
- **Failure rate:**
  `sum(rate(oz_worker_tasks_completed_total{result="failed"}[5m]))`
- **Reconnect storms:**
  `sum(rate(oz_worker_websocket_reconnects_total[5m])) > 0.1`

## License

Copyright © 2026 Warp
