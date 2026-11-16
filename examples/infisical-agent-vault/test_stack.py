import importlib.util
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("vault_stack", Path(__file__).with_name("local-stack.py"))
stack = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stack)
SENTINEL = "fixture-github-secret"


class API:
    def __init__(self, initialized=False, fail=None):
        self.calls = []
        self.initialized = initialized
        self.fail = fail

    def request(self, path, body=None, **kwargs):
        self.calls.append((path, body, kwargs))
        if self.fail == path:
            raise stack.launcher.ApiError(503)
        if path.endswith("/config"):
            return {"config": {"initialized": self.initialized}}
        if path.endswith("/bootstrap"):
            return {"identity": {"credentials": {"token": "fixture-admin"}},
                    "organization": {"id": "org"}}
        if "/projects" in path:
            return {"project": {"id": "project", "type": "agent-vault"}}
        if path.endswith("/access-bundles"):
            return {"accessBundle": {"id": "bundle"}}
        if path.endswith("/identities"):
            return {"identity": {"id": "identity"}}
        if path.endswith("/client-secrets"):
            return {"clientSecret": "fixture-client-secret"}
        if "/universal-auth/identities/" in path:
            return {"identityUniversalAuth": {"clientId": "fixture-client"}}
        if path.endswith("/proxies"):
            return {"proxyToken": "fixture-proxy"}
        return {}


class StackTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        stack.write_private(self.root / "pending-token", SENTINEL)

    def test_first_setup_scopes_access_and_restart_reuses_state(self):
        api = API()
        state = stack.provision(self.root, api)
        self.assertEqual(state["phase"], "ready")
        service = next(body for path, body, _ in api.calls if path.endswith("/services"))
        self.assertEqual(service["hostPattern"], "api.github.com:443")
        self.assertEqual(service["allowedMethods"], ["GET"])
        self.assertEqual(service["allowedPathPrefixes"], ["/user"])
        member = next(body for path, body, _ in api.calls if path.endswith("/agent-vault/members"))
        self.assertEqual(member["role"], "member")
        self.assertNotIn(SENTINEL, (self.root / "state.json").read_text())
        self.assertFalse((self.root / "pending-token").exists())
        self.assertEqual((self.root / "state.json").stat().st_mode & 0o777, 0o600)
        restarted = API(initialized=True)
        self.assertEqual(stack.provision(self.root, restarted), state)
        self.assertTrue(all(options.get("method") == "GET" for _, _, options in restarted.calls))

    def test_partial_failure_drops_token_and_refuses_duplicate_bootstrap(self):
        with self.assertRaises(stack.launcher.ApiError):
            stack.provision(self.root, API(fail="/api/v1/agent-vault/services"))
        self.assertFalse((self.root / "pending-token").exists())
        self.assertEqual(stack.read_state(self.root)["phase"], "provisioning")
        retried = API(initialized=True)
        with self.assertRaises(ValueError):
            stack.provision(self.root, retried)
        self.assertEqual(retried.calls, [])

    def test_existing_instance_without_ownership_is_not_modified(self):
        api = API(initialized=True)
        with self.assertRaises(ValueError):
            stack.provision(self.root, api)
        self.assertFalse((self.root / "state.json").exists())
        self.assertFalse((self.root / "pending-token").exists())
        self.assertTrue(all(options.get("method") == "GET" for _, _, options in api.calls))

    def test_unprotected_or_linked_secret_state_is_refused(self):
        secret = self.root / "secret"
        secret.write_text(json.dumps({"admin_token": SENTINEL}))
        secret.chmod(0o644)
        with self.assertRaises(ValueError):
            stack.read_private(secret)
        secret.chmod(0o600)
        link = self.root / "linked"
        link.symlink_to(secret)
        with self.assertRaises(OSError):
            stack.read_private(link)

    def test_foreign_compose_project_is_not_started_or_stopped(self):
        stack.write_private(self.root / ".env", "fixture")
        args = SimpleNamespace(state_dir=self.root, project_name="fixture")
        responses = [
            SimpleNamespace(returncode=0, stdout="container"),
            SimpleNamespace(returncode=0, stdout=json.dumps([{
                "Config": {"Labels": {"com.docker.compose.project.working_dir": "/unrelated"}},
            }])),
        ]
        with mock.patch.object(stack.subprocess, "run", side_effect=responses), \
                mock.patch.object(stack, "compose") as compose:
            with self.assertRaises(ValueError):
                stack.serve(args)
            compose.assert_not_called()

    def test_missing_keys_do_not_recreate_existing_state_or_data(self):
        for retained in ("state", "container", "volume"):
            with self.subTest(retained=retained):
                root = self.root / retained
                root.mkdir()
                if retained == "state":
                    stack.write_private(root / "state.json", '{"phase":"ready"}')
                args = SimpleNamespace(state_dir=root, project_name="fixture")

                def docker(command, **kwargs):
                    output = "owned-resource" if (
                        (retained == "container" and command[1] == "ps")
                        or (retained == "volume" and command[1] == "volume")
                    ) else ""
                    return SimpleNamespace(returncode=0, stdout=output)

                with mock.patch.object(stack.subprocess, "run", side_effect=docker), \
                        mock.patch.object(stack, "compose", side_effect=AssertionError("Must not recreate")):
                    with self.assertRaises(ValueError):
                        stack.serve(args)
                self.assertFalse((root / ".env").exists())


if __name__ == "__main__":
    unittest.main()
