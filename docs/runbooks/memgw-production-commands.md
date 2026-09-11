# Production command sheet — prepared, not executed

> **Nothing in this file has been run.** Every command below is written out so
> that an operator with the matching approval can execute it deliberately rather
> than improvise it at 3am. None of them has been executed against production by
> the work that wrote them, and none may be until the corresponding block of
> [`../plans/production-approval-form.md`](../plans/production-approval-form.md)
> is filled in.

## How to read a section

Every section has the same five parts, in this order:

| Part | Means |
|---|---|
| **Approval** | which block of the approval form must be signed first |
| **Precondition** | what must already be true; check it, do not assume it |
| **Command** | the literal command, with `<PLACEHOLDERS>` in angle brackets |
| **Expected** | what success looks like — if you see something else, stop |
| **Rollback** | how to undo it, or the explicit statement that you cannot |
| **Reversible** | `yes`, `partly`, or `NO` |

Placeholders are always `<UPPER_CASE_IN_ANGLE_BRACKETS>`. **A command with a
placeholder still in it must not be run.** No placeholder anywhere in this file
stands for a secret value typed on a command line: secrets are read from files
or from an interactive prompt, never from a flag and never from an environment
variable, because a command line is readable by every process on the machine and
a shell history outlives the session.

Sections are ordered by dependency. M0 comes first because it blocks M2.

---

## M0. The production migration path — implemented, proven on a disposable target

**Approval**: none of the blocks. This is a repository change, reviewed and
committed on its own.
**Reversible**: yes (revert the commit).
**Status**: the code exists and was exercised end to end against a disposable
PostgreSQL instance that is not production (`ledger_alpha` on a throwaway
container, TLS with a private CA, a second application's schema beside the
managed one). It has **never** been pointed at production, and this section does
not authorise pointing it there.

What was added, and what it does not change:

- `pgstore.VerifyNonProduction` is **unchanged** and still governs development,
  test and the test harness. Nothing was relaxed to make production fit.
- `pgstore.VerifyProduction` is a separate path, reached only when `MEMGW_ENV`
  is `production` **and** the target assertion below is present. It keeps the
  standby, `synchronous_commit` and foreign-object checks; it replaces the
  name/loopback checks with a positive assertion of the target; and it adds a
  transport check.
- Ownership and foreign-object checks look at the **managed schema only**.
  Another application's tables in the same database are neither adopted nor
  touched: they are not the gateway's business, and refusing because they exist
  would be a reason to grant the runner more scope, not less.
- Authorisation is per purpose. `MEMGW_PRODUCTION_ALLOW` names
  `schema_migration`, `history_import` and `serve` separately, so approving a
  schema migration does not approve a history import or a backfill.

The assertion, all of which must match the live connection:

```
MEMGW_ENV=production
MEMGW_PRODUCTION_DATABASE    must equal current_database()
MEMGW_PRODUCTION_SCHEMA      must equal MEMGW_SCHEMA
MEMGW_PRODUCTION_ROLE        must equal current_user
MEMGW_PRODUCTION_DEPLOYMENT  must equal the identity recorded in the schema
MEMGW_PRODUCTION_ALLOW       schema_migration | history_import | serve
```

`MEMGW_ENV=production` on its own selects nothing and refuses. The transport
must be a local unix socket or TLS with `sslmode=verify-ca`/`verify-full`;
`require` is refused, because it encrypts without proving who answered.

An apply additionally needs a plan file that was reviewed
(`memgw plan --out`), and refuses if the target, the migration files or the
applied history moved after the review — one digest covers all four. Two runners
cannot interleave: the second gets `ErrLockHeld` and exits 3 rather than
queueing behind a run that might be hung. Every refusal of the form "this is not
the target you said it was" exits **3**.

There are still no down migrations and there is no automatic restore. See §M2
for what recovery actually means.

### What the old text said, and why it is gone

The paragraph below described the state before that change and is kept because
the reasoning still applies to anyone tempted to work around the guard.

`pgstore.VerifyNonProduction` is called by the migration runner
(`pkg/memgw/migrations/runner.go`), by `memgw plan` and by
`pkg/memgw/migrate/run.go`, which is what `memgw history apply` uses. It refuses
unless **all** of the following hold:

1. `MEMGW_ENV` is `development` or `test` — `production` is refused by name;
2. the target is not a standby in recovery;
3. the database name matches `^(memgw_|test_|dev_)` or `(_test|_dev|_devdb|_testdb)$`;
4. the server is loopback, checked from both the DSN's host and
   `inet_server_addr()`;
5. `synchronous_commit` is not `off`;
6. the database holds no tables outside the gateway's own schema.

Production would fail at least (1), (3) and (6). So `memgw up` and
`memgw history apply` **cannot be pointed at production as the binary is built
today**, and no environment variable, flag or maintenance window changes that.

Do not work around this by renaming the production database, by declaring
`MEMGW_ENV=test` against it, or by deleting a check. Those are the three
workarounds the guard was written to stop, and each of them makes the guard
useless for the case it exists for — a DSN pointed at the wrong host.

That change has landed, in the shape listed at the top of this section: an
explicit production mode nobody enters by accident, the standby /
`synchronous_commit` / foreign-object checks retained, the name and loopback
checks replaced by a positive assertion of the target, and tests that the mode
refuses a target it was not named for.

§M2 below is therefore executable **as code**. It remains unauthorised as an
action: it still needs the cutover approval, a restored-and-reconciled backup,
and the maintenance window.

---

## M1. Commit the artifact identity, then deploy it

**Approval**: cutover block, plus the user's explicit approval to commit (§10 of
the gate). Commit grouping is
[`../plans/memgw-commit-plan.md`](../plans/memgw-commit-plan.md).
**Precondition**: `go build ./... && go vet ./... && go test ./...` green; the
working tree split reviewed; group A reviewed by its author.
**Reversible**: yes for the commit, `partly` for the deploy (the previous binary
has to be kept to go back).

The build stamps its own identity from Go's VCS metadata, so **the commit has to
happen before the build**, not after. A binary built from a dirty tree reports
`dirty_tree: true` and a `-dirty` suffix rather than claiming a commit it was
not built from — which is the correct behaviour, and also why building first
produces an artifact you cannot identify later.

```bash
# 1. commit, per the commit plan's group order: F, A, B, C, D, E, G
git -C <REPO> add <FILES_FOR_ONE_GROUP>
git -C <REPO> commit -m '<MESSAGE>'

# 2. build a release artifact from the committed tree
git -C <REPO> status --porcelain          # must print nothing
make -C <REPO> build                      # or: goreleaser build --clean

# 3. record what you built, before it leaves this machine
sha256sum <ARTIFACT_PATH>
<ARTIFACT_PATH> version
```

**Expected**: `git status --porcelain` silent; `<ARTIFACT> version` prints a real
commit, a real build time and `dirty_tree: false`.

**Deploy** (the production host is `<PROD_HOST>`; the unit is `enowx-rag`):

```bash
scp <ARTIFACT_PATH> <PROD_HOST>:/tmp/enowx-rag.new
ssh <PROD_HOST> 'sha256sum /tmp/enowx-rag.new'          # compare to step 3
ssh <PROD_HOST> 'sudo cp /opt/enowx-rag/enowx-rag /opt/enowx-rag/enowx-rag.prev'
ssh <PROD_HOST> 'sudo install -m 0755 /tmp/enowx-rag.new /opt/enowx-rag/enowx-rag'
ssh <PROD_HOST> 'sudo systemctl restart enowx-rag && sleep 2 && systemctl is-active enowx-rag'
```

**Expected**: identical checksums on both sides; `active`.

**Rollback**: `sudo cp /opt/enowx-rag/enowx-rag.prev /opt/enowx-rag/enowx-rag &&
sudo systemctl restart enowx-rag`. Keep `.prev` until §M3 has passed. Do not
delete it on the same day.

---

## M2. Schema migration

**Approval**: cutover block. M0 has landed; the approval has not.
**Precondition**: a verified backup exists per
[`memgw-backup-restore.md`](memgw-backup-restore.md) §1–§3; the approved
maintenance window is open; writers are fenced (see the recovery note below —
the fence is what makes the backup a usable fallback at all).
**Reversible**: `no`, in the sense operators usually mean. The migrations are
forward-only, and restoring the pre-migration snapshot is **not** a rollback:
see below.

```bash
export MEMGW_DSN='postgres://<USER>@<HOST>:<PORT>/<DB>?sslmode=verify-full&sslrootcert=<CA>'
export MEMGW_SCHEMA=memgw                      # password via ~/.pgpass, never inline
export MEMGW_ENV=production
export MEMGW_PRODUCTION_DATABASE=<DB>          # must equal current_database()
export MEMGW_PRODUCTION_SCHEMA=memgw           # must equal MEMGW_SCHEMA
export MEMGW_PRODUCTION_ROLE=<USER>            # must equal current_user
export MEMGW_PRODUCTION_DEPLOYMENT=<IDENTITY>  # recorded in the schema on first apply
export MEMGW_PRODUCTION_ALLOW=schema_migration # not history_import, not serve

enowx-rag memgw status                         # read it
enowx-rag memgw plan --out /tmp/memgw-plan.json
# review the file: target, current version, pending list, checksums, risky
# operations. It carries no credential; the DSN in it is redacted to host/db.
enowx-rag memgw up --plan /tmp/memgw-plan.json
enowx-rag memgw status                         # every row "applied", none DIRTY or DRIFTED
```

**Expected**: `plan` lists exactly the migrations you expect and nothing else;
`up` applies exactly that plan; the final `status` shows no `DIRTY` and no
`DRIFTED` row. `up` without `--plan` is refused in production.

**Stop immediately** if `status` shows `DRIFTED`: a migration file's checksum no
longer matches what was applied. That is a source-control problem, not a
database problem, and running `up` will not fix it.

**Exit code 3** means a refusal, not a crash: the target, the plan digest, the
history or the lock did not match. Read the reason printed. Do not try to
satisfy it by renaming a database, by setting `MEMGW_ENV=test`, or by editing
the plan file — the digest covers the plan, so an edited plan is refused as
stale.

### Recovery is not "restore the backup"

Restoring the pre-migration snapshot **discards every write that was
acknowledged after it was taken**. The gateway hands out receipts; a client that
holds a receipt has been told its event is in the ledger. Rolling the database
back to a point before that event silently un-tells it, and the collectors will
not resend, because as far as they are concerned the event was delivered. That
is data loss with a clean-looking database on top of it.

If a migration must be undone, the procedure is:

1. **Fence the writers first.** Open a new writer epoch and fence the old one,
   or stop the gateway. Nothing may be accepting events during the rest of this.
   [`memgw-rollback-and-fencing.md`](memgw-rollback-and-fencing.md).
2. **Record the high-water mark**: the last `seq` and the chain checksum, from
   the live database, before anything is restored.
3. **Restore into a new database**, never over the live one, and repoint only
   after step 4 passes.
4. **Reconcile receipts against events.** Every receipt issued after the
   snapshot names an event that the restored copy does not have.
   `receipts_without_event` is the count that matters, and it must be brought to
   zero by replaying those events forward into the restored copy — not by
   deleting the receipts.
5. **Replay the local outboxes.** Each host's collector still holds what it has
   not had acknowledged; `memgw` replay is what re-delivers it. Tombstones
   replay too, or a row deleted before the snapshot comes back to life.
6. **Only then reopen traffic**, with a new writer epoch, so a host that missed
   the fence cannot write under the old one.

A restore that skips steps 1, 4 and 5 produces a database that looks consistent
and has lost acknowledged history. There is no automated version of this, and
the runner will not attempt one.

---

## M3. Verify `/api/version`

**Approval**: none beyond the deploy in M1.
**Precondition**: M1 deployed.
**Reversible**: yes — it is a read.

The endpoint is token-gated, so it needs the bearer. Prompt for it; do not put
it in a variable that survives the command, in the history, or in `ps`.

```bash
read -rs -p 'token: ' T; echo
curl -s -H "Authorization: Bearer $T" http://127.0.0.1:7777/api/version; echo
unset T
```

**Expected**: a JSON body naming a real commit and `dirty_tree: false`, matching
the checksum recorded in M1 step 3. Production answers `"dev"` today; if it
still does, the deploy did not take effect and everything downstream is running
on an unidentified binary.

**Rollback**: n/a.

---

## M4. Rotate `RAG_ADMIN_TOKEN`

**Approval**: token rotation block.
**Precondition**: all 13 consumers enumerated and reachable; the window is open;
someone is watching the agent hosts, because they are unauthenticated between
the server update and the last consumer update.
**Reversible**: `partly` — see the rollback note.

The full procedure, consumer by consumer and in order, is
[`rag-admin-token-rotation.md`](rag-admin-token-rotation.md). It is not repeated
here; what is repeated is the shape of the two tests, because getting those
wrong is how a token ends up in a log.

```bash
# generate and place the new value without it ever reaching a terminal:
#   see rag-admin-token-rotation.md §3. Do not echo it.

# positive test — the new value authenticates
read -rs -p 'new token: ' T; echo
for p in /api/version /api/stats; do
  printf '%s %s\n' "$p" \
    "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $T" \
       http://127.0.0.1:7777$p)"
done
unset T

# negative test — the OLD value no longer authenticates
read -rs -p 'old token: ' T; echo
printf 'old-token %s\n' \
  "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $T" \
     http://127.0.0.1:7777/api/version)"
unset T
```

**Expected**: `200` for both paths on the new value; **`401`** on the old one.
A `200` on the old value means a consumer was missed or the service was not
restarted — the rotation has not happened.

**Rollback**: put the previous value back through the same order. Understand
what that means: it returns a **compromised** token to service. It buys hours,
not days, and the rotation must then be re-run.

Post-rotation sweep — filenames and counts only, never the match, with the
pattern passed on stdin so it never appears in `ps`:

```bash
read -rs -p 'new token: ' T; echo
printf '%s\n' "$T" | grep -rlFf - /opt/enowx-rag/ /etc/nginx/ 2>/dev/null
journalctl -u enowx-rag --since '-30d' | { printf '%s\n' "$T" | grep -cFf - ; }
unset T
```

**Expected**: only the files you deliberately wrote it into; `0` from the
journal count. Anything else is a leak to be dealt with before continuing.

---

## M5. nginx validation and reload

**Approval**: token rotation block (nginx is one of its consumers).
**Precondition**: the configuration edit is made and saved.
**Reversible**: yes, if you kept the previous file.

```bash
sudo cp /etc/nginx/sites-available/<SITE> /etc/nginx/sites-available/<SITE>.prev
# edit
sudo nginx -t
sudo systemctl reload nginx
```

**Expected**: `nginx -t` prints `syntax is ok` and `test is successful`.

**Never reload without `-t` first.** A bad configuration takes the site down at
reload, and the reload is the moment nobody is watching the syntax.

**Rollback**: restore `.prev`, `nginx -t`, reload again.

A related decision, deliberately not done here: the token currently sits in
cleartext in two site files. Moving it into a `0600` include is an improvement
that belongs in this window, and it is listed as an open item rather than
performed.

---

## M6. Off-host backup

**Approval**: off-host backup block. **No provider, destination, bucket,
credential or transport has been chosen**, so every identifier below is a
placeholder that the approval form has to fill in first.
**Precondition**: destination approved; encryption owner named; a key escrow
exists.
**Reversible**: yes for the copy; `NO` for anything that deletes a local copy
afterwards.

```bash
# 1. dump
pg_dump -Fc -h <PGHOST> -p <PGPORT> -U <PGUSER> -d <PGDATABASE> \
        -f <LOCAL_DUMP_PATH>
sha256sum <LOCAL_DUMP_PATH>

# 2. encrypt (the approval form names the scheme and the key owner)
<ENCRYPT_COMMAND> <LOCAL_DUMP_PATH> -o <LOCAL_DUMP_PATH>.enc

# 3. copy off-host
<TRANSPORT_COMMAND> <LOCAL_DUMP_PATH>.enc <OFF_HOST_DESTINATION>

# 4. verify at the destination, not at the source
<TRANSPORT_COMMAND> --checksum <OFF_HOST_DESTINATION> | grep <EXPECTED_SHA256>
```

**Expected**: the checksum read back **from the destination** equals the one
computed in step 1. A checksum computed only locally verifies nothing about the
copy.

**Rollback**: delete the remote object. Note that on most object stores this is
not immediate and may leave a version behind — check the bucket's versioning
policy before treating a delete as a delete.

A backup nobody has restored is a hypothesis. Step 5 is
[`memgw-backup-restore.md`](memgw-backup-restore.md), run **on a different
machine**, and its result belongs in `memgw-backup-approval.md` §7.

---

## M7. `archive_mode` maintenance

**Approval**: `archive_mode` block, including the axonhub impact owner.
**Precondition**: window open; every other database on the cluster accounted
for; the archive destination exists and is writable by the postgres user.
**Reversible**: yes, but only through a second restart.
**This requires a RESTART, not a reload.** It is a cluster-wide outage.

```bash
sudo -u postgres psql -c 'SHOW archive_mode;'            # expect: off
sudo -u postgres psql -c "ALTER SYSTEM SET archive_mode = 'on';"
sudo -u postgres psql -c "ALTER SYSTEM SET archive_command = '<ARCHIVE_COMMAND>';"
sudo -u postgres psql -c "ALTER SYSTEM SET wal_level = 'replica';"
sudo systemctl restart postgresql
sudo -u postgres psql -c 'SHOW archive_mode;'            # expect: on
sudo -u postgres psql -c 'SELECT * FROM pg_stat_archiver;'
```

**Expected**: `archive_mode on`; `pg_stat_archiver` shows `archived_count`
climbing and `failed_count` at 0 after a few minutes. A climbing `failed_count`
means WAL is accumulating on disk and the cluster will eventually fill it — that
is an incident, not a warning.

**Rollback**: `ALTER SYSTEM SET archive_mode = 'off';` then restart again.

---

## M8. Issue production principals

**Approval**: production principals block.
**Precondition**: M2 done, so the schema exists; roles decided per host; a
`0600` destination directory exists on each host.
**Reversible**: `partly`. The credential row cannot be deleted — a trigger
prevents it — only revoked. Revocation is the undo; deletion is not available,
by design.

```bash
enowx-rag memgw principal create <AGENT_ID> --type agent
enowx-rag memgw principal grant <PRINCIPAL_ID> <ROLE> \
        --project <PROJECT_ID> [--workspace <WORKSPACE_ID>] [--expires <RFC3339>]

# The secret is printed once, on stdout, and cannot be recovered.
# Redirect it straight into a 0600 file. Do not echo it, do not tee it,
# do not run this inside a pipeline whose output is logged.
umask 077
enowx-rag memgw principal issue <PRINCIPAL_ID> --label '<HOST>' --expires <RFC3339> \
        > <TOKEN_FILE_PATH>
chmod 600 <TOKEN_FILE_PATH>

enowx-rag memgw principal list <PRINCIPAL_ID>    # metadata only; no secret
```

**Expected**: `list` shows the principal, its grants and a credential row with
no `revoked` timestamp.

**Rollback**: `enowx-rag memgw principal revoke-credential <CREDENTIAL_ID>`, or
`enowx-rag memgw principal revoke <PRINCIPAL_ID>` for the whole principal.

Two things the rehearsal established, both of which change how you grant:

- **Roles overlap.** Revoking `checkpoint_write` alone did not stop
  `session.started`, because `work_read` also admits it. Grant and revoke role
  *sets*.
- **If a secret is ever printed anywhere** — a transcript, a log, a screen
  share — revoke that credential id immediately and issue a replacement. That
  has happened in this project; recovery took under a minute.

---

## M9. Install the adapters

**Approval**: adapter installation block, **per host**. One approval does not
cover six hosts.
**Precondition**: M8 done for that host; the collector is running on that host
(Windows hosts) or the gateway is reachable (Hermes).
**Reversible**: yes — remove the fragment and restart the host.

This changes a **global** agent configuration. Nothing in this repository has
done so, and `adapters/install-sandbox.ps1` deliberately targets an isolated
profile instead.

```powershell
# Windows hosts: start the collector first, so events survive a gateway outage
enowx-rag memgw collector run `
  --dir <SPOOL_DIR_ON_A_DATA_DISK> `
  --pipe <PER_HOST_PIPE_NAME> `
  --gateway <GATEWAY_BASE_URL> `
  --token-file <TOKEN_FILE_PATH>

# then install the host's fragment from adapters/<host>/ into the host's
# real configuration, per adapters/README.md
```

**Expected**, per host, verified one host at a time: fire one real lifecycle
hook, then find that event in the ledger **by its id**. A fixture run proves the
translation works; it does not prove the host is integrated.

```bash
enowx-rag memgw collector stats     # pending should drain to 0
enowx-rag memgw collector held      # should be empty; metadata only if not
```

**Rollback**: remove the fragment, restart the host, stop the collector. Events
already committed stay committed — the ledger is append-only and that is correct.

Per-host cautions, which are the reason the approval is per host:

- **Codex** — never observed firing a hook here. Installing it is the first real
  test of an inferred vocabulary.
- **Hermes** — Linux, no collector, **no offline durability**. A gateway outage
  loses its events rather than queuing them.
- **OpenCode** — no exit event exists in the plugin API, so `session.ended` is
  never recorded and nothing invents one.

---

## M10. Fencing

**Approval**: cutover block (`fence epoch`, `drain watermark`).
**Precondition**: the new epoch is open **and every host has moved to it**.
**Reversible**: `NO`. A fenced epoch number can never be re-opened; re-opening
is refused, and the refusal is correct.

Fencing is not a CLI verb. It is an admin-scoped event submitted to the gateway,
which is what makes it appear in the log like everything else.

```bash
# Read the credential from a file; never inline it.
TOKEN_FILE=<ADMIN_TOKEN_FILE>

curl -s -X POST http://127.0.0.1:7777/memgw/v1/events \
  -H "Authorization: Bearer $(cat "$TOKEN_FILE")" \
  -H 'Content-Type: application/json' \
  -d '{"type":"writer_epoch.opened",
       "idempotency_key":"<KEY>",
       "payload":{"epoch":<NEW_EPOCH>}}'

# ... move every host to <NEW_EPOCH> and confirm it, then and only then:

curl -s -X POST http://127.0.0.1:7777/memgw/v1/events \
  -H "Authorization: Bearer $(cat "$TOKEN_FILE")" \
  -H 'Content-Type: application/json' \
  -d '{"type":"writer_epoch.fenced",
       "idempotency_key":"<KEY>",
       "payload":{"epoch":<OLD_EPOCH>,"reason_class":"cutover"}}'
```

`reason_class` is a closed vocabulary: `cutover`, `host_replaced`,
`credential_rotation`, `incident`, `maintenance`. Anything else is
`policy_rejected`.

Omit `drain_watermark_seq` unless you mean something other than the default: it
defaults to the fencing event's own seq, which is correct by construction, since
nothing stamped with the old epoch can commit after the fence.

**Expected**: both receipts committed; `SELECT * FROM memgw.writer_epochs` shows
the old epoch with a `fenced_at` and the new one open. A host still on the old
epoch will have its events **quarantined** with `fail_class
writer_epoch_fenced` — held on disk, encrypted, not lost.

**Rollback**: you cannot un-fence. To recover a stranded host: roll it forward
to the new epoch, then `memgw collector release --seq <N>` or, if the key
collides with a held row, `memgw collector purge --seq <N>` and re-fire. Full
procedure in [`memgw-rollback-and-fencing.md`](memgw-rollback-and-fencing.md).

---

## M11. Cutover — the order

**Approval**: cutover block, and every block above it.
**Precondition**: all seven preconditions in
[`memgw-production-cutover.md`](memgw-production-cutover.md) §1 closed.
**Reversible**: `partly` — see M10 and M12.

The full narrative is in `memgw-production-cutover.md`. The order, which is the
part that must not be improvised:

1. Close every precondition.
2. Back up **and restore-verify** (M6, then `memgw-backup-restore.md` §1–§3).
3. `memgw plan`, read it, `memgw up` (M2 — blocked by M0).
4. Issue principals and grants (M8).
5. Install and start the collectors (M9).
6. Open the new writer epoch (M10, first call).
7. Move the hosts and install the fragments (M9).
8. Watch (§M13), and only then:
9. Fence the old epoch (M10, second call).
10. Import history, if any:

```bash
enowx-rag memgw history plan  --source <EXPORT_NDJSON> --out <PLAN_JSON> \
                              --project-id <LEDGER_PROJECT_UUID>
# read the plan
enowx-rag memgw history apply --plan <PLAN_JSON>
enowx-rag memgw history replay --project-id <LEDGER_PROJECT_UUID>
enowx-rag memgw projection rebuild --project <LEDGER_PROJECT_UUID>
enowx-rag memgw projection run
```

**`--project-id` is not optional in practice.** Without it the project id is
derived from the RAG project *name*, and a tombstone issued against the real
ledger project never reaches those rows — they stay readable after a deletion
has been ordered. That is a defect this project actually hit, not a
hypothetical.

**`replay` after every apply and every restore, before anything queries.** Same
reason.

---

## M12. Rollback

**Approval**: named in the cutover block's `rollback owner`.
**Precondition**: none — this is the emergency path and must work without one.
**Reversible**: n/a.

The ledger is append-only. A rollback stops writers; it does not delete events,
and no rollback below loses a canonical event.

```bash
# stop one host generation
#   -> fence its epoch (M10). Its in-flight events quarantine, they do not vanish.

# stop one principal — revoke the ROLE SET, not one grant
enowx-rag memgw principal revoke <PRINCIPAL_ID>

# return to a previous generation
#   -> open a NEW epoch number and move the hosts to it.
#      Re-opening a fenced number is refused, and that refusal is correct.

# roll a held row forward once authority is restored
enowx-rag memgw collector held
enowx-rag memgw collector release --seq <N>
```

**Expected**: the chain checksum over the pre-existing range is unchanged. That
was true throughout the whole rehearsal and it is the check that says nothing
canonical was lost.

**Do not** revoke a single role and believe the writer is stopped. Verified:
revoking `checkpoint_write` alone did not stop `session.started`.

---

## M13. What to watch, during and after

**Approval**: none — reads only. **Reversible**: yes.

```bash
enowx-rag memgw collector stats        # pending climbing while the gateway is up = a problem
enowx-rag memgw collector held         # anything quarantined; read fail_class
enowx-rag memgw projection status      # watermark not advancing, or dead letters
sudo journalctl -u enowx-rag -f        # "credential refused by the gateway" = 401, retried forever
```

```sql
SELECT max(seq) FROM memgw.events;     -- flat while hosts are active = a problem
SELECT * FROM memgw.writer_epochs;     -- the old epoch still open after M10 = a problem
```

A quarantined row is **not** data loss. The event is on disk, encrypted, waiting
for a decision.

There is no alerting on any of this. Someone has to look. That is readiness row
18, and it is `not_verified` for exactly this reason.

---

## Cross-references

- Approval form: [`../plans/production-approval-form.md`](../plans/production-approval-form.md)
- Cutover narrative and preconditions: [`memgw-production-cutover.md`](memgw-production-cutover.md)
- Token rotation, consumer by consumer: [`rag-admin-token-rotation.md`](rag-admin-token-rotation.md)
- Backup decisions: [`memgw-backup-approval.md`](memgw-backup-approval.md)
- Restore procedure: [`memgw-backup-restore.md`](memgw-backup-restore.md)
- Fencing and rollback: [`memgw-rollback-and-fencing.md`](memgw-rollback-and-fencing.md)
- Collector recovery: [`memgw-collector.md`](memgw-collector.md)
- Projection: [`memgw-projection-rebuild.md`](memgw-projection-rebuild.md)
- Readiness rows: [`../plans/shared-memory-gateway-implementation.md`](../plans/shared-memory-gateway-implementation.md) §6
