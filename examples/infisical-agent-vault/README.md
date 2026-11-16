# Local Oz worker with Infisical Agent Vault

This opt-in PoC stores a disposable GitHub token in a separate local Infisical
instance. A Docker task calls `github-user` through its proxy. The task receives
a scoped 20-minute session, not the GitHub token or Infisical management keys.

## Recommended: integrated oz-local startup

Use both branches in sibling checkouts:

- `oz-agent-worker`: `factory/infisical-local-worker-poc` ([PR 184](https://github.com/warpdotdev/oz-agent-worker/pull/184)).
- `warp-server`: `factory/oz-local-agent-vault`.

From `warp-server`, with the normal oz-local prerequisites installed and an
initialized local Warp database:

```bash
./script/oz-local up --worker-backend docker --with-agent-vault --detach --wait
./script/oz-local-agent-vault-test
```

The first command prompts, without echo, for a disposable GitHub token before
detaching. Supply a narrowly scoped token for an account you may use for testing.
Do not put it in a command, shell history, `.env`, or worker config. First startup
requires an interactive terminal. Subsequent starts reuse the stored deployment
and do not prompt or replace credentials.

Startup provisions the pinned Infisical stack, a scoped Member identity,
bundle/service and proxy, builds the task image, then starts the wrapped worker.
It never resets the Warp database. Normal server startup migrations still run.
Use `--no-worker-ngrok` as well when you do not need hosted-worker tunnels.
Docker Linux containers and access to the existing worker/agent images are
required. The existing database, GCP, Temporal and session-sharing dependencies
are not provisioned by this example.

The test requires the Oz CLI on PATH, or `OZ_CLI` set to its executable. It obtains
a local fixture API key without printing or saving it. It creates an environment
with the built image and no repository, submits to the configured local worker,
then probes `github-user` in the worker-claimed task container. Success requires
the authenticated login, selected local worker and a `SUCCEEDED` terminal state.
The script prints those facts and the run ID. Stack readiness alone is not E2E
proof. The test removes its environment after the run ends; on interruption or
failure it attempts cancellation first.

Inspect and stop the stack from `warp-server`:

```bash
./script/oz-local status
./script/oz-local logs agent-vault
./script/oz-local logs oz-worker
./script/oz-local down
```

Ordered shutdown stops the worker and revokes its session before stopping the
proxy and owned containers. The database volume and protected state persist.
Restart the stack after 20 minutes for a new session; there is no silent refresh.

## Standalone controller and worker

From the worker checkout, prepare an owner-only state directory and enter the
token at the hidden prompt:

```bash
python3 examples/infisical-agent-vault/local-stack.py prepare --state-dir .local-agent-vault
python3 examples/infisical-agent-vault/local-stack.py serve --state-dir .local-agent-vault
```

Keep the controller running. In another terminal, build the worker, obtain its
normal team-scoped `WARP_API_KEY` through your existing secure credential flow,
and export that variable without pasting its value into command history:

```bash
go build -o /tmp/oz-agent-worker .
python3 examples/infisical-agent-vault/run-worker.py \
  --worker /tmp/oz-agent-worker --credentials-file .local-agent-vault/state.json \
  -- --worker-id infisical-local-poc
```

The standalone config uses `oz-vault-agent` and image
`oz-infisical-poc-task:local`. Submit an Oz run to `infisical-local-poc` using an
environment with that image, no repositories and no managed GitHub secrets.
Ask it to run `github-user` and return the account login. For local Warp URLs and
automatic submission/image selection, use the integrated mode instead.
Stop the worker first to revoke, then stop the controller with Ctrl-C.

`init.py`, `.env.example`, and `compose.yaml` also support manual dashboard setup.
Do not combine a manually initialized deployment with controller bootstrap.

## Boundaries

- Infisical listens only on `127.0.0.1:8088`. Storage and management do not join
  the task network. Only the proxy joins task and management networks.
- The service allows GitHub `GET /user` **and `/user/...`**. Infisical's policy
  uses path prefixes, not exact matching. This is an accepted PoC limitation.
- Proxying applies only to the helper. Ordinary task egress remains possible;
  this is not a general network sandbox.
- One session is reused across the worker's sequential tasks. A holder can
  misuse that capability until revocation or expiry. This is not per-run isolation.
- A malicious host or Docker administrator can inspect local credentials.
- Vault mode rejects credential/proxy task overrides and disables oz-local's
  ambient `gh auth token` lookup and `GH_TOKEN` injection.
- Never mount the host home, Docker socket, state directory or Infisical volumes
  into tasks. Do not share state files or Docker inspection output.

## Troubleshooting and recovery

- Install missing normal oz-local dependencies with the server's documented
  bootstrap workflow. Confirm Docker and `docker compose version` work.
- Port 8088 must be free before startup. Stop the conflicting deployment
  yourself; the controller does not adopt an initialized unrelated instance.
- State lives in `warp-server/logs/oz-local/agent-vault/` for integrated mode.
  It must remain owned by you, with directory mode 0700 and files mode 0600.
  Never print `.env`, `state.json` or `pending-token`.
- Partial bootstrap deliberately stops instead of creating duplicate resources.
  Preserve the state directory and owned database volume. Do not edit
  `phase=provisioning` to `ready`. Recover the owned deployment manually using
  its stored control credentials, or explicitly choose to discard that PoC's
  data and state before a fresh bootstrap. No automatic destructive reset exists.
- If the GitHub token expires, restart does not replace it. Update the owned
  service through Infisical's API using the protected local admin token, then
  restart the worker for a fresh session. Bootstrap does not retain a dashboard
  password. Do not print the admin token or put it in command arguments.
- A hard kill can prevent revocation. The 20-minute TTL bounds the capability;
  revoke active sessions in Infisical when immediate invalidation is required.
- Setup diagnostics omit subprocess/API bodies to avoid leaking secrets.
  Inspect Docker availability and the named oz-local resource first. Do not
  paste raw container configuration or credential-bearing logs into reports.

## Verification

```bash
go test ./...
python3 -m unittest discover -s examples/infisical-agent-vault -p 'test_*.py'
```

`smoke-proxy.py` tests the real pinned proxy against a disposable TLS upstream.
It requires an organization-scoped Agent Vault admin access token, entered at
its hidden prompt, and the owned project's nonsecret ID:

```bash
python3 examples/infisical-agent-vault/smoke-proxy.py \
  --project <project-id> --management-network oz-infisical-poc_control
```

For oz-local, use the checkout-specific task/management networks and image
instead. The smoke checks header injection, method/path denial and revocation
within two polling intervals. This controlled upstream is not live GitHub/Oz
E2E proof. Use the integrated test command with real disposable credentials for
that proof.
