#!/usr/bin/env python3
"""Exercise a disposable Agent Vault bundle against the pinned proxy and a local TLS service."""

import argparse
import getpass
import importlib.util
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
import urllib.request


HERE = Path(__file__).parent
CLI_IMAGE = "infisical/cli:0.45.8@sha256:ef5cd9bef770e1d13acfa18414a65719a262c4b7681735086416852f1574d019"
UPSTREAM = """
import hmac, http.server, json, os, ssl
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass
    def do_GET(self):
        with open('/tmp/requests', 'a') as stream:
            stream.write('request\\n')
        valid = hmac.compare_digest(self.headers.get('Authorization', ''),
                                    'Bearer ' + os.environ['SMOKE_UPSTREAM_TOKEN'])
        self.send_response(200 if valid else 401)
        self.end_headers()
        self.wfile.write(json.dumps({'login': 'proxy-smoke'} if valid else {}).encode())
    do_POST = do_GET
server = http.server.HTTPServer(('0.0.0.0', 8443), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain('/fixture/cert.pem', '/fixture/key.pem')
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
"""


def docker(*args, environment=None):
    result = subprocess.run(
        ["docker", *args], env=environment, capture_output=True, text=True, timeout=60
    )
    if result.returncode:
        raise RuntimeError("Docker smoke operation failed; check images and named networks")
    return result.stdout.strip()


def smoke(args, api):
    suffix = secrets.token_hex(5)
    upstream = "oz-vault-smoke-upstream-" + suffix
    proxy = "oz-vault-smoke-proxy-" + suffix
    bundle_name = "oz-vault-smoke-" + suffix
    resources = []
    containers = []
    session_id = None
    with tempfile.TemporaryDirectory(prefix="oz-vault-smoke-") as directory:
        root = Path(directory)
        root.chmod(0o755)
        (root / "upstream.py").write_text(UPSTREAM)
        generated = subprocess.run(
            ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
             "-keyout", str(root / "key.pem"), "-out", str(root / "cert.pem"),
             "-days", "1", "-subj", f"/CN={upstream}",
             "-addext", f"subjectAltName=DNS:{upstream}"],
            capture_output=True, timeout=15,
        )
        if generated.returncode:
            raise RuntimeError("Could not generate disposable TLS certificate")
        environment = dict(os.environ)
        environment["SMOKE_UPSTREAM_TOKEN"] = secrets.token_urlsafe(32)
        try:
            bundle = api.request("/api/v1/agent-vault/access-bundles", {
                "projectId": args.project, "name": bundle_name,
            })["accessBundle"]
            resources.append("/api/v1/agent-vault/access-bundles/" + bundle["id"])
            service = api.request("/api/v1/agent-vault/services", {
                "projectId": args.project, "name": bundle_name, "type": "http",
                "hostPattern": f"{upstream}:8443", "allowedMethods": ["GET"],
                "allowedPathPrefixes": ["/user"], "accessBundleIds": [bundle["id"]],
                "credential": {"type": "bearer", "value": environment["SMOKE_UPSTREAM_TOKEN"]},
            })["service"]
            resources.append("/api/v1/agent-vault/services/" + service["id"])
            registered = api.request("/api/v1/agent-vault/proxies", {
                "projectId": args.project, "name": bundle_name,
                "trafficPolicy": "service-hosts", "pollInterval": 10,
            })
            resources.append("/api/v1/agent-vault/proxies/" + registered["proxy"]["id"])
            environment["INFISICAL_AGENT_VAULT_PROXY_TOKEN"] = registered["proxyToken"]
            docker("create", "--name", upstream, "--network", args.task_network,
                   "-e", "SMOKE_UPSTREAM_TOKEN", "-v", f"{root}:/fixture:ro",
                   args.image, "python3", "/fixture/upstream.py", environment=environment)
            containers.append(upstream)
            docker("start", upstream)
            docker("create", "--name", proxy, "--network", args.task_network,
                   "-e", "INFISICAL_AGENT_VAULT_PROXY_TOKEN",
                   "-e", "INFISICAL_DOMAIN=http://infisical:8080",
                   "-e", "SSL_CERT_FILE=/fixture/ca.pem",
                   "-v", f"{root / 'cert.pem'}:/fixture/ca.pem:ro",
                   CLI_IMAGE, "agent-vault", "proxy", "--telemetry=false",
                   environment=environment)
            containers.append(proxy)
            docker("network", "connect", args.management_network, proxy)
            docker("start", proxy)
            session_id, token = api.create_session(args.project, bundle_name)
            environment = {
                key: value for key, value in os.environ.items()
                if key in ("PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG",
                           "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH")
            }
            environment["INFISICAL_AGENT_VAULT_SESSION_TOKEN"] = token
            environment["INFISICAL_AGENT_VAULT_PROXY_ADDRESS"] = proxy + ":17323"

            def request(method="GET", path="/user"):
                result = subprocess.run(
                    ["docker", "run", "--rm", "--network", args.task_network,
                     "-e", "INFISICAL_AGENT_VAULT_SESSION_TOKEN",
                     "-e", "INFISICAL_AGENT_VAULT_PROXY_ADDRESS", args.image,
                     "infisical", "agent-vault", "run", "--no-skills", "--no-mcp-servers",
                     "--telemetry=false", "--", "curl", "-q", "--silent",
                     "--max-time", "5", "--proto", "=https", "-X", method,
                     "-w", "\n%{http_connect} %{http_code}", f"https://{upstream}:8443{path}"],
                    env=environment, capture_output=True, text=True, timeout=20,
                )
                body, _, statuses = result.stdout.rpartition("\n")
                connect, _, status = statuses.partition(" ")
                return result.returncode, body, status, connect

            deadline = time.monotonic() + 45
            while True:
                code, body, status, _ = request()
                if code == 0 and status == "200":
                    break
                if time.monotonic() > deadline:
                    raise RuntimeError("Proxy did not deliver an authenticated TLS response")
                time.sleep(1)
            if json.loads(body) != {"login": "proxy-smoke"}:
                raise RuntimeError("Unexpected upstream response")
            for method, path in (("POST", "/user"), ("GET", "/other")):
                code, _, status, _ = request(method, path)
                if code != 0 or status != "403":
                    raise RuntimeError("Proxy did not enforce method/path denial")
            count = docker("exec", upstream, "python3", "-c",
                           "print(len(open('/tmp/requests').readlines()))")
            if count != "1":
                raise RuntimeError("A denied request reached the upstream")
            print("PASS: TLS header injection; method/path denial before upstream", flush=True)
            api.revoke(session_id)
            session_id = None
            deadline = time.monotonic() + 20
            while True:
                code, _, status, connect = request()
                if (code == 0 and status in ("401", "403", "407")) or (
                    code == 56 and connect in ("401", "403", "407")
                ):
                    break
                if time.monotonic() > deadline:
                    raise RuntimeError("Revoked session was not rejected within two poll intervals")
                time.sleep(1)
            print("PASS: revoked session rejected within two 10-second poll intervals", flush=True)
        finally:
            if session_id is not None:
                try:
                    api.revoke(session_id)
                except Exception:
                    print("Smoke session cleanup failed; revoke its session in Infisical",
                          file=sys.stderr)
            for name in reversed(containers):
                docker("rm", "-f", name)
            for path in reversed(resources):
                req = urllib.request.Request(
                    api.url + path, headers={"Authorization": "Bearer " + api.token},
                    method="DELETE",
                )
                try:
                    with api.opener.open(req, timeout=10):
                        pass
                except OSError:
                    print("Smoke resource cleanup failed; remove its oz-vault-smoke resources in Infisical",
                          file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", required=True, help="Disposable Agent Vault project ID")
    parser.add_argument("--url", default="http://127.0.0.1:8088")
    parser.add_argument("--management-network", default="oz-infisical-poc_control")
    parser.add_argument("--task-network", default="oz-vault-agent")
    parser.add_argument("--image", default="oz-infisical-poc-task:local")
    args = parser.parse_args()
    module_spec = importlib.util.spec_from_file_location("launcher", HERE / "run-worker.py")
    launcher = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(launcher)
    api = launcher.Infisical(args.url, "", "")
    api.token = os.environ.get("INFISICAL_SMOKE_ADMIN_TOKEN") or getpass.getpass(
        "Infisical organization-scoped admin access token (smoke only): "
    )
    try:
        smoke(args, api)
    except (launcher.ApiError, OSError, RuntimeError, ValueError, KeyError,
            subprocess.TimeoutExpired):
        print("Proxy smoke failed; verify local admin access, images, networks, and proxy policy",
              file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
