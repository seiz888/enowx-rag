# Runbook — production cutover

> **Not authorised.** Nothing in this document has been performed against
> production, and performing it requires decisions that have not been made. The
> open blockers are listed in §1 and each one is a precondition, not a caveat.
> This is the procedure to follow *when* it is authorised, written down now while
> the rehearsal is fresh.

Everything below was rehearsed on disposable test resources: a loopback
PostgreSQL, a loopback Qdrant, a disposable credential and hooks fired by hand
through the same binary a host would invoke.

---

## 1. Preconditions — every one must be closed first

1. **`RAG_ADMIN_TOKEN` is compromised and has not been rotated.** Rotate it and
   confirm the old value no longer authenticates before any cutover step.
   Procedure: [rag-admin-token-rotation.md](rag-admin-token-rotation.md).
2. **No off-host backup destination has been chosen.** The rehearsal's dump sat
   on the same disk as its database. A cutover with no off-host copy has no
   recovery point. Decisions to make:
   [memgw-backup-approval.md](memgw-backup-approval.md).
3. **`archive_mode` and a maintenance window are unapproved.** Without WAL
   archiving there is no point-in-time recovery; the recovery point is the last
   full dump.
4. **Deployment artifact identity is not live.** The build-identity work exists
   but is not active on a deployed binary, so "which build is running" cannot be
   answered from the running service.
5. **No production principals or credentials have been issued.** Every rehearsal
   used a disposable test credential.
6. **Global adapter installation is not permitted.** No agent host has loaded a
   memgw adapter fragment; the hooks were fired by hand.
7. **Cutover and fencing against production are not permitted.**

An eighth, found while writing the command sheet and not an operational decision
at all: **the binary as built refuses to migrate a production database.**
`pgstore.VerifyNonProduction` gates the migration runner, `memgw plan` and
`memgw history apply`, and production fails its environment, database-name and
foreign-table checks. Step 3 below is therefore unexecutable until a reviewed
code change introduces an explicit production mode. See
[memgw-production-commands.md](memgw-production-commands.md) §M0 and the main
plan's readiness row 19. Do not satisfy the guard by renaming a database or by
declaring `MEMGW_ENV=test` against production.

Until 1–7 are closed, the honest status is "implemented and rehearsed", never
"production-ready" or "rolled out".

Each precondition has a status row in the main plan's production-readiness
checklist (section 6), with its evidence and its assumptions kept apart. The
per-host picture is section 7; the disposable resources still alive, and the
staged procedure for removing them, are section 8.

The decisions themselves are collected as blank fields in
[`../plans/production-approval-form.md`](../plans/production-approval-form.md).
The literal commands for every step below — with preconditions, expected
results, rollbacks and a reversibility marker — are in
[memgw-production-commands.md](memgw-production-commands.md). This document is
the reasoning; that one is the sheet you hold while typing.

## 2. Order

1. Close every precondition in §1.
2. Back up production and **verify the restore**, per `memgw-backup-restore.md`
   §1–§3. A backup nobody has restored is a hypothesis.
3. Run the schema migration: `memgw plan` first, read it, then `memgw up`.
   `plan` is a dry run that refuses for the same reasons `up` would.
4. Issue production principals and grants — one principal per host, least role
   that admits the event types that host emits. `memgw principal issue` prints
   the secret **once, on stdout**; redirect it straight into a `0600` file so it
   is never echoed.
5. Install and start the collector on each host with `--dir` on a data disk,
   `--token-file` pointing at that file, and a per-host pipe name.
6. Open the new writer epoch: `writer_epoch.opened {epoch: N+1}`.
7. Move the hosts: set `writer_epoch: N+1` in each adapter config and install the
   hook fragments.
8. Let the hosts run and watch (§4). Only then:
9. Fence the old epoch: `writer_epoch.fenced {epoch: N, reason_class: cutover}`.
10. Import history if there is any: `memgw history plan` → read it →
    `memgw history apply` → **`memgw history replay`** → `memgw projection
    rebuild` → `memgw projection run`.

Steps 6–9 are the order that matters and it is not negotiable: open, move,
fence. Fencing before the hosts have moved strands every host that has not, and
the fencing event is itself stamped with an epoch. See
`memgw-rollback-and-fencing.md` §1.

## 3. Pass `--project-id` on every history import

`memgw history plan` without `--project-id` derives the project id from the RAG
project *name*. A tombstone issued against the real ledger project then never
reaches those rows, and they stay readable after a deletion has been ordered.
This is not hypothetical — it is the defect the replay was written to repair.
Always name the ledger project explicitly, and always replay after applying.

## 4. What to watch, and what each reading means

| Check | Command | A problem looks like |
|---|---|---|
| Queue moving | `memgw collector stats` | `pending` climbing while the gateway is up |
| Credential accepted | collector log | repeated *credential refused by the gateway* (401 retries forever, loudly) |
| Refusals | `memgw collector held` | anything quarantined; read `fail_class` |
| Ledger growing | `max(seq)` on `events` | flat while hosts are active |
| Index catching up | `memgw projection status` | watermark not advancing, or dead letters |
| Epochs | `SELECT * FROM writer_epochs` | the old epoch still open after step 9 |

A quarantined row is not data loss. It is the design working: the event is on
disk, encrypted, waiting for a decision.

## 5. Rolling back a cutover

The ledger is append-only; a rollback does not delete events. It stops writers
and, if necessary, moves the hosts back.

- **Stop a host generation**: fence its epoch. Its in-flight events quarantine
  with `writer_epoch_fenced` and can be released after the host is rolled
  forward and its held rows purged.
- **Stop a principal**: `memgw principal revoke`. Revoke the whole role set, not
  one grant — verified: revoking `checkpoint_write` alone did not stop
  `session.started`, because `work_read` also admits it.
- **Return to the previous epoch**: open it again as a new epoch number. Do not
  reuse a fenced number; re-opening an existing epoch is refused, and the refusal
  is correct.
- **Nothing canonical is lost either way.** Verified across the whole rehearsal:
  the chain checksum over the pre-existing range was unchanged throughout.

Full procedure: `memgw-rollback-and-fencing.md`.

## 6. Verify the cutover, do not assume it

For each of the six hosts, separately: fire a real lifecycle hook, then find that
event in the ledger by its id. A fixture adapter run is not evidence that a host
is integrated — it is evidence that the translation works. Report per host, and
name any host that has never been observed firing a hook rather than counting it
as verified.

## 7. Secrets

No secret belongs on a command line, in an environment variable, in a log, in
the ledger, in a spool payload, in the RAG, or in an embedding request. The
collector reads its credential from `--token-file` only. If a secret is printed
anywhere, treat it as compromised, revoke that credential id immediately and
issue a replacement. That has happened in this project; recovery took under a
minute, and hiding it would have left a live credential in a transcript.
