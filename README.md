# warp-agent-worker

Self-hosted worker for Warp ambient agents.

## Overview

`warp-agent-worker` is a daemon that connects to warp-server via WebSocket to receive and execute ambient agent tasks on self-hosted infrastructure.

## Requirements

- Docker daemon running on localhost
- Service account API key with team scope
- Network egress to warp-server

## Installation

```bash
go install github.com/warpdotdev/warp-agent-worker@latest
```

Or build from source:

```bash
git clone https://github.com/warpdotdev/warp-agent-worker.git
cd warp-agent-worker
go build -o warp-agent-worker
```

## Usage

### Basic Usage

```bash
warp-agent-worker --api-key "wk-abc123" --worker-id "my-worker"
```

### Using Environment Variable

```bash
export WARP_API_KEY="wk-abc123"
warp-agent-worker --worker-id "my-worker"
```

## License

Copyright © 2026 Warp
