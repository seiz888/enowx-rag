# Runbook — backup, restore and the replay that must follow

What has to survive is the ordered log of canonical events and the receipts that
point at them. Everything else — the vector index, the code index, the outbox —
is derived and can be rebuilt from that log.

Exercised on the test server: `pg_dump -Fc` of the test database, restored into
a second database on the same server, reconciled, replayed and rebuilt.
**Off-host durability is untested and remains an open operational decision** —
see §7.

**A restore is not a rollback.** Restoring a snapshot discards every write that
was acknowledged after it was taken, and the gateway hands out receipts: a
client holding a receipt has been told its event is in the ledger, and the
collectors will not resend it, because as far as they are concerned it was
delivered. So a restore is only safe as part of a procedure that fences the
writers first, reconciles receipts against events afterwards, replays the local
outboxes and the tombstones, and reopens traffic under a **new writer epoch**.
Doing the restore alone produces a database that looks consistent and has
silently lost acknowledged history. The full sequence is in
[`memgw-production-commands.md`](memgw-production-commands.md) §M2; §3-§5 below
are its reconciliation and replay steps.

---

## 1. Taking a backup

```
pg_dump -Fc --dbname=<dsn> --file=<path>\memgw.dump
```

The custom format is used so a restore can be selective and parallel. The dump
must include the whole `memgw` schema: the event log, the receipts, the
principals and grants, the tombstone journal and `legacy_chunk_map`. A backup of
`events` alone restores a ledger that no durable client can reconcile against.

**The DSN carries a password.** Put it in a `0600` file or `PGPASSFILE`, not on
the command line, where it is readable by every process on the machine.

## 2. Restoring

```
createdb <target>
pg_restore --dbname=<target-dsn> <path>\memgw.dump
```

Restore into a **new** database first, always. Reconcile it (§3) before it
becomes the thing anything writes to. A restore straight over a live database
throws away the only copy of whatever the backup did not have.

## 3. Reconciling — the four checks

Run these against the restored database and, when you still have it, against the
original, and compare row by row.

1. **Counts.** `events`, `receipts`, `receipts` in state `committed`,
   `projection_outbox`, `tombstones`, `legacy_chunk_map`, `facts`, `max(seq)`.
2. **Referential integrity of the log.** `receipts_without_event` and
   `events_without_receipt` must both be `0`. A restore that kept the log but
   lost the receipts leaves every durable client permanently unable to resolve
   `unknown_commit_status`, and that failure is otherwise latent — nothing
   complains until a collector asks about an event it never got an answer for.
3. **Chain checksums.** A SHA-256 over the ordered log
   (`seq:event_id:payload_digest`) and the same over the receipts. Take them
   over the *order*, not over a table's physical contents: the physical layout
   legitimately differs after a restore; the sequence must not.
4. **Watermark and queue.** `memgw projection status`. The watermark is expected
   to be behind; the queue is expected to be incomplete. Both are repaired in
   §5, not here.

Verified: all 13 reconciliation rows were identical between the original and the
restored copy, including both chain checksums, with
`receipts_without_event 0` and `events_without_receipt 0`.

## 4. Replay the tombstones — before anything queries

```
enowx-rag memgw history replay [--project-id <uuid>]
```

**This is a required step of a restore, not an optional repair.** With no
`--project-id` it covers every project, which is what a restore wants: the
operator does not yet know which projects were behind.

Why it exists: a mapping row that arrives *after* a deletion was ordered — by an
import, or by restoring a backup taken before the deletion — is readable again
while a tombstone naming it sits in the journal. The replay walks the journal and
applies every deletion the map never received.

Its rules, and they matter:

- It uses the same predicate as the live writer (`subject_id` against
  `document_id`, or a named chunk id), including the deliberate absence of a
  `subject_type` filter. A replay that reached more or less than the writer would
  make the two disagree, and then neither could be trusted.
- The mark is the **tombstone's own `issued_at`**, not `now()`. Where several
  tombstones reach one row the earliest wins, and a row already marked is left
  alone: a replay never moves a deletion later.
- It is idempotent. A second run reports `newly marked 0` and says so in words.

Output:

```
scope         <project or every project>
tombstones    N
newly marked  M
```

`newly marked` greater than zero on a restored database means content was
readable that should not have been, for the window between the restore and this
command. Note the number; it is the size of the exposure.

Verified: on the restored database the replay found and repaired 2 rows, marked
them with the tombstone's own timestamp (earlier than the following event's), and
a second run marked 0. The same latent gap existed in the source database and was
repaired the same way.

## 5. Rebuild the derived state

Only after §3 and §4:

```
enowx-rag memgw projection rebuild [--project <uuid>]
enowx-rag memgw projection run --qdrant <url> --embedder tei --tei-url <url>
```

`rows restored` in the rebuild output is how much of the queue the backup did not
carry. See `memgw-projection-rebuild.md`.

The code index (`memgw graphify`) is derived from the working tree, not from the
ledger, and is unaffected by a database restore.

## 6. Order, in one line

dump → restore into a new database → reconcile → **replay** → rebuild → drain →
only then let anything query it.

## 7. What has *not* been proven

Say these plainly whenever the backup story is reported:

- **Off-host durability.** The rehearsal's dump sits on the same disk as the
  database it came from. It proves a dump/restore cycle preserves the schema and
  reconciles; it proves nothing about surviving the loss of that disk. Choosing
  an off-host destination is an open operational decision.
- **A different machine or PostgreSQL version.** Same server, same version.
- **Point-in-time recovery.** No WAL archiving was configured or exercised;
  `archive_mode` is untouched and its maintenance window is unapproved. Without
  it the recovery point is the last full dump, and everything after it is lost.
- **Restore under production credentials.** The rehearsal used a disposable test
  credential on a disposable database.
