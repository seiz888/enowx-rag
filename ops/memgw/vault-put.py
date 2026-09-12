#!/usr/bin/env python3
"""Put a file into the Multi-Brain vault over WebDAV, streaming the bytes.

Why this is not done through the webdav MCP tool
------------------------------------------------
The MCP tool takes the file content as a tool argument, which puts the bytes in
the harness transcript. For an ordinary note that is fine. For a recovery *key*
it is not: a key that has been echoed into a log is a key that has leaked. This
reads the payload from a file on disk and streams it over HTTP, so the only
thing that appears anywhere is a path.

Credentials come from the agent's MCP config (the same ones the MCP server
uses) and are never printed.

  put_file.py <local-path> <vault-path>          # PUT
  put_file.py --mkcol <vault-path>               # MKCOL (create directory)
  put_file.py --list <vault-path>                # PROPFIND depth 1
"""

import argparse
import base64
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

CONFIG = os.path.expandvars(r"%USERPROFILE%\.claude.json")
SERVER = "obsidian-webdav"


def credentials():
    with open(CONFIG, encoding="utf-8") as fh:
        cfg = json.load(fh)
    env = cfg["mcpServers"][SERVER]["env"]
    base = env["WEBDAV_ROOT_URL"].rstrip("/")
    user = env["WEBDAV_USERNAME"]
    pw = env["WEBDAV_PASSWORD"]
    token = base64.b64encode(f"{user}:{pw}".encode()).decode()
    return base, token


def open_auth(req, token):
    req.add_header("Authorization", "Basic " + token)
    return req


def do_mkcol(base, token, path):
    req = urllib.request.Request(base + path, method="MKCOL")
    open_auth(req, token)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            print("MKCOL", path, resp.status)
    except urllib.error.HTTPError as e:
        if e.code == 405:  # already exists
            print("MKCOL", path, "already exists")
        else:
            raise


def do_put(base, token, local, path):
    size = os.path.getsize(local)
    with open(local, "rb") as fh:
        data = fh.read()
    req = urllib.request.Request(base + path, data=data, method="PUT")
    open_auth(req, token)
    req.add_header("Content-Type", "application/octet-stream")
    req.add_header("Content-Length", str(len(data)))
    with urllib.request.urlopen(req, timeout=120) as resp:
        print("PUT", path, resp.status, "%d bytes" % size)


def do_list(base, token, path):
    req = urllib.request.Request(base + path, method="PROPFIND")
    open_auth(req, token)
    req.add_header("Depth", "1")
    with urllib.request.urlopen(req, timeout=60) as resp:
        body = resp.read().decode("utf-8", "replace")
    import re
    for href in sorted(set(re.findall(r"<[^>]*href>([^<]+)<", body))):
        print(" ", urllib.parse.unquote(href))


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("local", nargs="?")
    ap.add_argument("vault", nargs="?")
    ap.add_argument("--mkcol")
    ap.add_argument("--list")
    args = ap.parse_args(argv)

    base, token = credentials()
    if args.mkcol:
        do_mkcol(base, token, args.mkcol)
    elif args.list:
        do_list(base, token, args.list)
    elif args.local and args.vault:
        do_put(base, token, args.local, args.vault)
    else:
        ap.error("give <local> <vault>, --mkcol PATH, or --list PATH")
    return 0


if __name__ == "__main__":
    sys.exit(main())
