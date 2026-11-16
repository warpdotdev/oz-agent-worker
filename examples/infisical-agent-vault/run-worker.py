#!/usr/bin/env python3
"""Run a local worker with a finite Agent Vault session, never the API credential."""

import argparse
import getpass
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid


class ApiError(Exception):
    def __init__(self, status=None):
        super().__init__("Infisical request failed")
        self.status = status


class Interrupted(Exception):
    def __init__(self, signum):
        self.signum = signum


class Infisical:
    def __init__(self, url, client_id, client_secret):
        parsed = urllib.parse.urlsplit(url)
        if (
            parsed.scheme != "http"
            or parsed.hostname not in ("127.0.0.1", "localhost", "::1")
            or parsed.username
            or parsed.password
            or parsed.path not in ("", "/")
            or parsed.query
            or parsed.fragment
        ):
            raise ValueError("Use the loopback HTTP address of the local Infisical instance")
        self.url = url.rstrip("/")
        self.client_id = client_id
        self.client_secret = client_secret
        self.token = None
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect()
        )

    def request(self, path, body=None, authenticated=True, method="POST"):
        headers = {"Content-Type": "application/json"}
        if authenticated:
            headers["Authorization"] = f"Bearer {self.token}"
        request = urllib.request.Request(
            self.url + path,
            None if method == "GET" else json.dumps(body or {}).encode(),
            headers, method=method,
        )
        try:
            with self.opener.open(request, timeout=10) as response:
                raw = response.read(1024 * 1024 + 1)
                if len(raw) > 1024 * 1024:
                    raise ApiError()
                return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as exc:
            exc.close()
            raise ApiError(exc.code) from None
        except (OSError, ValueError, urllib.error.URLError):
            raise ApiError() from None

    def login(self):
        result = self.request(
            "/api/v1/auth/universal-auth/login",
            {"clientId": self.client_id, "clientSecret": self.client_secret},
            authenticated=False,
        )
        token = result.get("accessToken") if isinstance(result, dict) else None
        if not isinstance(token, str) or not token.strip():
            raise ApiError()
        self.token = token

    def create_session(self, project, bundle):
        result = self.request(
            "/api/v1/agent-vault/sessions",
            {"projectId": project, "accessBundle": bundle, "ttl": "20m",
             "label": "local-oz-worker-poc"},
        )
        session = result.get("session") if isinstance(result, dict) else None
        if not isinstance(session, dict):
            raise ApiError()
        try:
            session_id = str(uuid.UUID(session["id"]))
        except (KeyError, ValueError, TypeError, AttributeError):
            raise ApiError() from None
        token = session.get("token")
        if not isinstance(token, str) or not token.strip():
            # The session exists even if its response cannot be used.
            self.revoke(session_id)
            raise ApiError()
        return session_id, token

    def revoke(self, session_id):
        path = f"/api/v1/agent-vault/sessions/{session_id}/revoke"
        try:
            self.request(path)
        except ApiError as exc:
            if exc.status != 401:
                raise
            self.login()
            self.request(path)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def worker_environment(session_token):
    allowed = (
        "PATH", "HOME", "TMPDIR", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR",
        "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY",
        "DOCKER_CERT_PATH", "WARP_API_KEY",
    )
    environment = {key: os.environ[key] for key in allowed if key in os.environ}
    environment["INFISICAL_AGENT_VAULT_SESSION_TOKEN"] = session_token
    return environment


def run(api, args):
    session_id = None
    child = None
    status = 1
    def interrupt(signum, _frame):
        raise Interrupted(signum)

    previous = {sig: signal.signal(sig, interrupt) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        api.login()
        session_id, token = api.create_session(args.project, args.bundle)
        child = subprocess.Popen(
            [args.worker, "--config-file", str(args.config_file),
             "--server-root-url", args.oz_server, *args.worker_args],
            env=worker_environment(token),
            start_new_session=True,
        )
        print("Local worker started; Agent Vault session expires in 20 minutes", flush=True)
        status = child.wait()
        if status < 0:
            status = 128 - status
    except Interrupted as exc:
        status = 128 + exc.signum
    except (ApiError, OSError):
        print("Local worker launch failed; check local Infisical and worker configuration", file=sys.stderr)
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        if child is not None and child.poll() is None:
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                child.wait()
        if session_id is not None:
            try:
                api.revoke(session_id)
                print("Agent Vault session revoked", flush=True)
            except ApiError:
                print("Session revocation failed; revoke it in Infisical or wait for its 20-minute TTL", file=sys.stderr)
                status = 1
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    return status


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--worker", required=True, help="Path to the locally built oz-agent-worker")
    parser.add_argument("--project", help="Agent Vault project ID")
    parser.add_argument("--bundle", default="oz-github-poc")
    parser.add_argument("--url", default="http://127.0.0.1:8088")
    parser.add_argument("--oz-server", default="https://app.warp.dev")
    parser.add_argument("--config-file", type=Path, default=Path(__file__).with_name("worker.yaml"))
    parser.add_argument("--credentials-file", type=Path, help="Owner-only local setup state")
    parser.add_argument("worker_args", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.worker_args[:1] == ["--"]:
        args.worker_args.pop(0)
    if not os.environ.get("WARP_API_KEY"):
        parser.error("Export the Oz worker's team-scoped WARP_API_KEY first")
    if args.credentials_file:
        try:
            fd = os.open(args.credentials_file, os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(fd) as stream:
                info = os.fstat(stream.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_uid != os.getuid():
                    raise ValueError()
                state = json.load(stream)
            client_id, client_secret = state["client_id"], state["client_secret"]
            args.project = args.project or state["project"]
        except (OSError, ValueError, KeyError, TypeError):
            parser.error("Cannot read protected local setup state")
    else:
        client_id = os.environ.get("INFISICAL_UNIVERSAL_AUTH_CLIENT_ID") or input("Infisical machine Client ID: ").strip()
        client_secret = os.environ.get("INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET") or getpass.getpass("Infisical machine Client Secret: ")
    if not client_id or not client_secret:
        parser.error("Machine identity client ID and secret are required")
    if not args.project:
        parser.error("--project or provisioned --credentials-file is required")
    try:
        return run(Infisical(args.url, client_id, client_secret), args)
    except ValueError as exc:
        parser.error(str(exc))


if __name__ == "__main__":
    sys.exit(main())
