#!/usr/bin/env python3
"""Generate local deployment secrets without overwriting an existing instance."""

import base64
import os
from pathlib import Path
import secrets


if __name__ == "__main__":
    path = Path(__file__).with_name(".env")
    content = (
        f"POSTGRES_PASSWORD={secrets.token_hex(24)}\n"
        f"ENCRYPTION_KEY={secrets.token_hex(16)}\n"
        f"AUTH_SECRET={base64.b64encode(secrets.token_bytes(32)).decode()}\n"
    )
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as stream:
            stream.write(content)
    except FileExistsError:
        raise SystemExit(".env already exists; keeping the existing deployment keys")
    print("Created restricted local .env; no credential values printed")
