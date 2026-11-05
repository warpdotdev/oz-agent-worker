#!/usr/bin/env bash
# Check whether a Kubernetes cluster can run oz-agent-worker with
# backend.kubernetes.use_image_volumes: true (native ImageVolume sidecar mounts
# instead of copy-sidecar init containers).
#
# Read-only by default: it only runs kubectl version / get nodes / get --raw
# /metrics. With --run-preflight it additionally creates one short-lived Job in
# the target namespace, equivalent to the worker's own startup preflight, and
# deletes it afterwards.
#
# Usage: script/k8s-image-volume-check.sh [--namespace NS] [--run-preflight]
#          [--preflight-image IMAGE] [--node-selector KEY=VALUE]... [--timeout SECONDS]
#          [--context CONTEXT]
#
# Exit codes: 0 ready, 1 at least one hard failure, 2 usage or dependency error.

set -euo pipefail

namespace=""
context=""
run_preflight=false
preflight_image="busybox:1.36"
timeout_seconds=120
node_selectors=()

usage() {
  local line n=0
  while IFS= read -r line && [ $((n += 1)) -le 15 ]; do
    [ "$n" -ge 2 ] || continue
    line="${line#\# }"
    printf '%s\n' "${line#\#}"
  done <"$0"
}

need_value() {
  if [ -z "${2:-}" ]; then
    echo "$1 requires a value" >&2
    exit 2
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace|-n) need_value "$1" "${2:-}"; namespace="$2"; shift 2 ;;
    --context) need_value "$1" "${2:-}"; context="$2"; shift 2 ;;
    --run-preflight) run_preflight=true; shift ;;
    --preflight-image) need_value "$1" "${2:-}"; preflight_image="$2"; shift 2 ;;
    --node-selector) need_value "$1" "${2:-}"; node_selectors+=("$2"); shift 2 ;;
    --timeout) need_value "$1" "${2:-}"; timeout_seconds="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

for dependency in kubectl jq; do
  if ! command -v "$dependency" >/dev/null 2>&1; then
    echo "missing dependency: $dependency" >&2
    exit 2
  fi
done

kubectl_args=()
if [ -n "$context" ]; then
  kubectl_args+=(--context "$context")
fi
k() { kubectl "${kubectl_args[@]+"${kubectl_args[@]}"}" "$@"; }

failures=0
warnings=0
pass() { printf '[PASS] %s\n' "$*"; }
warn() { printf '[WARN] %s\n' "$*"; warnings=$((warnings + 1)); }
fail() { printf '[FAIL] %s\n' "$*"; failures=$((failures + 1)); }
info() { printf '[INFO] %s\n' "$*"; }
section() { printf '\n== %s ==\n' "$*"; }

# version_ge MAJOR MINOR WANT_MAJOR WANT_MINOR
version_ge() {
  [ "$1" -gt "$3" ] || { [ "$1" -eq "$3" ] && [ "$2" -ge "$4" ]; }
}

# parse_major_minor "v1.33.4-eks-abc" -> "1 33"; echoes nothing when unparseable.
parse_major_minor() {
  local version="${1#v}"
  if [[ "$version" =~ ^([0-9]+)\.([0-9]+) ]]; then
    echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]}"
  fi
}

# classify_kubernetes MAJOR MINOR -> ready | gate | unsupported
classify_kubernetes() {
  if version_ge "$1" "$2" 1 35; then
    echo ready
  elif version_ge "$1" "$2" 1 33; then
    echo gate
  else
    echo unsupported
  fi
}

section "Kubernetes API server"
if ! version_json="$(k version -o json 2>/dev/null)"; then
  echo "could not reach the Kubernetes API server with kubectl${context:+ (context $context)}" >&2
  exit 2
fi
server_git_version="$(jq -r '.serverVersion.gitVersion // empty' <<<"$version_json")"
# Managed clusters report minors like "33+"; keep the digits only.
server_major="$(jq -r '.serverVersion.major // empty' <<<"$version_json")"
server_minor="$(jq -r '.serverVersion.minor // empty' <<<"$version_json")"
server_major="${server_major//[!0-9]/}"
server_minor="${server_minor//[!0-9]/}"
if [ -z "$server_major" ] || [ -z "$server_minor" ]; then
  read -r server_major server_minor <<<"$(parse_major_minor "$server_git_version")" || true
fi
if [ -z "${server_major:-}" ] || [ -z "${server_minor:-}" ]; then
  fail "could not parse the API server version from: $server_git_version"
  server_class=unsupported
else
  server_class="$(classify_kubernetes "$server_major" "$server_minor")"
  case "$server_class" in
    ready) pass "API server $server_git_version: Kubernetes >= 1.35, ImageVolume is on by default" ;;
    gate) warn "API server $server_git_version: Kubernetes 1.33-1.34 needs --feature-gates=ImageVolume=true on kube-apiserver AND every kubelet" ;;
    unsupported) fail "API server $server_git_version: Kubernetes < 1.33 is not supported for image volumes; keep use_image_volumes: false" ;;
  esac
fi

# The API server exposes kubernetes_feature_enabled{name="ImageVolume"} on
# /metrics; reading it needs GET on the /metrics non-resource URL.
gate_state="unknown"
gate_metric_re='^kubernetes_feature_enabled\{name="ImageVolume"[^}]*\}[[:space:]]+([0-9]+)'
if metrics="$(k get --raw /metrics 2>/dev/null)"; then
  while IFS= read -r line; do
    if [[ "$line" =~ $gate_metric_re ]]; then
      if [ "${BASH_REMATCH[1]}" = "1" ]; then gate_state="enabled"; else gate_state="disabled"; fi
    fi
  done <<<"$metrics"
fi
case "$gate_state" in
  enabled) pass "ImageVolume feature gate is enabled on the API server" ;;
  disabled) fail "ImageVolume feature gate is disabled on the API server; pods with image volumes will be rejected or have the volume dropped" ;;
  unknown) info "could not read the ImageVolume gate from the API server /metrics endpoint (needs RBAC for GET /metrics); verify with your cluster administrator" ;;
esac

section "Nodes (kubelet and container runtime)"
if ! nodes_json="$(k get nodes -o json 2>/dev/null)"; then
  fail "could not list nodes (needs RBAC for get/list nodes); runtime support cannot be verified from here"
  nodes_json='{"items":[]}'
fi
node_count="$(jq -r '.items | length' <<<"$nodes_json")"
if [ "$node_count" = "0" ]; then
  warn "no nodes visible; per-node kubelet and runtime checks were skipped"
fi
printf '%-45s %-14s %-28s %s\n' NODE KUBELET RUNTIME RESULT
while IFS=$'\t' read -r node kubelet runtime; do
  [ -n "$node" ] || continue
  result=""
  kubelet_major="" kubelet_minor="" runtime_major="" runtime_minor=""
  read -r kubelet_major kubelet_minor <<<"$(parse_major_minor "$kubelet")" || true
  if [ -z "${kubelet_major:-}" ]; then
    result="FAIL kubelet version unparseable"
  else
    case "$(classify_kubernetes "$kubelet_major" "$kubelet_minor")" in
      ready) ;;
      gate) result="WARN kubelet 1.33-1.34 needs ImageVolume gate;" ;;
      unsupported) result="FAIL kubelet < 1.33;" ;;
    esac
  fi
  runtime_name="${runtime%%://*}"
  runtime_version="${runtime#*://}"
  read -r runtime_major runtime_minor <<<"$(parse_major_minor "$runtime_version")" || true
  case "$runtime_name" in
    containerd)
      if [ -n "${runtime_major:-}" ] && version_ge "$runtime_major" "$runtime_minor" 2 1; then
        result="${result:+$result }PASS containerd >= 2.1"
      else
        result="${result:+$result }FAIL containerd < 2.1 (need >= 2.1)"
      fi
      ;;
    cri-o)
      if [ -n "${runtime_major:-}" ] && version_ge "$runtime_major" "$runtime_minor" 1 31; then
        result="${result:+$result }PASS CRI-O >= 1.31"
      else
        result="${result:+$result }FAIL CRI-O < 1.31 (need >= 1.31)"
      fi
      ;;
    *)
      result="${result:+$result }FAIL runtime '$runtime_name' has no image-volume support (need containerd >= 2.1 or CRI-O >= 1.31)"
      ;;
  esac
  printf '%-45s %-14s %-28s %s\n' "$node" "$kubelet" "$runtime" "$result"
  case "$result" in
    *FAIL*) failures=$((failures + 1)) ;;
    *WARN*) warnings=$((warnings + 1)) ;;
  esac
done < <(jq -r '.items[] | [.metadata.name, .status.nodeInfo.kubeletVersion, .status.nodeInfo.containerRuntimeVersion] | @tsv' <<<"$nodes_json")
if [ "$node_count" != "0" ]; then
  info "only the nodes that run task Pods must pass; if tasks are pinned with pod_template nodeSelector/tolerations, judge that pool and pass the same selector to --run-preflight via --node-selector"
fi

section "Admission and policy"
info "image volumes are a distinct volume type ('image'). If Pod Security Admission 'restricted', Kyverno, Gatekeeper/OPA, or a mutating webhook limits volumes[*] types in the worker namespace, allow 'image' there (PSA restricted allows it from Kubernetes 1.33)."
info "image volumes pull with the same credentials as container images: node credentials, the task service account's imagePullSecrets, and pod_template imagePullSecrets."
info "the worker's own startup preflight creates a Job with an image volume and refuses to start if it is rejected or fails to mount, so a misconfiguration is caught before any task runs."

if [ "$run_preflight" = true ]; then
  section "Preflight Job (creates and deletes one Job)"
  if [ -z "$namespace" ]; then
    namespace="$(k config view --minify -o jsonpath='{..namespace}' 2>/dev/null || true)"
    namespace="${namespace:-default}"
  fi
  job_name="oz-image-volume-check-$(date +%s)"
  selector_json="{}"
  for selector in "${node_selectors[@]+"${node_selectors[@]}"}"; do
    key="${selector%%=*}"
    value="${selector#*=}"
    selector_json="$(jq -c --arg k "$key" --arg v "$value" '. + {($k): $v}' <<<"$selector_json")"
  done
  job_manifest="$(jq -n \
    --arg name "$job_name" \
    --arg namespace "$namespace" \
    --arg image "$preflight_image" \
    --argjson nodeSelector "$selector_json" '
    {
      apiVersion: "batch/v1",
      kind: "Job",
      metadata: {name: $name, namespace: $namespace, labels: {"app.kubernetes.io/name": "oz-image-volume-check"}},
      spec: {
        backoffLimit: 0,
        ttlSecondsAfterFinished: 600,
        template: {
          spec: ({
            restartPolicy: "Never",
            volumes: [{name: "preflight-image", image: {reference: $image, pullPolicy: "IfNotPresent"}}],
            containers: [{
              name: "main",
              image: $image,
              imagePullPolicy: "IfNotPresent",
              command: ["/bin/sh", "-c", "test -d /preflight-image"],
              resources: {requests: {cpu: "10m", memory: "16Mi"}, limits: {cpu: "100m", memory: "64Mi"}},
              volumeMounts: [{name: "preflight-image", mountPath: "/preflight-image", readOnly: true}]
            }]
          } + (if ($nodeSelector | length) > 0 then {nodeSelector: $nodeSelector} else {} end))
        }
      }
    }')"
  cleanup_job() {
    k -n "$namespace" delete job "$job_name" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  }
  trap cleanup_job EXIT
  info "creating Job $namespace/$job_name with an image volume of $preflight_image"
  if ! create_output="$(k create -f - <<<"$job_manifest" 2>&1)"; then
    fail "Job creation was rejected: $create_output"
    fail "an admission rejection here is exactly what makes the worker refuse to start with use_image_volumes: true"
  else
    deadline=$((SECONDS + timeout_seconds))
    outcome="timeout"
    while [ "$SECONDS" -lt "$deadline" ]; do
      job_json="$(k -n "$namespace" get job "$job_name" -o json 2>/dev/null || echo '{}')"
      if [ "$(jq -r '[.status.conditions[]? | select(.type=="Complete" and .status=="True")] | length' <<<"$job_json")" != "0" ]; then
        outcome="complete"; break
      fi
      if [ "$(jq -r '[.status.conditions[]? | select(.type=="Failed" and .status=="True")] | length' <<<"$job_json")" != "0" ]; then
        outcome="failed"; break
      fi
      sleep 2
    done
    pods_json="$(k -n "$namespace" get pods -l "job-name=$job_name" -o json 2>/dev/null || echo '{"items":[]}')"
    pod_name="$(jq -r '.items[0].metadata.name // empty' <<<"$pods_json")"
    case "$outcome" in
      complete)
        pass "preflight Job completed: the image volume was mounted and is readable on node $(jq -r '.items[0].spec.nodeName // "unknown"' <<<"$pods_json")"
        ;;
      failed|timeout)
        fail "preflight Job $outcome after ${timeout_seconds}s; pod phase: $(jq -r '.items[0].status.phase // "no pod"' <<<"$pods_json")"
        jq -r '.items[0].status.conditions[]? | "       condition \(.type)=\(.status) \(.reason // "") \(.message // "")"' <<<"$pods_json"
        jq -r '.items[0].status.containerStatuses[]? | .state | to_entries[] | "       container \(.key): \(.value.reason // "") \(.value.message // "")"' <<<"$pods_json"
        if [ -n "$pod_name" ]; then
          k -n "$namespace" get events --field-selector "involvedObject.name=$pod_name" -o json 2>/dev/null \
            | jq -r '.items[]? | "       event \(.reason): \(.message)"' || true
        fi
        k -n "$namespace" get events --field-selector "involvedObject.name=$job_name" -o json 2>/dev/null \
          | jq -r '.items[]? | select(.type=="Warning") | "       job event \(.reason): \(.message)"' || true
        ;;
    esac
  fi
fi

section "How to enable"
cat <<EOF
Worker config (backend.kubernetes):
    use_image_volumes: true
Helm:
    --set kubernetesBackend.useImageVolumes=true
Roll out on one canary worker first (a separate Helm release / worker_id on the
same task node pool), confirm its startup log says
"Kubernetes image-volume mode resolved: forced-on", run a few tasks, then flip
the primary workers. Rollback: set the value to false and restart the worker.
EOF

section "Summary"
if [ "$failures" -gt 0 ]; then
  printf 'NOT READY: %d failing check(s), %d warning(s). Keep use_image_volumes unset or false until they are resolved.\n' "$failures" "$warnings"
  exit 1
fi
if [ "$warnings" -gt 0 ]; then
  printf 'READY WITH CAVEATS: %d warning(s). Address them (typically the ImageVolume feature gate), then run again with --run-preflight.\n' "$warnings"
  exit 0
fi
if [ "$run_preflight" = true ]; then
  printf 'READY: version, runtime, and preflight checks passed. Enable use_image_volumes: true on a canary worker.\n'
else
  printf 'READY: version and runtime checks passed. Run again with --run-preflight -n <worker namespace> to also verify admission and mounting.\n'
fi
