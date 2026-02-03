# warp-agent-worker

Self-hosted worker for Warp ambient agents.

## Overview

`warp-agent-worker` is a daemon that connects to warp-server via WebSocket to receive and execute ambient agent tasks on self-hosted infrastructure.

## Requirements

- Docker daemon (accessible via socket or TCP)
- Service account API key with team scope
- Network egress to warp-server

## Usage

### Docker (Recommended)

The worker needs access to the Docker daemon to spawn task containers. Mount the host's Docker socket into the container:

```bash
docker run -v /var/run/docker.sock:/var/run/docker.sock \
  -e WARP_API_KEY="wk-abc123" \
  warpdotdev/warp-agent-worker --worker-id "my-worker"
```

> **Note:** Mounting the Docker socket gives the container access to the host's Docker daemon. This is required for the worker to create and manage task containers.

### Go Install

```bash
go install github.com/warpdotdev/warp-agent-worker@latest
warp-agent-worker --api-key "wk-abc123" --worker-id "my-worker"
```

### Build from Source

```bash
git clone https://github.com/warpdotdev/warp-agent-worker.git
cd warp-agent-worker
go build -o warp-agent-worker
./warp-agent-worker --api-key "wk-abc123" --worker-id "my-worker"
```

## Docker Connectivity

The worker automatically discovers the Docker daemon using standard Docker client mechanisms, in this order:

1. **`DOCKER_HOST`** environment variable (e.g., `unix:///var/run/docker.sock`, `tcp://localhost:2375`)
2. **Default socket location** (`/var/run/docker.sock` on Linux, `~/.docker/run/docker.sock` for rootless)
3. **Docker context** via `DOCKER_CONTEXT` environment variable
4. **Config file** (`~/.docker/config.json`) for context settings

Additional supported environment variables:
- `DOCKER_API_VERSION` - Specify Docker API version
- `DOCKER_CERT_PATH` - Path to TLS certificates
- `DOCKER_TLS_VERIFY` - Enable TLS verification

### Example: Remote Docker Daemon

```bash
export DOCKER_HOST="tcp://remote-host:2376"
export DOCKER_TLS_VERIFY=1
export DOCKER_CERT_PATH="/path/to/certs"
warp-agent-worker --api-key "wk-abc123" --worker-id "my-worker"
```

## License

Copyright © 2026 Warp
