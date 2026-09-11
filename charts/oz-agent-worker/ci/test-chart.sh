#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
COMMAND_VALUES="$SCRIPT_DIR/command-values.yaml"
OUTPUT_DIR="$(mktemp -d)"
trap 'rm -rf "$OUTPUT_DIR"' EXIT

fail() {
  printf 'helm chart test failed: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local text="$2"
  grep -Fq -- "$text" "$file" ||
    fail "expected $(basename "$file") to contain: $text"
}

assert_matches() {
  local file="$1"
  local pattern="$2"
  grep -Eq -- "$pattern" "$file" ||
    fail "expected $(basename "$file") to match: $pattern"
}

assert_not_matches() {
  local file="$1"
  local pattern="$2"
  if grep -Eq -- "$pattern" "$file"; then
    fail "expected $(basename "$file") not to match: $pattern"
  fi
}

DEFAULT_RENDER="$OUTPUT_DIR/default.yaml"
COMMAND_RENDER="$OUTPUT_DIR/command.yaml"

helm lint "$CHART_DIR" \
  --set worker.workerId=ci-kubernetes-worker \
  --set image.tag=ci
helm template oz-agent-worker "$CHART_DIR" \
  --namespace agents \
  --set worker.workerId=ci-kubernetes-worker \
  --set image.tag=ci >"$DEFAULT_RENDER"

helm lint "$CHART_DIR" --values "$COMMAND_VALUES"
helm template oz-agent-worker "$CHART_DIR" \
  --namespace agents \
  --values "$COMMAND_VALUES" >"$COMMAND_RENDER"

# The default remains Kubernetes-compatible and retains task Job RBAC.
assert_contains "$DEFAULT_RENDER" "- --backend"
assert_contains "$DEFAULT_RENDER" "- kubernetes"
assert_matches "$DEFAULT_RENDER" '^kind: Role$'
assert_matches "$DEFAULT_RENDER" '^kind: RoleBinding$'

# Command mode renders its config, worker args, script mount, and Secret-backed env.
assert_contains "$COMMAND_RENDER" 'dispatch_command: "python3 /opt/oz/dispatch.py"'
assert_contains "$COMMAND_RENDER" 'cancel_command: "python3 /opt/oz/cancel.py"'
assert_contains "$COMMAND_RENDER" 'dispatch_timeout: "45s"'
assert_contains "$COMMAND_RENDER" "- --backend"
assert_contains "$COMMAND_RENDER" "- command"
assert_contains "$COMMAND_RENDER" "mountPath: /opt/oz"
assert_contains "$COMMAND_RENDER" "name: dispatch-scripts"
assert_contains "$COMMAND_RENDER" "name: oz-dispatch"
assert_contains "$COMMAND_RENDER" "name: OZ_DISPATCH_AUTH_HEADER"
assert_contains "$COMMAND_RENDER" "secretKeyRef:"
assert_contains "$COMMAND_RENDER" "name: oz-dispatch-credentials"
assert_contains "$COMMAND_RENDER" "key: OZ_DISPATCH_AUTH_HEADER"
assert_not_matches "$COMMAND_RENDER" '^kind: Role$'
assert_not_matches "$COMMAND_RENDER" '^kind: RoleBinding$'

if helm template oz-agent-worker "$CHART_DIR" \
  --namespace agents \
  --set worker.workerId=ci-worker \
  --set image.tag=ci \
  --set backend.type=invalid >"$OUTPUT_DIR/invalid.yaml" 2>"$OUTPUT_DIR/invalid.err"; then
  fail "invalid backend.type rendered successfully"
fi
assert_contains "$OUTPUT_DIR/invalid.err" 'backend.type must be "kubernetes" or "command"'

if helm template oz-agent-worker "$CHART_DIR" \
  --namespace agents \
  --set worker.workerId=ci-worker \
  --set image.tag=ci \
  --set backend.type=command >"$OUTPUT_DIR/missing-dispatch.yaml" 2>"$OUTPUT_DIR/missing-dispatch.err"; then
  fail "command backend without commandBackend.dispatchCommand rendered successfully"
fi
assert_contains "$OUTPUT_DIR/missing-dispatch.err" "commandBackend.dispatchCommand is required when backend.type=command"

printf 'helm chart tests passed\n'
