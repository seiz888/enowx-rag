# Runbook — rotating `RAG_ADMIN_TOKEN`

> **EXECUTED — 2026-09-11.** Rotation was performed across all reachable
> consumers. The token was generated on the host (`openssl rand -hex 32`) and
> never printed; the nginx cleartext copies were replaced by a `0600`
> root-readable include. Verified: old value `401`, new value `200`, on both
> the loopback and the public endpoint, and the nginx-injected dashboard path.
> The steps below remain the procedure; §2's optional structural change (move
> the token out of the inline config) is now the state that shipped.

`RAG_ADMIN_TOKEN` is treated as **compromised**: it sits in cleartext in two
nginx site configs and it was rendered into a session transcript on 2026-09-09.

**No token value — old or new — appears in this file, and none may be pasted
into it.** Every command below is written so that no secret reaches a terminal,
a log, a process listing or a transcript. Where a secret is unavoidable the
operator types it into a `read -rs` prompt; nothing else ever holds it.

---

## 1. Consumers — names and paths only

| # | Consumer | Where the value lives | Confirmed |
|---|---|---|---|
| 1 | enowx-rag server | `RAG_ADMIN_TOKEN` in `/opt/enowx-rag/.env` | file exists; startup log confirms a token is in effect (never read) |
| 2 | nginx public vhost | `map $http_authorization $rag_upstream_auth` default in `/etc/nginx/sites-enabled/rag.conf` | yes, cleartext |
| 3 | nginx local dashboard | `proxy_set_header Authorization` in `/etc/nginx/sites-enabled/rag-dashboard.conf` (`127.0.0.1:7778`) | yes, cleartext |
| 4 | Claude Code | `mcpServers["enowx-rag"].headers.Authorization` in `~/.claude.json` | header present |
| 5 | Claude Code hooks | `rag-recall.py`, `rag-session-log.py`, `guard.py` read consumer #4's file | yes |
| 6 | Codex | `[mcp_servers.enowx-rag] bearer_token_env_var = "ENOWX_RAG_TOKEN"` — an env var, not a file | yes |
| 7 | OpenCode | `mcp["enowx-rag"].headers.Authorization` in `~/.config/opencode/opencode.json` | yes |
| 8 | OpenCode plugin | `enowx-rag-hooks.ts` falls back to `~/.claude.json` when #7 is absent | yes |
| 9 | Droid | `mcpServers["enowx-rag"].headers.Authorization` in `~/.factory/mcp.json` | yes |
| 10 | Windows `~/.enowx-rag/config.yaml` | may carry `admin_token` | file exists; **not read** — the operator must check |
| 11 | Host `/opt/enowx-rag/.enowx-rag/config.yaml` | `admin_token` key | exists, zero `admin_token` lines: the server takes the token from the environment only |
| 12 | Encrypted app bundles in `/opt/rag-backup/app/` | contain `.env` | **the old token survives in backups; rotation does not erase it** |
| 13 | nginx basic auth `/etc/nginx/.htpasswd-rag` | a *different* credential | present — rotate on its own schedule |

Not consumers: OMP/Pi and Hermes have no enowx-rag MCP entry.

## 2. Update order, and why it is this order

1. Freeze: announce a short window; no agent writes to `memory` during it.
2. Generate the new value **on the host, off-transcript**, straight into the
   file being edited. Never through an agent session, never echoed.
3. **Server first** — `/opt/enowx-rag/.env` (mode 0600, owner `enowxrag`),
   keeping a timestamped copy the way the existing `.env.pre-*` files do.
4. **nginx second**, in the same window — the `map` default in `rag.conf` and
   the header in `rag-dashboard.conf`.
5. Validate nginx, then restart the server, then reload nginx (§4).
6. Consumers 4–10, one at a time, smoke-testing each.

Server before nginx: the reverse order has nginx forwarding a new token to a
server that still expects the old one, and the failure looks like an outage.

**Done on 2026-09-11.** The token no longer sits inline in either vhost. It
lives in `/etc/nginx/.rag-admin-token` (mode `0600`, owner `root:root`) as a
`map $host $rag_admin_token { default "..."; }` fragment, and each vhost starts
with `include /etc/nginx/.rag-admin-token;` and sends
`proxy_set_header Authorization "Bearer $rag_admin_token"`. To rotate again,
rewrite that one file and `nginx -t && systemctl reload nginx`; the vhosts do
not change.

## 3. Validation that needs no token at all

Run these before touching anything, and again after. None of them authenticates,
so none of them can leak a secret:

```
systemctl is-active enowx-rag
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7777/api/stats   # expect 401
curl -s -o /dev/null -w '%{http_code}\n' https://rag.seiz.cloud/api/stats  # expect 401
ss -ltnp | grep -E '7777|7778'
```

A `401` here is the correct answer and is the cheapest proof that the service is
up and that auth is not failing open. A `200` without a token is an incident, not
a convenience.

## 4. nginx validation and reload

```
nginx -t                    # never reload on a failure
systemctl restart enowx-rag # the env file is read at start, so this is required
systemctl reload nginx
```

`nginx -t` prints the config path and a syntax verdict; it does not print
directive values. Do not `cat` the site configs to "check the change" — they
hold the token in cleartext. `grep -c` the directive name instead:

```
grep -c 'rag_upstream_auth' /etc/nginx/sites-enabled/rag.conf
```

## 5. Positive test — the operator types the secret, nothing stores it

Interactive, silent read; the value never appears on a command line (and so
never in `ps`), never in shell history, and never in the output:

```bash
read -rs -p 'new token: ' T; echo
for p in /api/version /api/stats; do
  printf '%s %s\n' "$p" \
    "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $T" \
       http://127.0.0.1:7777$p)"
done
unset T
```

Expect `200` twice. `/api/version` is the cheapest authenticated probe and
exists only once the build-identity change is deployed; until then use
`/api/stats` alone.

Also check MCP: `initialize` against `/mcp` with the same header should return
`200` and a `protocolVersion`.

**Never** put the token in a variable that is exported, in a `curl -H` written
literally into a script, in `~/.netrc`, or in any file this runbook does not
name. If a shell has `HISTFILE` unset or `set +o history` is in force, say so;
otherwise `read -rs` is what keeps it out of history.

## 6. Negative test — the old token must be dead

Same shape, with the retired value:

```bash
read -rs -p 'old token: ' T; echo
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $T" \
  http://127.0.0.1:7777/api/stats
unset T
```

Expect `401`. Then confirm no-token is also `401` (§3). A `200` from the old
value means a consumer or a cache is still presenting it and the rotation has
**not** taken effect — treat the old token as live until this returns 401.

## 7. Rollback — bounded, and it restores a compromised secret

If a consumer cannot be updated inside the window, restore the timestamped
`.env` and nginx copies and reload.

> **Rollback puts the compromised token back in service.** It buys hours, not
> days. Reschedule the rotation immediately, and record the window during which
> the old value was live again.

There is no partial rollback: the server and both nginx configs move together or
the system authenticates inconsistently.

## 8. Post-rotation sweep — find cleartext without printing it

Sweep for either value in the places it is known to have leaked. Every command
below prints **file names or counts only**, never the matching line, and passes
the pattern on stdin so it never appears in a process listing:

```bash
read -rs -p 'value to sweep for: ' T; echo
printf '%s\n' "$T" | grep -rlFf - /opt/enowx-rag/ /etc/nginx/ 2>/dev/null
printf '%s\n' "$T" | grep -rlFf - ~/.claude.json ~/.factory/mcp.json 2>/dev/null
journalctl -u enowx-rag --since '-30d' | { printf '%s\n' "$T" | grep -cFf - ; }
printf '%s\n' "$T" | grep -rlFf - /opt/rag-backup/*.log 2>/dev/null
unset T
```

Expected: zero hits for the **old** value everywhere except consumer #12, and
hits for the **new** value only in `/opt/enowx-rag/.env` and the two nginx
configs.

Sweep the repository too — `D:\PROJECTS\enowx-rag`, `docs/`, `CHANGELOG.md` and
this plan set. Expected: zero, for both values. Use `grep -rlF` there as well;
never `grep` without `-l` or `-c` when the pattern is a secret, because a match
prints the surrounding line into your transcript.

## 9. What rotation does not fix

- **Consumer #12.** Old encrypted bundles in `/opt/rag-backup/app/` keep the old
  token. They are GPG-encrypted and age out at 7 copies by retention. Decide
  explicitly: purge early, or let retention do it. Purging destroys recovery
  material.
- **The shape of the problem.** One shared bearer means rotation is
  all-or-nothing across every host. The memgw principal model — one credential
  per host, scoped, individually revocable — is the actual fix; rotation is
  containment until it is in service. See
  `docs/architecture/shared-memory-gateway.md` §4.
