# Command backend — HTTP REST reference

A templatable reference for the `oz-agent-worker` [`command` backend](../../README.md#command). The worker invokes a dispatch command per task and hands it the task payload as JSON on stdin; this reference **transforms** that payload into a runtime's API shape and forwards it to an HTTP REST endpoint so a self-hosted runtime can launch the agent on demand.

It is written in Python (standard library only — no dependencies to install) so the transformation logic is easy to read and extend.

## Files

- `dispatch.py` — reads the JSON payload on stdin, transforms it (see `transform()`), and `POST`s it to `OZ_DISPATCH_URL`. Exit `0` means dispatched (fire-and-forget); non-zero means the worker fails the task. This is the template for delegating to a remote HTTP runtime.
- `cancel.py` — `POST`s `{task_id, execution_id}` to `OZ_CANCEL_URL` when a dispatched task is cancelled (best-effort).
- `dispatch-oz-local.py` — a **local end-to-end** variant that, instead of forwarding to a remote API, launches the real `oz` agent on the host using the payload's `base_args` (fire-and-forget). Use it to exercise the command backend locally and confirm the worker forwards the right payload. See [Local end-to-end testing](#local-end-to-end-testing).

Requires `python3` on the worker host.

## The transformation

Real runtimes rarely accept the worker's payload verbatim. `dispatch.py` keeps the not-hard transformation in one place — the `transform()` function — that you replace to match your API. The example renames/reshapes the worker payload into a `run` object:

```python
def transform(payload):
    task = payload.get("task") or {}
    definition = task.get("task_definition") or {}
    return {
        "run": {
            "task_id": payload["task_id"],
            "execution_id": payload.get("execution_id", ""),
            "image": payload.get("docker_image", ""),
            "command": payload.get("base_args", []),   # the `oz agent run ...` argv
            "env": payload.get("env", {}),
            "mounts": [                                  # mount_path -> path
                {"image": s.get("image", ""), "path": s.get("mount_path", ""),
                 "read_write": s.get("read_write", False)}
                for s in (payload.get("sidecars") or [])
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
```

## Wiring it into the worker

Point the command backend at the scripts and template the endpoints via `environment` (or host env):

```yaml
worker_id: "my-worker"
backend:
  command:
    dispatch_command: "python3 /opt/oz/dispatch.py"
    cancel_command: "python3 /opt/oz/cancel.py"
    dispatch_timeout: "60s"
    environment:
      - name: OZ_DISPATCH_URL
        value: "https://my-runtime.internal/oz/dispatch"
      - name: OZ_CANCEL_URL
        value: "https://my-runtime.internal/oz/cancel"
      # Omit `value` to inherit the secret from the worker's host environment.
      - name: OZ_DISPATCH_AUTH_HEADER
```

(The scripts are executable, so `dispatch_command: "/opt/oz/dispatch.py"` also works.)

## What the worker hands the script (stdin)

```json
{
  "version": 1,
  "task_id": "...",
  "execution_id": "...",
  "server_root_url": "https://app.warp.dev",
  "worker_id": "my-worker",
  "docker_image": "ubuntu:22.04",
  "base_args": ["agent", "run", "--task-id", "...", "--server-root-url", "..."],
  "env": { "GITHUB_ACCESS_TOKEN": "...", "...": "..." },
  "sidecars": [ { "image": "...", "mount_path": "/agent", "read_write": false } ],
  "task": { "id": "...", "title": "...", "task_definition": { "prompt": "..." } }
}
```

The non-secret identifiers `OZ_TASK_ID`, `OZ_EXECUTION_ID`, `OZ_WORKER_BACKEND`, `OZ_SERVER_ROOT_URL`, and `OZ_DOCKER_IMAGE` are also set in the script's environment. Secrets appear only in the stdin payload.

Your runtime should launch the agent with `base_args` inside an environment built from `docker_image` + `sidecars`, injecting `env`. Because `base_args` already includes `--task-id` and `--server-root-url`, the agent reports its own progress and terminal state to Warp — the worker does not. Keep the exit-code contract: exit `0` only when the task is durably accepted for execution.

## Local end-to-end testing

`dispatch-oz-local.py` lets you exercise the whole command-backend path against a local stack — local warp-server, local session-sharing-server, and a running `oz-agent-worker` — using a real agent run. It reads the payload, logs a summary of what the worker forwarded (so you can verify the contract), writes the full payload to `OZ_LOCAL_RUN_LOG_DIR/payload-<task_id>.json`, then launches `$OZ_BIN <base_args...>` detached with the payload's `env` applied.

Required/optional environment for this script:

- `OZ_BIN` (required): the local `oz`/Warp binary to exec (the same kind of binary the `direct` backend uses).
- `OZ_LOCAL_RUN_LOG_DIR` (optional): where to write per-task payloads and run logs (defaults to a temp dir).

The easiest way to run the full stack is `warp-server`'s `script/oz-local`, which boots the servers and the worker for you. Once it supports the command backend, run:

```bash
# from warp-server, with WARP_API_KEY exported and a local oz bundle built
./script/oz-local --worker-backend command --oz-path <path-to-oz-binary>
```

Then trigger a run routed to this worker (e.g. from `warp-internal`):

```bash
WITH_LOCAL_SERVER=1 WITH_LOCAL_SESSION_SHARING_SERVER=1 ./script/run --host-id local-dev
```

In the `oz-agent-worker` log you should see the worker claim the task and the `[dispatch-oz-local]` lines showing the forwarded `task_id`, `base_args`, and `env` keys; the agent then runs against your local server and reports its own status. Because runs are launched detached (fire-and-forget), they keep running after `script/oz-local` is stopped — find and stop them with `pgrep -fl 'agent run'` / `pkill -f 'agent run'` if needed.
