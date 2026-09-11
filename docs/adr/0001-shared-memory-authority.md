# ADR 0001 — PostgreSQL is the authority for shared agent memory

- **Status:** Accepted (Phase 2, 2026-09-09). Contract frozen; nothing deployed.
- **Deciders:** repository owner.
- **Supersedes:** nothing. No prior ADR or equivalent decision document exists in this repository
  (`docs/` held only `HANDOFF.md`, `PLANNING.md`, `plans/`, `mockups/`, `screenshots/` before this
  file).
- **Related:** [`../architecture/shared-memory-gateway.md`](../architecture/shared-memory-gateway.md)
  (the frozen contract), [`../plans/shared-memory-gateway-implementation.md`](../plans/shared-memory-gateway-implementation.md)
  (phases and evidence).

## Context

Six agent hosts — Claude Code, OMP/Pi, Codex, OpenCode, Droid, Hermes — share one memory system whose
only durable store today is a Qdrant vector collection reached through enowx-rag.

Phase 1 established, with evidence, what that means in practice:

- Four hosts have MCP connectivity; **zero** have durable lifecycle capture. The only hook that was
  observed actually firing at runtime is the OpenCode plugin's own audit log. MCP being connected
  says nothing about whether anything is being written.
- Writes are best-effort HTTP. There is no receipt, no idempotency key, no revision, so a retry after
  a timeout can duplicate and a lost response is indistinguishable from a lost write.
- Every caller presents the same shared bearer and is authorised for everything. There is no
  principal, so there is nothing to scope, attribute or revoke — and the token is now treated as
  compromised (it was found in cleartext in two nginx site files).
- Similarity search is not a state query. "What is the current state of this work?" is answered by
  whatever chunk embeds closest, which can be a superseded checkpoint written days earlier.
- Deletion is not durable: deleting a vector removes it from search, and re-indexing can bring the
  same content back because nothing records that a deletion was ever ordered.

The question this ADR settles: what holds the authoritative state of work, checkpoints, facts,
decisions and deletions?

## Decision

**PostgreSQL, on the existing PG16 cluster, is the single authority** for work state, checkpoints,
accepted facts, decisions, receipts, provenance and tombstones. It is an append-only event ledger
with per-aggregate CAS revisions, server-assigned canonical ordering, and receipts written in the
same transaction as the event.

Everything else is a rebuildable projection or a scoped participant:

- **enowx-rag / Qdrant** remains the owner of sanitised *history* retrieval and is a projection for
  facts. It is never asked what is true now.
- **Graphify** is a derived code index. Projection only.
- **Hindsight** is optional shadow evaluation, outside the production path, with no write capability.
- **Obsidian vault** keeps credentials and regulated data. The ledger stores opaque handles, never
  values.

Consequences that follow directly and are not negotiable within this decision: principal + scope from
the first schema rather than retrofitted; a shared bearer demoted to a legacy transport credential
that is never the ACL authority; writer epochs so a forgotten legacy writer can be fenced at cutover;
and tombstones as monotonic ledger records that survive projection rebuilds and backup restores.

## Alternatives considered

**Keep Qdrant as authority and add discipline.** Rejected. A vector store has no transactions, no
compare-and-set and no ordering; "acknowledged write" and "durable write" cannot be made to mean the
same thing there. The failures above are structural, not procedural.

**A second PostgreSQL instance dedicated to the ledger.** Rejected for now, by explicit operational
decision: the host has 27 GB free and 12 GB RAM shared with `axonhub`, and a second instance doubles
the backup, patching and failure surface for a workload that is small. Revisit if ledger write volume
or an isolation requirement justifies it.

**SQLite on each host as the authority, replicated later.** Rejected. Six independent authorities is
six divergent truths; the merge problem is harder than the problem being solved. SQLite is kept where
it belongs — the local encrypted durable outbox on each host, which is a queue, not an authority.
`locally_queued` is explicitly not a commit.

**A second Go gateway service beside enowx-rag.** Rejected. enowx-rag already carries the auth
middleware, the credential write guard, the metrics store and — critically — `pgx/v5` with a live
`pgxpool`. A second service means a second deployment, a second token surface and a second thing to
fence at cutover, for no capability that extending the existing binary does not give.

**Deriving state from an LLM summarising history.** Rejected. It cannot produce a receipt, cannot be
made deterministic, and would make recalled text into an instruction path — which INV-15 exists to
forbid.

## Consequences

**Gained.** A write that is acknowledged is committed (process-crash durable) and provable by
receipt. Concurrent writers cannot silently lose each other's updates. Deletion is auditable and
survives rebuilds. Every fact names who promoted it and on what evidence. Projections become
disposable, so a corrupted Qdrant collection is a rebuild rather than a data loss.

**Paid.** More moving parts: an outbox, a projection worker, and lag between commit and searchability
(SLO ≤ 60 s healthy). Every host adapter must generate stable idempotency keys across crashes —
verified for none of the six today, and the first thing each adapter's fixture must prove. Writes now
depend on PostgreSQL availability; the local outbox absorbs that, at the cost of a queue that can
grow.

**Explicitly not promised.** A single PostgreSQL node gives process-crash durability, not survival of
host or disk loss. There is currently no WAL archiving (`archive_mode=off`), no off-host backup
destination, and no PG dump of the cluster — so the ledger's real RPO today is "everything since the
last backup that does not exist". INV-04 is worded to match that. Enabling `archive_mode` restarts a
cluster shared with `axonhub` and needs a maintenance window; choosing an off-host destination is
undecided. Until both are resolved, no document may claim zero acknowledged-event loss beyond process
crash, and cutover to ledger-authoritative operation should not be treated as complete.
