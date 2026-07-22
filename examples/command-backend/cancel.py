#!/usr/bin/env python3
"""Reference cancel command for the oz-agent-worker "command" backend.

Invoked best-effort when a dispatched task is cancelled. The worker sets
``OZ_RUN_ID`` and ``OZ_EXECUTION_ID`` in the environment; this script POSTs them
to ``OZ_CANCEL_URL`` so your runtime can stop the corresponding agent. Adapt the
request body to your runtime's API if needed.

Required environment:
  OZ_CANCEL_URL             REST endpoint to POST the cancellation to.

Optional environment:
  OZ_DISPATCH_AUTH_HEADER   Authorization header value (e.g. "Bearer ...").
  OZ_DISPATCH_TIMEOUT_SECS  Request timeout in seconds (default 30).
"""

import json
import os
import sys
import urllib.error
import urllib.request


def main():
    url = os.environ.get("OZ_CANCEL_URL")
    if not url:
        sys.stderr.write("OZ_CANCEL_URL must be set\n")
        return 2
    timeout = float(os.environ.get("OZ_DISPATCH_TIMEOUT_SECS", "30"))

    body = json.dumps(
        {
            "run_id": os.environ.get("OZ_RUN_ID", ""),
            "execution_id": os.environ.get("OZ_EXECUTION_ID", ""),
        }
    ).encode("utf-8")

    request = urllib.request.Request(url, data=body, method="POST")
    request.add_header("Content-Type", "application/json")
    auth = os.environ.get("OZ_DISPATCH_AUTH_HEADER")
    if auth:
        request.add_header("Authorization", auth)

    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            response.read()
    except urllib.error.HTTPError as exc:
        sys.stderr.write(f"cancel endpoint returned HTTP {exc.code}\n")
        return 1
    except urllib.error.URLError as exc:
        sys.stderr.write(f"failed to reach cancel endpoint: {exc}\n")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
