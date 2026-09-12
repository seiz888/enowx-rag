#!/usr/bin/env python3
"""Fetch a file from the Multi-Brain vault into a local path, streaming it.

The counterpart to vault-put.py, for the same reason: a recovery private key
must not travel through a tool argument or a transcript, so it is read from HTTP
straight into a file with mode 0600 and never echoed.

  vault-get.py <vault-path> <local-path>
"""

import base64
import json
import os
import sys
import urllib.request

CONFIG = os.path.expandvars(r"%USERPROFILE%\.claude.json")
SERVER = "obsidian-webdav"


def credentials():
    with open(CONFIG, encoding="utf-8") as fh:
        env = json.load(fh)["mcpServers"][SERVER]["env"]
    base = env["WEBDAV_ROOT_URL"].rstrip("/")
    token = base64.b64encode(
        f'{env["WEBDAV_USERNAME"]}:{env["WEBDAV_PASSWORD"]}'.encode()).decode()
    return base, token


def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: vault-get.py <vault-path> <local-path>")
    vault_path, local_path = sys.argv[1], sys.argv[2]
    if not vault_path.startswith("/"):
        vault_path = "/multibrain/" + vault_path

    base, token = credentials()
    req = urllib.request.Request(base + vault_path)
    req.add_header("Authorization", "Basic " + token)
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = resp.read()

    # Create with restrictive permissions where the platform supports it, then
    # write. On Windows os.open's mode is largely advisory; the file is removed
    # by the caller immediately after use.
    fd = os.open(local_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "wb") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())

    # Silent by default: this is called from PowerShell under
    # $ErrorActionPreference='Stop', where a native command's stderr becomes a
    # terminating error.
    if os.environ.get("VAULT_VERBOSE"):
        print("fetched %s -> %s (%d bytes)" % (vault_path, local_path, len(data)),
              file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
