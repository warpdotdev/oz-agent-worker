#!/usr/bin/env python3
"""Local real-run dispatch command for the oz-agent-worker "command" backend.

Unlike dispatch.py (which forwards the payload to an HTTP runtime), this
reference runs the agent *for real* on the host using the worker-provided
``base_args``. It exists for local end-to-end testing of the command backend —
for example via warp-server's ``script/oz-local --worker-backend command`` — so
you can both (a) verify the worker forwards the right payload to the command and
(b) trigger an actual run against your local server.

It reads the ``DispatchPayload`` (JSON) on stdin and:
  1. logs a summary of what it received and writes the full payload to
     ``OZ_LOCAL_RUN_LOG_DIR/payload-<task_id>.json`` for inspection,
  2. launches ``$OZ_BIN <base_args...>`` detached (fire-and-forget) with the
     payload's ``env`` applied, writing the run's output to a per-task log file,
  3. exits 0 once the run is launched. The agent reports its own terminal state
     to the server (base_args already carries --task-id / --server-root-url), so
     the worker does not finalize the task.

This mirrors how the `direct` backend executes the host oz binary, but routed
through the command backend, so it ignores docker_image / sidecars.

Required environment:
  OZ_BIN                The oz/Warp binary to exec; base_args is appended to it.

Optional environment:
  OZ_LOCAL_RUN_LOG_DIR  Directory for per-task payload + run logs (default: tmp).

Exit 0  => run launched (the worker treats the task as dispatched).
Exit !=0 => failed to launch; the worker marks the task failed.
"""

import json
import os
import shlex
import subprocess
import sys
import tempfile


def main():
    oz_bin = os.environ.get("OZ_BIN")
    if not oz_bin:
        sys.stderr.write("OZ_BIN must be set to the oz/Warp binary path\n")
        return 2

    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as exc:
        sys.stderr.write(f"invalid dispatch payload on stdin: {exc}\n")
        return 1

    task_id = payload.get("task_id", "unknown")
    base_args = payload.get("base_args") or []
    env_overlay = payload.get("env") or {}

    log_dir = os.environ.get("OZ_LOCAL_RUN_LOG_DIR") or tempfile.gettempdir()
    os.makedirs(log_dir, exist_ok=True)

    # Persist the full payload so the forwarded contents are easy to inspect.
    payload_path = os.path.join(log_dir, f"payload-{task_id}.json")
    with open(payload_path, "w", encoding="utf-8") as payload_file:
        json.dump(payload, payload_file, indent=2, sort_keys=True)

    # Log a summary so the worker log shows exactly what was forwarded.
    sys.stderr.write(
        "[dispatch-oz-local] task_id=%s execution_id=%s image=%s\n"
        % (task_id, payload.get("execution_id", ""), payload.get("docker_image", ""))
    )
    sys.stderr.write(
        "[dispatch-oz-local] base_args: %s\n"
        % " ".join(shlex.quote(arg) for arg in base_args)
    )
    sys.stderr.write(
        "[dispatch-oz-local] env keys: %s\n" % ", ".join(sorted(env_overlay))
    )
    sys.stderr.write("[dispatch-oz-local] full payload: %s\n" % payload_path)

    if not base_args:
        sys.stderr.write("payload has no base_args; nothing to run\n")
        return 1

    # Inherit the current environment and overlay the task env (incl. secrets).
    child_env = dict(os.environ)
    child_env.update({str(key): str(value) for key, value in env_overlay.items()})

    argv = [oz_bin, *base_args]
    run_log_path = os.path.join(log_dir, f"oz-run-{task_id}.log")

    # Launch detached so the run outlives this short-lived dispatch command,
    # matching the command backend's fire-and-forget contract.
    try:
        run_log = open(run_log_path, "ab")
    except OSError as exc:
        sys.stderr.write(f"failed to open run log {run_log_path}: {exc}\n")
        return 1

    try:
        proc = subprocess.Popen(
            argv,
            env=child_env,
            stdin=subprocess.DEVNULL,
            stdout=run_log,
            stderr=run_log,
            start_new_session=True,
        )
    except OSError as exc:
        sys.stderr.write(f"failed to launch oz run: {exc}\n")
        return 1
    finally:
        run_log.close()

    sys.stderr.write(
        f"[dispatch-oz-local] launched pid={proc.pid}; run output -> {run_log_path}\n"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
