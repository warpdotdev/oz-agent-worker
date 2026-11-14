# Local worker credential access with Infisical Agent Vault

## Summary

Build an opt-in PoC that Daniel can check out, configure with credentials on his
machine, and exercise through a real local Docker-backed Oz worker. Infisical
holds a GitHub token. A tool inside the task uses a scoped session to call
`GET https://api.github.com/user` without receiving that token.

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

## Proposed changes

### One branch, one opt-in worker setting

Keep the spec and implementation on `factory/infisical-local-worker-poc` in
`oz-agent-worker`. No server, client, protobuf or database migration changes.

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
Compose networks from `oz-vault-agent`, the named bridge shared by tasks and
proxy. Only the proxy joins both the task and Infisical management networks.
Tasks retain ordinary outbound networking for Oz control traffic.

The operator creates an Agent Vault project, a GitHub service restricted to
`api.github.com:443`, method `GET`, and path `/user`, then adds it to one bundle.
Use a narrowly scoped, disposable GitHub token and Bearer injection. Store it in
Infisical, never in the worker config, task image or shell history.
Set proxy traffic policy to **Service hosts only** without broad exceptions.
Create a Member machine identity granted only this bundle and a separate proxy
token. Human setup may use the local dashboard; agent verification remains CLI.

The local launcher receives only machine-identity setup values. Compose passes
the proxy token only to the proxy. Strip machine-identity/proxy credentials from
the worker child environment; inherit only the required worker credentials and
scoped session. The task receives the scoped session and proxy address, not
Infisical admin credentials or the GitHub token.

Do not mount the host home directory, Docker socket, proxy data, Infisical data
or operator secret files into task containers. The GitHub helper sets proxy
variables only for its child; worker/agent control traffic stays unchanged.
There is no direct fallback after a proxied request fails.

## Decisions and assumptions

- The requester clarified: "basically a branch or set of branches were i can
  provide credentials on my local and then a local oz worker uses those
  credentials". "just let it rip" delegated the proposed defaults.
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
