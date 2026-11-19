# Local worker credential access with Infisical Agent Vault

## Summary

Build an opt-in PoC that Daniel can check out, configure with credentials on his
machine, and exercise through a real local Docker-backed Oz worker. Infisical
holds a GitHub token. A tool inside the task uses a scoped session to call
`GET https://api.github.com/user` without receiving that token.

The opt-in `warp-server/script/oz-local --with-agent-vault` mode automates the
local setup and prints the command that submits the real worker smoke run.

Tracking: [REMOTE-3768](https://linear.app/warpdotdev/issue/REMOTE-3768).

## Context

Worker source references use revision `8553d80ba06479e93de78b7ee37e374062487c5a`:

- [`internal/config/config.go:53-65`](https://github.com/warpdotdev/oz-agent-worker/blob/8553d80ba06479e93de78b7ee37e374062487c5a/internal/config/config.go#L53-L65)
  defines Docker volumes, image policy and environment, but no task network.
- [`internal/worker/docker.go:173-207`](https://github.com/warpdotdev/oz-agent-worker/blob/8553d80ba06479e93de78b7ee37e374062487c5a/internal/worker/docker.go#L173-L207)
  passes configured task environment, mounts agent sidecars and runs
  `/agent/entrypoint.sh` in the assigned task image.
- [`README.md:171-190`](https://github.com/warpdotdev/oz-agent-worker/blob/8553d80ba06479e93de78b7ee37e374062487c5a/README.md#L171-L190)
  documents setup hooks for Direct mode, not Docker mode.

Use the maintained Infisical-integrated Agent Vault, not the deprecated standalone
`agent-vault` server. Start with Infisical `v0.167.10` and CLI `v0.45.8`; resolve
their published image tags/digests before committing the runnable setup.
[Infisical's proxy documentation](https://github.com/Infisical/infisical/blob/v0.167.10/docs/documentation/platform/agent-vault/proxies.mdx)
explicitly excludes WebSockets and HTTP/2-only clients such as gRPC.
Proxying the entire Oz runtime could break its control connections.

The oz-local extension uses warp-server revision
`dbd81c87c9e47466a309e3bd970230ee3493e574`:

- [`script/oz-local`](https://github.com/warpdotdev/warp-server/blob/dbd81c87c9e47466a309e3bd970230ee3493e574/script/oz-local)
  owns start options and command routing.
- [`script/lib/oz-local-config.sh:486-537`](https://github.com/warpdotdev/warp-server/blob/dbd81c87c9e47466a309e3bd970230ee3493e574/script/lib/oz-local-config.sh#L486-L537)
  generates Docker worker configuration. Lines 687-692 also obtain an ambient
  GitHub token, which must not be injected in Vault mode.
- [`script/oz-local-process:504-588`](https://github.com/warpdotdev/warp-server/blob/dbd81c87c9e47466a309e3bd970230ee3493e574/script/oz-local-process#L504-L588)
  starts the worker with local WebSocket/server URLs and task environment.
- [`script/oz-local.process-compose.yaml`](https://github.com/warpdotdev/warp-server/blob/dbd81c87c9e47466a309e3bd970230ee3493e574/script/oz-local.process-compose.yaml)
  owns dependency ordering, readiness and shutdown.

## Proposed changes

### Repository ownership

Keep the spec and implementation on `factory/infisical-local-worker-poc` in
`oz-agent-worker`. Add the oz-local integration on
`factory/oz-local-agent-vault` in `warp-server`, with a separate cross-linked PR.
The server PR depends on the worker PR. Keep this spec as their shared contract.
No client, server API, protobuf or database migration changes.

### One opt-in worker setting

Add optional `backend.docker.network` to `internal/config/config.go`, pass it
through `main.go` to `DockerBackendConfig.Network`, and use it as the task
container's `HostConfig.NetworkMode` in `internal/worker/docker.go`.
An omitted value preserves Docker's current default networking. A named network
must already exist; a missing network fails task startup rather than falling
back. This setting does not change sidecar materialization containers.
Document the setting and test its configuration-to-container wiring.

### Runnable example

Add `examples/infisical-agent-vault/` with:

- `compose.yaml`: version-pinned Infisical, PostgreSQL, Redis and CLI proxy.
- `.env.example` and a local ignore rule: variable names and placeholders only.
  Generate deployment encryption/database secrets locally with restricted file
  permissions. Never commit or print generated values.
- `Dockerfile`: a task image based on the worker-compatible Ubuntu task base,
  with the pinned Infisical CLI, curl and the GitHub helper installed. Preserve
  the worker's `/agent/entrypoint.sh` launch contract.
- `github-user`: a fixed-purpose executable that runs curl under
  `infisical agent-vault run --no-skills --no-mcp-servers --telemetry=false --`.
  Session token and proxy address come from environment, not command arguments.
  Request only `GET https://api.github.com/user`, verify TLS with the proxy CA,
  disable curl configuration-file loading, redirects and ambient proxy bypass,
  and return only the account login. Never log headers or upstream error bodies.
- `worker.yaml`: the named task network, inherited scoped session variable,
  proxy address, one concurrent task, and no global `HTTP_PROXY`/`HTTPS_PROXY`.
- `run-worker.py`: a standard-library launcher that authenticates to local
  Infisical, creates a session, starts the configured worker binary, waits for
  it, and revokes the session on normal exit, failure, SIGINT or SIGTERM.
  Pass the worker's real `--config-file` flag, not `--config`.
  Permit oz-local to supply its generated config and local worker arguments.
- `README.md`: exact local setup, task-image selection, worker start, run
  submission, smoke checks, shutdown and troubleshooting commands.

The launcher creates one `20m` session per worker launch, scoped to the example
bundle. Restart the worker for a fresh session; do not refresh silently.
Reusing it across local tasks is an explicit PoC limit, not per-run isolation.
Set agent idle time to zero for the smoke run. Do not configure a standby pool.

Use bounded HTTP calls to Universal Auth login, session creation, and session
revocation. Disable redirects and ambient HTTP proxy handling for these calls.
Accept HTTP only for the loopback local Infisical endpoint. Validate required
response fields without printing response bodies or tokens. If startup fails
after session creation, still revoke. Use a fresh login for cleanup if the
original management access token expired. Revocation failure produces a
secret-free warning and nonzero exit; session TTL is the crash backstop.

### Local credential and network boundary

Publish Infisical management on `127.0.0.1:8088` only. Do not publish PostgreSQL,
Redis or proxy ports to the host. Put storage and management traffic on separate
Compose networks from the named bridge shared by tasks and proxy.
The standalone example defaults to `oz-vault-agent`; oz-local overrides it with
a checkout-scoped name. Only the proxy joins both task and management networks.
Tasks retain ordinary outbound networking for Oz control traffic.

Create an Agent Vault project and a GitHub service restricted to
`api.github.com:443`, method `GET`, and path prefix `/user`, then add it to one bundle.
Infisical does not offer exact-path matching here: a session holder can also
request `/user/...`. The requester explicitly accepted this PoC limitation.
Use a narrowly scoped, disposable GitHub token and Bearer injection. Store it in
Infisical, never in the worker config, task image or shell history.
Set proxy traffic policy to **Service hosts only** without broad exceptions.
Create a Member machine identity granted only this bundle and a separate proxy
token. Standalone setup may use the local dashboard; oz-local automates setup.
Agent verification remains CLI.

The local launcher receives only machine-identity setup values. Compose passes
the proxy token only to the proxy. Strip machine-identity/proxy credentials from
the worker child environment; inherit only the required worker credentials and
scoped session. The task receives the scoped session and proxy address, not
Infisical admin credentials or the GitHub token.

Do not mount the host home directory, Docker socket, proxy data, Infisical data
or operator secret files into task containers. The GitHub helper sets proxy
variables only for its child; worker/agent control traffic stays unchanged.
There is no direct fallback after a proxied request fails.

### Opt-in oz-local setup

The user starts the integrated mode from the warp-server checkout:

```bash
./script/oz-local up --worker-backend docker --with-agent-vault --detach --wait
```

The normal oz-local prerequisites still apply: an initialized local Warp
database, the sibling worker/session-sharing checkouts, Docker Linux containers,
and the normal local server dependencies. Do not reset the Warp database.

- Add `--with-agent-vault` to `script/oz-local`. Reject non-Docker backends,
  unsupported task-image/credential overrides, and missing worker example assets
  before starting services. Ordinary oz-local startup remains unchanged.
- Keep reusable Vault deployment, bootstrap and session code in the worker
  example. The warp-server integration supplies lifecycle and local URLs; do not
  copy the Infisical implementation into a second repository.
- Add an opt-in process-compose resource for the Compose stack and a prerequisite
  for the existing worker resource. Reuse oz-local's status/log/wait/down model.
  The Vault resource must remain supervised after Docker containers start.
- Give the deployment and task network checkout-specific identities. Keep the
  management listener on loopback port 8088. Fail on a conflicting listener or
  initialized instance without matching local ownership state; do not adopt an
  unrelated instance or stop its containers.
- Before detaching, collect the disposable GitHub token through a hidden local
  prompt on first setup. A noninteractive first setup must fail with an actionable
  message rather than hang. Do not put the token in argv or process-compose YAML.
- Keep deployment/bootstrap state in a gitignored, owner-only runtime directory.
  Use mode 0600 and atomic writes for secret files. A pending token file may carry
  the first-run token to the provisioner; delete it after the attempt. Never pass
  that token to worker, agent, or unrelated server processes.
- Bootstrap only the owned local Infisical instance using its release-pinned
  public APIs: local owner, Agent Vault project, HTTP service, bundle, Member
  machine identity/Universal Auth, bundle grant and separate proxy registration.
  Persist only the local control credentials needed to resume that instance.
  Keep the GitHub credential in Infisical after provisioning.
- Restart reuses owned resources instead of creating duplicate projects or
  silently replacing stored credentials. Missing/inconsistent state fails
  clearly; it does not delete data or reuse broad administrator task credentials.
- Build the existing task Dockerfile. Set the generated worker config to the
  checkout's task network, scoped session/proxy environment, `IfNotPresent`,
  one concurrent task and zero completed-task idle time. Do not enable a standby
  pool in this mode. The submitted test must select the built task image.
- Wrap worker startup with the existing session launcher. Preserve oz-local's
  WebSocket, server, RTC and session-sharing settings. Start one 20-minute session
  per worker launch and revoke it before stopping the proxy on ordered shutdown.
  Failures before/after worker startup must not leave an unbounded capability.
- Disable oz-local's ambient `gh auth token` lookup and `GH_TOKEN` injection in
  this mode. Reject explicit task environment entries for raw GitHub,
  management/proxy credentials and reserved Vault session/proxy variables.
  Do not change ordinary oz-local GitHub behavior.
- Print a secret-free, copyable E2E test command after readiness. Supply a local
  test helper if authentication/image selection would otherwise require manual
  plumbing. Use the existing local fixture/API-key and Oz CLI/API paths, scoped to
  the same local team as the worker. Never print or persist the raw Oz API key.
- The E2E test uses an environment with the built image, no repositories and no
  managed GitHub secrets. It targets the configured local worker and calls
  `github-user`. Report run ID, worker claim, terminal state and returned login.
  Do not equate stack readiness or an LLM's unsupported assertion with this proof.
- `down` stops the owned Vault containers after worker revocation. Preserve the
  local Infisical data across restarts. No automatic volume deletion or credential
  rotation command is part of this PoC.
- Document startup, the token prompt, the printed E2E command, shutdown,
  20-minute expiry, missing dependencies, port conflicts and interrupted bootstrap
  in `docs/oz-local/README.md` and the worker example README.

## Decisions and assumptions

- The requester clarified: "basically a branch or set of branches were i can
  provide credentials on my local and then a local oz worker uses those
  credentials". "just let it rip" delegated the proposed defaults.
- The requester later asked for easier setup in warp-server oz-local and replied
  "ok do that" to the opt-in integrated startup proposal and explicit prefix-policy
  question. This accepts `/user/...` access as a documented PoC limit.
- Choose the integrated oz-local mode over a Compose-only convenience wrapper:
  the requester repeatedly asked for the full local-worker E2E path. Reuse
  existing worker example assets instead of maintaining two Vault stacks.
- Assume bootstrap owns a separate local instance. Importing or taking over an
  existing manually configured Infisical instance is outside this setup command.
- Choose a helper-only proxy over whole-agent proxying because the upstream
  proxy cannot carry all Oz control protocols. Direct egress remains possible.
- Choose one bounded session per worker launch over modifying worker task
  lifecycle for automatic per-task sessions. The smaller example proves local
  credential brokering without a new Infisical-specific worker subsystem.
- Assume Linux containers under Docker Engine or Docker Desktop. No native
  macOS/Windows worker change or platform-specific proof is promised.
- Assume an existing Oz account/team-scoped worker key and access to the normal
  agent images. The example changes GitHub tool authentication, not Oz model or
  worker authentication. Submit the smoke task without a repository or managed
  GitHub secret so assigned credentials cannot explain its success.
- Command-line verification only. No computer use, new UI or design exploration.

## Out of scope

Production hardening, general egress enforcement, protection against a malicious
host/Docker administrator, per-task sessions, transparent support for every
tool/SDK/MCP server, Git cloning, credential rotation, and remote deployment.
The session itself is a capability that a compromised task can misuse until
revocation or expiry. This PoC does not claim to remove all task credentials.

## Testing and validation

- `go test ./...`: cover default networking,
  named-network forwarding and missing-network failure; preserve current tests.
- `python3 -m unittest discover -s examples/infisical-agent-vault -p 'test_*.py'`:
  use controlled HTTP/process fixtures for session creation, failed worker
  startup, shutdown/revocation, and errors containing sentinel secrets. Assert
  machine/proxy secrets do not reach the worker child or printed diagnostics.
  Include a regression that checks the real worker argument contract so a fake
  worker cannot hide a misspelled config flag.
- `docker compose --env-file <local-env> -f examples/infisical-agent-vault/compose.yaml config --quiet`
  and a task-image build validate the shipped local deployment inputs.
- Run `go vet ./...`, `go build ./...` and `golangci-lint run`, then format changed
  Go files once with `gofmt -s -w`.
- Provide a CLI smoke command using the real pinned proxy and a disposable
  authenticated test upstream. Prove header injection, method/path denial and
  rejection after revocation within the configured proxy poll interval.
  The upstream must never echo the received credential.
- With operator-supplied GitHub/Oz credentials, submit a real task to the local
  worker using the example task image. Its prompt calls `github-user` and
  returns the account login. Record run ID, worker claim, terminal state and
  authenticated result, not credentials. Stop the worker and confirm revocation.
- Never call a fixture-backed check a live Oz/GitHub verification. If credentials
  or required infrastructure are unavailable, report the live smoke as pending
  and provide the exact operator command; do not claim end-to-end success.
- Extend the existing oz-local shell tests for flag parsing, unchanged disabled
  mode, Docker-only rejection, dependency ordering, generated network/session
  config, local URL forwarding, disabled ambient GitHub injection and redacted
  diagnostics. Run the existing oz-local test entry point documented by the repo.
- Add fixture-backed bootstrap tests for first setup, restart reuse, partial
  failure and ownership refusal. Assert that secret files are protected and
  credentials do not enter output, worker argv, generated YAML or task environment.
- Run `bash -n` and the repository's applicable ShellCheck checks on changed shell
  scripts. Exercise the real worker parser with the launcher's generated flags.
- On a prepared local stack, run the new startup command, inspect resource
  readiness, execute the printed E2E command, then run `down` and verify revocation
  and persistent data. Record any missing local infrastructure or credentials;
  the conditional live-proof rule above still applies.
