# Runbook — projection: status, drain and rebuild

The ledger is the truth and the vector index is a cache of it. Every command
here reads events and writes points; none of them modifies canonical state. That
is the property that makes a rebuild safe to run when you are unsure: the worst
outcome of an unnecessary rebuild is wasted embedding work.

Exercised against the test database and a real Qdrant (`memgw-qdrant`,
`v1.12.4`) on the test machine. Not run against production, which has no
projection worker yet.

---

## 1. Reading the state

```
enowx-rag memgw projection status
```

Environment: `MEMGW_DSN`, `MEMGW_SCHEMA` (default `memgw`), `MEMGW_ENV`
(`development` or `test`; `production` is refused).

It prints the redacted target, the watermark (`applied through seq N`), a row
per `projection × state`, and then **lists** the dead letters individually with
their outbox id, event id, seq and attempt count. They are listed rather than
counted because "5 dead letters" is not something an operator can act on.

Three states matter:

- `pending` / `claimed` — work in flight. A growing `pending` with the worker
  running means the embedder or the store is refusing.
- `failed` — retried and still failing. Read `last_error_class`.
- `dead` — retries exhausted. These will never move on their own.

## 2. Draining

```
enowx-rag memgw projection run --qdrant <url> --embedder tei --tei-url <url>
```

`--embedder fixture` produces deterministic vectors with **no semantic
content**. It exists so the pipeline can be exercised without a model, and an
index built with it is useless for retrieval. Never point a fixture drain at a
store anyone will query.

`--max-sensitivity` (default `internal`) is the ceiling on what may be embedded.
Raising it sends more classes of text to the embedding provider; that is a
policy decision, not a tuning knob.

The worker never guesses a store: with no `--qdrant` and no `RAG_QDRANT_URL` it
exits 2 rather than picking a default.

## 3. Rebuilding the queue

```
enowx-rag memgw projection rebuild [--project <uuid>]
```

`rebuild` **queues work; it does not do it.** Nothing is written to the vector
store by this command. It reconstructs the outbox from the events in the ledger:

```
rebuild scope    <project or "every project">
rows re-queued   N
rows restored    M (events that had no outbox row at all)
watermark        W (unchanged: it records how far the projection has been
                    applied at least once)
```

- **re-queued** — rows that existed and were put back to `pending`.
- **restored** — events whose outbox row was *missing entirely*. This is the
  number that matters after a restore: it is how much of the queue the backup
  did not carry.
- **watermark** — deliberately not moved backwards. It records how far the
  projection has been applied at least once, and rewinding it would make a
  rebuild look like data loss to anything reading it.

Then drain with §2. Verified on the restored database: `rows re-queued 20`,
`rows restored 2`, watermark held at 3, and the subsequent drain applied all 20
and moved the watermark to 20. Both canonical chain checksums were byte-identical
before and after — a rebuild touches nothing canonical.

## 4. Rebuild after a restore: replay first

**Run `enowx-rag memgw history replay` before rebuilding or querying a restored
database.** A restore can bring back a mapping row that a committed tombstone
had already reached; that row is readable again until the replay re-applies the
deletion. Full procedure in `memgw-backup-restore.md` §4.

The projection worker itself consults the tombstone journal before rendering
anything, so a rebuild cannot resurrect a document the journal knows is deleted.
Replay is what makes the journal's reach match reality again after rows arrive
out of order.

## 5. When a row will not apply

Read the class first — `projection status` lists dead letters with their ids,
and `last_error_class` on `projection_outbox` says why.

- `index_delete_failed` on `tombstone.issued` rows was a real defect, fixed in
  `pkg/rag/qdrant.go`: `DeletePoints` sent raw document ids while `Index` stored
  points under `pointID(id)`, so a deletion could never name the point that was
  written. If this class reappears, check that both paths still go through
  `pointID` before assuming an infrastructure fault.
- Embedder failures are transport-shaped and clear on their own once the model
  is back; the row stays `failed` and is retried.
- A row that keeps failing on the same class after the cause is fixed can be put
  back with a rebuild — re-queuing is idempotent.

The projection fails a row and retries rather than marking work applied that did
not happen. A dead letter is honest; a silently skipped row would not be.

## 6. What a rebuild is not

It is not a way to repair the ledger, not a migration, and not a substitute for
the replay. It re-derives a cache. If the events are wrong, the rebuild will
faithfully re-index wrong events.
