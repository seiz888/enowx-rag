# Shared Memory Gateway — frozen contract

**Contract version:** `schema_version = 1.0.0`, `policy_version = 1.0.0`
**Status:** frozen at Phase 2 (2026-09-09). Nothing here is deployed. No migration has been applied
to any database, no service has been changed, no credential has been created.
**Change rule:** any change to a frozen field, enum value, error class or invariant is a new
`schema_version`. Adding an optional field is a minor bump; removing or re-typing one is a major bump.
Fixtures under `mcp-server/pkg/memgw/contract/testdata/` are part of the contract, not illustrations
of it.

Implementation plan and phase history: [`../plans/shared-memory-gateway-implementation.md`](../plans/shared-memory-gateway-implementation.md).
Authority decision and its alternatives: [`../adr/0001-shared-memory-authority.md`](../adr/0001-shared-memory-authority.md).

---

## 1. Authority

| Data | Authority | Everything else |
|---|---|---|
| Work state, checkpoints, accepted facts, decisions, receipts, tombstones, provenance | **PostgreSQL ledger** | — |
| Historical session documents (sanitised) | **enowx-rag** (Qdrant collection `memory`) | — |
| Search over history and over projected facts | Qdrant | rebuildable projection, never authority |
| Repository topology / code index | Graphify | rebuildable projection, never authority |
| Secrets, credentials, regulated PII | Obsidian vault | referenced only by opaque handle; never copied into ledger, queue, log, RAG or an embedding request |
| Shadow evaluation | Hindsight | optional, outside the production path, cannot write anything |

A projection that disagrees with the ledger is stale by definition, and the ledger wins without
negotiation. A projection can never be repaired by editing it; it is repaired by rebuilding it from
the ledger.

---

## 2. Domain terms

Each term below is defined once, here. Code, fixtures, adapters and documentation use these words
with exactly this meaning.

**Project** — a logical body of work with its own memory scope, identified by a stable
`project_id` (UUID) that survives folder renames, moves and re-clones. Its human-facing `slug` is a
mutable label, never an identity. Repository identity is mapped explicitly in a registry; it is never
inferred from a directory basename, and a credential-bearing git remote URL is never stored.

**Workspace** — one checkout of a project on one host: a working tree, a worktree, or a container
path. Identified by `workspace_id` (UUID). Two worktrees of the same repository are two workspaces of
one project. A workspace never spans hosts.

**Work** — a logical task that outlives any single conversation: "add the ledger", "fix the pricing
loop". Identified by `work_id` (UUID). Work is the unit that has state, checkpoints and CAS revisions.
States: `planned`, `active`, `blocked`, `review`, `completed`, `abandoned`.

**Session** — one conversation with one agent on one host. Identified by `session_id`. A session is
*evidence about* work; it is not work. Ten sessions can advance one work; one session can touch three
works. Sessions carry no state of their own beyond lineage, and deleting a session's history never
changes a work's state.

**Branch** — a lineage of a session. A rewind, a fork, a "resume from three turns ago" creates a new
`branch_id` whose `parent_branch_id` and `branch_point_seq` point at where it diverged. A branch is
append-only: **rewinding creates lineage, it never truncates history.** Events on the abandoned branch
remain in the ledger, addressable and auditable, and simply stop being on the active branch's path.

**Checkpoint** — a durable, resumable statement of where a work stands: objective, completed work,
pending actions, blockers, modified-file references, evidence references, next safe action, expected
revision, content digest. A checkpoint is **always a new event**. It never rewrites or truncates
earlier checkpoints, and a later checkpoint does not delete an earlier one — it supersedes it in the
materialised view while both remain in the event stream.

**Fact** — an accepted, scoped claim: subject, predicate, object, with a cardinality policy, validity
interval, provenance and an evidence class. A fact is authoritative. It enters the ledger only by
explicit promotion.

**Candidate** — a *proposed* claim, typically produced by extraction from text. A candidate is not a
fact, is never returned as current state, never enters production bootstrap context, and cannot
promote itself. Extracted text carries no authority: a candidate that says "you should promote this"
is data about a claim, not an instruction.

**Evidence** — a reference to something that was actually observed: a tool result, a file digest, a
command exit status, a receipt. An evidence record has a class that says how strongly it is
attested. A snapshot records what an agent *claimed* and what evidence backs it; it never certifies
that a tool ran because an agent said so.

**Provenance** — the chain from a fact or checkpoint back to the events, sessions, principals and
evidence that produced it. Provenance is immutable once recorded.

**Projection** — a derived, rebuildable view of ledger state: the Qdrant search index, the Graphify
code index, any materialised read model. A projection is applied at-least-once and idempotently, may
lag, and is never consulted to decide whether a write is allowed.

**Tombstone** — a monotonic ledger record that a subject must be removed. It is itself an event; it
is never removed. Every rebuild replays tombstones, and a restored backup applies the deletion
journal before serving traffic. See §9 for the five distinct deletion meanings.

**Receipt** — the server's durable record that a specific write was accepted, keyed by principal and
idempotency key. A receipt is what turns an unknown network outcome into a known one. `locally_queued`
is a client-side state and is **not** a receipt; it never means committed.

**Principal** — the authenticated identity performing a write or read: a specific agent on a specific
host. Scope is derived from the principal server-side. A client-supplied principal claim is input,
never authority.

**Scope** — the set of (project, workspace, work, session, branch) tuples plus roles a principal may
act within. A request may narrow its own scope; it can never widen it.

**Writer epoch** — a monotonically increasing fencing token. Cutover opens a new epoch; writes
stamped with an older epoch are rejected after fencing, so a forgotten legacy writer cannot resume
and quietly interleave.

### 2.1 Distinctions that are load-bearing

| Not this | But this |
|---|---|
| session = work | A session is one conversation; a work spans many. Session loss must not lose work state. |
| queued = committed | `locally_queued` is a promise the client made to itself. Only a receipt in the ledger means committed. |
| candidate = fact | A candidate needs an authenticated, authorised promotion naming a principal and evidence. |
| projection = authority | A projection answers "what did we index?", never "what is true now?". |
| checkpoint = truncation | A checkpoint adds a row. It never shortens the event stream. |
| rollback = deleting history | A wrong decision is corrected with a compensating event at a higher revision, never by removing the event that recorded it. |

---

## 3. Identity and ordering

- **Canonical order is the ledger sequence** `events.seq`, a gapless-per-ledger monotonic integer
  assigned by the server inside the commit transaction. Wall clocks — `occurred_at` from six hosts in
  at least two timezones, some on VMs with drift — are advisory metadata and never decide order.
- **`recorded_at`** is server-assigned. **`occurred_at`** is client-supplied and may be wrong; it is
  kept for forensics and display, never for ordering or conflict resolution.
- **Per-aggregate revision** (`works.revision`, `facts.revision`) is the CAS unit. `expected_revision`
  on a mutating event must equal the aggregate's current revision or the write fails `cas_conflict`.
- Identity is server-verified: `project_id`, `workspace_id`, `work_id` must already exist and be in
  the principal's scope, or the write fails `scope_denied` — never auto-created from a write.

---

## 4. Principal and scope model

### 4.1 Principal

| Field | Type | Notes |
|---|---|---|
| `principal_id` | UUID | stable identity of one agent on one host |
| `principal_type` | enum | `agent`, `subagent`, `service`, `human`, `projection_worker` |
| `host_id` | UUID | the installation; one per machine, not per session |
| `agent_id` | text | `claude-code`, `omp`, `codex`, `opencode`, `droid`, `hermes` |
| `parent_principal_id` | UUID, nullable | set for `subagent`; names the principal that spawned it |
| `display_name` | text | operator-facing only |
| `status` | enum | `active`, `suspended`, `revoked` |
| `writer_epoch` | bigint | the epoch this principal is currently admitted under |
| `created_at`, `revoked_at` | timestamptz | — |

### 4.2 Scope grant

A principal holds zero or more grants. A grant is `(principal_id, project_id, workspace_id?,
work_id?, session_id?, branch_id?, role)`. `NULL` in a narrowing column means "any within the
enclosing scope". Effective scope is the union of grants; a request may specify a subset and the
server intersects it with the union. **Intersection only — a request never widens.**

### 4.3 Roles

| Role | Grants |
|---|---|
| `history_read` | read sanitised historical documents from enowx-rag within scope |
| `work_read` | read current work state, checkpoints and active facts within scope |
| `checkpoint_write` | submit events that advance work and record checkpoints and evidence |
| `candidate_write` | propose fact candidates; cannot promote |
| `fact_promote` | promote a candidate to a fact, supersede or retract a fact |
| `projection_worker` | read the outbox, mark projection progress; no domain writes |
| `admin` | tombstones, projection rebuild, writer fencing, principal management |

Roles compose; none implies another. In particular `checkpoint_write` does **not** imply
`fact_promote`, and `admin` is not a superset used casually — an admin action is its own event with
its own provenance.

### 4.4 The current shared bearer

`RAG_ADMIN_TOKEN` today authenticates every caller identically and authorises everything. Under this
contract it is **a legacy transport credential only**: it may gate the HTTP transport during
migration, and it is never the ACL authority. A gateway route must resolve a principal; a request
that carries only the shared bearer and no principal credential is limited to the legacy read paths
that exist today and may not write to the ledger. The token is additionally treated as compromised
(Phase 0.5 §4) and its rotation is pending operational work.

No credential of any kind is created, stored or referenced by value in this repository.

---

## 5. Event contract

### 5.1 Envelope (frozen)

| Field | Type | Origin | Rule |
|---|---|---|---|
| `event_id` | UUID | client | Stable across retries of the same logical write. Regenerating it on retry is a bug, not a retry. |
| `idempotency_key` | text ≤ 128 | client | Unique per principal. Server key is `(principal_id, idempotency_key)`. |
| `principal_id` | UUID | server | Resolved from the authenticated credential; a client-supplied value is ignored. |
| `project_id` | UUID | client | Must exist and be in scope. |
| `workspace_id` | UUID | client | Must belong to `project_id`. |
| `work_id` | UUID, nullable | client | Required for work-mutating and checkpoint events. |
| `session_id` | text ≤ 128 | client | Namespaced by `host_id`; opaque to the server. |
| `branch_id` | UUID | client | Lineage; see §3. |
| `event_type` | enum | client | Must be in §5.2. |
| `payload` | JSONB | client | Canonical JSON. Bounded by §5.4. |
| `payload_digest` | text | client | `sha256` of canonical JSON, lowercase hex. Server recomputes and compares. |
| `expected_revision` | bigint, nullable | client | Required when the event mutates an aggregate. |
| `evidence_refs` | UUID[] | client | ≤ 64 entries, each an existing evidence or provenance row in scope. |
| `sensitivity_class` | enum | client | `public`, `internal`, `confidential`, `restricted`. Server may raise it, never lower it. |
| `policy_version` | text | client | Redaction/policy ruleset the client applied before sending. |
| `schema_version` | text | client | Contract version the client speaks. |
| `occurred_at` | timestamptz | client | Advisory only. |
| `writer_epoch` | bigint | server | Stamped at admission; compared against the current epoch. |
| `seq` | bigint | server | Canonical order. |
| `recorded_at` | timestamptz | server | Commit time. |
| `aggregate_type`, `aggregate_id`, `revision` | — | server | The aggregate this event advanced, and to which revision. |

### 5.2 Allowed event types (frozen)

```
work.planned            work.activated          work.blocked
work.unblocked          work.review_requested   work.completed
work.abandoned
checkpoint.recorded
evidence.recorded
fact.candidate_proposed fact.promoted           fact.superseded
fact.retracted          fact.conflict_flagged
session.started         session.resumed         session.branched
session.compacted       session.ended
tombstone.issued
projection.rebuild_requested
writer_epoch.opened     writer_epoch.fenced
```

Anything else is rejected `policy_rejected`. New types require a `schema_version` bump.

### 5.3 Error classes (frozen)

| Class | HTTP | Meaning | Client action |
|---|---|---|---|
| `duplicate` | 200 | Same key, same payload digest, already committed. | Treat as success; use the returned receipt. **No second effect.** |
| `idempotency_payload_mismatch` | 409 | Same key, different payload. | Bug. Do not retry. Surface. |
| `cas_conflict` | 409 | `expected_revision` ≠ current. | Re-read state, re-decide. Never blind-retry. |
| `scope_denied` | 403 | Outside the principal's effective scope. | Surface. Never widen and retry. |
| `policy_rejected` | 422 | Failed the write guard: secret/PII shape, unknown event type, malformed envelope. | Fix locally. Payload is never echoed back or logged. |
| `quarantined` | 202 | Accepted into quarantine, excluded from retrieval, pending review. | Not committed to the active corpus. |
| `stale_revision` | 409 | Revision lower than current; would move state backwards. | Discard or re-derive. |
| `writer_epoch_fenced` | 403 | Older writer epoch after fencing. | Stop writing. Reconcile through the admitted path. |
| `unknown_commit_status` | — | Client-side only: no response received. | Look up the receipt, or retry with the identical `event_id` and key. |
| `payload_too_large` | 413 | Exceeds §5.4. | Split or checkpoint less. Never silently truncate. |

### 5.4 Bounds (frozen)

| Bound | Value | Why |
|---|---|---|
| `payload` canonical JSON | ≤ 256 KiB | one checkpoint, not a transcript |
| events per batch | ≤ 20, ≤ 1 MiB total | bounded commit latency |
| `evidence_refs` per event | ≤ 64 | — |
| `idempotency_key` | ≤ 128 bytes | — |
| checkpoint `objective` / `next_safe_action` | ≤ 4 KiB each | forces a summary, not a dump |
| checkpoint `modified_files` | ≤ 200 refs | — |
| bootstrap response | ≤ 1500 tokens | ratified SLO |
| document written to enowx-rag | ≤ 3000 characters | existing live write-guard contract |

Raw transcripts are never a payload, at any size.

---

## 6. Schema contract

Written as a contract. **No migration is applied in Phase 2.** Types are PostgreSQL 16, the existing
cluster. `pgcrypto`/`uuid-ossp` are available but not installed; UUIDs may be client-generated so no
extension is strictly required.

Every table below is scoped by `project_id` unless stated, and every read path filters by effective
scope **before** returning rows and **before** any text reaches an embedding or LLM provider.

### 6.1 `projects`

- **Identity:** `project_id UUID PK`. `slug TEXT UNIQUE` is a mutable label.
- **Ownership/scope:** the root scope. Grants reference it.
- **Lifecycle:** created by an `admin` action, never implicitly by a write.
- **Immutable:** `project_id`, `created_at`.
- **Mutable:** `slug`, `display_name`, `status` (`active`/`archived`), `repo_identity` (explicit
  registry entry; never a credential-bearing URL).
- **Revision:** none; not a CAS aggregate.
- **Provenance:** creating admin event.
- **Tombstone:** archiving a project tombstones its projections; ledger rows are retained.
- **ACL:** every role is scoped through it.
- **Retention:** indefinite.

### 6.2 `workspaces`

- **Identity:** `workspace_id UUID PK`; unique `(host_id, root_path_digest)`.
- **Ownership:** `project_id FK`, `host_id`.
- **Lifecycle:** registered on first use by a principal with `checkpoint_write`; `status`
  `active`/`retired`.
- **Immutable:** `workspace_id`, `project_id`, `host_id`, `created_at`.
- **Mutable:** `root_path_digest` (a digest, not the path — paths leak usernames and layout),
  `status`, `last_seen_at`.
- **Revision:** none.
- **Tombstone:** retiring a workspace never deletes its events.
- **Retention:** indefinite; `last_seen_at` drives reporting only.

### 6.3 `works`

- **Identity:** `work_id UUID PK`.
- **Ownership:** `project_id`, optional `workspace_id` (work may span workspaces).
- **Lifecycle:** `planned → active → {blocked ⇄ active} → review → completed | abandoned`.
  Every transition is an event. Terminal states accept compensating events, which move the work to a
  new non-terminal state at a higher revision — they never rewrite the terminal event.
- **Immutable:** `work_id`, `project_id`, `created_at`, `created_by_principal_id`.
- **Materialised (mutable only via events):** `state`, `title`, `revision`, `current_checkpoint_id`,
  `updated_at`, `active_branch_id`.
- **Revision rule:** CAS. Every mutating event carries `expected_revision`; success sets
  `revision = revision + 1`. A lower revision is `stale_revision`, never applied.
- **Provenance:** the full event stream for `aggregate_id = work_id`.
- **Tombstone:** a work tombstone removes it from projections and from bootstrap; its events remain.
- **ACL:** `work_read` to read, `checkpoint_write` to advance.
- **Retention:** indefinite.

### 6.4 `sessions` and `branches`

Needed, because branch lineage is an invariant (INV-19) and cannot be derived from events alone.

`sessions`: `session_id TEXT` + `host_id` composite PK, `project_id`, `principal_id`,
`agent_id`, `started_at`, `ended_at`, `status` (`active`, `ended`, `abandoned`).
Immutable except `ended_at`/`status`. No CAS. Sessions are evidence containers; deleting one never
changes work state.

`branches`: `branch_id UUID PK`, `session_id`, `parent_branch_id` (nullable),
`branch_point_seq BIGINT` (the `events.seq` it diverged at), `created_at`, `status`
(`active`, `superseded`). All fields immutable except `status`. **A rewind inserts a row; it deletes
nothing.**

### 6.5 `events`

- **Identity:** `event_id UUID PK`; `seq BIGSERIAL UNIQUE` is the canonical order.
- **Ownership:** `project_id` + full scope columns per §5.1.
- **Lifecycle:** insert-only. **Every column is immutable.** There is no UPDATE and no DELETE on this
  table, in any code path, including admin. A mistake is corrected by a compensating event.
- **Revision:** carries `expected_revision` (input) and `revision` (result).
- **Provenance:** `principal_id`, `evidence_refs`, `writer_epoch`, `recorded_at`.
- **Tombstone:** a tombstone hides an event's *payload* from projections and retrieval and records
  the erasure obligation; the row itself and its ordering are retained (§9).
- **ACL:** readable within scope; writable per event type and role.
- **Retention:** indefinite. Payload erasure is a separate, explicit operation.
- **Indexes (contract-level):** `(project_id, seq)`, `(aggregate_type, aggregate_id, revision)`,
  `(project_id, work_id, seq)`, `(branch_id, seq)`.

### 6.6 `checkpoints`

Materialised from `checkpoint.recorded` events; the event stream remains the source.

- **Identity:** `checkpoint_id UUID PK` (= the recording `event_id`).
- **Ownership:** `project_id`, `work_id`, `branch_id`, `principal_id`.
- **Lifecycle:** insert-only. A newer checkpoint supersedes an older one for reads; both persist.
- **Immutable:** everything. `superseded_by_checkpoint_id` is the one field the projection sets, and
  it is set once, from an event, never edited.
- **Fields:** `objective`, `completed`, `pending_actions`, `blockers`, `modified_file_refs`,
  `evidence_refs`, `next_safe_action`, `expected_revision`, `content_digest`, `sensitivity_class`.
- **Revision:** inherits the work's revision at commit.
- **Tombstone:** removable from projections; its event stays.
- **Retention:** indefinite; superseded checkpoints may be pruned from the *projection* only, never
  from the ledger.

### 6.7 `facts`

- **Identity:** `fact_id UUID PK`.
- **Scope of the claim:** `(project_id, workspace_id NULLABLE, work_id NULLABLE)` — a fact scoped to a
  work does not silently apply project-wide.
- **Claim:** `subject`, `predicate`, `object` (all text; object may be JSONB for structured values).
- **Cardinality:** `cardinality` ∈ `single` | `multi`. **`single` means a new value supersedes the
  previous one for the same (scope, subject, predicate). `multi` never auto-supersedes** — two values
  coexist until one is explicitly retracted.
- **Temporal:** `valid_from`, `valid_to` (nullable = open), `recorded_at`. Overlapping conflicting
  claims are permitted and are marked, not silently resolved.
- **Status:** `active`, `superseded`, `retracted`, `conflicted`.
- **Lineage:** `superseded_by` (UUID, nullable), `promoted_from_candidate_id`.
- **Provenance:** `promoted_by_principal_id`, `promotion_event_id`, `evidence_refs`, `evidence_class`.
- **Revision:** CAS aggregate; `revision` bumps on supersede/retract/conflict-flag.
- **Immutable:** `fact_id`, claim triple, scope, `recorded_at`, promotion provenance.
- **Mutable via events only:** `status`, `valid_to`, `superseded_by`, `revision`.
- **ACL:** `work_read` to read; `fact_promote` to create, supersede, retract.
- **Tombstone:** removes from projections and marks for erasure; the promotion event remains.
- **Retention:** indefinite.

### 6.8 `fact_candidates`

*Addition beyond the table list frozen in the plan, and deliberate: candidates must be physically
unable to be read as facts. Sharing a table with a status column is one forgotten `WHERE` away from
serving an extraction as current state.*

- **Identity:** `candidate_id UUID PK`.
- **Fields:** claim triple + scope (as §6.7), `source` (what text it came from), `extractor`
  (component name), `model` and `model_version` (nullable — a deterministic extractor has none),
  `confidence` (numeric 0–1), `provenance` (event/evidence refs), `created_at`,
  `review_status` ∈ `pending` | `promoted` | `rejected` | `expired`, `reviewed_by_principal_id`,
  `reviewed_at`, `expires_at`, `sensitivity_class`.
- **Lifecycle:** created by `candidate_write`; leaves only through an explicit `fact.promoted`
  (naming principal and evidence) or rejection/expiry.
- **Hard prohibitions:** a candidate never changes work state, never appears in production bootstrap
  context, never writes a canonical fact, and never bypasses ACL or redaction.
- **Retention:** `expires_at` defaults to 90 days; expired candidates are deleted from this table
  (they are proposals, not history) while the proposing event remains in the ledger.

### 6.9 `provenance_refs`

- **Identity:** `ref_id UUID PK`.
- **Fields:** `subject_type` (`work`, `checkpoint`, `fact`, `candidate`, `event`), `subject_id`,
  `evidence_type` (`tool_result`, `file_digest`, `command_exit`, `receipt`, `external_doc`,
  `human_statement`), `evidence_class` (`observed`, `derived`, `asserted`, `unverified`),
  `locator` (opaque handle — a digest or an id, never a secret and never a raw path),
  `recorded_at`, `principal_id`.
- **Lifecycle:** insert-only, fully immutable.
- **Rule:** `asserted` and `unverified` are never presented as observed. An agent saying it ran a
  command produces `asserted`; a captured exit status produces `observed`.
- **Retention:** indefinite.

### 6.10 `write_receipts`

- **Identity:** `(principal_id, idempotency_key)` PK.
- **Fields:** `event_id`, `payload_digest`, `state` ∈ `committed` | `duplicate` | `conflict` |
  `rejected_policy` | `quarantined`, `seq` (nullable), `error_class` (nullable), `recorded_at`.
- **Lifecycle:** written **inside the same transaction** as the event. Immutable thereafter.
- **Rule:** `locally_queued` never appears here — it is a client state and has no server row.
  Projection status is tracked separately (§6.11) and is never conflated with commit status.
- **ACL:** a principal reads only its own receipts.
- **Retention:** minimum 90 days, and **never pruned before the retention floor of the backup that
  could restore the ledger behind it** — a receipt must outlive any restore that could otherwise
  make a committed write look uncommitted.

### 6.11 `projection_outbox`

- **Identity:** `outbox_id BIGSERIAL PK`.
- **Fields:** `event_id`, `projection` ∈ `qdrant` | `graphify`, `state` ∈ `pending` | `in_flight` |
  `applied` | `failed` | `dead_letter`, `attempts`, `not_before`, `last_error_class`
  (class only, never payload), `applied_at`.
- **Lifecycle:** inserted in the commit transaction; drained by a `projection_worker`.
- **Rule:** at-least-once with idempotent application. Applying twice is a no-op. Tombstones take
  precedence over any pending non-tombstone row for the same subject, at any lag.
- **Retention:** `applied` rows pruned after 30 days; `dead_letter` retained until reviewed.

### 6.12 `tombstones`

- **Identity:** `tombstone_id UUID PK` (= the issuing `event_id`).
- **Fields:** `subject_type`, `subject_id`, `scope` columns, `reason_class` (a class, never free text
  containing the offending content), `issued_by_principal_id`, `issued_at`,
  `erasure_required BOOL`, `erasure_completed_at`, `legacy_chunk_ids TEXT[]`.
- **Lifecycle:** insert-only and monotonic. **A tombstone is never deleted, never superseded, and
  never lost in a rebuild.**
- **Rule:** every rebuild replays the tombstone journal; a restored backup applies the journal
  *before* serving traffic. `legacy_chunk_ids` links pre-gateway Qdrant chunk ids to source document
  identity so historical deletions survive re-chunking.
- **Retention:** indefinite — the journal is the proof that a deletion happened.

### 6.13 `writer_epochs`

- **Identity:** `epoch BIGINT PK`, monotonically increasing.
- **Fields:** `opened_at`, `opened_by_principal_id`, `fenced_at`, `reason_class`,
  `drain_watermark_seq`, `notes_ref`.
- **Lifecycle:** opened by `admin`; fencing an epoch is itself an event.
- **Rule:** a write stamped with an epoch < current fenced epoch is rejected `writer_epoch_fenced`.
  Fencing is server-side and does not depend on the writer noticing.
- **Retention:** indefinite.
- **Payload (implemented):** `writer_epoch.opened` and `writer_epoch.fenced` carry
  `{epoch, reason_class, drain_watermark_seq?}`. `reason_class` is closed: `cutover`,
  `host_replaced`, `credential_rotation`, `incident`, `maintenance`. `epoch <= 0` is rejected —
  zero is what an omitted field decodes to.
- **Opening** an epoch that already exists is `policy_rejected`: two opens of one epoch mean two
  hosts believe they were admitted separately, and every event already stamped with it points at
  that row. **Fencing** an epoch nobody opened is `policy_rejected` and creates nothing. Fencing an
  already-fenced epoch leaves the first `fenced_at`, `reason_class` and watermark untouched — when
  an epoch stopped accepting writes is evidence, and a retry during an incident must not rewrite it.
- **`drain_watermark_seq`** defaults to the fencing event's own `seq`. That is correct by
  construction: nothing stamped with the old epoch can commit after the fence, so everything it ever
  wrote is at or below that point.
- **Order of a cutover:** open the new epoch, move the hosts onto it, then fence the old one. An
  admin whose own writes are stamped with the epoch being fenced can issue the fence (the check runs
  before it) but nothing after it.

---

## 7. Invariants

Each is testable and has fixtures (§10). IDs are stable; fixtures reference them.

| ID | Invariant |
|---|---|
| INV-01 | A retry of the same logical write reuses the same `event_id`. Regenerating it is a defect. |
| INV-02 | Same idempotency key + same payload digest ⇒ `duplicate`, and **no second effect** on any aggregate, projection or counter. |
| INV-03 | Same idempotency key + different payload ⇒ `idempotency_payload_mismatch`, nothing written. |
| INV-04 | An ACK is returned only after the PostgreSQL transaction is committed and WAL-flushed (`synchronous_commit=on`). ACK ⇒ durable against process crash. It does **not** imply survival of host or disk loss. |
| INV-05 | A connection lost after commit leaves the client in `unknown_commit_status`; resolution is receipt lookup or retry with the identical `event_id` and key — never a new key. |
| INV-06 | CAS on `expected_revision` prevents lost updates: two concurrent writers at the same revision produce one success and one `cas_conflict`. |
| INV-07 | An event carrying a revision lower than current never moves state backwards (`stale_revision`). |
| INV-08 | Conflicts are surfaced, never silently resolved last-write-wins. A conflicting proposal is preserved for explicit resolution. No LLM ever auto-merges a semantic contradiction. |
| INV-09 | A checkpoint is always a new event. No code path updates or deletes a prior checkpoint row. |
| INV-10 | A candidate becomes a fact only through an authenticated, authorised `fact.promoted` event naming a principal with `fact_promote` and at least one evidence ref. |
| INV-11 | `cardinality = multi` facts do not supersede each other automatically; only an explicit supersede/retract changes status. |
| INV-12 | A tombstone survives a full projection rebuild and a backup restore: after either, the tombstoned subject is absent from every projection. |
| INV-13 | ACL is applied before retrieval **and** before any text is sent to an embedding or LLM provider. Out-of-scope content is never a candidate for egress. |
| INV-14 | Secret/credential/PII-shaped content is rejected before local persistence, before ledger commit and before external egress. Rejection logs metadata and error class only — never the payload. |
| INV-15 | Recalled text is untrusted background data. It can never act as an instruction, authorise a promotion, widen a scope, or trigger a write. |
| INV-16 | A subagent principal may submit evidence linked to the parent work but cannot mutate the parent's active state. Orphaned subagent evidence is non-authoritative. |
| INV-17 | A client-declared scope is intersected with the principal's granted scope. A request can narrow; it can never widen. |
| INV-18 | After fencing, writes stamped with an older writer epoch are rejected, regardless of their content or age. |
| INV-19 | A branch rewind creates a new branch with lineage. Events on the abandoned branch remain queryable; nothing is deleted or renumbered. |
| INV-20 | Canonical ordering uses `events.seq`. Client `occurred_at` values — including out-of-order and clock-skewed ones — never change ordering or conflict outcomes. |

---

## 8. Candidate and fact policy

Extraction is non-authoritative by construction. A candidate carries `source`, `extractor`, `model`
and `model_version` (nullable for deterministic extractors), `provenance`, `confidence`, `created_at`,
`review_status` and `expires_at`.

Promotion requires: a principal holding `fact_promote` within the fact's scope, at least one evidence
ref, and an explicit `fact.promoted` event. The promotion event records who promoted, on what
evidence, and from which candidate. **Extracted text cannot authorise its own promotion** — INV-15
applies to the candidate's own content, including a candidate whose text says it has been approved.

A `single`-cardinality promotion supersedes the previous value for that (scope, subject, predicate)
and records `superseded_by`. A `multi` promotion adds a value and leaves the others `active`. Two
`active` values that contradict each other are marked `conflicted` and both are returned to the
reader with their evidence, so a human resolves it. Nothing auto-resolves.

---

## 9. Deletion and tombstone policy

Five distinct operations, deliberately not synonyms:

1. **Logical deletion** — a `tombstone.issued` event. Monotonic, replayable, never removed. The
   subject stops being served as current.
2. **Projection deletion** — removal from Qdrant/Graphify. A consequence of (1), applied
   at-least-once, and re-applied on every rebuild.
3. **Source/archive retention** — the sanitised source archive keeps the material needed to rebuild
   projections, and is itself subject to the tombstone journal.
4. **Physical erasure** — actually overwriting or dropping payload bytes. Tracked by
   `erasure_required` / `erasure_completed_at`, scheduled explicitly, never implied by (1).
5. **Backup expiry** — old backups still contain the data until they age out. A restored backup
   applies the deletion journal **before** serving traffic.

**Replay is a real step, not a figure of speech.** A mapping row that arrives *after* a deletion was
ordered — imported from the pre-gateway corpus, or restored from a backup taken before the tombstone
— is readable again while a tombstone naming it sits in the journal. `enowx-rag memgw history replay`
re-applies the journal to `legacy_chunk_map` using the same predicate the live writer uses, marks
each row with the tombstone's own `issued_at` (earliest wins, an existing mark is never moved later),
and is idempotent. Run it after every import and after every restore, **before** anything queries the
corpus.

**Deleting by document id must use the same id mapping as writing.** Qdrant accepts only a UUID or an
unsigned integer as a point id, so the provider stores a document under a UUIDv5 of its id; a
deletion that sends the raw id addresses a point that was never written. A raw non-UUID id fails
loudly, but an id that happens to parse as a UUID would delete nothing and report success — which is
the worst outcome a deletion can have.

**Vector deletion does not retract data already sent to an external embedding provider.** Deleting a
point in Qdrant removes it from search; it does nothing about the text Voyage already received. That
asymmetry is the reason every egress path — embedding, reranking, LLM call, shadow evaluation — must
pass the redaction and policy gate *before* transmission. A gate after the fact is not a gate.

A memory tombstone deletes memory. It never deletes repository code, containers or volumes.

---

## 10. Fixture suite

Location: `mcp-server/pkg/memgw/contract/testdata/`. Machine-readable contract in `contract.json`;
one file per scenario in `fixtures/`. Fixtures are JSON, not Go structs, because the adapters that
must honour the same contract are TypeScript (OMP, OpenCode), Python (Hermes, Claude hooks) and Go —
a fixture encoded in Go structs would only be enforceable in one of them.

`contract_test.go` validates the fixtures against the frozen contract: known event types, known error
classes, known invariant ids, envelope completeness, bounds, `payload_digest` correctness over
canonical JSON, and the scenario-specific expectations each fixture declares. It tests the observable
contract; it does not test an implementation, because there is none yet.

| Fixture | Invariants |
|---|---|
| `duplicate-retry` | INV-01, INV-02 |
| `payload-mismatch` | INV-03 |
| `concurrent-cas` | INV-06 |
| `out-of-order-event` | INV-07, INV-20 |
| `branch-rewind` | INV-19 |
| `conflicting-facts` | INV-08 |
| `multi-valued-facts` | INV-11 |
| `tombstone-rebuild` | INV-12 |
| `principal-scope-denial` | INV-13, INV-17 |
| `candidate-promotion` | INV-10 |
| `subagent-evidence` | INV-16 |
| `writer-epoch-fencing` | INV-18 |
| `secret-pii-rejection` | INV-14 |
| `recalled-prompt-injection` | INV-15 |
| `unknown-ack-reconciliation` | INV-04, INV-05 |

---

## 11. Operational blockers carried into Phase 3

These are not schema questions and are not resolved by this contract. They are prerequisites for
*deploying* a ledger, and remain open:

1. **`RAG_ADMIN_TOKEN` rotation** — treated as compromised (Phase 0.5 §4). Runbook prepared, not run.
2. **Off-host backup destination** — undecided. Today every backup shares a disk with production.
3. **`archive_mode` maintenance window** — enabling WAL archiving restarts the cluster shared with
   `axonhub`. Without it there is no PITR, so the ledger's RPO is "the last dump", and no PG dump
   exists yet.
4. **nginx token moved to a 0600 `include`** — pending.
5. **Phase 0.5 commit and deployment** — the build-identity patch is in the working tree, uncommitted
   and undeployed; production still reports `"dev"`.
6. **`/api/version` verification over HTTP** — only exercised via unit test and the `version`
   subcommand so far.

Until (2) and (3) are done, no document may claim zero acknowledged-event loss for anything beyond
process crash. INV-04 is worded to match what a single node can actually promise.

## 12. Unverified assumptions

- That the existing PG16 cluster's spare capacity (2 vCPU, 12 GB RAM shared, 27 GB free disk) is
  sufficient at the ratified SLOs. Not benchmarked; Phase 3/8 work.
- That `pgcrypto`/`uuid-ossp` install cleanly on this cluster. They are listed as available, not
  installed, and installing them is a production change.
- That client-generated UUIDv7 ordering is acceptable everywhere it is convenient. The contract does
  not depend on it: `seq` is the only ordering authority.
- That every host adapter can produce a stable `idempotency_key` across a crash. Verified for none of
  the six; it is the first thing each adapter's fixture must prove.
- That the 3000-character write-guard limit and the 256 KiB event payload bound do not collide in
  practice for real checkpoints. Not measured.
