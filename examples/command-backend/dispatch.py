#!/usr/bin/env python3
"""Reference dispatch command for the oz-agent-worker "command" backend.

It reads the task ``DispatchPayload`` (JSON) on stdin, transforms it into a
hypothetical runtime's REST API shape, and POSTs it to ``OZ_DISPATCH_URL``.

Use it as a template for delegating task execution to a self-hosted runtime that
is already configured to run oz agents on demand. The part you customize is
``transform()`` — adapt it to your runtime's request schema. Everything else
(reading stdin, auth, timeouts, exit-code contract) can stay as-is.

Only the Python standard library is used, so there are no dependencies to
install on the worker host.

Required environment:
  OZ_DISPATCH_URL           REST endpoint to POST the transformed body to.

Optional environment:
  OZ_DISPATCH_AUTH_HEADER   Authorization header value (e.g. "Bearer ...").
  OZ_DISPATCH_TIMEOUT_SECS  Request timeout in seconds (default 30).

Also provided by the worker (no need to set these yourself):
  OZ_TASK_ID, OZ_EXECUTION_ID, OZ_WORKER_BACKEND, OZ_SERVER_ROOT_URL, OZ_DOCKER_IMAGE

Exit semantics (the contract the command backend relies on):
  exit 0    => task accepted for dispatch; the remote runtime now owns it and
               the agent reports terminal state to Warp itself.
  exit != 0 => dispatch failed; the worker marks the task failed.
"""

import json
import os
import sys
import urllib.error
import urllib.request


def transform(payload):
    """Map the worker's DispatchPayload onto our runtime's API shape.

    This is an example of an arbitrary, not-hard transformation: fields are
    renamed and nested under a ``run`` object, sidecar mounts are restructured
    (``mount_path`` -> ``path``), and a few values are lifted into a metadata
    block. Replace the body of this function to match your own API.
    """
    task = payload.get("task") or {}
    definition = task.get("task_definition") or {}

    return {
        "run": {
            "task_id": payload["task_id"],
            "execution_id": payload.get("execution_id", ""),
            "image": payload.get("docker_image", ""),
            # base_args is the `oz agent run ...` argv the runtime should exec.
            "command": payload.get("base_args", []),
            "env": payload.get("env", {}),
            "mounts": [
                {
                    "image": sidecar.get("image", ""),
                    "path": sidecar.get("mount_path", ""),
                    "read_write": sidecar.get("read_write", False),
                }
                for sidecar in (payload.get("sidecars") or [])
            ],
            "callback_url": payload.get("server_root_url", ""),
            "metadata": {
                "worker_id": payload.get("worker_id", ""),
                "payload_version": payload.get("version"),
                "title": task.get("title", ""),
                "prompt": definition.get("prompt", ""),
            },
        }
    }


def main():
    url = os.environ.get("OZ_DISPATCH_URL")
    if not url:
        sys.stderr.write("OZ_DISPATCH_URL must be set\n")
        return 2
    timeout = float(os.environ.get("OZ_DISPATCH_TIMEOUT_SECS", "30"))

    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as exc:
        sys.stderr.write(f"invalid dispatch payload on stdin: {exc}\n")
        return 1

    body = json.dumps(transform(payload)).encode("utf-8")

    request = urllib.request.Request(url, data=body, method="POST")
    request.add_header("Content-Type", "application/json")
    request.add_header("X-Oz-Task-Id", os.environ.get("OZ_TASK_ID", ""))
    auth = os.environ.get("OZ_DISPATCH_AUTH_HEADER")
    if auth:
        request.add_header("Authorization", auth)

    try:
        # Any 2xx is success; urllib raises HTTPError for status >= 400.
        with urllib.request.urlopen(request, timeout=timeout) as response:
            response.read()
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        sys.stderr.write(f"dispatch endpoint returned HTTP {exc.code}: {detail}\n")
        return 1
    except urllib.error.URLError as exc:
        sys.stderr.write(f"failed to reach dispatch endpoint: {exc}\n")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
