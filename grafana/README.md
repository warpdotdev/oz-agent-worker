# Oz Agent Worker — Grafana Resources

This directory contains a ready-to-import Grafana dashboard and Prometheus alerting rules for self-hosted Oz worker deployments.

## Files

| File | Purpose |
|------|---------|
| `oz-agent-worker-overview.json` | Importable Grafana dashboard (Grafana 10+) |
| `alerts.yaml` | Prometheus alerting rules (vanilla Prometheus or Prometheus Operator) |

## Prerequisites

Before importing, enable Prometheus metrics on the worker:

```bash
export OTEL_METRICS_EXPORTER=prometheus
export OTEL_EXPORTER_PROMETHEUS_HOST=0.0.0.0
export OTEL_EXPORTER_PROMETHEUS_PORT=9464
oz-agent-worker --api-key "$WARP_API_KEY" --worker-id my-worker
```

Or in the Helm chart:

```bash
helm install oz-agent-worker ./charts/oz-agent-worker \
  --namespace warp-oz \
  --set worker.workerId=my-worker \
  --set image.tag=VERSION \
  --set metrics.enabled=true
```

Verify the endpoint is serving metrics before importing the dashboard:

```bash
curl -s localhost:9464/metrics | grep oz_worker_
```

## Importing the dashboard

### Option 1 — Grafana UI

1. In Grafana, go to **Dashboards** → **Import**.
2. Click **Upload JSON file** and select `oz-agent-worker-overview.json`.
3. When prompted, select your Prometheus datasource.
4. Click **Import**.

### Option 2 — Grafana API

```bash
curl -X POST \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GRAFANA_API_KEY" \
  --data-binary @oz-agent-worker-overview.json \
  http://your-grafana/api/dashboards/import
```

### Option 3 — Direct URL

If the worker binary is available on GitHub, you can import from the raw URL directly in the Grafana UI (**Import** → **Import via grafana.com or URL** → paste the raw GitHub URL):

```
https://raw.githubusercontent.com/warpdotdev/oz-agent-worker/main/grafana/oz-agent-worker-overview.json
```

## Dashboard layout

The dashboard has five sections:

| Row | Panels |
|-----|--------|
| **Worker Health** | Connected workers, active workers, tasks active, fleet saturation |
| **Task Throughput** | Task completion rate by result, 5m success rate, 5m rejection rate |
| **Task Duration** | p50 / p95 / p99 latency |
| **Failure Analysis** | Failure rate by reason, failure rate by phase |
| **Connection Health** | WebSocket reconnect rate, worker fleet info table |

### Instance filter

The **Instance** variable at the top of the dashboard lets you filter to a specific worker pod or scrape target. Set it to **All** (the default) to see fleet-wide aggregations.

## Using the alert rules

### Vanilla Prometheus

1. Copy `alerts.yaml` to your Prometheus rules directory (e.g., `/etc/prometheus/rules/`).
2. Reference it in `prometheus.yml`:

   ```yaml
   rule_files:
     - /etc/prometheus/rules/alerts.yaml
   ```

3. Reload Prometheus: `curl -X POST http://localhost:9090/-/reload`

### Prometheus Operator (Kubernetes)

Wrap the `groups` block in a `PrometheusRule` custom resource:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: oz-agent-worker
  namespace: warp-oz
  labels:
    # Match your Prometheus Operator's ruleSelector labels
    prometheus: kube-prometheus
    role: alert-rules
spec:
  groups:
    # Paste the contents of alerts.yaml `groups:` block here
```

### Tuning thresholds

All alert thresholds are conservative starting points. Tune them to match your fleet size, task volume, and SLOs:

- `OzWorkerFleetSaturated`: raise the 0.9 threshold for fleets that run near capacity by design.
- `OzWorkerHighFailureRate`: lower the 0.1 threshold (10%) to catch smaller failure spikes.
- `OzWorkerReconnectStorm`: adjust the 0.1/s threshold based on your expected reconnect baseline.

## Related documentation

- [Self-hosted worker monitoring](https://docs.warp.dev/platform/self-hosting/monitoring/) — full metrics reference and PromQL examples
- [Self-hosted worker overview](https://docs.warp.dev/platform/self-hosting/) — architecture and deployment patterns
- [Helm chart configuration](https://docs.warp.dev/platform/self-hosting/managed-kubernetes/) — enabling metrics via Helm values
