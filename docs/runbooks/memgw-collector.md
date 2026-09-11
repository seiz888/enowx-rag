# Runbook — collector recovery, quarantine and the spool key

Applies to the collector (`enowx-rag memgw collector`) on Windows and on Linux.
Sections 1-7 describe the Windows collector; section 8 is the Linux one, which
differs in exactly two places -- the local transport and the key store -- and is
identical everywhere else. It is the
process that stands between an agent host's hook and the gateway: the hook hands
it an event, it writes that event to an encrypted spool on disk, and it forwards
from the spool. A hook is told "durable" only once the row is committed to disk.

Everything in sections 1-7 has been exercised against a real collector process,
a real named pipe and a real gateway on the test machine. Section 8 has been
exercised as a real process in a disposable Linux container. None of it has run
against production, which has no collector installed on any host.

---

## 1. What is where

| Thing | Default | Notes |
|---|---|---|
| Spool | `<dir>\spool.db` | SQLite, WAL mode. `--dir` defaults to `D:\memgw-collector`. |
| Key | `<dir>\spool.key` | The spool key, wrapped by DPAPI in **user** scope. |
| Credential | `--token-file` | Read from a file, never from a flag or the environment. |
| Pipe | `--pipe` | Named pipe the adapters connect to. |

`--dir` is on `D:` by design: the spool is the one file on this machine that has
to survive `C:` filling up.

## 2. Reading the queue

```
enowx-rag memgw collector stats --dir <dir> --token-file <file>
enowx-rag memgw collector held  --dir <dir> --token-file <file>
```

`stats` answers "is anything stuck": `pending`, `sending`, `sent`,
`quarantined`, `dead`, `acked_seq`. `held` lists the quarantined and dead rows
with their `fail_class` and `fail_reason` — **metadata only; the payload is
never printed**.

Nothing here needs the gateway to be up.

## 3. Retryable versus held

The collector retries what can succeed later and holds what cannot. The split is
made in `forwarder.go` from the gateway's *answer*, not from a guess:

- **Retryable** — a transport failure, `503 unavailable`, any `5xx`, `429
  rate_limited`, or a receipt that could not be read. The row stays `pending`
  and is retried with backoff, forever if necessary. This is the normal shape of
  a laptop that closed its lid.
- **Retryable, but logged as an error** — `401 unauthenticated`. The credential
  is wrong, expired or revoked. Retrying is correct, because an operator can fix
  it and the queue will then drain on its own; but nothing moves until somebody
  acts, so it is logged at error level rather than left to look like ordinary
  backoff. If `stats` shows `pending` climbing and the log repeats *collector
  credential refused by the gateway*, go to §7.
- **Non-retryable, quarantined** — any other `4xx`. The gateway *answered*, and
  the answer says this event will never be accepted as it stands:
  `scope_denied`, `policy_rejected`, `writer_epoch_fenced`, `cas_conflict`,
  `stale_revision`, `idempotency_payload_mismatch`, `payload_too_large`,
  `malformed`, `quarantined`. The row moves to `quarantined` immediately and is
  not retried.
- **Unreadable** — the payload fails to authenticate against the spool key
  (`ErrKeyMismatch`). Quarantined without ever being handed to the forwarder.

A quarantined row is a decision waiting for a person. It is still on disk and
still encrypted.

## 4. Deciding a held row

```
enowx-rag memgw collector release --seq N   # back in the queue, attempts reset
enowx-rag memgw collector discard --seq N   # stop retrying, keep the row and its key
enowx-rag memgw collector purge   --seq N   # delete the row, freeing its idempotency key
```

Choosing between them:

- **release** when the *server* was wrong and has been fixed — a grant that was
  revoked and has been restored, a principal that was suspended, a project that
  was archived. The event's own bytes are still correct, so put them back.
  Verified: revoking both of a writer's grants quarantined the next event with
  `scope_denied` after one attempt; restoring the grants and releasing it
  delivered that exact event.
- **discard** when the event should never be delivered but you want the record
  of it. The row becomes `dead` with `fail_class operator_discarded`. Its
  idempotency key stays taken, so the host cannot re-queue the same event by
  accident.
- **purge** when the *event* was wrong and the host will produce a corrected one
  that derives the same idempotency key. While the held row exists the
  correction deduplicates against it and can never be queued. Purging frees the
  key. This is the only operation that destroys an event.

If a host fires a hook whose key collides with a held row, the collector answers
class `held` and the adapter exits non-zero:

```
{"accepted":false,"via":"collector","class":"held",
 "message":"an earlier event with this idempotency key is held and will not be
            delivered; an operator must release or purge it"}
```

That is not a duplicate-and-fine; it means nothing will reach the ledger until
the held row is decided.

## 5. Recovery after a crash or a restart

Do nothing special. On start the collector:

- opens the spool and reclaims rows left in `sending` by a process that died
  (they become claimable again);
- keeps the ack cursor, so nothing already settled is re-sent;
- re-derives every event id from its idempotency key, so a hook that fires again
  after the crash produces the same id and the ledger holds one event.

A duplicate reaching the gateway is safe by design: the receipt comes back
`duplicate` with the original `seq`, and the collector records that verbatim
rather than collapsing it into "committed".

If the spool file is gone, events that were in it are gone. That is the one
loss the design accepts, and it is why the spool lives on `D:` and is the thing
a host backup must include.

## 6. The spool key

- Generated once, on first use, 32 bytes from the system CSPRNG.
- Stored wrapped by **DPAPI, user scope**, with a fixed entropy string mixed in.
  The wrapped blob is bound to this Windows account on this machine: copying the
  spool directory to another machine, or reading it as another local user,
  produces a file that will not unwrap.
- There is no export path, no import path, and no way to supply a key from the
  environment. A key an operator can print is a key that ends up in a terminal
  scrollback.

**If the key will not unwrap** the message says so explicitly and the collector
refuses to start. The usual cause is a spool directory copied from another
account or machine — *not* corruption. Do not delete it reflexively: under the
right account the queued events are still perfectly good. Options, in order:

1. Run the collector as the account that created the spool.
2. If that account is gone, the queued events cannot be recovered. Move the
   directory aside (do not delete it until the incident is closed), let the
   collector create a fresh spool and key, and expect the hosts to re-emit. The
   idempotency keys are derived from meaning, so re-emitted events deduplicate
   at the gateway rather than doubling the ledger.

**Rotating the key** is the same procedure as (2) and drains first: stop
accepting new events, wait for `pending` and `sending` to reach zero, stop the
collector, move `spool.key` and `spool.db` aside together — they are a pair —
and restart.

## 7. The credential

The collector reads its memgw credential from `--token-file` and nothing else. A
command line is readable by every process on the machine; an environment
variable is readable by anything that can open the process. Keep the file at
`0600`-equivalent ACLs, owned by the account the collector runs as.

To rotate: issue a new credential (`memgw principal issue <principal-id>`,
redirecting stdout **straight into the file** so the secret is never echoed),
restart the collector, then revoke the old credential
(`memgw principal revoke-credential <credential-id>`). Revoking a *principal*
revokes every way of being it at once, immediately.

If a secret is ever printed anywhere, treat it as compromised, revoke that
credential id, and issue a replacement. It has happened in this project and the
recovery took under a minute; hiding it would have left a live credential in a
transcript.

---

## 8. Linux

Same binary, same spool format, same protocol, same commands. Two things differ,
and both are the same argument the Windows collector makes: the endpoint carries
an access control the operating system enforces, and the key is held by the
operating system rather than by a file the process can print.

| Thing | Default | Notes |
|---|---|---|
| Spool | `/var/lib/memgw-collector/spool.db` | `StateDirectory=` in the unit produces it with the right owner. |
| Socket | `/run/memgw/collector.sock` | `--socket`, not `--pipe`. Mode **0600**, created that way by umask, then verified. |
| Key | `$CREDENTIALS_DIRECTORY/memgw-collector-spool-key` | Handed to the unit by systemd. **Never written to disk by the collector.** |
| Credential | `--token-file` | Unchanged; `0600`, owned by the service account. |

**The key is loaded, never created.** On Windows the collector can generate its
own key because DPAPI will wrap it; Linux has no equivalent the process can call
for itself, so a key it created would have to be written beside the ciphertext
it protects. Instead systemd decrypts a credential -- sealed to the TPM, or to
the root-only host key in `/var/lib/systemd/credential.secret` -- and drops it
into a ramfs mounted for this unit alone.

If the credential is absent, the collector **refuses to start**. There is no
plaintext fallback and no flag that produces one. A credential readable beyond
its owner is refused for the same reason: a key that has been readable cannot be
un-read.

Setting it up:

```bash
head -c 32 /dev/urandom | xxd -p -c 64 |
  systemd-creds encrypt --name=memgw-collector-spool-key - \
    /etc/memgw/memgw-collector-spool-key.cred
```

```ini
[Service]
Type=simple
LoadCredentialEncrypted=memgw-collector-spool-key:/etc/memgw/memgw-collector-spool-key.cred
StateDirectory=memgw-collector
RuntimeDirectory=memgw
ExecStart=/usr/local/bin/enowx-rag memgw collector run \
  --dir /var/lib/memgw-collector \
  --socket /run/memgw/collector.sock \
  --gateway http://127.0.0.1:7777 \
  --token-file /etc/memgw/token
```

The adapter is pointed at it with `"collector": {"endpoint": "/run/memgw/collector.sock"}`.
The older `"pipe"` key is still read, so a Windows configuration keeps working;
`endpoint` wins if both are set.

**Rotating the key** is the Windows procedure (section 6) with the credential
step in place of the file: drain to `pending`/`sending` zero, stop the unit,
`systemd-creds encrypt` a new key over the old `.cred`, move `spool.db` aside,
start. The spool and the key are a pair; a new key with an old spool makes every
queued row unreadable, which the collector reports as a key mismatch rather than
as corruption.

**What has been proven, and where.** A disposable `debian:12-slim` container:
refusal with no credential and no key file written, refusal on a 0644
credential, a 0600 socket, a second collector refused on the same socket, two
events submitted through the real `memgw adapter` and reported durable, nothing
of the payload readable in the spool file, the queue intact across `kill -9`,
the same hook reported as a duplicate, and no token in either log. **The Hermes
host has not been touched and has no collector installed.** Those are different
claims and this runbook does not blur them.
