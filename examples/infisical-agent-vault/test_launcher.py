import http.server
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest


HERE = Path(__file__).parent
SESSION_ID = "ae2f2b13-a4cc-42a5-b37f-e4efad39317c"
SENTINEL = "DO_NOT_PRINT_REFLECTED_CREDENTIAL"


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.calls.append((self.path, body))
        status = 200
        if self.path.endswith("/login"):
            result = {"accessToken": "test-management-token"}
        elif self.path.endswith("/sessions"):
            if self.server.create_error:
                status, result = 403, {"message": SENTINEL}
            else:
                result = {"session": {"id": SESSION_ID, "token": "test-session-token"}}
        elif self.path.endswith("/revoke"):
            self.server.revokes += 1
            status = self.server.revoke_status
            if status == 401 and self.server.revokes > 1:
                status = 200
            result = {"message": SENTINEL} if status != 200 else {}
        else:
            status, result = 404, {}
        self.send_response(status)
        self.end_headers()
        self.wfile.write(json.dumps(result).encode())


class LauncherTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.calls = []
        self.server.revokes = 0
        self.server.revoke_status = 200
        self.server.create_error = False
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.close_server)
        self.worker = self.root / "worker"
        self.capture = self.root / "child.json"
        self.worker.write_text(
            f"#!{sys.executable}\n"
            "import json, os\n"
            f"open({str(self.capture)!r}, 'w').write(json.dumps(dict(os.environ)))\n"
        )
        self.worker.chmod(0o700)
        self.environment = {
            "PATH": os.environ["PATH"],
            "HOME": str(self.root),
            "WARP_API_KEY": "test-worker-key",
            "INFISICAL_UNIVERSAL_AUTH_CLIENT_ID": "test-client",
            "INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET": SENTINEL,
            "INFISICAL_AGENT_VAULT_PROXY_TOKEN": SENTINEL,
            "GITHUB_TOKEN": SENTINEL,
            "HTTP_PROXY": "http://127.0.0.1:1",
        }

    def close_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def command(self):
        return [
            sys.executable, str(HERE / "run-worker.py"),
            "--worker", str(self.worker), "--project", "test-project",
            "--url", f"http://127.0.0.1:{self.server.server_port}",
        ]

    def launch(self):
        return subprocess.run(
            self.command(), env=self.environment, capture_output=True, text=True, timeout=10
        )

    def test_worker_gets_session_not_management_or_github_credentials(self):
        result = self.launch()
        self.assertEqual(result.returncode, 0, result.stderr)
        child = json.loads(self.capture.read_text())
        self.assertEqual(child["INFISICAL_AGENT_VAULT_SESSION_TOKEN"], "test-session-token")
        self.assertEqual(child["WARP_API_KEY"], "test-worker-key")
        self.assertNotIn(SENTINEL, json.dumps(child))
        self.assertNotIn("HTTP_PROXY", child)
        session = next(body for path, body in self.server.calls if path.endswith("/sessions"))
        self.assertEqual(session["ttl"], "20m")
        self.assertEqual(session["accessBundle"], "oz-github-poc")
        self.assertEqual(self.server.revokes, 1)

    def test_failed_process_start_still_revokes(self):
        self.worker.unlink()
        result = self.launch()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.server.revokes, 1)

    def test_rejected_session_does_not_start_worker_or_echo_error(self):
        self.server.create_error = True
        result = self.launch()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.capture.exists())
        self.assertNotIn(SENTINEL, result.stdout + result.stderr)

    def test_cleanup_refreshes_expired_management_token(self):
        self.server.revoke_status = 401
        result = self.launch()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.server.revokes, 2)
        self.assertEqual(sum(path.endswith("/login") for path, _ in self.server.calls), 2)

    def test_cleanup_failure_is_reported_without_reflected_secret(self):
        self.server.revoke_status = 503
        result = self.launch()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("revocation failed", result.stderr)
        self.assertNotIn(SENTINEL, result.stdout + result.stderr)

    def test_termination_stops_worker_and_revokes(self):
        self.worker.write_text(self.worker.read_text() + "import time\ntime.sleep(60)\n")
        process = subprocess.Popen(
            self.command(), env=self.environment, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True,
        )
        try:
            deadline = time.monotonic() + 5
            while not self.capture.exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(self.capture.exists(), "worker did not start")
            process.send_signal(signal.SIGTERM)
            stdout, stderr = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 143, stderr)
            self.assertEqual(self.server.revokes, 1)
            self.assertNotIn(SENTINEL, stdout + stderr)
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()


class HelperTest(unittest.TestCase):
    def test_helper_filters_output_and_never_retries_failed_proxy(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cli = root / "infisical"
            environment = dict(os.environ, PATH=directory + ":" + os.environ["PATH"])
            environment["INFISICAL_AGENT_VAULT_SESSION_TOKEN"] = "test-session"
            environment["INFISICAL_AGENT_VAULT_PROXY_ADDRESS"] = "proxy:17323"
            for response, code, expected in (
                ('{"login":"octocat"}', 0, "octocat\n"),
                (SENTINEL, 1, ""),
                ('{"login":"bad\\n' + SENTINEL + '"}', 0, ""),
            ):
                with self.subTest(response=response):
                    cli.write_text(f"#!/bin/sh\nprintf '%s' '{response}'\nexit {code}\n")
                    cli.chmod(0o700)
                    result = subprocess.run(
                        ["sh", str(HERE / "github-user")], env=environment,
                        capture_output=True, text=True, timeout=5,
                    )
                    self.assertEqual(result.stdout, expected)
                    self.assertEqual(result.returncode == 0, bool(expected))
                    self.assertNotIn(SENTINEL, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
