#!/usr/bin/env python3
"""Prepare and supervise an owned, persistent local Infisical Agent Vault stack."""

import argparse
import base64
import fcntl
import getpass
import importlib.util
import json
import os
from pathlib import Path
import secrets
import signal
import stat
import subprocess
import sys
import tempfile
import time


HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("vault_launcher", HERE / "run-worker.py")
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


def protected_directory(path):
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise ValueError("Vault state must be an owner-only directory")


def read_private(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd) as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            raise ValueError("Vault state files must be owner-only regular files")
        return stream.read()


def write_private(path, text):
    fd, temporary = tempfile.mkstemp(prefix=".vault-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(text)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def read_state(root):
    return json.loads(read_private(root / "state.json"))


def save_state(root, state):
    write_private(root / "state.json", json.dumps(state))


def prepare(root):
    if (root / "state.json").exists():
        state = read_state(root)
        if state.get("phase") != "ready":
            raise ValueError("Vault bootstrap was interrupted; preserve its state and follow README recovery guidance")
        return
    if (root / "pending-token").exists():
        read_private(root / "pending-token")
        return
    with open("/dev/tty", "r+") as terminal:
        token = getpass.getpass("Disposable GitHub token for local Agent Vault: ", stream=terminal).strip()
    if not token or any(character.isspace() for character in token):
        raise ValueError("A nonempty GitHub token without whitespace is required")
    write_private(root / "pending-token", token)


def provision(root, api):
    state_path = root / "state.json"
    if state_path.exists():
        state = read_state(root)
        if state.get("phase") != "ready":
            raise ValueError("Vault bootstrap is incomplete; see README recovery guidance")
        api.token = state["admin_token"]
        project = api.request("/api/v1/projects/" + state["project"], method="GET")["project"]
        if project["id"] != state["project"] or project["type"] != "agent-vault":
            raise ValueError("Stored Vault project does not match this instance")
        return state
    try:
        token = read_private(root / "pending-token")
        initialized = api.request("/api/v1/admin/config", authenticated=False, method="GET")["config"]["initialized"]
        if initialized:
            raise ValueError("Refusing an initialized Infisical instance without matching local state")
        save_state(root, {"phase": "provisioning"})
        bootstrap = api.request("/api/v1/admin/bootstrap", {
            "email": "oz-local@example.invalid",
            "password": "Oz!" + secrets.token_urlsafe(40),
            "organization": "Oz local credential PoC",
        }, authenticated=False)
        state = {
            "phase": "provisioning",
            "admin_token": bootstrap["identity"]["credentials"]["token"],
            "organization": bootstrap["organization"]["id"],
        }
        save_state(root, state)
        api.token = state["admin_token"]
        project = api.request("/api/v1/projects", {
            "projectName": "Oz local Agent Vault", "slug": "oz-local-vault",
            "type": "agent-vault", "shouldCreateDefaultEnvs": False,
        })["project"]["id"]
        state["project"] = project
        save_state(root, state)
        bundle = api.request("/api/v1/agent-vault/access-bundles", {
            "projectId": project, "name": "oz-github-poc",
        })["accessBundle"]["id"]
        api.request("/api/v1/agent-vault/services", {
            "projectId": project, "name": "github-user", "type": "http",
            "hostPattern": "api.github.com:443", "allowedMethods": ["GET"],
            "allowedPathPrefixes": ["/user"], "accessBundleIds": [bundle],
            "credential": {"type": "bearer", "value": token},
        })
        del token
        identity = api.request("/api/v1/identities", {
            "organizationId": state["organization"], "name": "oz-local-worker",
            "role": "no-access",
        })["identity"]["id"]
        ua = api.request("/api/v1/auth/universal-auth/identities/" + identity, {
            "accessTokenTTL": 1200, "accessTokenMaxTTL": 1200,
        })["identityUniversalAuth"]
        secret = api.request("/api/v1/auth/universal-auth/identities/" + identity + "/client-secrets", {})
        state.update(client_id=ua["clientId"], client_secret=secret["clientSecret"])
        save_state(root, state)
        api.request("/api/v1/agent-vault/members", {
            "projectId": project, "machineIdentityIds": [identity], "role": "member",
        })
        api.request("/api/v1/agent-vault/access-bundles/" + bundle + "/members", {
            "machineIdentityIds": [identity],
        })
        proxy = api.request("/api/v1/agent-vault/proxies", {
            "projectId": project, "name": "oz-local", "trafficPolicy": "service-hosts",
            "pollInterval": 10,
        })
        state.update(proxy_token=proxy["proxyToken"], phase="ready")
        save_state(root, state)
        return state
    finally:
        (root / "pending-token").unlink(missing_ok=True)


def docker_environment():
    return {
        name: os.environ[name] for name in (
            "PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG",
            "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "TMPDIR",
        ) if name in os.environ
    }


def compose(args, *command, proxy_token=None):
    environment = docker_environment()
    environment["AGENT_VAULT_TASK_NETWORK"] = args.network
    if proxy_token:
        environment["INFISICAL_AGENT_VAULT_PROXY_TOKEN"] = proxy_token
    result = subprocess.run([
        "docker", "compose", "--project-name", args.project_name,
        "--project-directory", str(args.state_dir),
        "--env-file", str(args.state_dir / ".env"), "-f", str(HERE / "compose.yaml"),
        "--profile", "proxy", *command,
    ], env=environment, capture_output=True, text=True, timeout=600)
    if result.returncode:
        raise RuntimeError("Vault Docker operation failed; check Docker, pinned images and port 8088")
    return result.stdout


def serve(args):
    root = args.state_dir
    existing = subprocess.run([
        "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=" + args.project_name,
    ], env=docker_environment(), capture_output=True, text=True, timeout=15)
    if existing.returncode:
        raise RuntimeError("Cannot inspect existing Vault containers")
    if not (root / ".env").exists():
        if (root / "state.json").exists() or existing.stdout.strip():
            raise ValueError("Restore the original Vault deployment keys before restarting existing resources")
        volumes = subprocess.run([
            "docker", "volume", "ls", "-q", "--filter",
            "label=com.docker.compose.project=" + args.project_name,
        ], env=docker_environment(), capture_output=True, text=True, timeout=15)
        if volumes.returncode:
            raise RuntimeError("Cannot inspect existing Vault data volumes")
        if volumes.stdout.strip():
            raise ValueError("Restore the original Vault deployment keys before restarting existing data")
        write_private(root / ".env", (
            f"POSTGRES_PASSWORD={secrets.token_hex(24)}\n"
            f"ENCRYPTION_KEY={secrets.token_hex(16)}\n"
            f"AUTH_SECRET={base64.b64encode(secrets.token_bytes(32)).decode()}\n"
        ))
    else:
        read_private(root / ".env")
    if existing.stdout.split():
        inspected = subprocess.run([
            "docker", "inspect", *existing.stdout.split(),
        ], env=docker_environment(), capture_output=True, text=True, timeout=15)
        if inspected.returncode:
            raise RuntimeError("Cannot verify existing Vault containers")
        for container in json.loads(inspected.stdout):
            labels = container["Config"]["Labels"]
            if (labels.get("com.docker.compose.project.working_dir") != str(root)
                    or labels.get("com.docker.compose.project.config_files") != str(HERE / "compose.yaml")):
                raise ValueError("Refusing Vault containers owned by a different state directory or configuration")
    api = launcher.Infisical("http://127.0.0.1:8088", "", "")
    try:
        compose(args, "up", "-d", "--wait", "infisical", "db", "redis")
        deadline = time.monotonic() + 180
        while True:
            try:
                api.request("/api/v1/admin/config", authenticated=False, method="GET")
                break
            except launcher.ApiError:
                if time.monotonic() >= deadline:
                    raise RuntimeError("Local Infisical did not become ready") from None
                time.sleep(1)
        state = provision(root, api)
        result = subprocess.run([
            "docker", "build", "-t", args.image, str(HERE),
        ], env=docker_environment(), capture_output=True, timeout=600)
        if result.returncode:
            raise RuntimeError("Vault task-image build failed; check Docker and pinned image access")
        compose(args, "up", "-d", "--wait", "proxy", proxy_token=state["proxy_token"])
        print("Agent Vault stack ready", flush=True)
        while True:
            time.sleep(5)
            rows = compose(args, "ps", "--format", "json")
            containers = [json.loads(line) for line in rows.splitlines() if line.strip()]
            if len(containers) != 4 or any(
                row["State"] != "running" or row.get("Health") not in ("healthy", "", None)
                for row in containers
            ):
                raise RuntimeError("An owned Vault container stopped or became unhealthy")
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        compose(args, "down", "--timeout", "15")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("prepare", "serve"))
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--project-name", default="oz-infisical-poc")
    parser.add_argument("--network", default="oz-vault-agent")
    parser.add_argument("--image", default="oz-infisical-poc-task:local")
    args = parser.parse_args()
    args.state_dir = args.state_dir.absolute()
    os.umask(0o077)
    try:
        protected_directory(args.state_dir)
        with open(args.state_dir / "lock", "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            if args.command == "prepare":
                prepare(args.state_dir)
            else:
                def interrupt(_signum, _frame):
                    raise KeyboardInterrupt()
                signal.signal(signal.SIGTERM, interrupt)
                signal.signal(signal.SIGINT, interrupt)
                try:
                    serve(args)
                finally:
                    (args.state_dir / "pending-token").unlink(missing_ok=True)
    except KeyboardInterrupt:
        return 143
    except (OSError, ValueError, KeyError, TypeError, RuntimeError,
            launcher.ApiError, subprocess.TimeoutExpired):
        print("Local Agent Vault setup failed; check dependencies, protected state and README recovery guidance. First setup needs an interactive terminal.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
