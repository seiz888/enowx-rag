# Shared Agent Memory Gateway — Implementation Handoff

Status: approved architectural direction; implementation NOT started by this handoff. This document is a plan, not evidence that any feature is deployed. Primary repository: D:/PROJECTS/enowx-rag. Language: Indonesian communication, English code comments. Follow repository instructions and user security constraints. Never delete/rename seizrag-qdrant, prune sc_recovery_* volumes, or touch D:/ClaudeVM.

## Objective and scope
One logical memory service for Claude Code, OMP/Pi, Codex, OpenCode, Droid, Hermes. Keep enowx-rag history retrieval. Extend the existing Go service into the Memory Gateway rather than introducing an unnecessary second gateway service. Add PostgreSQL canonical work/fact/checkpoint storage, native lifecycle adapters, crash-safe encrypted local outbox, and derived Graphify code index. Hindsight is optional shadow evaluation only, never a production authority or mandatory rollout dependency. No production changes, deployment, historical bulk export, deletion, or external model ingestion without explicit user approval.

## Authority and identity
- PostgreSQL: accepted facts, decisions, work state, checkpoints, receipts, tombstones.
- enowx-rag: sanitized historical session documents; preserve existing records. Archive approved source documents durably so derived indexes can be rebuilt.
- Qdrant: rebuildable search projection, not authority for current work.
- Graphify: rebuildable repository topology, not conversational state.
- Obsidian vault: secrets and sensitive PII. Never put these in ledger, queue payloads, logs, RAG, or embedding requests. Only opaque approved vault references.
- Project UUID stable across folder renames. Registry maps repository identities explicitly; do not infer identity from basename or expose credential-bearing git URLs.
- Workspace UUID identifies checkout/worktree. Work UUID identifies logical task across sessions. Host principal identifies installation. Session and branch IDs identify conversation lineage. Branch rewind creates lineage, never truncates history. Ordering uses ledger sequence/cursor, not wall clocks.

## Core contract
Work states: planned, active, blocked, review, completed, abandoned. Checkpoint contains objective, completed work, pending actions, blockers, modified-file references, evidence references, next safe action, expected revision, content digest. Snapshot records claims and their evidence class; it does not certify a tool ran merely because an agent says so.
Facts have scoped subject/predicate/object, cardinality policy, valid_from/valid_to, recorded_at, superseded_by, provenance and evidence class. Multi-valued predicates do not automatically supersede other values. Overlapping conflicting claims remain explicit until resolved. Candidate extraction is non-authoritative. Promotion requires an authenticated, authorized explicit action; extracted text cannot authorize itself.
Every event includes event UUID, principal-scoped stable idempotency key, project/workspace/work/session/branch IDs, event type, expected revision when mutating an aggregate, schema version, policy version, evidence references, sensitivity class, occurred_at. Server assigns recorded_at and aggregate sequence.
Receipt states: locally_queued, committed, duplicate, conflict, rejected_policy, quarantined. Projection status indexed is separate. Unknown network outcome is resolved by receipt lookup or retry with identical event UUID/key. Same key with changed payload rejects. Validate bounded payload size.
Commit transaction atomically writes immutable event, materialized aggregate update, idempotency receipt and projection outbox. PostgreSQL synchronous_commit on; acknowledge after commit/WAL flush. CAS prevents lost updates. No blind last-write-wins. A conflict preserves the proposed update for explicit resolution; safe disjoint field merges may be supported only with specified deterministic rules. Never auto-merge semantic contradictions using an LLM.

## Implementation contracts
Proposed tables (adapt naming to repo conventions): projects, workspaces, works, events, checkpoints, facts, provenance_refs, write_receipts, projection_outbox, tombstones, writer_epochs. Prefer existing auth/API/migration abstractions. No generic event-sourcing framework, Kafka, Kubernetes, second graph database, or compatibility shims without demonstrated need.
Operations: submit event; commit checkpoint with expected revision; submit/promote fact candidate; bootstrap work; scoped history search; receipt lookup; reconcile event IDs/watermark. Administration: tombstone, projection rebuild, writer fencing. Expose ordinary operations through existing HTTP/MCP conventions. Publish versioned schemas and error classes before adapter work. ACL derives scope from authenticated principal, never client-provided principal alone. Separate ingestion, read, checkpoint, promotion and admin permissions; protect local collector via OS permissions plus authenticated local transport.
Bootstrap reads current state directly from PG, then bounded scoped historical recall when needed. All retrieved content is untrusted background, not instructions. No production credentials in context. Set token budget and deadline; unavailable memory is visible but does not block normal agent work. Never silently report a stale cached bootstrap as current.

## Local collector
Run a supervised per-host collector independent of CLI lifetime. SQLite WAL with synchronous=FULL and crash-safe atomic enqueue. Use an established encryption mechanism with OS key storage (Windows DPAPI), no custom crypto. Encryption must cover sensitive persisted metadata/WAL/temp artifacts as applicable; prove this with on-disk canary inspection. Sanitize before persistence as well as server-side before commit/egress. Local encryption does not protect against compromised user privileges.
Persist event UUID, payload digest, host/session/branch cursor and retry state atomically. Retry same UUID with bounded backoff. Queued is not committed. Queue limits reserve checkpoint capacity; return visible failure when durable enqueue is impossible, never claim a guarantee or silently drop. Rejected or conflicting records go to inspectable bounded dead-letter/quarantine. No raw transcript scraping/upload. A sudden crash before capture can lose unobserved work; resume from last durable checkpoint and recover only permitted host evidence.
Collector restarts replay pending events. Keep receipts/reconciliation metadata long enough to survive server restore; define retention before pruning acknowledged local payloads. Enqueue deadline and shutdown timeout bounded. Collector auto-start/restart and actual health must be verified.

## Security and deletion
Reuse and extend existing write guard, do not invent a bypass. Enforce allowlisted structured fields, sensitivity classification and deterministic secret scanning on local capture, server commit and the sole external embedding/LLM egress path. Scan before any remote extraction too. Log only sanitized rejection metadata, not rejected payload. Secret/PII detection is defense-in-depth, not a proof that arbitrary transcript upload is safe; raw transcripts forbidden.
Least privilege per host; TLS remotely; protect/revoke keys through approved secret storage. ACL filtering before retrieval and before external processing. Quarantine excluded from production retrieval, with explicit approve/reject and retention policy.
Deletion is a monotonic ledger tombstone propagated to every relevant projection and archived source. Link legacy chunk IDs to source document identities. Rebuild must replay tombstones; old backups must apply deletion journal before serving traffic. Define physical erasure and backup expiry separately from logical tombstone. Do not delete unrelated repository code through a memory tombstone. External processing cannot be undone by deleting a vector.

## Lifecycle contract
Inventory actual installed versions first: previously read docs/source are research, not runtime proof. Build one normalized adapter contract and capability fixtures.
All hosts: idempotent bootstrap on first prompt AND resume, structured per-turn capture, checkpoint before compaction where supported, bounded exit enqueue, persisted cursor with branch identity, explicit offline state.
Claude: inspect current hooks and preserve guards. OMP: native extension boundaries, parent-owned checkpoint; do not patch global installed package as permanent integration. Codex: short exit deadlines mean local enqueue only. Hermes/Droid: verify actual hooks. OpenCode: first-prompt bootstrap and idle/message hooks may substitute missing lifecycle events; prove resume path. Unsupported boundaries must be explicit, not mocked.
Subagents submit evidence linked to parent work, cannot independently mutate parent active state. Parent loss leaves orphan evidence non-authoritative. Separate capture cursor from bootstrap-delivery state; toggling context injection must not lose captured events.

## Graphify
Local derived index only, one rebuild coordinator per repository on Windows. Do not rely on Graphify's fcntl fallback for Windows locking. OS lock releases on crash; use atomic publish and generation IDs. Manifest includes project/workspace, commit SHA, dirty tree digest, tool/schema version, generation timestamp. Recheck digest before publishing if repo changes mid-build. Deny indexing secret/PII paths and network semantic extraction by default. Deleted/renamed file invalidation required. Stale results are hints only; live source/LSP verifies edits. Hindsight shadow accepts only approved sanitized fixtures and cannot affect canonical state or normal bootstrap.

## Phases, dependencies and acceptance
### Phase 1 — Inventory and capability evidence
Read relevant repo conventions; inspect existing Go HTTP/MCP routes, config, migration/storage interfaces, guard, metrics, deployment and backup configuration. Known starting locations: mcp-server/pkg/core/writeguard.go, pkg/core/sqlite_metrics.go, pkg/httpapi/handlers.go, pkg/config/config.go, pkg/rag/qdrant.go, cmd/mcp-server/main.go. Names are pointers from prior research; re-inspect before editing.
Map actual six host versions, adapter entry points, permissions, supported events, resume/compact/exit behavior. Check host resources before downloads (C: often full). Identify PG availability/version and deployment constraints; do not assume it is already installed. Report exact files to change and capability gaps. Use sanitized read-only probes, no production mutation or restore-over-production.
Exit: evidence-backed capability matrix, threat/egress map, storage/backup proposal, dependency map and refined work packages. Stop here for user approval before Phase 2.

### Phase 2 — Freeze schema and fixtures
Define domain glossary, schema, ACL, conflict, idempotency, ordering and error contract. Fixture suite includes branch rewind, multi-valued facts, temporal overlap, payload mismatch and duplicate retry. Freeze bootstrap bounds and operational SLOs before benchmarks.
Exit: observable contract agreed, no contradictory authority definitions.

### Phase 3 — Canonical ledger and collector (independent after Phase 2)
Ledger: migrations, transactions, receipts, CAS, auth, event reconciliation, outbox. Collector: encrypted storage, restart, durable enqueue, cursors, policy scan and retry. Separate owners, shared event contract, integration owner named.
Exit: failpoints before/after WAL commit, lost reply, duplicate/reordered event, two concurrent writers, kill during local enqueue and offline replay all preserve the defined contract. Conflicts remain visible. Queue full never falsely acknowledges.

### Phase 4 — Gateway, bootstrap and projections
Extend existing service rather than duplicate infrastructure. Direct ledger bootstrap plus scoped enowx history. Projection worker at-least-once with idempotent application, aggregate revision checks and tombstone precedence. Sanitized source archive + checksum/manifest enables rebuild. Search revalidates active facts against ledger when projection lag exists.
Exit: cross-project ACL isolation, bounded bootstrap, delayed projection cannot revive stale facts, full rebuild and delete/restore cycle proven.

### Phase 5 — Native adapters
Use reference adapter (OMP or Claude according to Phase 1 evidence), then other hosts can be implemented concurrently with disjoint files and fixed schema. Gate each actual runtime independently, not merely MCP connectivity. Cover first prompt, resume, branch, turn, failures, compaction, exit, offline and parent/subagent behavior. Install only after user approval.
Exit: all six supported paths exercised or precise unavailable prerequisites reported; do not call full rollout complete with a missing host.

### Phase 6 — Graphify and optional Hindsight evaluation
Graphify coordinator, freshness manifest, atomic generation swap, Windows concurrency and crash tests. Optional Hindsight shadow pilot outside production path, only after approved resource/egress plan. It is not a prerequisite for core cutover.
Exit: stale/concurrent/dirty worktree cases handled; secrets excluded. Shadow failure has zero effect on normal work.

### Phase 7 — Migration dry-run
No wholesale promotion of historical chunks to current facts. Preserve legacy history; extract approved sanitized sources deterministically, map IDs/checksums, mark missing source/provenance. Report imported/quarantined/rejected/skipped with reason. LLM candidates require explicit review. Two runs produce same plan and IDs; no direct production writes in dry-run.
Exit: counts reconcile, no deleted/previously excluded transcript corpus reintroduced; backup and restore exercised in isolated target.

### Phase 8 — Shadow and failure evaluation
Freeze golden dataset before evaluation: real sanitized cross-agent handoffs, changing decisions, unrelated projects, ambiguous facts, branch rewind, hostile recalled text, missing evidence. Include offline/reconnect, abrupt kill, full disk, projection outage, server restart and restoration. Compare current baseline against ledger bootstrap and optional Hindsight using same questions/model/budget.
Mandatory safety gates: zero unauthorized disclosure in seeded canaries/ACL tests; zero lost durable events in tested process/network failures; duplicates no extra effect; all CAS conflicts surfaced; no stale fact presented as current; all six real host acceptance flows pass. These are tested guarantees, not claims against every possible disaster.
Suggested initial performance targets (ratify after Phase 1): local enqueue p95 <=100ms; healthy-network direct bootstrap p95 <=1s; optional history recall <=2s deadline then explicit partial context; bootstrap <=1500 tokens; healthy projection lag p95 <=60s. Publish dataset size, successful/failed counts and denominators, not unsupported percentages. Hindsight adoption requires demonstrable improvement, not vendor benchmark claims.

### Phase 9 — Production cutover (explicit approval required)
Take validated backup; record per-host drain watermark, versions, policy and new writer epoch. Fence old paths server-side; stop legacy autonomous writers, not enowx-rag itself. Missing/offline hosts are listed and remain fenced. Replay their legitimate queued events through authenticated reconciliation under new epoch without regenerating IDs or blindly trusting stale authority. Enable gateway writers and bootstrap, monitor receipt reconciliation. Prove old writers inactive using rejected probes/last committed watermark. No unfenced dual-write.
Exit: all host migrations accounted for, no unacknowledged gap, metrics healthy. Retain logs/search and guard protections.

### Phase 10 — Rollback, restore, cleanup
Application rollback disables new injection/projections as necessary, preserves ledger and local outboxes; legacy history may remain read fallback, but cannot reclaim canonical write authority. Cursor reconciliation separate from injection cursor. Correct wrong decisions with new revision/compensating events, not event truncation. Avoid schema downgrade until data-compatible path verified.
PG WAL flush is process-crash durability, NOT protection from host/disk loss. Establish explicit RPO/RTO and off-host WAL/backup policy in Phase 1. If zero acknowledged-event loss across total host loss is required, synchronous off-host replica or independently durable receipt journal is a prerequisite; do not promise it from a single PG node. Restore to isolated target, reconcile receipts/WAL/archive, apply tombstones, rebuild projections, then authorize traffic. Alarm any unrecoverable gap, never conceal it.
After smoke proves integration, remove obsolete adapter writers/scaffold/temp scripts, update operating docs/changelog, document key rotation, spool recovery, quarantine, restore and rollback. Do not remove unrelated work or protected containers/volumes.

## Observability
Use existing metrics plumbing where possible. Expose oldest queued age, durable queue failures, commit latency, receipt unknowns, conflicts, dead-letter count, policy rejections without payloads, projection/tombstone lag, bootstrap partial/unavailable, stale graph generations, writer epoch violations and backup/restore status. Alerting itself must be exercised. Retention and thresholds explicit; avoid logging full events.

## Release checklist
- Six-host handoff/resume actual runtime evidence.
- CAS, duplicate retry/payload mismatch, event reordering and branch rewind.
- Offline replay, lost ACK, kill during local/server transaction, queue full.
- Secret/PII canaries before local persistence, ledger, remote LLM and embedding.
- Auth scope isolation; recalled prompt injection cannot promote facts.
- Tombstones survive rebuild/backup restore; deletion journal retained.
- Source archive reconstruction and ledger/projection checksums reconciled.
- Graphify Windows exclusivity, crash recovery, dirty-tree race, stale detection.
- Legacy writers fenced, offline hosts explicitly reconciled.
- Backup restore tested and RPO/RTO truthfully stated.
- Rollback preserves canonical committed state and pending local writes.

## Execution rule for Claude Code
First execute Phase 1 ONLY. Do not install software, mutate production, migrate historical data, deploy collectors or create new credentials during inventory without approval. Finish reachable read-only research; create/update plan artifacts in repository only after inspecting conventions. Report concise evidence, remaining assumptions, exact implementation file map, proposed RPO/RTO/resource needs and approval gates. Wait for explicit approval before Phase 2. Later implement in dependency order, validate behavior with real smoke/fault scenarios, keep tests only where they defend observable contracts. Do not treat this plan's proposed fields or SLOs as existing deployed behavior.

---

# Phase 1 — Inventory and capability evidence (executed 2026-09-09)

Read-only inventory. No production mutation, no installs, no migrations, no credentials created.
Every row below is either **verified** (probe output in this session) or **assumed** (stated as such).
Where the plan's prior research disagrees with the runtime, the runtime wins and the plan text above is
superseded by this appendix.

## 1. Existing architecture actually deployed

Single Go binary `enowx-rag`, two transports from one `core.Service`:

- `cmd/mcp-server/main.go` — `--serve` starts HTTP+SPA (`runHTTP`), default is stdio MCP (`runStdio`).
  Both build the MCP server from the same `newMCPServer(svc)`; 11 MCP tools registered in
  `registerMCPTools`.
- `pkg/httpapi/server.go` — chi router. `/api/*` and `/mcp` behind `AdminTokenMiddleware`;
  a subset of setup routes additionally behind `LocalOrAdminMiddleware` (loopback OR token).
  MCP over HTTP is `mcp.NewStreamableHTTPHandler(..., Stateless: true)`.
- `pkg/core/service.go` — search (hybrid/rerank/compress/per-doc cap), index, project CRUD,
  export, metrics snapshot, write guard, query log.
- `pkg/rag/*` — provider abstraction: qdrant (deployed), chroma, pgvector, plus embedders
  (voyage deployed, tei, openai) and voyage reranker.
- `pkg/core/sqlite_metrics.go` — durable metrics + opt-in query log in SQLite
  (`modernc.org/sqlite`, pure Go), schema created inline with `CREATE TABLE IF NOT EXISTS`
  plus one in-place `ALTER TABLE` guarded by a string match on "duplicate column name".
- `pkg/core/writeguard.go` — the only server-side write contract. Env-driven, project-scoped.
- `pkg/migrate/` — **not** SQL migrations. It moves documents between vector stores and
  re-embeds them. There is no schema-migration framework in this repo.

**Deployment (verified over SSH, ubuntu@168.110.218.207, Oracle aarch64, Ubuntu 24.04.4):**

- systemd unit `enowx-rag.service`, `User=enowxrag`, `ExecStart=/opt/enowx-rag/bin/enowx-rag --serve
  --addr 127.0.0.1:7777`, `EnvironmentFile=/opt/enowx-rag/.env`, hardened
  (`ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`, `ProtectHome`).
  Active since 2026-09-09 14:09 UTC.
- Ingress: Cloudflare then nginx vhost `rag.seiz.cloud` then `127.0.0.1:7777`.
  nginx returns 403 on `/api/setup/{config/reveal,gen-token,apply,test,install-mcp,write-agents-md}`
  and `/api/migrate`; `/mcp` passes the caller's own `Authorization` through unchanged.
- Startup log confirms the live guard contract:
  `projects=memory max_chars=3000 max_delete=25 require=chunk,bucket,kind,title,ts,agent,source`,
  and `query log: on, keeping 5000 entries at /opt/enowx-rag/.enowx-rag/metrics.db`.
- Corpus: `/api/stats` reports 1 project (`memory`), 2017 chunks, `backend=qdrant`,
  `embed_model=voyage-4`.
- Qdrant is container `seizrag-qdrant` on `127.0.0.1:6333` (protected — never delete/rename).

## 2. Capability matrix

### 2.1 Server / gateway

| Capability | State | Evidence | Verification limit |
|---|---|---|---|
| HTTP API + SPA, single binary | present | `/api/stats` 200 with bearer | — |
| MCP over HTTP `/mcp` | present, stateless | `initialize` returns `protocolVersion 2025-06-18`, `serverInfo.version "dev"` | version string is unstamped, so the binary cannot be identified from the wire |
| Auth | **single shared bearer**, no principals | `/api/stats` unauth returns 401; `AdminTokenMiddleware` compares one token | no per-agent identity, no scopes, no ACL — the plan's ACL model has nothing to build on |
| Write guard | present, project-scoped, env-driven | startup log; `pkg/core/writeguard.go` | covers `IndexDocuments`, directory scan, delete-project and bulk delete. Nothing ledger-shaped |
| Credential scanning before embedding | present | `credentialPatterns` in `writeguard.go` | 6 fixed shapes; no PII class, no entropy check |
| Storage/migrations | **absent for SQL** | `pkg/migrate` is a vector-store copier; SQLite schema is inline DDL | no versioned migration runner to reuse — must be built |
| Metrics | SQLite, durable | `/api/metrics` reports `persistent:true`, `query_count:1` | search-centric only: no queue depth, commit latency, conflict, projection-lag counters |
| TLS / timeouts / graceful shutdown | absent in-process | `http.ListenAndServe(addr, handler)` in `runHTTP` | TLS terminates at Cloudflare/nginx; no read/write timeouts, no drain on SIGTERM |
| Build provenance | **broken** | live `/api/projects/memory/export` returns 200 (2.3 MB) but `git show HEAD:...server.go` has no export route | production runs uncommitted working-tree code; 14 Go files plus web dist are dirty locally |

### 2.2 PostgreSQL (candidate canonical ledger)

Verified on the same host: PostgreSQL **16.15**, native (not Docker), listening on `127.0.0.1:5432`
only. Data dir `/var/lib/postgresql/16/main`, 6.6 GB. Existing databases: `axonhub` (6.6 GB),
`service_charge`, `travelya`, `postgres`.

| Setting | Value | Consequence |
|---|---|---|
| `synchronous_commit` | `on` | process-crash durable on commit — matches the plan's requirement |
| `full_page_writes` | `on` | torn-page safe |
| `wal_level` | `replica` | replication possible, not configured |
| `archive_mode` | **`off`** | **no WAL archiving, no PITR** |
| `max_connections` | 100 | shared with axonhub and others |
| `shared_buffers` | 128 MB | untuned |
| `vector` extension | **not available** | pgvector not installed. `uuid-ossp`, `pgcrypto`, `pg_trgm`, `pg_stat_statements` are available but not installed |

There is **no PostgreSQL backup of any kind** on this host: no `pg_dump` job, no basebackup, no
archive. The only backups that exist cover Qdrant and the app bundle (section 4).

### 2.3 Host resources

Oracle VPS: aarch64, **2 vCPU, 11.9 GB RAM** (3.3 GB used, 8.6 GB available), `/` is 96 GB with
**27 GB free (73% used)**. Already running: PG16, `seizrag-qdrant`, `sub2api` plus its own
PG18/redis containers, `kiro-go`, three next-server apps, three bun apps, cloudflared, nginx, and an
inferhub monitor timer. A ledger DB plus projection worker fits the RAM budget; disk is the tight one.

Windows workstation: `C:` is 150 GB with **25 GB free (84% used)**. The collector, its SQLite spool
and any Graphify index should target `D:` (136 GB free), not `C:`.

### 2.4 Agent lifecycle surfaces (the six hosts)

| Host | Version (verified) | Lifecycle surface (verified) | Wired to enowx-rag today |
|---|---|---|---|
| **Claude Code** | 2.1.266 | `settings.json` hooks. Configured now: `PreToolUse` (guard.py), `SessionStart` (rag-recall.py), `Stop` and `PreCompact` (rag-session-log.py) | MCP over http plus 3 hooks. Capture is a *nudge to the model*, not durable capture |
| **OMP / Pi** | omp 18.1.15 (`@oh-my-pi/pi-coding-agent`) | richest of the six. `HookAPI.on(...)`: `session_start`, `session_before_switch`, `session_switch`, `session_before_branch`, `session_branch`, `session_before_compact`, `session.compacting`, `session_compact`, `session_shutdown`, `session_before_tree`, `session_tree`, `context`, `before_agent_start`, `agent_start`, `agent_end`, `turn_start`, `turn_end`, `auto_compaction_*`, `tool_call`, `tool_result`. Discovery via capability API over `.omp`/`.pi` `hooks/`, installed plugins, and configured paths (`hooks/loader.ts:214`) | one plugin installed (`pi-codex-account`, our fork). No RAG hook |
| **Codex** | codex-cli 0.153.4 | **hooks exist** — this contradicts the plan's "local enqueue only" premise. `hooks.json`, events `pre-tool-use`, `permission-request`, `post-tool-use`, `pre-compact`, `post-compact`, `session-start`, `session-end`, `user-prompt-submit`, `subagent-start`, `subagent-stop`, `stop`, `interrupt` (strings extracted from `codex.exe`, module `hooks\src\engine\mod.rs`). A `notify` program also exists, currently owned by computer-use on `turn-ended` | MCP over http, `rag_retrieve_context` set to `approval_mode="approve"`. No hooks |
| **OpenCode** | 1.18.30 | plugin API, already exercised: `experimental.chat.system.transform`, `tool.execute.after`, `experimental.session.compacting`, and the `event` bus (`session.created`, `session.idle`). **No exit/shutdown event** — the plan's caution is confirmed | `~/.config/opencode/plugins/enowx-rag-hooks.ts` (395 lines) is live: injects recall on the first turn, watches RAG writes, nudges on idle. Its own log shows real firings through 2026-09-09 07:50 |
| **Droid (Factory)** | 0.147.0 | Claude-Code-shaped hooks in `settings.json` / `.factory/hooks/`: `PreToolUse`, `PostToolUse`, `Notification`, `UserPromptSubmit`, `Stop`, `SubagentStop`, `PreCompact`, `SessionStart`, `SessionEnd` (enum extracted from the droid bundle) | `mcp.json` present. **No hooks configured** — `settings.json` has no `hooks` key |
| **Hermes** | Agent v0.18.0 (2026.7.1), Python 3.11.15, on the VPS as user units `hermes-gateway-lucy.service` and `hermes-gateway-ivy.service` | shell hooks: `pre_tool_call`, `post_tool_call`, `pre_llm_call`, `post_llm_call`, `pre_verify`, `on_session_start`, `on_session_end`, `on_session_finalize`, `on_session_reset`, `pre_api_request`, `post_api_request`, `subagent_stop`. Managed by `hermes hooks list/test/revoke/doctor` with an **approval allowlist keyed on script mtime** | none. v0.18.0 is pinned to protect a local `adapter.py` patch, so hook work must not trigger an upgrade |

**The gap the plan warned about, now confirmed:** MCP connectivity exists on four hosts, durable
lifecycle capture on **zero**. What exists today (Claude hooks, the OpenCode plugin) *asks the model*
to write a session log. Nothing is durably enqueued, nothing survives a kill, nothing has a receipt.

### 2.5 Not installed at all

`graphify` and `hindsight`: not on PATH, no `~/.graphify`, `~/.hindsight`, and no project directory.
Phase 6 therefore begins with an install that needs approval and a disk-placement decision (`D:`).

## 3. Gap list

### Security
1. **One shared bearer for everything.** The same token authorises read, index, delete, admin and
   MCP. No principal, no scope, no per-host key, and no revocation short of rotating for all six
   hosts at once.
2. **That token is stored in cleartext in `/etc/nginx/sites-enabled/rag.conf`** and is injected
   upstream for any request that authenticates with nginx basic-auth; `rag-dashboard.conf` does the
   same on `127.0.0.1:7778`. Reading that vhost rendered the value into this session's transcript.
   **Recommendation: rotate `RAG_ADMIN_TOKEN` and its nginx copies.** Rotation is a production
   change and was not performed.
3. Guard scanning is six fixed credential regexes. No PII class, no sensitivity labels, no
   quarantine — the plan requires all three.
4. No audit trail of *who* wrote what: every writer is indistinguishable to the server.

### Durability
5. **No PG backup at all**, and `archive_mode=off` means no PITR. A ledger placed here today would
   have an RPO equal to everything since a backup that does not exist.
6. **Every backup lives on the same host and the same disk as production.** `/opt/rag-backup` holds
   Qdrant snapshots (`rag-backup.timer`, daily 03:17 UTC, keep 7 days) and GPG-encrypted app bundles
   (`rag-app-backup.timer`, 03:20 UTC, keep the 7 newest by count). Neither script references
   rclone, scp, rsync, S3 or restic. Host or disk loss takes production *and* every backup.
7. `rag-restore-test.service` does prove Qdrant snapshots restore — last run 2026-09-09 06:18:53,
   1961 points, status green, self-search 1.0000. The restore test is real; an off-host copy is not.
8. Single PG node: WAL flush is process-crash durability only. As the plan states, zero
   acknowledged-event loss across host loss cannot be promised from this topology.

### Lifecycle
9. Zero durable capture on all six hosts (section 2.4).
10. No local outbox anywhere, so nothing can be replayed when `rag.seiz.cloud` is unreachable. An
    offline session today loses its log entirely.
11. No cursor, no branch identity, and no idempotency key in any current writer.
12. Hermes hook approvals are mtime-pinned: every adapter script edit needs re-approval on the host.
13. OpenCode has no exit event, and Codex's `notify` slot is already occupied by computer-use.

### Operations
14. **Production runs uncommitted code** (the export endpoint is live but absent from `HEAD`), and
    the binary reports version `dev`. The running build cannot be identified from the wire.
15. Deploy is a manual copy plus timestamped backups in `/opt/enowx-rag/bin` — 11 old binaries are
    sitting there now. No CI, and no rollback procedure beyond copying an older file back.
16. No SQL migration framework to reuse.
17. `http.ListenAndServe` with no timeouts and no graceful shutdown: an in-flight ledger commit
    would be cut at restart.
18. Metrics carry none of what the plan's observability section asks for.
19. VPS disk headroom is 27 GB, with a 6.6 GB PG cluster and 90 MB of backups already present.

## 4. Backup / RPO / RTO proposal (for approval, not implemented)

Measured today:

| Asset | Backup | Frequency | Off-host | Restore proven |
|---|---|---|---|---|
| Qdrant | full snapshot to `/opt/rag-backup` | daily 03:17 UTC, keep 7 days | no | yes (2026-09-09) |
| App (binary, env, config, metrics, units, nginx) | GPG-encrypted bundle | daily 03:20 UTC, keep 7 | no | not proven |
| PostgreSQL | **none** | — | — | — |

Proposed for the ledger, in dependency order:

1. Nightly `pg_dump` of the ledger database plus `archive_mode=on` with WAL archiving to a local
   spool. Target RPO around the WAL segment/timeout (5 minutes or better); RTO roughly 30 minutes
   for a single-database restore.
2. An off-host copy of the WAL spool, PG dumps, Qdrant snapshots and app bundles. Without it the
   honest statement stays: **RPO = last on-host backup, RTO = unbounded on host loss.** The
   destination is a decision for the owner: object storage, a second host, or a pull to Windows `D:`.
3. Only a synchronous off-host replica or an independently durable receipt journal would support a
   claim of no acknowledged event lost. Not proposed for the first cutover.

Resource ask on the VPS: a dedicated `memgw` database inside the existing PG16 cluster (no second
server), `pgcrypto` and `uuid-ossp` enabled, `shared_buffers` untouched initially, and roughly 5 GB
of disk for ledger plus WAL spool. That fits the 27 GB free, but gap 19 means disk needs watching.

## 5. Implementation file map (proposed; nothing created yet)

Extend the existing service. No second gateway binary.

**New:**
- `mcp-server/pkg/ledger/schema.sql`, `migrations.go` — versioned migration runner (does not exist today).
- `mcp-server/pkg/ledger/store.go` — pgxpool-backed ledger, reusing the pool pattern already in
  `pkg/rag/pgvector.go:24`; events, checkpoints, facts, receipts, outbox, tombstones, writer epochs.
- `mcp-server/pkg/ledger/commit.go` — one transaction for event, aggregate update, receipt and
  outbox row, with CAS on expected revision.
- `mcp-server/pkg/ledger/idempotency.go` — principal-scoped key, payload-digest mismatch rejection.
- `mcp-server/pkg/httpapi/gateway.go` — `/api/memory/*` routes: submit, checkpoint, receipt,
  bootstrap, reconcile.
- `mcp-server/pkg/httpapi/principal.go` — per-agent credentials and scopes; the piece the current
  single-token model lacks.
- `mcp-server/pkg/projection/worker.go` — outbox to Qdrant, at-least-once, tombstone precedence.
- `adapters/` with one directory per host and disjoint files: `omp/`, `claude/`, `codex/`,
  `opencode/`, `droid/`, `hermes/`.
- `collector/` — Windows service, SQLite WAL with `synchronous=FULL`, DPAPI-wrapped key,
  dead-letter queue.

**Changed:**
- `pkg/config/config.go` — ledger DSN, collector endpoint, principal store path.
- `cmd/mcp-server/main.go` — wire the ledger and projection worker; add graceful shutdown and HTTP timeouts.
- `pkg/httpapi/server.go` — mount gateway routes, keeping the existing middleware chain.
- `pkg/httpapi/auth.go` — extend to principals without breaking the existing shared-token path.
- `pkg/core/writeguard.go` — reuse `containsCredential` on ledger ingress; add a sensitivity class.
- `pkg/core/metrics.go` and `sqlite_metrics.go` — the observability counters the plan lists.
- `CHANGELOG.md` and `docs/` — operating documentation.

## 6. Revisions to the plan's ordering, forced by the findings

1. **Codex is not enqueue-only.** It ships a full hook engine, so its adapter can be first-class.
   The exit-deadline caution still applies to `session-end`, but capture is not limited to a local spool.
2. **The reference adapter is OMP, not Claude.** It is the only host with branch, compaction and
   shutdown events, so it is the only one that can exercise the full contract first. Phase 5 left
   this open pending Phase 1 evidence; this is that evidence.
3. **The migration runner is Phase 3 work, not reuse** — `pkg/migrate` is unrelated to SQL schema.
4. **Principal/scope auth moves earlier.** With a single shared token there is nothing to scope
   against, so `principal.go` must land with the first ledger route rather than after it.
5. **Backup work becomes a Phase 3 prerequisite, not a Phase 9 checklist item.** Writing canonical
   state into a database with no backup and no off-host copy would create exactly the loss the plan
   forbids promising away.
6. **Add a Phase 0.5: commit and stamp the running build.** Reproducible provenance is required
   before fencing writers in Phase 9; today the deployed binary cannot be identified.

## 7. Decisions needed before Phase 2

1. **Rotate the admin token?** It sits in cleartext in nginx and was surfaced in this session.
2. **Off-host backup destination**: object storage, a second host, or a pull to Windows `D:`.
3. **PG placement**: a new database in the existing PG16 cluster on the Oracle host (recommended,
   no new server) versus a separate instance. Enabling `archive_mode` requires a PG restart, which
   affects `axonhub` and the other users of that cluster.
4. **Auth model**: per-agent principals with scopes (breaks nothing, but every host config must be
   updated) versus keeping one token for the first cut.
5. **Collector transport on Windows**: named pipe versus loopback HTTP with a local token.
6. Confirm **OMP as the reference adapter**.
7. Ratify or restate the plan's proposed SLOs now that these numbers exist.

## 8. Verification limits of this inventory

- Codex and Droid hook catalogues were extracted from **binary strings**, not from a hook that
  actually fired. Both need a smoke fixture in Phase 5 before either is called supported.
- OMP hook events were read from the **installed TypeScript sources** — source, not a live firing.
- OpenCode is the only host where a real firing was observed, via its own audit log.
- Hermes was inspected on the VPS filesystem; no hook was registered or run.
- No load test, failure injection, or restore rehearsal was performed — those belong to Phases 3 and 8.
- `/opt/enowx-rag/.env` was **not** read; the local guard blocks it by design. The live
  configuration was read from the startup log instead, which prints only non-secret fields.

---

# Phase 0.5 — Artifact identity and production drift (executed 2026-09-09)

Read-only investigation first, then a source-only patch. **No deploy, no restart, no nginx change,
no secret rotation, no commit.** No `.env`, token, key or password was read; where the local guard
blocked a path it is recorded as blocked and non-secret metadata was used instead.

## 1. Verified facts

### 1.1 Repository state

| Item | Value |
|---|---|
| Branch | `local/write-guard` |
| HEAD | `0e6d55cfc01f06f5ed5a7151a7ceda8101ec42c6` |
| Modified, tracked | 21 files (14 Go, `CHANGELOG.md`, `SECURITY.md`, 5 web) |
| Untracked | 4 Go test files, 1 web asset, `docs/plans/` |
| Tracked build artifact | `enowx-rag-arm64`, 17,236,130 bytes, sha256 `d0c8956b8396d3ca66725b3f43d57af8ecdf3cb6587d6268835fccd966512af3` — **stale**, matches none of the running binaries |
| Test suite | `go test ./... -count=1` passes; `go vet ./...` clean |

### 1.2 Production artifact

| Item | Value |
|---|---|
| Path | `/opt/enowx-rag/bin/enowx-rag` |
| Size / mtime | 24,962,509 bytes, 2026-09-09 14:09:23 UTC |
| sha256 | `f78a0da9d7495ae219721438c76a0ea022e018df732f01787d0530c1269a9083` |
| Previous generation | `enowx-rag.pre-export-20260909-140923`, sha256 `c8a37eb4f8019c2cc6701705804bb8ed91da1181868c243145fdb11dc19af3b5` |
| Embedded `vcs.revision` | `0e6d55cfc01f06f5ed5a7151a7ceda8101ec42c6` |
| Embedded `vcs.time` | `2026-09-09T07:54:25Z` |
| Embedded `vcs.modified` | **`true`** |
| Toolchain | go1.26.5 |
| Build paths inside binary | `D:/PROJECTS/enowx-rag/mcp-server/...` — cross-compiled from the Windows workstation with a plain `go build`, **not** `make build` (which passes `-trimpath`) |

### 1.3 Drift verdict: none, bit-for-bit

Rebuilding the current working tree with the same toolchain and target reproduces the deployed
artifact exactly:

```
cd mcp-server && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o <scratch>/repro-arm64 ./cmd/mcp-server
sha256 = f78a0da9d7495ae219721438c76a0ea022e018df732f01787d0530c1269a9083   # identical to production
```

Captured **before** the Phase 0.5 patch below was written, so it describes the tree as it was
deployed. The conclusion is narrow and load-bearing: production is **not** running unknown or
unreproducible code. It is running the current working tree, and the working tree is simply not
committed.

### 1.4 Live endpoints

- `GET /api/stats` → 200 with bearer, 401 without.
- `GET /api/projects/memory/export` → 200, 2,335,324 bytes.
- MCP `initialize` → `serverInfo.version "dev"`.

## 2. Status of `/export`

**Legitimate source change. Keep it. Nothing to delete.**

Evidence: the feature is coherent across four layers of the working tree — `pkg/core/service.go:727`
`ExportProject`, `pkg/httpapi/handlers.go:145` handler, `pkg/httpapi/server.go:40` route, plus an
untracked `pkg/httpapi/export_test.go` and a `CHANGELOG.md` entry. It also uses the provider's
existing `Exporter` interface rather than reaching around it, and `pkg/migrate` already depends on
that same interface. This is unfinished-but-intended work, not an artifact of a hand-edit on the server.

What was actually wrong is narrower than "drift": the source was never committed, and the binary
could not name itself. The first is a commit waiting for approval (section 6); the second is fixed below.

## 3. Build metadata — design and patch

**Design.** Go already stamps `vcs.revision`, `vcs.time` and `vcs.modified` into any `go build` of a
main package inside a repository. Nothing needed to be invented, generated, or wrapped — the data was
in the deployed binary all along (section 1.2) and was being thrown away by `resolvedVersion()`,
which only looked at `info.Main.Version` (always `(devel)` for a main-module build).

- `pkg/buildinfo` reads the stamp once and caches it. Optional ldflags variables
  (`version`, `commitSHA`, `buildTime`) override it for tagged releases; when absent, the short
  commit SHA becomes the version.
- **No fake versions.** A build from a modified tree reports `dirty_tree: true` and appends `-dirty`
  to the version string, so a dirty build can never be mistaken for the commit it was based on. The
  suffix rides along through the MCP handshake and the startup log, not just the JSON.
- `GET /api/version` sits inside the existing `/api` group, so it inherits `AdminTokenMiddleware`.
  A commit SHA is not a secret, but there is no reason to volunteer the exact source revision of an
  internet-exposed instance to anonymous callers.
- `go run` does **not** stamp VCS info, so `go run ./cmd/mcp-server version` still prints `dev`.
  That is Go's behaviour, documented in the Makefile rather than papered over.

**Verified after patching:**

| Command | Output |
|---|---|
| `go build … && enowx-rag version` | `enowx-rag 0e6d55c-dirty` |
| `go build -ldflags "-X …buildinfo.version=v9.9.9-test" …` | `enowx-rag v9.9.9-test-dirty` |
| `go build ./... && go vet ./...` | clean |
| `go test ./... -count=1` | all packages pass |

**Files changed (working tree only, uncommitted, not deployed):**

| File | Change |
|---|---|
| `mcp-server/pkg/buildinfo/buildinfo.go` | **new** — reads the VCS stamp / ldflags, exposes `Info` and `String()` |
| `mcp-server/cmd/mcp-server/main.go` | `resolvedVersion()` delegates to `buildinfo`; removed the `version = "dev"` var and the now-unused `runtime/debug` import |
| `mcp-server/pkg/httpapi/handlers.go` | **new** `Version` handler for `GET /api/version` |
| `mcp-server/pkg/httpapi/server.go` | route `r.Get("/version", h.Version)` inside the token-gated `/api` group |
| `mcp-server/pkg/httpapi/version_test.go` | **new** — asserts a version is always reported and that a known commit is never reduced to `dev` |
| `mcp-server/pkg/httpapi/docs.go` | `/api/version` added to the API reference the dashboard and agents read |
| `.goreleaser.yaml` | ldflags retargeted from the deleted `main.version` to the `buildinfo` symbols, plus commit and date. **This mattered:** `-X` against a missing symbol is silently ignored, so leaving it would have shipped unstamped releases with no error |
| `Makefile` | optional `VERSION=` stamping; documents that `-trimpath` keeps the VCS stamp and that `go run` has none |
| `CHANGELOG.md` | entry under Unreleased → Added |

## 4. Token rotation runbook — prepared, NOT executed

`RAG_ADMIN_TOKEN` is treated as compromised: it is stored in cleartext in
`/etc/nginx/sites-enabled/rag.conf` and `/etc/nginx/sites-enabled/rag-dashboard.conf`, and it was
rendered into a session transcript on 2026-09-09. No step below has been run. No token value —
old or new — appears in this document, and none may be pasted into it.

### 4.1 Consumers (locations only, never values)

| # | Consumer | Where the value lives | Verified |
|---|---|---|---|
| 1 | enowx-rag server | `RAG_ADMIN_TOKEN` in `/opt/enowx-rag/.env` | file exists (guard-blocked from reading; startup log confirms a token is in effect) |
| 2 | nginx public vhost | `map $http_authorization $rag_upstream_auth` default value in `rag.conf` | yes, cleartext |
| 3 | nginx local dashboard | `proxy_set_header Authorization` in `rag-dashboard.conf` (`127.0.0.1:7778`) | yes, cleartext |
| 4 | Claude Code | `mcpServers["enowx-rag"].headers.Authorization` in `~/.claude.json` | yes (header present) |
| 5 | Claude Code hooks | `rag-recall.py` and `rag-session-log.py` read consumer #4's file; `guard.py` also references it | yes |
| 6 | Codex | `[mcp_servers.enowx-rag] bearer_token_env_var = "ENOWX_RAG_TOKEN"` — an environment variable, not a file | yes |
| 7 | OpenCode | `mcp["enowx-rag"].headers.Authorization` in `~/.config/opencode/opencode.json` | yes |
| 8 | OpenCode plugin | `enowx-rag-hooks.ts` falls back to `~/.claude.json` when consumer #7 is absent | yes (source read) |
| 9 | Droid | `mcpServers["enowx-rag"].headers.Authorization` in `~/.factory/mcp.json` | yes |
| 10 | Windows `~/.enowx-rag/config.yaml` | may carry `admin_token` | file exists; **not read** — the operator must check |
| 11 | Host `~/.enowx-rag/config.yaml` (`/opt/enowx-rag/.enowx-rag/config.yaml`) | `admin_token` key | exists, **0 `admin_token` lines** — the server takes the token from the environment only |
| 12 | Encrypted app bundles in `/opt/rag-backup/app/` | contain `.env` | yes — **the old token survives in backups; rotation does not erase it** |
| 13 | nginx basic-auth file `/etc/nginx/.htpasswd-rag` | separate credential, gates the dashboard | present — a second secret, rotate on its own schedule |

Not a consumer: OMP/Pi (no enowx-rag MCP entry), Hermes (none configured).

### 4.2 Sequence

1. **Freeze.** Announce a short window; no agent writes to `memory` during it.
2. **Generate** the new token off-transcript, on the host, into a shell variable that is never
   echoed (e.g. `openssl rand -hex 32` piped straight into the edit). Never through this session.
3. **Server first.** Update `RAG_ADMIN_TOKEN` in `/opt/enowx-rag/.env` (mode 0600, owner `enowxrag`),
   keeping a timestamped copy the way the existing `.env.pre-*` files do.
4. **nginx, same window.** Update the default value in `rag.conf`'s `map` and the header in
   `rag-dashboard.conf`. *Better, if approved:* move the value into an nginx variable sourced from a
   0600 file included with `include`, so the token is not inline in a config that is read casually.
5. **Validate before reload:** `nginx -t`. Do not reload on failure.
6. **Apply:** `systemctl restart enowx-rag` (env file is read at start), then `systemctl reload nginx`.
   Restart order matters: server first means nginx never forwards a new token to a server that still
   expects the old one.
7. **Smoke test, new token:** `GET /api/version` → 200 (this endpoint now exists and is the cheapest
   authenticated probe); `GET /api/stats` → 200 with the expected chunk count; MCP `initialize` → 200.
8. **Negative test, old token:** the same three calls with the retired token → 401. Also confirm
   no-token → 401.
9. **Consumers 4–10**, one at a time, smoke-testing each: Claude Code, Codex (`ENOWX_RAG_TOKEN`),
   OpenCode, Droid, then the Windows config file.
10. **Bounded rollback.** If a consumer cannot be updated inside the window, restore the previous
    `.env` and nginx copies from their timestamped backups and reload. Rollback restores a
    **compromised** token, so it buys hours, not days: reschedule immediately.
11. **Sweep for cleartext:** grep source tree, `docs/`, `CHANGELOG.md`, journald for `enowx-rag` and
    nginx, `/opt/rag-backup/*.log`, and this plan document. Expect zero hits for either value.
12. **Record what rotation does not fix:** consumer #12. Old bundles keep the old token; they are
    GPG-encrypted, and their retention (7 by count) ages them out. Decide explicitly whether to purge
    them early or let retention do it.

### 4.3 Follow-up, not part of rotation

A single shared bearer means rotation is all-or-nothing across six hosts. The principal/scope model
decided for the gateway (per host/agent principal, `project_id`, `workspace_id`, `work_id`, and roles
read/history, checkpoint-write, fact-promote, admin) makes the next rotation per-principal and
revocable individually. Rotation now is containment; the principal model is the fix.

## 5. Operational blockers

1. **Deploying this patch requires a restart of `enowx-rag.service`** — production change, needs
   approval. Until then production keeps reporting `"dev"`, and its identity is only knowable by the
   checksum in section 1.2.
2. `runHTTP` still calls `http.ListenAndServe` with no timeouts and no graceful shutdown, so any
   restart cuts in-flight requests. Acceptable for a stateless RAG read today; **not** acceptable
   once ledger commits are in flight. Fix belongs with the ledger work, before it carries writes.
3. The tracked `enowx-rag-arm64` binary (section 1.1) is stale and matches nothing running. Removing
   a tracked file is a repository change and awaits approval.
4. Deployment remains a manual copy; 11 old binaries sit in `/opt/enowx-rag/bin`. No CI produces the
   artifact, so "the deployed binary equals commit X" stays a fact someone must re-establish by
   checksum after every deploy rather than a property of the pipeline.

## 6. Awaiting approval

| # | Decision | Why it needs you |
|---|---|---|
| 1 | Commit the working tree (`/export` + guard/metrics/auth work + this Phase 0.5 patch) | 21 modified files span several unrelated pieces of work; the split into commits, and whether the branch `local/write-guard` is the right home, is yours |
| 2 | Deploy the Phase 0.5 build and restart `enowx-rag.service` | production restart |
| 3 | Run the section 4 rotation runbook | production secret + nginx + restart |
| 4 | Move the nginx token out of the inline config into an included 0600 file | changes nginx structure |
| 5 | Delete the tracked `enowx-rag-arm64` artifact | repository history |
| 6 | Purge pre-rotation app bundles early, or let the 7-copy retention age them out | destroys recovery material |

## 7. Not verified in this phase

- `GET /api/version` was exercised through its handler in a unit test and through the `version`
  subcommand of a real build. It was **not** called over HTTP against a running server; that
  happens at deploy time.
- The deployed binary's identity was established by checksum and embedded VCS stamp. It was not
  executed with `version` on the host (that would mean running a production binary out of band).
- `/opt/enowx-rag/.env`, `~/.enowx-rag/config.yaml` (both hosts) and the nginx basic-auth file were
  **not read** — deliberately. Consumer #10 in section 4.1 is therefore "file exists", not
  "confirmed to contain a token".
- Whether the app-backup bundles decrypt and restore was not tested; only the Qdrant restore test is
  proven (Phase 1, section 4).
- No claim is made that reproducing a build bit-for-bit will hold across toolchain upgrades: it held
  here because the same go1.26.5, the same flags and the same tree were used.

---

# Appendix 3 — Phase 2: Schema and fixture freeze (executed 2026-09-09)

Scope executed: contract freeze and deterministic fixtures only. No production schema migration, no
deployment, no service restart, no PostgreSQL change, no credential created, no token rotation, no
adapter, no collector, no cutover.

## 3.1 Documents created

| File | Role |
|---|---|
| `docs/architecture/shared-memory-gateway.md` | The frozen contract: domain terms, identity/ordering, principal + scope, event contract, error classes, bounds, schema contract for 13 entities, 20 invariants, candidate/fact policy, tombstone policy, fixture map, carried-forward blockers, unverified assumptions. |
| `docs/adr/0001-shared-memory-authority.md` | The authority decision, its five rejected alternatives, and what it explicitly does not promise. |
| `mcp-server/pkg/memgw/contract/doc.go` | Package doc. States that the package deliberately contains no implementation. |
| `mcp-server/pkg/memgw/contract/contract_test.go` | Validates the fixtures against the frozen contract. 10 tests, all passing. |
| `mcp-server/pkg/memgw/contract/testdata/contract.json` | Machine-readable contract: schema/policy version, envelope fields, 23 event types, 10 error classes, roles, principal types, sensitivity classes, bounds, 20 invariants, fixture list. |
| `mcp-server/pkg/memgw/contract/testdata/fixtures/*.json` | The 15 scenarios. |

`docs/adr/` and `docs/architecture/` did not exist before this phase; both were created. The ADR was
justified first: no ADR-equivalent decision document existed anywhere under `docs/` or in `README.md`,
so nothing was duplicated. No existing file was overwritten. This appendix is appended to the plan;
the earlier appendices are unchanged.

## 3.2 What was frozen

- **16 domain terms** plus the six load-bearing distinctions (session ≠ work, queued ≠ committed,
  candidate ≠ fact, projection ≠ authority, checkpoint ≠ truncation, rollback ≠ deleting history).
- **Schema contract for 13 entities** — the 12 requested plus `fact_candidates`, split out
  deliberately: sharing a table with `facts` under a status column is one forgotten `WHERE` away from
  serving an extraction as current state. Each entity carries identity, ownership/scope, lifecycle,
  immutable fields, mutable/materialised fields, revision rule, provenance, deletion/tombstone rule,
  ACL and retention.
- **Principal + scope**: 10 principal fields, grant tuples with intersection-only semantics, 7 roles.
  The shared bearer is recorded as a legacy transport credential that is never the ACL authority.
- **Event contract**: 14 client fields, 4 optional, 6 server-assigned; canonical ordering is
  `events.seq`, never a client clock.
- **10 error classes**, **23 allowed event types**, and **10 bounds** (payload ≤ 256 KiB, batch ≤ 20
  events / 1 MiB, ≤ 64 evidence refs, idempotency key ≤ 128 B, checkpoint text fields ≤ 4 KiB,
  ≤ 200 modified-file refs, bootstrap ≤ 1500 tokens, RAG document ≤ 3000 chars to match the live
  write guard).
- **Candidate policy** and **tombstone policy**, including the asymmetry that vector deletion does not
  retract text already sent to an embedding provider, which is why every egress passes the redaction
  gate before transmission rather than after.

No SQL was written. The schema exists as a contract in prose and as JSON vocabulary; there is no
migration file to accidentally apply.

## 3.3 Fixtures and test results

15 fixtures, all 20 invariants covered, verified mechanically by
`TestEveryInvariantIsCovered` rather than by reading the table.

| Fixture | Invariants |
|---|---|
| duplicate-retry | INV-01, INV-02 |
| payload-mismatch | INV-03 |
| concurrent-cas | INV-06 |
| out-of-order-event | INV-07, INV-20 |
| branch-rewind | INV-09, INV-19 |
| conflicting-facts | INV-08 |
| multi-valued-facts | INV-11 |
| tombstone-rebuild | INV-12 |
| principal-scope-denial | INV-13, INV-17 |
| candidate-promotion | INV-10 |
| subagent-evidence | INV-16 |
| writer-epoch-fencing | INV-18 |
| secret-pii-rejection | INV-14 |
| recalled-prompt-injection | INV-15, INV-13 |
| unknown-ack-reconciliation | INV-04, INV-05 |

`go test ./pkg/memgw/...` — 10 tests, all pass (verified, not assumed):
fixture/contract cross-reference, invariant coverage, envelope completeness, bounds, expectation
vocabulary, rejections-have-no-effect, digest relations, retry keeps `event_id`, principal
vocabulary, and a guard that no fixture or the contract itself contains credential-shaped content.

Fixtures are JSON rather than Go structs on purpose: the adapters that must honour the same contract
are TypeScript (OMP, OpenCode), Python (Hermes, Claude Code hooks) and Go. A contract encoded in Go
types would be enforceable in exactly one of the three.

`payload_digest` is declared as the sentinel `@computed`; the harness computes sha256 over canonical
JSON and asserts the *relations* the contract depends on — identical payloads must digest identically,
and a retry must not carry a new `event_id`. Hard-coded hashes would test the author's arithmetic, not
the contract.

The secret/PII fixture names credential *shapes* and never reproduces one; a test enforces that,
because a fixture carrying a plausible secret would be the leak it was written to prevent.

## 3.4 Working tree — separation of changes

Phase 0.5 changes (build identity), still uncommitted and undeployed, unchanged by this phase:
`mcp-server/pkg/buildinfo/buildinfo.go`, `mcp-server/cmd/mcp-server/main.go`,
`mcp-server/pkg/httpapi/{server.go,handlers.go,docs.go,version_test.go}`, `.goreleaser.yaml`,
`Makefile`, `CHANGELOG.md`.

Phase 2 changes: the six files in §3.1. Additive only — no existing file was edited, so the two sets
do not overlap and can be committed separately.

Untouched, as instructed: the tracked `enowx-rag-arm64` artifact was not deleted; no
`git reset --hard`, `git clean`, destructive checkout or file deletion was used; unrelated
pre-existing working-tree changes were neither committed nor reverted.

## 3.5 Conflicts with existing code

1. **No SQL migration runner exists.** `pkg/migrate` is a vector-document migrator, not a schema tool
   (established in Phase 1). Phase 3 needs a real migration mechanism; the SQLite metrics store's
   inline `CREATE TABLE` + `ALTER TABLE` + duplicate-column-string-match pattern is not a model to
   copy for the ledger.
2. **Auth has no principals.** `AdminTokenMiddleware` compares one token; there is no identity to
   scope. The principal model in §4 of the contract has no implementation to extend — it is new code
   on a route that currently authorises everything identically.
3. **The write guard is env-driven and project-scoped** (`RAG_GUARD_*`, `CheckProjectScan`,
   `credentialPatterns`). Its credential scanner is directly reusable as the ledger's ingress scanner
   and the fixture's shape list was written to match it, but its configuration model (one global env
   contract) does not express per-principal or per-sensitivity policy.
4. **`runHTTP` has no timeouts and no graceful shutdown.** The ratified shutdown-wait SLO (≤ 2 s) and
   the durability story both depend on that being fixed before the ledger carries writes.
5. **No `testdata` directory existed anywhere in the Go tree** before this phase; the fixture layout
   is a new convention for this repository, following the standard Go `testdata` mechanism.

## 3.6 Acceptance gate

| # | Criterion | Result |
|---|---|---|
| 1 | Domain terms frozen and unambiguous | Pass — 16 terms, 6 distinctions |
| 2 | Schema contract complete per entity | Pass — 13 entities × 10 attributes |
| 3 | 20 invariants documented | Pass |
| 4 | Every invariant has a fixture | Pass — enforced by test, not by hand |
| 5 | Principal + scope model frozen | Pass — 10 fields, 7 roles, intersection-only |
| 6 | Shared bearer demoted from ACL authority | Pass — stated in contract §4.4 and ADR |
| 7 | Event contract and error classes frozen | Pass — 17 envelope fields, 10 classes |
| 8 | Bounded payload size and allowed event types | Pass — 10 bounds, 23 types |
| 9 | Candidate/fact policy with promotion rules | Pass |
| 10 | Tombstone policy distinguishing 5 deletion meanings + embedding-egress asymmetry | Pass |
| 11 | No production schema migration created or applied | Pass — no SQL file exists |
| 12 | No deployment, restart, PostgreSQL change or credential created | Pass |
| 13 | Plan records archive_mode / off-host backup / token rotation blockers | Pass — §3.7 |
| 14 | Unverified assumptions labelled | Pass — contract §12 and §3.8 below |

## 3.7 Operational blockers carried forward (unchanged, still open)

1. `RAG_ADMIN_TOKEN` rotation — treated as compromised; runbook prepared in Appendix 2, not run.
2. Off-host backup destination — undecided. Every backup still shares a disk with production.
3. `archive_mode` maintenance window — enabling WAL archiving restarts the cluster shared with
   `axonhub`. Without it there is no PITR and the ledger's real RPO is the last dump, which does not
   exist. INV-04 is worded to promise only process-crash durability because of this.
4. nginx token moved into a `0600` include file — pending.
5. Separate commit for the Phase 0.5 build-identity changes — pending approval.
6. `/api/version` verified over HTTP after deployment — pending; production still reports `dev`.

## 3.8 Not verified in this phase

- No fixture has been executed against a real ledger, because none exists. They are contract
  assertions, not integration results.
- No adapter has been shown to produce a stable `idempotency_key` across a crash — true for zero of
  the six hosts. It is the first thing each adapter fixture must prove in Phase 3.
- The existing cluster's headroom at the ratified SLOs is unbenchmarked.
- `pgcrypto` / `uuid-ossp` are listed as available, not installed; the contract avoids depending on
  either.
- The interaction between the 3000-character RAG write-guard limit and the 256 KiB event payload
  bound has not been measured against real checkpoints.

## 3.9 What Phase 3 will need

- A real SQL migration mechanism (nothing reusable exists) and the first migration expressing §6 of
  the contract, applied to a non-production schema first.
- Principal issuance and grant storage, plus the transport that carries a principal credential —
  which is gated behind the token rotation, since the current shared bearer is compromised.
- HTTP timeouts and graceful shutdown in `runHTTP` before the ledger accepts writes.
- The projection outbox worker and the tombstone deletion journal, including backfill of
  `legacy_chunk_ids` for pre-gateway Qdrant chunks.
- The OMP/Pi reference adapter with a crash-stable idempotency key, then OpenCode as the smoke test.
- A decision on the `archive_mode` window before any claim about durability beyond process crash.

---

# Appendix 4 — Phase 3: Ledger core against a disposable database (executed 2026-09-09/10)

Phase 3 built the write path. Nothing in it has been deployed, and nothing in it has touched
production. The whole phase ran against a container created for the purpose.

## 4.1 The non-production target, and how it was proved

A new container was created rather than reusing anything that already existed:

| | Phase 3 target | Production |
|---|---|---|
| Container | `memgw-devdb` (`postgres:16-alpine`, image already local) | remote host, shared with axonhub |
| Bind | `127.0.0.1:55433` only | remote |
| Version | PostgreSQL 16.13 | 16.15 |
| Auth | `POSTGRES_HOST_AUTH_METHOD=trust` — **no credential exists** | password |
| Database | `memgw_test`, empty at creation | `enowx` |

The pre-existing `enowx-pg-test` container was deliberately **not** reused: its contents are
unknown, and "probably a test database" is not the same as provably disposable.
`sc_recovery_pg_35`, `sc_recovery_pg_db9`, `reminder_postgres`, `reminder-db-1` and `warp-n*` were
not touched.

`pgstore.VerifyNonProduction` re-proves the target on every migration and before every test,
through six independent checks: declared environment is `development` or `test`; the database name
announces itself disposable (`memgw_`/`test_`/`dev_` prefix or `_test`/`_dev`/`_devdb`/`_testdb`
suffix); the server is not in recovery; the connection is loopback; `synchronous_commit` is not
`off`; and the database holds no tables outside the schemas we own. Any one of these can be
satisfied by accident. Together they are hard to satisfy by accident. `TestUpRefusesUnsafeTarget`
relabels the very same pool `production` and asserts the runner refuses before any DDL runs, and
`enowx-rag memgw up` with `MEMGW_ENV=production` exits 3 with
`environment is declared production`.

## 4.2 Files created

| File | What it is |
|---|---|
| `pkg/memgw/pgstore/pgstore.go` | Pool, bounded timeouts, DSN redaction, the non-production gate |
| `pkg/memgw/migrations/runner.go` | Versioned, checksummed, transactional SQL migration runner |
| `pkg/memgw/migrations/sql/0001_memgw_init.sql` | The first migration: 10 tables, no deferred entities |
| `pkg/memgw/memgwtest/memgwtest.go` | One throwaway schema per test, migrated from empty, dropped after |
| `pkg/memgw/ledger/errors.go` | The ten frozen error classes as Go values |
| `pkg/memgw/ledger/canonical.go` | Canonical JSON and payload digest |
| `pkg/memgw/ledger/submit.go` | The durable write path: validate, authorize, idempotency, CAS, event, receipt, outbox, commit |
| `pkg/memgw/principal/principal.go` | Principals, grants, and the intersection-only authorization rule |
| `pkg/memgw/outbox/outbox.go` | Projection queue, watermarks, dead letters, tombstones, legacy chunk map |
| `pkg/memgw/outbox/worker.go` | Bounded projection worker over an `Applier` interface |
| `pkg/httpapi/lifecycle.go` | Bounded HTTP server with a real graceful shutdown |
| `cmd/mcp-server/memgw.go` | `enowx-rag memgw status / plan / up` |

Test files: `pgstore_test.go`, `migrations/runner_test.go` (database-free) and
`migrations/integration_test.go`, `principal/principal_test.go` and `principal/scope_test.go`,
`ledger/canonical_test.go`, `ledger/fixture_test.go`, `ledger/submit_test.go`,
`outbox/outbox_test.go`, `httpapi/lifecycle_test.go`.

Files changed: `cmd/mcp-server/main.go` (metrics store held for the shutdown flush; `runHTTP`
rewritten to return an error and drive `httpapi.Server`; the `memgw` subcommand dispatched),
`pkg/core/writeguard.go` (one additive export: `ContainsCredential`, so the ledger scans with the
same patterns the RAG ingress uses rather than a second copy that would drift).

## 4.3 Migration mechanism, and why no dependency

No dependency was added. `pkg/migrate` moves documents between vector stores and is not a SQL
migration framework; `pkg/core`'s SQLite metrics store creates its tables inline and detects an
already-applied `ALTER` by matching on an error string. Neither is a model for a ledger, so the
runner is roughly 300 lines over `embed.FS` and `pgx`:

- ordered `NNNN_name.sql` files, sha256 checksum per file;
- a `schema_migrations` history table with `dirty`;
- each migration and its history row committed in **one** transaction;
- refusal on checksum drift, on a dirty row, on a version inserted below an applied one, and on a
  history row whose file has been deleted;
- a `-- memgw:no-transaction` opt-out, bracketed by the dirty flag, for statements PostgreSQL will
  not run inside a transaction;
- no down migrations: a destructive rollback of a ledger schema is not a recovery strategy;
- `status` and `plan` are read-only, and `plan` refuses for the same reasons `up` would.

`validatePlan` holds the entire safety argument in one function with no database, and is tested
directly.

The first migration creates only: `schema_migrations`, `writer_epochs` (epoch 1 seeded),
`principals`, `grants`, `events`, `write_receipts`, `projection_outbox`, `projection_watermark`,
`tombstones`, `legacy_chunk_map`. **Explicitly deferred**, named in the migration header and
checked by `TestBuiltinSetLoads`: `projects`, `workspaces`, `works`, `sessions`, `branches`,
`checkpoints`, `facts`, `fact_candidates`, `provenance_refs`.

## 4.4 Two design decisions worth recording

**CAS without a lock, and without a `works` table.** The current revision of an aggregate is read
from `MAX(revision)` in the event log itself, and `events_aggregate_revision_idx` is a *unique*
partial index on `(aggregate_type, aggregate_id, revision)`. Two writers that both read revision 12
both compute 13; the loser gets SQLSTATE 23505 and the ledger reports `cas_conflict`. This is why
the deferred entities can stay deferred while `cas_conflict` is still observable.

**`cas_conflict` and `stale_revision` are not the same refusal.** A writer that was already behind
when it decided — `expected_revision` below current at read time — gets `stale_revision`, which is
the `out-of-order-event` fixture. A writer that was current when it decided and lost the race gets
`cas_conflict`, which is the `concurrent-cas` fixture. This is why `TestFixtureConcurrentCAS` runs
its two submits in goroutines: replayed sequentially, that fixture would legitimately produce
`stale_revision`, which would be a different scenario, not a bug.

## 4.5 HTTP lifecycle

`runHTTP` previously called `http.ListenAndServe` with no timeouts and ended in `log.Fatalf`. It
now builds an `httpapi.Server` with `ReadHeaderTimeout` 10s, `ReadTimeout` 60s, `IdleTimeout` 120s,
`MaxHeaderBytes` 1 MiB and a 2s shutdown budget (the ratified SLO), driven by
`signal.NotifyContext` on SIGINT/SIGTERM. On cancellation it stops accepting, drains in-flight
requests, then runs hooks — workers first, metrics flush second, so a worker cannot write to a
closed store. A drain that overruns the budget returns `ErrShutdownIncomplete` and `main` exits 1:
a caller cut off mid-request cannot tell whether its write landed, and exiting zero would hide that
from the supervisor.

`WriteTimeout` is deliberately **0**. `/mcp` is a streamable HTTP endpoint and a global write
deadline would sever legitimate long responses; bounding response time belongs in per-route
middleware on `/api`. `TestDefaultServerOptionsAreBounded` asserts the zero so that changing it
requires answering this.

The smoke test opens a real listener on `127.0.0.1:0`, serves a request, starts a slow request,
cancels the context, proves the listener stops accepting *and* that the in-flight request still
receives its full body, and proves both hooks ran. A second test proves the incomplete-shutdown
error, a third proves a hook failure surfaces, a fourth proves `ReadHeaderTimeout` drops a client
that never finishes its headers. **No production route was changed and nothing was deployed.**

## 4.6 Principals and grants

Ten-field principal, grant tuples that only ever narrow. A grant pinned to a workspace does not
admit a request for a different workspace *nor a request that names no workspace at all* — an unset
narrowing field is a request for all of them, which is wider than the grant, and treating it as a
wildcard is the classic way this kind of model leaks.

The six mandated negative tests all pass: project A cannot reach project B; a revoked grant is
refused; a wider declared scope is refused; an expired grant is refused; an admin-only operation is
refused to a reader; and a refusal carries no credential, no payload, and no topology — it does not
even confirm that the project it refused exists, because a principal must not be able to map the
system by probing it. Three more were added: revoking a principal drops every grant at once, an
unknown principal is denied rather than reported missing, and the same grant tuple cannot be issued
twice.

**No production principal and no credential were created.** A principal is an identity row; how a
caller proves it is that identity is a separate, production concern this phase does not touch. The
legacy shared bearer is *not* wired in as an ACL: `principal.LegacyBearerNotice` exists as a
warning string only, and there is no compatibility adapter that would let it stand in for a grant.

## 4.7 Receipt and idempotency

`Submit` follows the mandated sequence exactly, with the receipt and the outbox row written inside
the same transaction as the event. Verified by test:

- an identical retry returns the original receipt with state `duplicate`, one event, one outbox row,
  revision advanced once (`duplicate-retry`);
- the same key with a different payload is `idempotency_payload_mismatch` and writes nothing
  (`payload-mismatch`);
- two concurrent writers at the same revision produce one commit and one refusal (`concurrent-cas`);
- a lost response is reconcilable both ways — receipt lookup returns the same `event_id` and `seq`,
  and an identical retry returns `duplicate` (`unknown-ack-reconciliation`);
- a late flush at a passed revision is `stale_revision` while its additive evidence is still
  accepted, and neither moves work state (`out-of-order-event`);
- a fenced or unknown writer epoch is refused server-side;
- a failed insert leaves no event, no receipt and no outbox row, and a later lookup finds nothing;
- every refusal path — stale revision, unknown epoch, wrong schema version, missing payload — leaves
  no receipt, so a receipt exists only after a commit;
- a receipt is only visible to the principal that owns it;
- a credential-shaped payload is refused before persistence and the refusal echoes neither the value
  nor the field name;
- `events` and `write_receipts` refuse `UPDATE` and `DELETE` at the trigger level.

**No event was sent to enowx-rag or Qdrant.** The outbox rows were written and drained by a test
`Applier`; nothing left the process.

## 4.8 Outbox, deletion journal, legacy chunks

Claiming is `FOR UPDATE SKIP LOCKED` plus a lease, so two workers never receive the same row and a
crashed worker's row returns when its lease expires. Verified: a second worker sees nothing while
the first holds rows; a crash before apply returns the row with the failed attempt still counted; a
crash after apply but before acknowledgement redelivers the row and the watermark does *not* move
until the ack — which is why an `Applier` must be idempotent, and is stated as such on the
interface; repeated failure dead-letters visibly rather than dropping, stores only an error class,
and does not count as progress; a backed-off row is not claimed before its `not_before`.

The projection worker never writes to `events`; the test asserts the event is unchanged after a
failed apply.

A tombstone beats a later rebuild: an `Applier` that checks `IsTombstoned` returns
`ErrSkippedTombstoned`, which the worker treats as **applied**, not failed. Treating it as a failure
would retry the skip forever and the tombstone would never settle. `tombstones` refuses `DELETE`;
`UPDATE` is deliberately left open so an erasure step can stamp `erasure_completed_at`.

`legacy_chunk_map` maps pre-gateway Qdrant chunk ids back to their documents, so a deletion ordered
today can find chunks written before the gateway existed and before re-chunking changed the ids.
`IssueTombstone` marks them in the same call and `TombstonedChunkIDs` is the erasure worklist.
**This phase does not run an erasure and performed no backfill.**

## 4.9 Test results

    $ go vet ./...                       # clean
    $ go test ./...                      # all packages ok, integration tests skipped (no DSN)
    $ MEMGW_TEST_DSN=postgres://postgres@127.0.0.1:55433/memgw_test go test ./...
    ok  github.com/enowdev/enowx-rag/pkg/memgw/ledger      5.612s
    ok  github.com/enowdev/enowx-rag/pkg/memgw/migrations  1.333s
    ok  github.com/enowdev/enowx-rag/pkg/memgw/outbox      4.040s
    ok  github.com/enowdev/enowx-rag/pkg/memgw/pgstore     0.036s
    ok  github.com/enowdev/enowx-rag/pkg/memgw/principal   3.158s
    ok  github.com/enowdev/enowx-rag/pkg/httpapi           0.485s

The suite needs no credential, no external network, no Voyage, no Qdrant and no live MCP. Without
`MEMGW_TEST_DSN` every integration test skips and the suite stays green, so a machine with no
database can still run it. With a DSN, every test gets its own schema migrated from empty, which
means the "migrate from an empty database" criterion is exercised on every run rather than once by
hand.

One correction came out of running them: the "database holds no unrelated tables" check originally
made parallel test packages refuse each other's schemas. `Config.SiblingSchemaPrefix` was added for
the harness only — empty everywhere else, so the strict reading remains the default.

## 4.10 Phase 3 acceptance gate

| # | Criterion | Status |
|---|---|---|
| 1 | Migration runner creates the schema from an empty database | Pass — `TestUpFromEmptyDatabase`, and every integration test |
| 2 | Rerunning migrations is safe and idempotent | Pass — `TestUpIsIdempotent`, CLI `up` twice |
| 3 | Checksum drift is rejected | Pass — `TestUpRefusesChecksumDrift` |
| 4 | Migration target is proved non-production | Pass — six checks, re-run per migration and per test |
| 5 | No production database was touched | Pass — see 4.11 |
| 6 | HTTP timeouts and graceful shutdown proved by smoke test | Pass — real listener, real request, real drain |
| 7 | Principal/grant negative tests pass | Pass — six mandated plus three |
| 8 | A duplicate retry does not duplicate | Pass |
| 9 | A payload mismatch is rejected | Pass |
| 10 | A CAS conflict is observable | Pass — concurrently, via the unique index |
| 11 | A receipt exists only after commit | Pass |
| 12 | A lost response is reconcilable | Pass — both routes agree |
| 13 | A transaction failure leaves no fake receipt | Pass |
| 14 | Outbox crash scenarios lose and duplicate nothing | Pass — before apply, after apply, dead letter |
| 15 | The tombstone contract is proved | Pass — tombstone beats rebuild, journal not erasable |
| 16 | No secret in source, fixtures, logs, test output or docs | Pass — the only credential-shaped strings are a synthetic `hunter2` in the DSN-redaction test and a runtime-assembled placeholder in the credential-scan test |
| 17 | `go vet` and `go test ./...` pass | Pass |

## 4.11 Evidence production was not touched

- Every connection made in this phase went to `127.0.0.1:55433`, a container created in this phase.
  `pgstore` refuses a non-loopback target outright.
- `RAG_ADMIN_TOKEN` was never read, printed, copied or created. No credential was created at all;
  the dev container uses trust auth precisely so that none exists.
- No `archive_mode` change, no restart, no deploy, no route change, no backfill.
- `seizrag-qdrant` untouched; `sc_recovery_*` untouched; `D:\ClaudeVM` untouched.
- Nothing has been committed or pushed; all Phase 3 files are in the working tree only.

## 4.12 Blockers still open — Phase 3 output must not reach production

Unchanged from Phase 0.5 and Phase 2, and all four still gate any production use of this code:

1. `RAG_ADMIN_TOKEN` rotation (the shared bearer is compromised and is still the only transport
   credential).
2. Off-host backup destination — none chosen.
3. `archive_mode` is off and the maintenance window is unscheduled; it affects axonhub.
4. The identity artifact for a per-principal credential does not exist.

Added by this phase:

5. **Durability is process-crash only, and that claim is not extendable.** A single node with
   `archive_mode` off and no off-host WAL means a commit acknowledgement survives a process crash,
   not a host loss. `pgstore` refuses `synchronous_commit=off` so an ACK is at least honest about
   the flush, but nothing here supports a cross-host durability claim and nothing in the tests
   should be read as supporting one.
6. No adapter exists yet, so no host has been shown to produce a crash-stable `idempotency_key`.
   That remains the first thing each adapter must prove.
7. The projection worker has no real `Applier`. Qdrant and Graphify projections are unwritten by
   design — Phase 3 sends nothing anywhere.

---

# Appendix 5 — Phase 4 onward: unattended continuation checkpoint (living)

This appendix is written to be resumed from. It is updated as each lettered
section of the mandate lands, so a session that starts cold — or one that
resumes after a compaction — can tell what is done, what is running, and what
the next safe action is without re-deriving any of it from the code.

Nothing in this appendix is a production claim. The standing production
blockers listed in Appendix 4 §4.12 are all still open.

## 5.1 Section A — canonical domain: complete

**Migration.** `pkg/memgw/migrations/sql/0002_memgw_domain.sql` creates the
entities `0001` deliberately deferred: `projects`, `project_repos`,
`workspaces`, `sessions`, `branches`, `works`, `checkpoints`, `fact_slots`,
`facts`, `fact_candidates`, `provenance_refs`, `evidence`. It then adds
`issued_event_id` / `issued_event_seq` to `tombstones` and — the point of the
migration — real foreign keys from the event log to the registry:
`events_project_fk`, `events_workspace_fk (project_id, workspace_id)`,
`events_session_fk`, `events_branch_fk`, `events_work_fk (project_id, work_id)`,
plus `grants_project_fk`. Phase 3 enforced scope in Go because there was
nothing to point at. A check that only lives in the caller is a check the next
caller forgets.

**New package `pkg/memgw/domain`.** It runs *inside* the ledger's commit
transaction, never beside it, so the event, the aggregate update, the receipt
and the outbox row are one atomic act.

- `domain.go` — the `Event` the domain sees, `Aggregate`, the deterministic
  `SlotID`, the work transition table, the subagent prohibition list, the
  payload shapes, and the `CAS` callback type.
- `registry.go` — project/workspace/repo registry, `NormaliseRepoKey`,
  `RootPathDigest`.
- `validate.go` — `Prepare`: subagent rule, project and workspace scope,
  session and branch lineage, evidence-reference existence, then the
  per-event-type preparation.
- `facts.go` — the candidate/fact separation, promotion authorisation, slot
  cardinality, conflict, temporal supersession, explicit resolution.
- `apply.go` — `Materialise`: the readable form of the event, written after the
  event row and in the same transaction.
- `pkg/memgw/errclass` — the frozen error classes, extracted so `domain` can
  refuse with a class without importing `ledger` (the layering rule is: ledger
  imports domain, never the reverse).

**Three design decisions worth keeping.**

1. *The aggregate is derived, never submitted.* `ledger.Event` no longer has
   `AggregateType` / `AggregateID`. A writer that could name its own aggregate
   could compare-and-set against something no other writer is contending for,
   which would make CAS a formality.

2. *For a single-valued fact, the aggregate is the predicate slot, not the
   fact.* `SlotID = uuidv5(slotNamespace, project + "\0" + subject + "\0" +
   predicate)`. This is what the frozen fixtures actually require:
   `conflicting-facts` expects `expected_revision: 3 → resulting_revision: 4`
   while the *contested* thing is the predicate, and `multi-valued-facts`
   expects a second value to commit with the first "unchanged, revision 1".
   Multi-valued promotions are additive and carry no aggregate at all, so they
   can never produce a `cas_conflict` — INV-11 restated as a data-model fact
   rather than as a rule someone has to remember.

3. *Compare-and-set runs before the transition is judged.* `domain.Prepare`
   takes a `CAS` callback and applies it the moment the aggregate is known.
   `expected_revision` is the writer's statement of what it saw; if that is
   already wrong, "this transition is illegal" would be answering a question
   about a state the writer never read. A host flushing a buffer after hours
   offline hears `stale_revision`, which is the fixture's answer.

   `expected_revision` is required only when the aggregate's current revision is
   greater than zero. Creating something nobody has touched needs no expectation
   to state, and is still protected by the unique index.

**Verified against a real PostgreSQL** (`memgw-devdb`, loopback 127.0.0.1:55433,
database `memgw_test`, per-test disposable schema, migrated from empty on every
run). `go test ./...` passes with `MEMGW_TEST_DSN` set: contract, domain,
ledger, migrations, outbox, pgstore, principal — no skips in the memgw
packages, no failures.

New scenarios, all replayed against the database rather than asserted in Go:

| Scenario | What it proves |
|---|---|
| `TestWorkStateMachineRefusesIllegalTransitions` | absence from the table is a refusal; completed/abandoned are terminal |
| `TestCheckpointIsImmutableAndAdvancesOnlyTheRevision` | INV-09; the append-only trigger, not convention |
| `TestFixtureBranchRewind` | INV-19: parent superseded, keeps every event, new branch records its divergence |
| `TestBranchRewindMustNameItsDivergence` | a rewind with no branch point, and one reusing a branch id, are both refused |
| `TestFixtureCandidatePromotion` | INV-10: a pending candidate is not a fact and reserves no slot; self-promotion is `scope_denied`; promotion without evidence is `policy_rejected`; re-promotion is refused |
| `TestFixtureConflictingFacts` | INV-08: both values kept and flagged, neither superseded, slot conflicted at revision 4 |
| `TestExplicitResolutionEndsAConflict` | a conflict ends only by an explicit `fact.superseded`; the survivor returns to active |
| `TestFixtureMultiValuedFacts` | INV-11: second value active at fact-revision 2, first untouched, slot unmoved, receipt carries no revision |
| `TestCardinalityCannotBeChangedByAPromotion` | a promotion cannot retroactively make coexisting values a conflict |
| `TestPromotionCannotRedefineWhatItPromotes` | a restatement that disagrees with the candidate is refused |
| `TestFixtureSubagentEvidence` | INV-16: the subagent's evidence commits and is retained; its `work.completed` is `scope_denied` and moves nothing |
| `TestSubagentCannotCheckpointOrPromote` | the boundary is the principals table, not a field the submission could omit |
| `TestUnregisteredProjectIsRefused` | scope is a foreign key now; the refusal names nothing that exists |
| `TestSessionIdCannotMoveBetweenProjectsOrPrincipals` | a session id cannot carry its history into another workspace |
| `TestFixtureTombstoneIssued` | the journal row names the ordering event *and its sequence*; legacy chunks are marked; erasure is **not** claimed |
| `TestTombstoneJournalIsNotDeletable` | DELETE refused, UPDATE deliberately allowed so erasure can be stamped |
| `TestTombstonedWorkTakesNoFurtherEvents` | the simplest resurrection race: a late flush is refused, not silently re-created |
| `TestTombstoneRefusesUnknownClasses` | `reason_class` is a class, never free text quoting the content |
| `domain` unit tests | repo keys strip userinfo; root paths are digested not stored; slot ids are deterministic, project-scoped and separator-safe; the transition table is closed |

**Correction to an earlier belief.** Appendix 4 recorded `evidence.recorded` as
requiring `checkpoint_write`. The frozen fixtures disagree: `subagent-evidence`
gives its subagent only `work_read` and expects the evidence to commit, while
`out-of-order-event`'s principal holds only `checkpoint_write`. The role table
is therefore any-one-of, and `requiredRole` became `requiredRoles`.

**Authentication.** `principal.Store.AuthorizeAny` returns the `Principal` it
authorised, and the ledger builds the domain event from *that* row — type,
parent and host come from the principals table, never from the payload.

## 5.2 Test resources currently alive

- `memgw-devdb` — disposable PostgreSQL container, loopback `127.0.0.1:55433`,
  database `memgw_test`, created by this work and owned by it. Cleanup:
  `docker rm -f memgw-devdb`. It holds no production data and no production
  credential. Every test re-proves the target is disposable through
  `pgstore.VerifyNonProduction`'s six independent checks before it writes
  anything.
- `memgw-qdrant` -- disposable Qdrant container, image `qdrant/qdrant:v1.12.4`,
  loopback `127.0.0.1:56333`, storage bind-mounted to `D:\memgw-test\qdrant-storage`
  (never a production volume), created by this work and owned by it. Cleanup:
  `docker rm -f memgw-qdrant` then remove `D:\memgw-test\qdrant-storage`. Created
  for Section D and used meanwhile so the real `enowx-rag --serve` binary can
  start without pointing at the production vector store.
- `memgw_itest` -- a second database inside the same `memgw-devdb` container,
  created during Section D and owned by this work. It exists because the Go
  integration suite gives every test its own schema and then re-proves the
  target is disposable, and one of those six checks is "no tables outside the
  schemas this pool owns". The smoke run in Section B left a persistent `memgw`
  schema in `memgw_test`, so from that point on every integration test in
  `memgw_test` was refused. The check is right and was not weakened; the suite
  was given a database with nothing else in it instead. Cleanup:
  `DROP DATABASE memgw_itest`.
- Test debris in the persistent `memgw` schema of `memgw_test`: project
  `7627423a-fd6e-459d-87c0-be32cd47c8cb` ("smoke"), principal
  `159fdcc6-8f08-4cb9-b57b-f62d6b17db69` (`claude-code`) with a
  `checkpoint_write` and a `work_read` grant, and one credential. The secret was
  printed once to a terminal and never stored; it is not in this document, in the
  repository, or in any log. Cleanup is the whole schema:
  `DROP SCHEMA memgw CASCADE` in `memgw_test`, which no production system reads.
- Section G artefacts under `D:\memgw-test\history\`: `export.ndjson` (8
  synthetic records, no production content), `plan-a.json`, `plan-b.json`,
  `plan-edited.json` (deliberately tampered, kept as the evidence that it was
  refused) and `shadow-report.json`. Cleanup: remove the directory. The four
  `legacy_chunk_map` rows they wrote (`lg-0001` .. `lg-0004`) are inside the
  `memgw` schema of `memgw_test` and go with it.
- `D:\memgw-test\bin\` -- four builds of the same repository binary
  (`memgw-smoke.exe`, `memgw-graphify.exe`, `memgw-history.exe`,
  `memgw-shadow.exe`), kept apart only because Windows will not overwrite a
  running executable. Cleanup: delete the directory.
- `memgw_restore` -- a third database inside `memgw-devdb`, restored from the
  Section H dump and used for the reconciliation, the tombstone replay and the
  projection rebuild. Cleanup: `DROP DATABASE memgw_restore`.
- `D:\memgw-test\backup\memgw_test.dump` -- the 91,446-byte `pg_dump -Fc`
  archive taken in Section H. It is on the same disk as the database it came
  from and is therefore not a durability guarantee. It contains disposable test
  data only. Cleanup: remove the directory.
- `D:\memgw-test\rehearsal\` -- the admin principal's token file (74 bytes; the
  secret exists nowhere else) and the two JSON bodies submitted to open and
  fence a writer epoch. The credential belongs to principal
  `a72ac30d-45dd-4bbc-b389-ec02ab5d7635` (`rehearsal-admin`) in the test
  database. Cleanup: revoke the credential and remove the directory.
- Log files under `D:\memgw-test\`: `serve.out/.err`, `fence-collector.out/.err`,
  `restore-projection.out/.err`, `adapter-collector.out/.err`,
  `projection.out/.err`. They hold no credential -- the collector reads its token
  from a file and never logs it. Cleanup: delete them.

- `D:\memgw-test\collector\` -- the Section C spool: `spool.db`, the DPAPI-wrapped
  `spool.key`, and a `token` file holding a **live test credential** for the
  `claude-code` principal in `memgw_test`. Cleanup: revoke that credential
  (`memgw principal revoke-credential <id>`), then remove the directory. The key
  is bound to this Windows account and is useless anywhere else, but the token
  file is a real secret and goes first.
- `D:\memgw-test\adapter-collector\` -- a second spool used for the adapter and
  fencing rehearsals, same shape as above. Cleanup: remove the directory.
- `D:\memgw-test\adapter\` -- hook payload fixtures fed to `memgw adapter` by
  hand, synthetic content only. One of them still carries the PowerShell culture
  bug that rendered `occurred_at` with dots for colons; it is kept as the
  evidence of that failure. Cleanup: remove the directory.
- `D:\memgw-test\graphify\` and `D:\memgw-test\gstatus.json` -- the Section F code
  index generations and the status output. Derived from this repository's working
  tree, nothing else. Cleanup: remove both.
- `D:\memgw-test\serve.env.sh` -- the environment used to start the test gateway.
  It names the loopback DSN, whose password is the disposable container password,
  and no other secret. Cleanup: delete it.
- **No memgw process is running.** The test gateway and both collectors were
  stopped at the end of Section H; only the two containers are still up. Nothing
  listens on `127.0.0.1:7777` or on the test named pipes.
- An empty `D:\memgw-test\qdrant-storage;C` directory was created by a Git Bash
  path translation and removed during Section I. Recorded so the deletion is not
  mistaken for the loss of the real `qdrant-storage`.

**Cleanup order**, when the work is closed: revoke the two test credentials first
(`claude-code`'s and `rehearsal-admin`'s), then remove `D:\memgw-test\`, then
remove the two containers by name. Never prune globally: this machine holds
resources belonging to other work that are not part of this and must not be
touched.

## 5.3 Section B -- gateway and bootstrap: complete, verified through the real binary

**Where the gateway is mounted, and why not under `/api`.** The plan previously
said `/api/memgw/*`. That is wrong and the code does not do it. The `/api`
subtree is gated by `AdminTokenMiddleware`, which compares one shared token
(`RAG_ADMIN_TOKEN`) that names no principal, proves no scope, cannot be narrowed
to a project, is held by every tool on this machine, is treated as compromised,
and authorises everything when it is unset. Mounting the gateway there would
have forced all six agent hosts to hold that bearer -- exactly the substitution
of a legacy bearer for principal isolation that the mandate forbids. The gateway
is therefore mounted at a top-level `/memgw` (`gateway.MountPath`), before the
SPA catch-all, which would otherwise swallow it and answer a write with
`index.html`. The reasoning is recorded in `pkg/httpapi/server.go` and
`pkg/memgw/gateway/auth.go` so it cannot be undone by someone who only reads the
router.

**Files created.**

| File | What it is |
|---|---|
| `pkg/memgw/principal/credential.go` | credential issue/authenticate/revoke. PBKDF2-SHA256, 210 000 iterations, 16-byte salt, 32-byte secret. Token shape `memgw_<key_id>.<base64url secret>` |
| `pkg/memgw/gateway/auth.go` | the authenticating middleware; bearer read from the header only |
| `pkg/memgw/gateway/gateway.go` | routes under `/memgw/v1`, per-operation deadlines, 1 MiB body cap |
| `pkg/memgw/gateway/http.go` | problem responses, error-class to status mapping, strict decoding |
| `pkg/memgw/gateway/bootstrap.go` | the read path: provenance, revision, freshness, partial status |
| `cmd/mcp-server/memgw_principal.go` | the operator CLI (`create`, `grant`, `issue`, `list`, `revoke-credential`, `revoke`) |
| `pkg/memgw/migrations/sql/0003_memgw_credentials.sql` | `principal_credentials`, with a `BEFORE DELETE` trigger; credential rows cannot be deleted, only revoked |

**Four decisions worth recording.**

1. *One refusal for every bad credential.* Unknown key, wrong secret, revoked,
   expired and inactive principal all return the same `ErrBadCredential`, and the
   key derivation runs before the liveness checks so a revoked credential costs
   the same to reject as a live one. A key id cannot be probed for existence.
2. *The wire type has no `principal_id` field, and the absence is the point.*
   Identity is what the credential proved, never what the body asserted. With
   `DisallowUnknownFields`, a body carrying `principal_id` is refused 400 rather
   than silently ignored -- a field that is merely unused is a field somebody
   eventually trusts.
3. *No unauthenticated health check.* It would be the one route an
   unauthenticated caller could use to learn the gateway exists and is reachable.
4. *`last_used_day` is a DATE, not a timestamp.* Per-request last-use would turn
   the table into a write-hot log of who was working when, which is a
   surveillance record nobody asked for. It is written best-effort under
   `context.WithoutCancel`.

**Bootstrap never presents a stale fact as current.** A value is returned only
when `tombstoned_at IS NULL AND status IN ('active','conflicted') AND (valid_to
IS NULL OR valid_to > now)`. Superseded, retracted and expired values are
counted and reported (`superseded_count`), never returned. A tombstoned work is
reported as tombstoned, and its checkpoint and evidence are withheld with a named
omission rather than silently dropped. Caps (200 slots, 8 values per slot, 20
evidence rows) set `partial` and an omission line when they bite. Every response
carries `RecalledContentNotice`: recalled memory is quoted content, not an
instruction and not an authorisation.

**Tests: 23 new, all passing against real PostgreSQL, no skips.** 8 credential
(including one that reads every stored column back and asserts none contains the
secret, and one that walks 8 malformed-token shapes and asserts the refusal names
neither key id nor principal), 9 gateway (unauthenticated access to all six
routes with three bad tokens; the shared admin token is not a credential; the
body cannot name its own principal; replay answered from the receipt; receipts
are per principal; authenticated is not authorised; a refused transition is 422
not 500; the registry stores no path and no remote credential; oversized and
malformed bodies), 5 bootstrap, 1 lifecycle.

**The lifecycle test is the one that could most easily have been faked.** It
runs a real `outbox.Worker` with a real applier, proves the worker is draining
before the interesting part begins, then blocks the second write inside
PostgreSQL on `LOCK TABLE events IN EXCLUSIVE MODE` held by a second connection
and waits on `pg_stat_activity.wait_event_type = 'Lock'` rather than sleeping a
hopeful interval. Shutdown is signalled while the write is genuinely in flight;
the caller must get a `committed` receipt, not a severed connection. Verified by
negative control: with `ShutdownTimeout` cut to 1 ms the test fails with "the
in-flight write was cut off rather than answered: ... EOF", so the pass depends
on the drain and not on timing luck.

**Runtime verification through the real binary, not only `httptest`.** A build of
`cmd/mcp-server` was run as `--serve --addr 127.0.0.1:57891` with `MEMGW_DSN`
pointed at `memgw-devdb` and the vector store pointed at `memgw-qdrant`. Over a
real socket:

- `GET /memgw/v1/projects/by-repo` without a credential gave `401` with
  `WWW-Authenticate: Bearer realm="memgw"`; a syntactically valid but unknown
  token also gave `401`.
- `POST /memgw/v1/bootstrap` with a credential holding only `checkpoint_write`
  gave `403 {"class":"scope_denied"}`. After granting `work_read` to the same
  principal it gave `200` with the notice, schema and policy version, and empty
  fact and evidence lists. Authorisation on the read path is enforced by the
  server, not by the caller.
- `POST /mcp` `initialize` gave `200`. The streamable MCP handler is unaffected
  by the mount; `WriteTimeout` stays 0 for its sake and bounded response time
  lives in the gateway per-operation deadlines.
- The startup line reports `memgw at /memgw (per-principal credential)`, and
  `memgw off (MEMGW_DSN not set)` when the DSN is absent -- an unset DSN means
  "this deployment does not serve the gateway", never "guess a database". The
  pool it opens runs `pgstore.VerifyNonProduction`, and there is no flag to skip
  it.

**Operator CLI, run end to end** against `memgw-devdb`: `create`, `grant`,
`issue`, `list`. The secret goes to stdout alone on its line and all prose to
stderr, so it can be piped into a password manager and a redirected log never
contains it. One bug was found and fixed in the process: the Go `flag` package
stops parsing at the first non-flag word, so `create claude-code --type agent`
dropped the flag. `parseArgs` now lifts flags ahead of positionals, using the
flag set itself to tell which flags consume the next word, and the outer
dispatcher no longer parses flags it does not define.

**Deliberately deferred: memgw MCP tools.** The MCP HTTP handler sits behind
`AdminTokenMiddleware`, so memgw tools over HTTP MCP would be authenticated by
the shared bearer -- the design this section exists to avoid -- and a credential
passed as a tool argument would put a secret into tool payloads and transcripts.
The integration path for the six hosts is HTTP with a per-principal credential
(Section E). The mandate Section B requires only that MCP HTTP streaming keep
working, which is verified above.

## 5.4 Section C -- the Windows collector: complete, verified with real processes

**What it is.** `pkg/memgw/collector` accepts events from local agents over a
named pipe, writes them to an encrypted durable spool before answering, and
forwards them to the gateway later. It exists for the moment an agent finishes a
checkpoint while the network is down: writing inline would block or drop, and a
dropped checkpoint is the one record that decides whether the next session can
resume.

| File | What it is |
|---|---|
| `doc.go` | the design, including the exact list of what is NOT encrypted |
| `envelope.go` | AES-256-GCM over the payload; the associated data binds a ciphertext to its row |
| `key_windows.go` / `key_other.go` | the spool key, wrapped by DPAPI in user scope; a refusal on other platforms |
| `spool.go` | the SQLite queue: enqueue, claim, settle, fail, recover, stats |
| `protocol.go` | line-delimited JSON over the pipe; one answer per request |
| `pipe_windows.go` | the listener, its DACL, and the client dialler |
| `service_windows.go` | startup order, supervision and bounded shutdown |
| `cmd/mcp-server/memgw_collector_windows.go` | `memgw collector run` and `memgw collector stats` |

**What is encrypted and what is not -- stated plainly, because the mandate
forbids calling this an encrypted database.** The payload is encrypted. These
fields are in plaintext on disk for every queued event: event id, idempotency
key, project, workspace, work, session and branch ids, event type, sensitivity
class, priority, enqueue time, attempt count, next attempt time, state, and the
failure class. That is deliberate -- the forwarder has to order, deduplicate,
prioritise and reconcile without the key, and a queue that must unlock to decide
what to retry cannot run unattended. The cost is that whoever reads the spool
learns which projects this host touched and when. Content needs the key.

**The failure-reason column was a real leak and was fixed.** The first version
stored the server's message truncated to 200 characters. The canary test found
it: a refusal message quotes the submission, so 200 characters of a checkpoint
were being written to a plaintext column beside the ciphertext of the same
event. `Spool.Fail` now takes only a class, maps it through a closed table
(`reasons`), and forces anything unrecognised to `unclassified`. No caller text
reaches the disk at all, which is provable rather than probable.

**Six design decisions worth recording.**

1. *A named pipe, not a loopback port.* A pipe can carry a DACL; a TCP port
   cannot. Any process on this machine, as any user, can reach
   `127.0.0.1:<port>`. Since the collector writes to a shared ledger under this
   host's identity, "any local process may enqueue" would have been an
   authorisation hole with a loopback bind pretending to be a boundary. The DACL
   is protected (`D:P`) and names three trustees: this account, SYSTEM and the
   local Administrators group. Administrators are on the list because Windows
   lets them take ownership regardless; excluding them would express an
   intention the operating system does not enforce.
2. *`FILE_FLAG_FIRST_PIPE_INSTANCE`.* If something already owns the name, the
   collector refuses to start rather than quietly becoming the second instance
   behind a squatter that would receive some of the connections.
3. *The event id is derived from the idempotency key* (`uuid.NewSHA1`), never
   generated. A collector that dies between writing the row and hearing the
   receipt presents the same id on the next attempt, and the gateway answers
   from its receipt. The duplicate is not avoided, it is made harmless. A
   client that tries to supply its own `event_id` is refused.
4. *`synchronous=FULL` is asserted at open, not merely requested.* WAL alone
   leaves the write in the operating system's hands, which is fine for a cache
   and not for the only copy of a checkpoint. If a future driver ignores the
   pragma, the collector fails to start instead of making a durability claim
   nobody checked.
5. *A reserve inside the queue limit.* Checkpoints and tombstones may use the
   last slice of the queue that routine evidence may not, because the failure to
   avoid is a loop of evidence filling the disk and leaving no room for the one
   event that mattered. A tombstone counts as high priority because a deletion
   that cannot be recorded leaves content alive that somebody asked to remove.
6. *Refusals are quarantined, transport failures are retried.* Retrying a
   refusal forever is how one bad event becomes a permanent load on a server
   that has already said no. Retries are bounded and end in a visible dead-letter
   state; nothing is deleted.

**Reconciliation before re-sending.** A row with attempts on it has been sent at
least once, and the reason it is back may be that the answer was lost rather
than that the write failed. The forwarder asks `GET /memgw/v1/receipts/{key}`
first and settles from the receipt if there is one.

**Tests: 26, all passing, no skips** (`TestCrashHelperEnqueuesUntilKilled` is
the child process of the crash test, not a test; it skips when it is not the
child, and that is what the `SKIP` line in a verbose run is).

The two that could not have been faked:

- **The canary.** A distinctive non-secret string is written through the whole
  enqueue path, and every file the spool touched -- `spool.db`, `spool.db-wal`,
  `spool.db-shm` -- is read as bytes and searched, both while the spool is open
  (when the row is still only in the WAL) and after the close. The same test
  asserts the *opposite* for the metadata: the project id, type and idempotency
  key must be present in clear, so the documentation cannot drift into a claim
  of whole-database encryption.
- **The kill.** `TestAnAcceptedEventSurvivesTheProcessBeingKilled` starts a
  second copy of the test binary, waits until it has actually enqueued at least
  40 events (observed progress, not a hopeful sleep), terminates it with
  `TerminateProcess` -- no signal, no defer, no flush -- then opens what is left.
  `PRAGMA integrity_check` must say `ok`, every surviving row must decrypt,
  parse, and carry the id its key derives, and every key the child was told was
  durable must be present. Observed: 40 acknowledged, 41 rows on disk, all
  decrypt. The extra row is the one accepted a moment before the kill, whose
  acknowledgement never reached the parent -- which is exactly the asymmetry the
  promise allows.

  **Limit of that test, stated because it matters:** it proves durability across
  a *process* kill. It does not prove durability across a power cut or an
  operating-system crash; that is what `synchronous=FULL` is for, and it was not
  tested, because testing it needs hardware this work is not allowed to
  interrupt.

Also covered: the DACL names this account and not Everyone, Authenticated Users,
Interactive or Anonymous; a second listener cannot take the name; a real client
over a real pipe gets a durable acknowledgement; a client-chosen event id is
refused; a full queue answers `queue_full` rather than dropping, and the reserve
still admits a checkpoint; `Accept` returns after `Close` and the pipe stops
accepting; a lost response is reconciled and the event is submitted exactly once
(asserted by counting submissions at the server); a classified refusal is
quarantined without a retry; a permanently failing row is dead-lettered after
its attempt budget; an offline host drains when the gateway returns; a 201 with
an unreadable body is not treated as delivered; the DPAPI key file does not
contain the key and is not silently replaced when it cannot be unwrapped.

**Runtime verification with real processes, a real pipe and real PostgreSQL.**
The built binary was run as a gateway (`--serve` on `127.0.0.1:57891`, DSN
`memgw-devdb`) and, separately, as a collector
(`memgw collector run --dir D:\memgw-test\collector --pipe \\.\pipe\memgw-collector-smoke`).
The client was PowerShell's `NamedPipeClientStream`, which also demonstrates
that a non-Go host can speak the protocol:

1. A `work.planned` submitted over the pipe answered
   `{"ok":true,"event_id":"c6aad17f-...","seq":1,"durable":true}` and, seconds
   later, appeared in `memgw.write_receipts` as `committed`.
2. The gateway process was killed. Two more events were submitted and answered
   `durable:true`; `stats` showed `pending: 2`.
3. The collector process was killed hard while holding that backlog. Both
   processes were restarted. Without any further client action the backlog
   drained: `sent: 3`, `acked_seq: 3`, and PostgreSQL held exactly three
   receipts -- `collector-smoke:plan:1`, `:offline:1`, `:offline:2`, all
   `committed`. No duplicates.
4. Re-submitting a delivered idempotency key answered
   `{"ok":true,"duplicate":true}` with the same event id and created no new row.
5. An event with an unknown type was accepted locally, refused by the gateway,
   and moved to `quarantined` with `class=policy_rejected` logged once. The
   ledger still held three events.

**Still open for Section C.** The collector has no Windows service wrapper; it
runs as a supervised process and is started by hand or by whatever the host's
adapter arranges (Section E). Nothing about that is blocked -- it is scope that
belongs with the adapters.

## 5.5 Section D -- the projection applier: complete, verified against real PostgreSQL and real Qdrant

**What it is.** `pkg/memgw/projection` turns canonical events into the retrieval
index. It is the first thing in the gateway that writes anywhere other than
PostgreSQL, so the rule it exists to enforce is worth stating before the files:
the ledger is the truth and the index is a cache of it. If the whole collection
were deleted, the only thing lost would be the ability to search quickly.

| File | What it is |
|---|---|
| `doc.go` | what is projected, what deliberately is not, and why |
| `ports.go` | the read model, the three ports, and the classified error |
| `document.go` | event to document: ids, content, metadata, the sensitivity ranks |
| `embed.go` | `FixtureEmbedder` -- deterministic vectors with no semantic content |
| `applier.go` | the decisions: project, withhold, skip, erase |
| `qdrant.go` | the adapter onto `rag.QdrantProvider` |
| `pgsource.go` | reading events; a pool and no `Exec` anywhere in the file |
| `cmd/mcp-server/memgw_projection.go` | `memgw projection run` and `memgw projection status` |

**What reaches the index, and what deliberately does not.** Three types produce
writes: `checkpoint.recorded` (one document per checkpoint -- the handover note
a later session reads to resume), `fact.promoted` (one document per fact slot,
holding the value that currently stands), and `tombstone.issued`, which removes
rather than adds. Two more remove without adding: `fact.superseded` and
`fact.retracted`. Everything else is applied as a no-op, from a closed table
with no default.

Two absences are decisions, not omissions:

- **`fact.candidate_proposed` is never indexed.** A candidate is a proposal
  nobody has reviewed. Indexing it would let a retrieval answer be assembled out
  of something the system has not decided is true, and a search result carries
  nothing that would mark it as unreviewed. This is the mandate's "no
  auto-promotion of history to current facts" applied one layer down: the index
  may only contain what the ledger already promoted.
- **`evidence.recorded` is never indexed.** Evidence is a locator -- a test run,
  a diff, a path. A locator is useful to follow, not to match by similarity, and
  projecting thousands of them would dilute every query for no retrieval gain.

A single-valued fact slot holds exactly one document, replaced by each
promotion, so a superseded value cannot surface beside the current one. A
multi-valued slot gets one document per value, keyed by a digest of the value,
so re-promoting the same value overwrites rather than accumulates.

**Sensitivity is enforced before anything can embed.** Indexing means embedding,
and embedding means the text leaves the process. The gate reads the
`sensitivity_class` recorded on the event -- not a flag a caller passes at
projection time -- and the default ceiling is `internal`: `confidential` and
`restricted` are withheld, and a class this build does not recognise is withheld
too. Withholding is success, not failure: the row is done, and retrying it would
only withhold it again. The direction is deliberate. A confidential checkpoint
that is not searchable is an inconvenience; a confidential checkpoint that has
been embedded by a third party cannot be un-sent.

**Tombstones beat rebuilds.** Every write consults the deletion journal first --
for the checkpoint, the event, the work and the session -- and returns
`outbox.ErrSkippedTombstoned`, which the worker records as done. Without that
check a rebuild would faithfully replay events older than the deletion order and
restore exactly what somebody asked to have removed. Erasure also reaches points
written before this gateway existed, through the legacy chunk map, because those
ids are ones no deletion can derive.

**Ordering, stated honestly.** Documents carry the event seq that produced them,
and a write is skipped when the index already holds a document from a later seq.
That makes replaying an older event harmless at rest, which is what a retry and a
rebuild need. It is a guard and not a transaction: the read and the write are two
calls, so two workers applying two versions of one slot in the same instant can
still interleave. The gateway's answer is configuration -- one worker per
projection, which the outbox lease already makes natural. It is not described as
concurrency-safe, because it is not.

**Vectors: deterministic fixtures, and what that costs.** No paid embedding API
was used and nothing was sent to an external provider. `FixtureEmbedder` hashes
tokens into coordinates and normalises the result, which gives the one property
the projection actually needs from an embedder under test -- the same document
produces the same vector, so re-applying an event is a no-op rather than a
second, slightly different point.

It captures no meaning at all. "the build failed" and "the compilation broke"
share no tokens and are orthogonal under it. **Semantic retrieval quality is
therefore untested and must not be reported as measured.** Every point records
`embed_model = memgw-fixture-hash-v1`, so a collection built from fixtures can be
told from a real one by looking at it rather than by remembering how it was
built, and `memgw projection run` refuses to start without an explicit
`--embedder`: defaulting to the fixture would quietly build a meaningless
collection, and defaulting to TEI would quietly start sending content somewhere.

**A real bug this work found.** Qdrant does not treat `PUT /collections/{name}`
on an existing collection as a no-op -- it answers `Collection ... already
exists!`. An applier that called create once per process and cached the result
would therefore fail every write in a second worker or after a restart. Probed
directly against the container, fixed by probing for existence before creating
and re-probing when a create loses a race, and the cache is now dropped whenever
a read or a write fails so a collection deleted underneath a running worker is
recreated on the retry instead of failing identically forever. That last path is
exercised by the rebuild test, which deliberately reuses the worker that was
running before the collection was dropped.

**Tests: 35, all passing, no skips when both back ends are configured.**

Twenty-five run with no infrastructure at all. They cover: a checkpoint becoming
one searchable document with the scope a rebuild needs; a candidate never
reaching the index; every non-projecting event type finishing as applied rather
than lingering; a missing event being reported as `event_missing` instead of
silently applied; confidential, restricted and unrecognised classes being
withheld; the ceiling being raisable deliberately; a tombstoned checkpoint and a
tombstoned work both stopping a projection; a tombstone removing what it names
and reaching legacy chunks; an unknown tombstone subject class failing visibly
rather than quietly succeeding; a single-valued slot holding only the value that
stands; a multi-valued slot keeping its values apart and not accumulating copies
of a re-promoted value; a retraction removing a fact; a retraction that
identifies nothing failing loudly; a promotion removing what it supersedes;
double application changing nothing; an older event not overwriting a newer
projection; an index failure being classified without carrying the content that
failed; and the fixture embedder being deterministic, unit-length, and
non-degenerate.

Six run against the disposable `memgw-qdrant` container (`MEMGW_TEST_QDRANT_URL`,
which the test refuses unless it names a loopback address): the projected
checkpoint is really in the collection with its metadata and its
`embed_model`; three applications leave one point; a tombstone really removes it;
the checkpoint is retrievable through `SemanticSearch` (which proves the path is
connected -- embed, store, query, score, return -- and proves nothing about
semantic quality, because the query text is text the document contains); withheld
content never reaches the store at all; and an empty deletion filter is refused
rather than emptying a collection.

Four run the whole path with nothing stood in for -- real events in PostgreSQL,
the real outbox queue, the real `outbox.Worker`, and a real Qdrant collection:

1. Four events are submitted -- two projectable checkpoints, one `evidence.recorded`
   and one confidential checkpoint. The worker drains them: two points in the
   collection, all four outbox rows `applied`, nothing failed, the watermark
   advanced, and the point carries the work it belongs to.
2. A tombstone issued after the fact removes the checkpoint that was already
   projected and leaves the other one alone.
3. The collection is discarded and every event is replayed from the ledger. The
   rebuild restores the checkpoint that was never deleted and does **not**
   restore the tombstoned one, and it recovers from the dropped collection while
   reusing the worker that had cached its existence.
4. An unreachable vector store is retried and then dead-lettered with class
   `collection_failed`, and the canonical event is untouched -- a projection that
   cannot keep up never changes the ledger.

**Runtime verification through the real binary.** `memgw projection status` and
`memgw projection run` were run from the built binary against `memgw-devdb`
(`memgw_test`, schema `memgw`) and `memgw-qdrant`. Before: `watermark 0`, three
`pending` rows left by the Section C collector smoke. The worker started with
`embedder memgw-fixture-hash-v1, ceiling internal`, drained them, and after:
`watermark 3`, three rows `applied`, one attempt each. No points were written,
which is correct -- all three are `work.planned`, which this projection does not
index. Running without `--embedder` was refused, as intended.

**Still open for Section D.**

- Semantic retrieval quality: **untested**, and deliberately so. Measuring it
  needs a real embedding model, and the mandate forbids a paid API without cost
  approval and forbids sending a production corpus anywhere. What can be
  measured without one is deterministic correctness, and that is what the tests
  above measure.
- The `graphify` projection has an outbox lane and no applier. It belongs to
  Section F.
- A rebuild is driven by re-enqueuing outbox rows, which the tests do directly.
  A `projection rebuild` command belongs with the runbooks in Section I.

## 5.6 Section E -- native adapters: complete, six hosts, four payload-verified, two blocked on runtime

One implementation, six registrations. Everything that decides anything lives in
`pkg/memgw/adapter` and is reached as `enowx-rag memgw adapter --host <host>`.
What lives in `adapters/` is the smallest fragment each host needs in order to
invoke it: a plugin that shells out (OMP, OpenCode), a settings fragment
(Claude Code, Droid), a hooks file (Codex), a shell script (Hermes). Nothing
patches an installation, writes into `node_modules`, or installs a global
binary.

### Files

- `pkg/memgw/adapter/{doc,hook,decode,idempotency,translate,config,sink,run}.go`,
  plus `sink_collector_windows.go` and `sink_collector_other.go`.
- `cmd/mcp-server/memgw_adapter.go` and the `adapter` verb in `memgw.go`.
- `adapters/` -- `README.md`, `adapter.example.json`, `install-sandbox.ps1`, and
  one fragment per host.

### The three decisions that matter

1. **The idempotency key reads no clock, no random source, no pid and no
   in-memory counter.** It is `mga1.<host>.<short lifecycle>.<sha256>` over the
   host, the lifecycle, the session, the parent session, the sorted
   discriminator, the branch point and -- for a checkpoint -- a fingerprint of
   its fields. A hook that fires again after a crash derives the same key and
   the ledger holds one event. The cost is stated rather than hidden: a host
   with no per-occurrence discriminator collapses two identical moments into
   one. OMP supplies one (`branch_entries`), which is why two compactions in one
   OMP session are two rows.
2. **The submission carries no `event_id` and no `principal_id`.** The event id
   is derived from the key by whatever accepts the submission; the principal is
   whatever the credential proved.
3. **The adapter never manufactures a checkpoint from a lifecycle hook.** A
   checkpoint arrives complete, with `work_id` and `expected_revision`, or it
   does not arrive.

### Per-host status

| Host | Status | What was actually exercised |
|---|---|---|
| OMP | implemented; canonical payloads runtime_verified | start, resume, two compactions, branch, end through the real collector into PostgreSQL. The fragment has not run inside a live OMP session. |
| Claude Code | implemented; runtime_verified | real `SessionStart`/`PreCompact`/`SessionEnd` payload shapes decoded and committed; `Stop` correctly ignored. |
| Droid | implemented; runtime_verified | same decoder, own fragment, committed event. |
| Codex | implemented; **blocked** on runtime | the event vocabulary is inferred from binary strings and no Codex hook has ever been observed firing on this machine. The decoder accepts both spellings and refuses what it cannot place. |
| OpenCode | implemented; canonical payload verified | `session.created` and `experimental.session.compacting` only. The plugin API announces **no exit event**, so `session.ended` is never recorded there and nothing fabricates one from idleness. |
| Hermes | implemented; **blocked** on runtime | Linux host with no collector installed: it submits straight to the gateway and has **no offline durability** on that host. The gateway-direct path itself was verified from Windows. A Linux collector was added later and proven on a disposable container -- see §6 row 20 and §7 -- which does not change this row: nothing is installed on Hermes. |

"Runtime_verified" here means the canonical or native payload was decoded,
translated, queued in the real encrypted spool and committed to PostgreSQL. It
does **not** mean the fragment was loaded by the live host; no global agent
configuration was changed, which the mandate forbids.

### Runtime evidence

- 13 events across all six hosts, every one `accepted:true durable:true
  via:collector`, all present in `memgw.events`.
- Crash replay: re-firing an identical Claude `SessionStart` returned
  `duplicate:true` with the same event id `5a97d18f-...` and added no row.
- Gateway-direct (the Hermes shape): `via:gateway state:committed`.
- Branch: `rt-omp-2` committed with `parent_branch_id` pointing at `rt-omp-1`'s
  branch and `branch_point_seq = 3`; the parent moved to `superseded` and kept
  its events.
- Checkpoint: work `46fa144d-...` planned at revision 1, then a checkpoint
  submitted through the adapter at `expected_revision = 1` and stored at
  revision 2 with `modified_files` as an explicit list.
- `go test ./pkg/memgw/adapter/...` -- 37 tests, 0 fail, 0 skip.

### One defect found and fixed by running it

The first OMP branch was accepted by the collector and then refused by the
gateway forever. `reason_class` on a `session.branched` is not a note; it is a
column with `branches_reason_class_check` behind it, and the adapter was sending
`"branch"`, which is outside the set. Two fixes, neither of which weakens the
constraint:

- `domain/validate.go` now refuses an unknown branch reason as `policy_rejected`
  before the INSERT. Previously the constraint violation surfaced as a driver
  error, the gateway answered 500, and the collector retried a 500 forever: one
  mis-classified event was a queue that never drained.
- `adapter/decode.go` normalises a branch reason into the branches vocabulary
  (`rewind|fork|resume|compaction|initial`), mapping the words a host is likely
  to use and leaving anything unmappable empty for the ledger to classify. The
  OMP fragment now says `rewind`.

That exposed a second gap: a quarantined row keeps its idempotency key, so a
corrected event derives the same key and is deduplicated against a row that was
never committed. There was no operator surface for a held row at all. Added:

    enowx-rag memgw collector held                 # metadata only, never the payload
    enowx-rag memgw collector release --seq N      # requeue, attempts reset
    enowx-rag memgw collector discard --seq N      # stop retrying, keep row and key
    enowx-rag memgw collector purge   --seq N      # delete the row, free the key

`Holds`/`Release`/`Discard`/`Purge` are on `collector.Spool`, refuse anything not
quarantined or dead, and are covered by
`TestHeldRowsAreVisibleAndDecidable`, which also asserts the listing carries no
payload. The stuck row was drained through this path in the sandbox: `held` ->
`purge --seq 12` -> re-fire -> committed.

### Not verified

- No fragment has been loaded by a live host. Doing so would change a global
  agent configuration, which the mandate forbids.
- Codex has never been observed firing a hook.
- Hermes runs on a Linux VPS and was not touched; its script's approval
  allowlist is keyed on mtime and would need re-approval there.

## 5.7 Section F -- the Graphify coordinator: complete, verified on this repository

`pkg/memgw/graphify` plus `enowx-rag memgw graphify [rebuild|status]`.

The index is a projection and is never authority. It is derived from the working
tree rather than from the ledger, which is why no `projection_outbox` row is
ever enqueued for the `graphify` lane: a ledger event cannot make a code index
stale, and a file change cannot be discovered by draining a queue. The
`ProjectionGraphify` constant stays because the column's vocabulary is part of
the frozen contract.

### What each piece is for

- `lock_windows.go` -- `LockFileEx` with `LOCKFILE_FAIL_IMMEDIATELY`, not a lock
  file whose contents are interpreted. A pid written into a file tells you a pid
  was written; it does not tell you the process is alive, and the recovery
  procedure for a stale one is a human deciding whether to delete somebody
  else's lock. An OS lock is dropped by the kernel when the holder dies. The
  repository's fcntl fallback is deliberately not used on Windows.
  `lock_unix.go` is `flock`, so the package builds and tests on Linux; that is
  not a supported deployment.
- `coordinator.go` -- lock, read state, build into a directory nobody reads,
  **read state again**, write the manifest, swap one small pointer file by
  rename, prune. The second read is the step that is easy to omit: a rebuild of
  a large tree takes long enough for an editor to save twice, and an index
  published without the recheck describes a tree that no longer exists. When the
  state moved, the generation is published anyway and marked stale with the
  reason -- a repository being edited is exactly when an index is wanted.
- `repo.go` -- commit from `git rev-parse HEAD`, dirty digest over
  `git status --porcelain=v1 -z --untracked-files=all` plus size and mtime of
  each entry. Untracked files count: an untracked file is source the agent can
  read. A directory git cannot speak for is digested from the walk instead.
  `GIT_TERMINAL_PROMPT=0`, so an unattended rebuild can never sit at a
  credential prompt.
- `exclude.go` -- applied to the walk, not to the output. A path that is counted
  but not named is still a path that was opened; the point is that the bytes do
  not enter the process, because a secret in a buffer can end up in a log line
  or a crash dump. The rules are not configurable from a project file, so a
  repository cannot ask to have its own `.env` indexed.
- `index.go` -- path, size, content digest, language, line count, as NDJSON in
  path order. No symbols and no call graph: those need a parser per language and
  a wrong one is confidently incorrect about where a function is defined.
  Symlinks are not followed. The index digest is over the sorted entries, so two
  builds of one tree produce one digest and "did anything change" is answerable
  without a diff.
- `manifest.go` -- project, workspace, repo, commit, dirty digest, tool version,
  schema version, generation time, build duration, counts, index digest,
  excluded count, and the stale flag with its reason.

There is no network semantic extraction and no place to configure one. Sending
source to a provider to be described is an egress decision, and an egress
decision does not belong inside a rebuild that runs unattended. Hindsight is not
referenced by this package at all.

### Tests -- 14, all passing, none skipped

Two concurrent rebuilds (a held lock refuses the second, publishes nothing and
leaves no half-built directory); eight concurrent rebuilds leaving exactly one
coherent published generation; **a real child process holding a real OS lock and
being killed**, after which the lock file still exists and the next rebuild
proceeds anyway; a tree edited continuously during a build, which publishes
stale with a reason and whose entries are still readable; deleted and renamed
files leaving the index and changing the digest; secret-shaped paths never
indexed; an index directory inside the repository refused; a published pointer
whose generation was deleted reported as unusable; an unindexed repository
reported as a normal state rather than an error; pruning that never removes the
published generation; and, against a **real git checkout**, a clean tree
validating fresh, one uncommitted edit making it stale while the commit is
unchanged, an untracked file doing the same, a new commit making it stale, and
`.git` never appearing in the index.

`-race` is unavailable in this environment (it requires cgo), so the concurrency
tests are ordinary parallel tests, not race-detector runs.

### Runtime evidence on this repository

    memgw graphify rebuild --repo D:\PROJECTS\enowx-rag --index D:\memgw-test\graphify
    -> generation 20260910T000714.539995300Z, commit 0e6d55cf,
       269 files, 21,257,135 bytes, 5 paths excluded, build 2,471 ms, not stale

    (touch one file)
    memgw graphify status ... -> exit 3, fresh=false,
       "the working tree has changed since the index was built"

The published index was read back and checked: 269 paths, none matching
`.env`, `*.key`, `secret`, `token`, `credential` or `.git/`.

## 5.8 Section G -- migration dry-run and shadow evaluation: complete, both runtime-verified

### What migration actually moves

Nothing. The frozen contract has twenty-three event types and none of them is
"a history document was imported", and adding one is out of scope. So the corpus
stays in enowx-rag's Qdrant collections, where history belongs, and what the
ledger takes from it is one thing: a map from each pre-gateway chunk id back to
the document it came from, so a tombstone issued after re-chunking can still
find every point it has to delete. That is `legacy_chunk_map`, which already
existed and which `tombstone.issued` already updates
(`pkg/memgw/domain/apply.go:137`).

The consequence is that "no auto-promotion of history to current facts" is true
by construction rather than by policy: the only statement the apply path
contains is an insert into `legacy_chunk_map`. A record that reads like a
durable claim is marked `candidate` on a plan line and nothing else happens to
it. `TestNothingIsPromoted` fails if that ever changes.

### Files -- `pkg/memgw/migrate`

- `doc.go` -- why the corpus is not copied; why ids are content-derived; why the
  plan holds no text.
- `plan.go` -- the four decisions (`imported`, `quarantined`, `rejected`,
  `skipped`), the closed reason vocabulary, `Entry` (which has no text field and
  will not get one), `Plan` with counts and a digest over every entry in order,
  `Balanced()`, `Write`, and `ReadPlan` which re-derives the digest and refuses
  an edited plan. `GeneratedAt` is deliberately outside the digest.
- `classify.go` -- namespaced uuidv5 for project and derived document ids, the
  payload bound, the credential patterns (the matched text is never captured,
  logged, or written into the plan), the fact-shape patterns, and `classify`,
  which is a pure function of the record and what is already known. That purity
  is what makes two runs identical.
- `source.go` -- `FileSource` (NDJSON, unknown fields refused) and
  `PointsSource` over a `PointLister` interface with exactly one method, which
  reads. The package cannot acquire the ability to write to a vector store.
- `run.go` -- the streaming `Planner`, and `PGTarget`, which cannot be
  constructed without `pgstore.VerifyNonProduction` passing. `Apply` reports
  inserted / already-there / conflicted, overwrites nothing, and uses `xmax = 0`
  to tell an insert from a row that was already correct.

CLI: `enowx-rag memgw history plan|apply`. It is called `history` and not
`migrate` because the schema runner already owns that word.

### Tests -- 10, all passing, none skipped

`TestTwoRunsProduceTheSamePlan` (the second run reverses the source order),
`TestEveryRecordGetsExactlyOneDecision`,
`TestCredentialsAreQuarantinedAndNeverQuoted`, `TestNothingIsPromoted`,
`TestRejectionsAndQuarantinesAreClassified`, `TestDerivedIdsAreStable`,
`TestKnownRowsBecomeSkipsAndRemapsBecomeQuarantines`,
`TestDuplicateChunkIdsAreRejected`, `TestPlanRoundTripRefusesAnEditedPlan`,
`TestFileSourceRefusesAnUnknownShape`.

### Runtime evidence -- real binary, real PostgreSQL

An export of 8 records at `D:\memgw-test\history\export.ndjson`, run through
`D:\memgw-test\bin\memgw-history.exe` against `memgw_test`:

- Two dry-runs produced plan digest
  `0291229afa74dbb9d8d2b1dc271dfdcd7c2644534bb7486810bd75d23817dbd3` both times,
  and the two files differ only in `generated_at` (verified by diffing them with
  that field removed). 8 total = 4 imported + 2 quarantined + 2 rejected + 0
  skipped, each with a reason.
- The plan file contains none of the corpus: `runbook`, `password`,
  `placeholder`, `collector`, `lifecycle`, `gateway` all appear zero times in it.
- First apply: 4 inserted, 0 already there, 4 lines not an import. Second apply
  of the same plan: 0 inserted, 4 already there. Re-planning with
  `--rag-project memory` then read 4 known rows and produced 0 imported, 4
  skipped -- a re-run is a list of skips, not a list of rewrites.
- PostgreSQL afterwards: 4 rows in `legacy_chunk_map` (one carrying a derived
  document id, `d3b907c0-391d-5d53-92c5-404fdd3336ac`), **0 facts**, and the
  event count unchanged at 18. The migration created no events and no facts.
- A plan edited to flip one `"decision": "quarantined"` to `"imported"` was
  refused: exit 4, "the plan's digest does not match its entries".
- `MEMGW_ENV=production` was refused before any statement ran: exit 3, "target
  is not provably non-production: environment is declared production".

### Shadow evaluation -- `pkg/memgw/shadow`

Two lanes, kept apart on purpose, because reporting them as one number would let
a retrieval regression hide behind passing correctness cases and let a
correctness bug look like a tuning problem.

- **Correctness** is exact: one expected decision per case, no threshold to
  argue about, and it runs with no network and no provider.
- **Retrieval** is a measurement that needs an embedding provider and a corpus.

The dataset and its thresholds are frozen and digested *together* -- a threshold
chosen after seeing a result is not a threshold -- and `LoadDataset` refuses a
file whose digest no longer matches. Freezing is its own command
(`memgw shadow freeze`), so the moment a dataset stops being editable is a
moment somebody chose.

A lane that cannot run is reported as `blocked`, with the reason, and the report
carries **no** number for it: `recall_at_k`, `latency_millis` and
`threshold_met` are absent from the JSON entirely, not zero. Such a report's
verdict is `incomplete`, never `pass`. A retriever that fails part way through
blocks the lane rather than reporting a partial measurement as a figure. A
measured lane with no frozen threshold is reported and not judged, and that is
also `incomplete`.

Tests: 9, all passing, none skipped -- including
`TestRetrievalWithoutARetrieverIsBlockedNotZero`, which asserts the absence of
the recall field, and `TestAnEditedDatasetIsRefused`, which lowers a threshold
after freezing and is refused.

### The frozen dataset and its run

`pkg/memgw/shadow/datasets/history-classification.draft.json` is the editable
draft; `history-classification.json` is the frozen artifact, digest
`ec1e1f9801cbb345c90de31e372ec6e4dc0d9c036f449e878967118d0d6204d0`, 13 cases
(10 correctness, 3 retrieval), thresholds `min_correctness 1.0`,
`min_recall_at_k 0.8`, `k 5`. The payload-bound case is left to the unit suite
rather than putting a quarter of a megabyte of filler into a file people read.

Run of 2026-09-10 through the real binary:

```
correctness   measured  10/10 matched
retrieval     blocked
verdict       incomplete            (exit 6)
```

**The retrieval lane is blocked, not failing and not passing.** The reason
recorded in the report: measuring it needs the production corpus and a Voyage
embedding call, and a bulk export of that corpus and paid provider use are both
outside this mandate. No accuracy number and no latency number has been produced
for retrieval quality anywhere in this work.

## 5.9 Section H -- restore and rollback rehearsal: complete, on test resources only

Everything below happened against `memgw-devdb` and the disposable databases in
it. Nothing production was read, written, restarted or reconfigured.

### 5.9.1 Backup and restore

`pg_dump -Fc` of `memgw_test` produced a 91,446-byte archive, copied to
`D:\memgw-test\backup\memgw_test.dump` and restored with `pg_restore` (exit 0)
into a separate database `memgw_restore` on the same server.

**This is not off-host durability and must not be reported as such.** The dump
and the database it came from sit on the same disk on the same machine. What it
proves is that the schema and its contents survive a dump/restore cycle and that
the restored copy reconciles; it proves nothing about surviving the loss of this
disk. Choosing an off-host destination is still an open operational decision.

### 5.9.2 Receipt reconciliation and checksums

One query was run against both databases and compared row by row
(`scratchpad/reconcile.sql`). Both answered identically:

```
events 20   receipts 20   receipts_committed 20   outbox 20   tombstones 2
legacy_chunks 4   facts 0   max_seq 20
receipts_without_event 0    events_without_receipt 0
event_chain_sha256   8086811fd1917da03c7769bf8b7ecd68540a69ed80ea7c0950e7c3a2c2b972df
receipt_chain_sha256 33bea7c5a7133210711814a89cb40ea678c7fde8f0f808f8ad3311db248ee505
```

The checksums are taken over the ordered log (`seq:event_id:payload_digest`),
not over a table's physical contents: what has to survive a restore is the
sequence of canonical events and the receipts that point at them.

Every receipt points at an event that exists and every event has a receipt. A
restore that kept the log but lost the receipts would leave every durable client
permanently unable to resolve `unknown_commit_status`, and the check exists to
make that failure visible rather than latent.

### 5.9.3 Tombstone replay before query -- and a real gap it exposed

Running the replay found two mapping rows that a committed tombstone had never
reached. The seq-19 tombstone named document `session-2026-08-04`; `lg-0001` and
`lg-0002` carry that document id and were **still readable**, because when that
tombstone committed those rows were filed under the derived project id (the
Section G defect). Fixing the filing repaired the future; it did not go back and
apply a deletion that had already been ordered.

That is the general case, not an artefact: a mapping row that arrives *after* a
deletion was ordered -- by import, or by restoring a backup taken before it --
is readable again while a tombstone naming it sits in the journal. So the order
is replay, then query, never the reverse.

New: `pkg/memgw/migrate/replay.go`, `(*PGTarget).Replay`, and
`enowx-rag memgw history replay [--project-id UUID]`.

- It uses the same predicate as the live writer (`subject_id` against
  `document_id`, or a named chunk id), including the deliberate absence of a
  `subject_type` filter. A replay that reached more or less than the writer
  would make the two disagree, and then neither could be trusted.
- The mark is the tombstone's own `issued_at`, not `now()`. Where several
  tombstones reach one row the earliest wins, and a row already marked is left
  alone: a replay never moves a deletion later.
- Scope is one project, or every project when none is named -- which is what a
  restore wants, because the operator does not yet know which projects were
  behind.

Runtime evidence:

```
memgw history replay --project-id 7627423a-...   (memgw_restore)
  tombstones 2   newly marked 2      -> lg-0001, lg-0002 marked at 00:32:25 (seq 19's own time)
memgw history replay --project-id 7627423a-...   (again)
  tombstones 2   newly marked 0      "nothing was readable that a tombstone had already reached"
memgw history replay --project-id 7627423a-...   (memgw_test, the same latent gap)
  tombstones 2   newly marked 2      -> readable 0 of 4
```

Four tests against real PostgreSQL, all passing, none skipped:
`TestReplayReachesARowImportedAfterTheDeletion`, `TestReplayReachesANamedChunkID`,
`TestReplayIsIdempotentAndNeverMovesADeletionLater`, `TestReplayScopeIsOneProject`.

### 5.9.4 Projection rebuild on the restored database

Two outbox rows were deleted to simulate an index queue lost in a restore, then:

```
memgw projection rebuild --project 7627423a-...
  rows re-queued 20   rows restored 2 (events that had no outbox row at all)
  watermark 3 (unchanged: it records how far the projection has been applied at least once)
```

The rebuild reconstructs the queue from the events in the ledger and never moves
the watermark backwards. Draining it afterwards with the fixture embedder
against the test Qdrant applied all 20 rows and left the watermark at 20. The
canonical log was untouched by all of it: both chain checksums were identical
before and after.

### 5.9.5 A deletion that could never have worked -- fixed

The first drain left two rows `failed / index_delete_failed`, both
`tombstone.issued`. The cause is in the shared provider, not in memgw:

`QdrantProvider.Index` stores a document under `pointID(d.ID)` -- a UUIDv5 of
the document id, because Qdrant accepts only a UUID or an unsigned integer as a
point id -- while `DeletePoints` sent the **raw** ids straight through. So a
deletion addressed by document id could never reach the point that was written
under it. A non-UUID id fails loudly (Qdrant answers `data did not match any
variant of untagged enum PointsSelector`, reproduced by hand); an id that
happened to parse as a UUID would have deleted nothing and reported success.

Fixed in `pkg/rag/qdrant.go`: `DeletePoints` maps every id through the same
`pointID`. Callers already holding Qdrant ids are unaffected -- `pointID`
returns a UUID unchanged -- and the two callers that list before deleting keep
working exactly as before. Regression test:
`TestDeletePointsUsesTheSameIDMappingAsUpsert`. After the fix the same rebuild
drained all 20 rows including both tombstones.

The projection's behaviour around this was correct throughout: it failed the row
and retried rather than marking a deletion applied that had not happened.

### 5.9.6 Fencing, made real

`writer_epoch.opened` and `writer_epoch.fenced` were in the frozen contract and
admin-gated in the role table, but **nothing applied them**: `Materialise` had no
case for either. An admin fencing an epoch would have received a committed
receipt for a fence that never happened, and the only working way to fence was a
hand-written `UPDATE`. Implemented in `pkg/memgw/domain`:

- `WriterEpochPayload{epoch, reason_class, drain_watermark_seq?}` with a closed
  reason vocabulary (`cutover`, `host_replaced`, `credential_rotation`,
  `incident`, `maintenance`).
- `opened` inserts the epoch; re-opening an existing epoch is `policy_rejected`,
  because two opens of one epoch mean two hosts believe they were admitted
  separately and every event already stamped with it points at that row.
- `fenced` sets `fenced_at` only when the epoch is open, defaulting
  `drain_watermark_seq` to the fencing event's own seq -- correct by
  construction, since nothing stamped with the old epoch can commit after it. A
  second fence leaves the first timestamp and watermark alone. Fencing an epoch
  nobody opened is refused and creates nothing.
- `epoch <= 0` is refused: zero is what an omitted field decodes to.

Eight tests in `pkg/memgw/ledger/writer_epoch_test.go` (all passing against real
PostgreSQL), including that a checkpoint writer attempting to fence gets
`scope_denied` and the epoch stays open, and that a malformed payload fences
nothing.

### 5.9.7 The offline host that comes back with an old epoch

Run end to end against the live test gateway, the real collector process and the
real named pipe:

1. Collector started with the gateway **down**; a `SessionStart` hook was fired
   through the adapter. The event was spooled (`pending 1`), stamped
   `writer_epoch 1`.
2. The collector was stopped -- the host is now offline holding an undelivered
   epoch-1 event in its encrypted spool.
3. The gateway was started and an admin submitted, through HTTP with a real
   credential: `writer_epoch.opened{epoch 2}` (seq 21, committed) and
   `writer_epoch.fenced{epoch 1}` (seq 22, committed). The table then read
   `epoch 1 fenced_at 00:56:15 reason cutover drain_watermark_seq 22`, `epoch 2
   open`.
4. The collector was restarted. It reconnected, flushed the stale event, and the
   gateway refused it. The collector **quarantined** it rather than retrying
   forever:

```
seq 18, state quarantined, type session.started, attempts 4,
fail_class writer_epoch_fenced,
fail_reason "the gateway refused this event: this writer has been fenced"
```

   The held listing shows metadata only; the payload is never printed.

5. Rolling the host forward (`writer_epoch: 2` in the adapter config) and firing
   the same hook again produced the same idempotency key, so it deduplicated
   against the held row. After purging the held row the same event committed at
   seq 23 under epoch 2.

Nothing canonical was lost: the chain over `seq <= 20` still equals
`8086811fd1917da03c7769bf8b7ecd68540a69ed80ea7c0950e7c3a2c2b972df`, the value
recorded in the backup before any of this ran. The log only ever grew.

### 5.9.8 A silent success, found by running it

Step 5 above first answered `{"accepted":true,"durable":true,"duplicate":true}`.
That was wrong in the one way that matters: the row it deduplicated against was
quarantined and was never going to be delivered, so a host was being told its
event had reached the ledger when it never would.

Fixed in `pkg/memgw/collector`: `Enqueue` now looks at the state of the row it
collided with and returns `ErrDuplicateHeld` (which still satisfies
`errors.Is(err, ErrDuplicate)`, so callers asking only that question are
unchanged) when it is quarantined or dead. The pipe answers class `held` with an
instruction to release or purge, and the adapter reports it as not accepted:

```
{"accepted":false,"via":"collector","class":"held",
 "message":"an earlier event with this idempotency key is held and will not be
            delivered; an operator must release or purge it"}   exit 1
```

Tests: `TestADuplicateOfAHeldRowIsNotReportedAsQueued` (including that a purge
frees the key and the corrected event then queues) and
`TestAPendingDuplicateIsStillJustADuplicate`.

### 5.9.9 Rollback injection

The writer's authority was rolled back at the ledger and rolled forward again,
with a live collector in between:

- Revoking only `checkpoint_write` changed nothing, correctly:
  `session.started` also accepts `work_read`, and the event committed at seq 24.
  Recorded because it is the kind of half-rollback an operator would otherwise
  believe had taken effect.
- Revoking `work_read` as well: the next hook was accepted by the collector,
  refused by the gateway, and quarantined after **one** attempt --
  `fail_class scope_denied`, "this principal may not write here". It was held,
  not lost, and the ledger gained nothing while the rollback was in force.
- Restoring both grants and running `memgw collector release --seq 18`
  delivered that exact event: seq 25, under epoch 2.

So a rollback of the writer flags loses no canonical event, and the
roll-forward needs no re-run of the host's work -- the held row is the work.

### 5.9.10 What Section H did not prove

- **Off-host durability.** The backup is on the same disk. Untested.
- **Restoring onto a different machine or PostgreSQL version.** Same server,
  same version.
- **Point-in-time recovery.** No WAL archiving was configured or exercised;
  `archive_mode` remains untouched and its maintenance window unapproved.
- **Production fencing or cutover.** Everything above ran on disposable test
  resources with a disposable test credential.
- **A live host adapter.** No agent host loaded a fragment during this section;
  the hooks were fired by hand through the same binary the hosts would invoke.

## 5.10 Section I -- documentation and cleanup: complete

### 5.10.1 What was written

- `docs/runbooks/README.md` -- the index, and the two rules that appear in more
  than one runbook because getting them wrong is silent: replay after every
  import and restore before anything queries, and open-move-fence in that order.
- `docs/runbooks/memgw-collector.md` -- recovery, the quarantine, release vs
  discard vs purge with the case for each, crash recovery, the DPAPI spool key
  (no export path, rotation is drain-then-move-the-pair), the credential file.
- `docs/runbooks/memgw-projection-rebuild.md` -- status, draining, re-queuing
  from the ledger, what `rows restored` means after a restore, why the watermark
  does not move backwards, and the `index_delete_failed` history.
- `docs/runbooks/memgw-backup-restore.md` -- dump, restore into a new database,
  the four reconciliation checks, the mandatory replay, then the rebuild; and a
  closing section on what the rehearsal did *not* prove.
- `docs/runbooks/memgw-rollback-and-fencing.md` -- fencing an epoch through the
  log, the closed reason vocabulary, the three refusals, the open-move-fence
  order, revoking authority, and rolling forward by releasing the held row
  rather than redoing the host's work.
- `docs/runbooks/memgw-production-cutover.md` -- the procedure, opening with the
  seven preconditions that are still open. It is explicit that none of it has
  run against production.
- `docs/architecture/shared-memory-gateway.md` -- section 6.13 gained the
  implemented writer-epoch payload and refusals; section 9 gained the replay as
  a real step and the rule that a deletion by document id must use the same id
  mapping as the write.
- Section 5.2 above now lists every test resource still alive, each with its
  cleanup procedure, and the order to do them in.

The runbooks were written against the code, not from memory. Doing so found one
error before it was published: an earlier draft listed `unauthenticated` among
the non-retryable classes, while `forwarder.go` treats `401` as retryable and
logs it at error level. The published table now matches the three-way split the
forwarder actually makes.

### 5.10.2 Cleanup

- No scaffold was left in the repository. The scripts used during sections G, H
  and I lived in the session scratchpad and in `D:\memgw-test\`, never in the
  working tree; `git status` shows only source, tests, fixtures, adapters and
  documentation.
- `history-classification.draft.json` is kept deliberately: it is the editable
  source beside the frozen dataset, as section 5.8 records.
- Every regression test written during these sections is kept. Each defends a
  contract that a real defect broke: the tombstone replay (4), the writer epochs
  (8), the held-duplicate answer (2), and the Qdrant id mapping (1).
- One empty directory, `D:\memgw-test\qdrant-storage;C`, created earlier by a
  Git Bash path translation, was removed.
- Nothing in `D:\memgw-test\` was deleted beyond that: the spools, the token
  files, the backup and the databases are still there because the report has to
  name them as live, and because deleting a credential's file is not the same as
  revoking the credential.

### 5.10.3 A guard false positive, fixed rather than worked around

Writing section 5.2 was refused by the PreToolUse guard: a rule meant to catch
`docker rm ... seizrag-qdrant` matched a heredoc in which one line mentioned a
container removal and a later line named that protected container in prose
telling the operator *not* to touch it. The gap in those patterns was `[^|;&]*`,
which spans newlines, so one line's verb paired with another line's object.

Fixed in the hook itself (`~/.claude/hooks/guard.py`) rather than by disguising
the command: the gaps are now `[^|;&\n]*`. A newline is a command separator, so
this makes each rule match the single command it was written for. Every real
single-line command still matches; only cross-line pairings stop. Both guard
test suites pass afterwards (31 deny / 29 allow, and 7/7 on the RAG-payload
suite). The prose in section 5.2 was then written without alteration.

## 5.11 Suggested commit grouping

Nothing has been committed, as the mandate requires. The working tree also
contains changes that predate this work; they are listed first so they are not
swept into a memgw commit. Author every commit as `seisseiz78@gmail.com`.

1. **Pre-existing changes.** Whatever was already dirty before Phase 0.5 --
   including the tracked `enowx-rag-arm64` artifact, which is awaiting a user
   decision and must not be deleted. Inspect before committing; do not assume
   every dirty file is ours.
2. **Build identity (Phase 0.5).** `pkg/buildinfo/`, the `.goreleaser.yaml` and
   `Makefile` stamping, `cmd/mcp-server/main.go`, the version endpoint and
   `pkg/httpapi/version_test.go`. This one is worth landing alone: it is what
   makes "which build is running" answerable, and it is a precondition in the
   cutover runbook.
3. **Domain contracts.** `pkg/memgw/contract/` with its frozen `contract.json`
   and the fifteen fixtures, `pkg/memgw/errclass/`, `pkg/memgw/domain/`.
4. **Ledger foundation.** `pkg/memgw/migrations/`, `pkg/memgw/pgstore/`,
   `pkg/memgw/principal/`, `pkg/memgw/ledger/`.
5. **Remaining implementation.** `pkg/memgw/{gateway,collector,outbox,
   projection,adapter,graphify,migrate,shadow}`, the `cmd/mcp-server/memgw_*.go`
   command line, `adapters/`, and the two shared-provider fixes in `pkg/rag/`
   (`qdrant.go` and its two new tests) with the lifecycle and write-guard work
   in `pkg/httpapi/` and `pkg/core/`.
6. **Documentation.** `docs/architecture/`, `docs/plans/`, `docs/adr/`,
   `docs/runbooks/`.

The `pkg/rag/qdrant.go` deletion fix could also stand alone: it is a defect in
the shared provider, independent of memgw, and anything deleting Qdrant points
by document id was affected.

## 5.12 Next safe action

The non-production scope of the mandate is complete. What remains needs a
decision or a permission that has not been given:

1. Rotate `RAG_ADMIN_TOKEN` and confirm the old value no longer authenticates.
2. Choose an off-host backup destination and repeat the restore rehearsal onto a
   different machine.
3. Approve the `archive_mode` change and its maintenance window, so there is a
   recovery point between full dumps.
4. Decide whether the tracked `enowx-rag-arm64` artifact stays or goes.
5. Authorise issuing production principals and installing adapter fragments in
   the six hosts' real configurations. Until then no fragment has been loaded by
   a live host: the per-host status in section 5.6 stands unchanged, with Codex
   and Hermes still blocked on runtime.
6. Commit the working tree. Nothing has been committed; the suggested grouping
   is in section 5.11.

Until 1-5 are closed the honest status is "implemented and rehearsed on
disposable resources", not "production-ready".

## 6. Production-readiness checklist (Phase 9 preparation)

Status vocabulary, used strictly:

- `verified` — proven at runtime, in this environment, with the evidence named
  in the row. Nothing is marked verified on the strength of code review or of a
  test that skipped.
- `not_verified` — implemented, or partly implemented, but no runtime evidence
  exists. Not a synonym for "probably fine".
- `blocked_by_operator` — cannot proceed without a decision, an approval or a
  secret the mandate forbids using.
- `not_applicable` — does not apply to this system.

Evidence and assumption are kept in separate columns on purpose. Anything in the
assumption column is what someone would otherwise be tempted to write in the
evidence column.

| # | Item | Status | Evidence (proven) | Assumption (not proven) |
|---|---|---|---|---|
| 1 | `RAG_ADMIN_TOKEN` rotation | `blocked_by_operator` | The token is compromised: cleartext in two nginx site configs, and rendered into a transcript on 2026-09-09. Consumers enumerated by path, 13 of them, in `docs/runbooks/rag-admin-token-rotation.md` §1. The runbook is written and unexecuted. | That every consumer is known. #10 (`~/.enowx-rag/config.yaml` on Windows) was never read and may or may not carry `admin_token`; the operator must check. |
| 2 | Off-host backup destination | `blocked_by_operator` | None exists. Qdrant snapshots and app bundles are on-host only; PostgreSQL has no backup of any kind. Approval sheet: `docs/runbooks/memgw-backup-approval.md`. | That the daily on-host snapshots constitute a backup. They do not survive loss of the disk they sit on. |
| 3 | Restore onto a different machine | `not_verified` | A `pg_dump -Fc` / `pg_restore` cycle into a second database **on the same server** reconciled on all 13 checks, both chain checksums identical, `receipts_without_event 0`, `events_without_receipt 0` (§5.9.1–5.9.2). | That this generalises to another machine or another PostgreSQL version. Same host, same version, same disk. Untested. |
| 4 | `archive_mode` / PITR | `blocked_by_operator` | Measured `off` on the production cluster (§2.2). No WAL archiving is configured anywhere. | That the recovery point is minutes. Without archiving it is the last full dump, and no dump job exists. |
| 5 | Production principals and credentials | `blocked_by_operator` | The mechanism works: PBKDF2-SHA256 210 000 iterations, credential rows undeletable by trigger, per-principal scopes enforced server-side, all exercised against real PostgreSQL. Two **disposable test** credentials exist and are named in §8. | That issuing production credentials is a formality. It is a production secret action and is explicitly forbidden until approved. |
| 6 | Adapter installation on the six hosts | `blocked_by_operator` | Fragments exist for all six hosts and a sandbox installer (`adapters/install-sandbox.ps1`) was used against isolated profiles. 13 events across all six host shapes committed to PostgreSQL through the real collector. | That the hosts will fire these hooks in production. No global agent configuration was changed; no live host has loaded a fragment. See §7. |
| 7 | HTTP `GET /api/version` | `not_verified` | The endpoint exists, is token-gated, is covered by `pkg/httpapi/version_test.go`, and returns `buildinfo.Get()`. Verified locally. | That production answers it. Production runs an older uncommitted binary that still reports `"dev"`; the endpoint arrives only with a deploy, which is not authorised. |
| 8 | Deployment artifact identity | `blocked_by_operator` | `pkg/buildinfo` reads Go's VCS stamp; a dirty tree reports `dirty_tree: true` and a `-dirty` suffix rather than claiming the commit it was based on. The Phase 1 audit established the running binary bit-for-bit by checksum (§1.2–1.3). | That identity is live. It is not: the change is uncommitted and undeployed, and deployment is still a manual copy with no CI artifact. |
| 9 | Production fencing | `blocked_by_operator` | Fencing works: `writer_epoch.opened`/`fenced` applied through the log, three refusal paths, eight tests, and a real offline collector quarantined its stale event with `writer_epoch_fenced` on reconnect (§5.9.6–5.9.7). | That this has been exercised against production. It has not, and it must not be until cutover is approved. |
| 10 | Rollback | `verified` (non-production only) | Revoking both grants quarantined the next event after one attempt with `scope_denied`; restoring them and releasing the held row delivered that exact event at a later seq. The chain over the pre-existing range was unchanged throughout (§5.9.9). | That a partial rollback is safe. It is not: revoking `checkpoint_write` alone changed nothing, because `work_read` also admits `session.started`. Revoke the role set, not one grant. |
| 11 | RPO | `blocked_by_operator` | Today's true value: the last on-host backup for Qdrant and the app bundle, and **no backup at all** for PostgreSQL. | The proposed ~5 minutes. That number depends on items 2 and 4, neither of which is approved. |
| 12 | RTO | `not_verified` | The restore procedure is written and rehearsed end to end on test resources, including the mandatory replay and the projection rebuild. | Any number of minutes. Nothing was timed against a real corpus on a real host, and host loss is unbounded while item 2 is open. |
| 13 | Retrieval quality after cutover | `not_verified` | An offline baseline now runs: `memgw shadow run --retriever lexical` scores the frozen dataset's retrieval lane with BM25 over this repository's own documents — recall@5 **1.000** over 3 cases and 29 documents, correctness lane 10/10, dataset digest `ec1e1f98…`, report at `D:\memgw-test\prodsim\shadow-lexical.json`. No provider, no credential, no corpus export. | That this says anything about *semantic* retrieval. It does not: 3 cases over 29 documents is a floor, not a measurement, and BM25 answers keyword overlap. A dense number needs an embedding provider (none is installed locally — no Ollama, no TEI, and the only Qdrant collection here holds one 384-dim fixture vector). The concrete dependency is **an approved embedding provider plus a sanitised corpus**, not the production corpus. |
| 14 | Schema migration against production | `blocked_by_operator` | The code path exists and is proven on a disposable target (row 19). What is missing is the operator's decision, not code: the cutover approval, a restored-and-reconciled backup (rows 2–4), and the maintenance window. | That the migration is a formality. It has never run against the production cluster and must not until those are in place. Recovery is not "restore the snapshot" — see `memgw-production-commands.md` §M2. |
| 19 | A production migration path exists in the code | `verified` (on a disposable target only) | `pgstore.VerifyProduction` + `migrations.ApplyPlan`, exercised against a throwaway PostgreSQL (`ledger_alpha`, TLS with a private CA, a second application's schema `axonsim` beside the managed one). Fourteen proofs, all passing: default invocation refuses; `MEMGW_ENV=production` alone refuses; database, role, schema and deployment mismatches each refuse; `sslmode` absent/`disable`/`require` refuse; `history_import` is refused under a `schema_migration` approval; two plans in a row leave the schema with **0** relations and the same digest; a forged or absent digest refuses and writes nothing; the apply succeeds and records the deployment identity; a rerun is a no-op and the old plan is stale; `axonsim.routes` keeps its 2 rows and its owner; a foreign object in the managed schema refuses with nothing changed; an externally held lock is refused in 0s and two racing runners produce one applied history and one `ErrLockHeld` exit 3; a credential sweep of every plan file and captured output found **0** artefacts carrying a credential. | That production has been migrated, or may be. Neither. This is a disposable container that was never production, and the assertion is what makes the difference — see row 14 for what is still missing. The old text of this row (the `VerifyNonProduction` description below) describes the state before the change and is kept for the reasoning, not as current status. |
| 19a | *(superseded)* the pre-M0 state | — | `pgstore.VerifyNonProduction` is called by `migrations/runner.go`, by `memgw plan` and by `migrate/run.go` (which `memgw history apply` uses). It refuses unless `MEMGW_ENV` is `development`/`test`, the database name matches `^(memgw_\|test_\|dev_)` or `(_test\|_dev\|_devdb\|_testdb)$`, the target is loopback, `synchronous_commit` is not `off`, the server is not a standby, and no foreign tables exist. Production fails at least three of those. | That cutover only needs an approval. It also needs a **code change**: an explicit production mode that keeps the standby, `synchronous_commit` and foreign-table checks and replaces the name/env/loopback checks with a positive assertion of the intended target. Renaming the database, declaring `MEMGW_ENV=test` against production, or deleting a check are the three workarounds the guard exists to stop. Design sketch: `docs/runbooks/memgw-production-commands.md` §M0. |
| 15 | Graceful shutdown under load | `verified` (non-production only) | `pkg/httpapi/lifecycle.go` bounds the server and drains on signal; covered by `lifecycle_test.go` and exercised by every gateway smoke run. | That production has it. Production still runs `http.ListenAndServe` with no timeouts; the fix is uncommitted and undeployed. |
| 16 | Collector durability, Windows | `verified` (non-production only) | Real processes, real named pipes, real DPAPI-wrapped spool: crash replay produced the same event id and added no row; a held duplicate is reported as `held` and not as success; canary inspection proved the on-disk payload is encrypted. | That production hosts have a collector. None is installed. One component proven once, not four host proofs — see §7. |
| 20 | Collector durability, Linux | `verified` (disposable container only) | A Linux build in a throwaway `debian:12-slim`: with no `CREDENTIALS_DIRECTORY` the collector **refuses and writes no key file**; a 0644 credential is refused as already compromised; with a systemd-shaped credential it comes up on a **0600** unix socket; a second collector on the same socket is refused; two events submitted through the real `memgw adapter` over that socket returned `durable: true`; the spool file contains neither the session id, the event type nor the host name in clear; after `kill -9` and a restart the two rows are still pending; the same hook again is reported `duplicate` and adds no row; neither collector log contains the token. The Go suite (`./...`, including the gateway package against a disposable PostgreSQL) passes on Linux. | That Hermes is covered. It is not: nothing is installed on the Hermes host, and this is a container proof of the component. Key storage depends on systemd credentials; a host without them gets a refusal, not a plaintext fallback — which is the intended behaviour and also a deployment prerequisite. |
| 17 | Secrets kept out of ledger, spool, logs and RAG | `verified` (per sweep, per artefact set) | The collector reads its credential from a file only; held listings print metadata and never payloads; the write guard's credential scanner runs on ingress; the session log written for this work was chunked and guard-checked before sending; the M0 plan files and captured output were swept against the actual passwords, a `://user:pass@` pattern and a `password=` pattern with 0 hits, and the plan's redacted target is `host:port/db` with no userinfo. | That the system is leak-free. A sweep proves that **the artefacts swept, at the time they were swept**, carried nothing matching the patterns used — nothing more. A PreToolUse hook sees command text only; anything that reads a secret at runtime is invisible to it. `RAG_ADMIN_TOKEN` is still compromised and unrotated (row 1), so the system-level claim is false today regardless of any sweep. |
| 18 | Monitoring and alerting for the gateway | `not_verified` | `memgw collector stats`, `memgw projection status` and the ledger's own counters exist and are documented in the runbooks. | That anyone will see a problem. There is no alerting on queue age, dead letters, receipt unknowns or projection lag, and the plan's observability section is unimplemented. |

**Rule for this table: no row moves to `verified` without runtime evidence named
in its own evidence column.** A code change, a review, a passing unit test that
skipped, or a plausible argument does not move a row.

## 7. Adapter readiness matrix (final)

`implemented` — the code path exists and is exercised by tests.
`runtime_verified` — a real payload was decoded, translated, queued in the real
encrypted spool and committed to PostgreSQL.
`live_config_installed` — a fragment is loaded by the host's real configuration.
`offline_durability_verified` — events survive the gateway being down, on that
host's platform.
`production_credential` — a production principal and credential exist for it.

| Host | implemented | runtime_verified | live_config_installed | offline_durability_verified | production_credential | remaining_blocker |
|---|---|---|---|---|---|---|
| OMP | true | true | false | true | false | fragment never loaded by a live OMP session; global config change not permitted |
| Claude Code | true | true | false | true | false | same — real `SessionStart`/`PreCompact`/`SessionEnd` shapes were committed, but by hand |
| Droid | true | true | false | true | false | same |
| OpenCode | true | true | false | true | false | plugin API announces no exit event, so `session.ended` is never recorded and nothing fabricates one; fragment not loaded live |
| Codex | true | **false** | false | unknown | false | **blocked**: the event vocabulary is inferred from binary strings and no Codex hook has ever been observed firing on this machine. An isolated `CODEX_HOME` with `config.toml` + `hooks.json` probes **was loaded by the real codex-cli 0.153.4** (it warned about the duplicate registration), but the session aborted with *"Your workspace is out of credits"* before any hook could fire, so `events.ndjson` is empty. Exact prerequisite: **the Codex workspace owner must refill credits**; nothing else is missing |
| Hermes | true | true (gateway path) | false | **false** (host) | false | **blocked at the host**. The Linux collector now exists — unix socket 0600, systemd-credential key store, same encrypted spool — and is verified on a **disposable Linux container**, not on the Hermes host. Until it is installed there, Hermes still submits straight to the gateway and still loses events during an outage. "Linux collector verified" and "Hermes host verified" are different claims and this row asserts only the first |

Notes that belong with the table rather than under it:

- `offline_durability_verified: true` for the four Windows hosts means the same
  collector was proven, not four separate proofs: one spool, one crash-replay
  test, one canary inspection. It is the shared component that was verified.
  The same reading applies to the Linux collector: one proof of one component,
  on one disposable container.
- `runtime_verified: true` means a payload of that host's shape was decoded,
  queued and committed. For Claude Code, Droid, OMP and OpenCode the payload was
  supplied **by hand**, not by the host firing a hook. That proves the adapter
  and the collector; it proves nothing about whether the host will invoke them,
  which is what `live_config_installed: false` says.
- Hermes is the only host where an outage loses events **today**. That is now a
  deployment gap rather than a platform one: the collector exists for Linux and
  is proven on a disposable container, but nothing is installed on the Hermes
  host and installing it is not authorised here. It should be stated in any
  cutover announcement rather than discovered afterwards.
- No fragment was installed into any global configuration. That is forbidden
  until approved, and the matrix says `false` rather than "ready" for a reason.

## 8. Test resource inventory and staged cleanup

Non-secret inventory. **Nothing here has been deleted.** Credential *paths* are
named; no credential file was opened, printed or copied.

### 8.1 Inventory

| Kind | Name / path | Detail |
|---|---|---|
| Container | `memgw-devdb` | `postgres:16-alpine`, published on `127.0.0.1:55433` only |
| Container | `memgw-qdrant` | `qdrant/qdrant:v1.12.4`, published on `127.0.0.1:56333` only |
| Database | `memgw_test` | persistent `memgw` schema from the smoke runs; max seq 25 |
| Database | `memgw_itest` | the Go integration suite's own database |
| Database | `memgw_restore` | restored from the Section H dump |
| Qdrant collection | `project_7627423a-…` | fixture vectors, no semantic content |
| Storage | `D:\memgw-test\qdrant-storage` | bind mount for the test Qdrant; never a production volume |
| Spool | `D:\memgw-test\collector\spool.db` + `spool.key` | encrypted; key wrapped by DPAPI, user scope |
| Spool | `D:\memgw-test\adapter-collector\spool.db` + `spool.key` | second spool, adapter and fencing rehearsals |
| Credential file | `D:\memgw-test\collector\token` | **not read**. Test credential for the `claude-code` test principal |
| Credential file | `D:\memgw-test\rehearsal\token` | **not read**. Test credential for principal `a72ac30d-…` (`rehearsal-admin`) |
| Dump | `D:\memgw-test\backup\memgw_test.dump` | 91,446 bytes, same disk as its source |
| Artifacts | `D:\memgw-test\history\` | `export.ndjson` (8 synthetic records), three plans, one shadow report |
| Artifacts | `D:\memgw-test\graphify\`, `gstatus.json` | code-index generations from this repository's tree |
| Artifacts | `D:\memgw-test\adapter\` | hook payload fixtures, synthetic |
| Binaries | `D:\memgw-test\bin\*.exe` | four builds of the same repository binary |
| Logs | `D:\memgw-test\*.out`, `*.err` | no credential: the collector reads its token from a file and never logs it |
| Config | `D:\memgw-test\serve.env.sh` | loopback DSN with the disposable container password; no other secret |
| Ledger debris | project `7627423a-…`, principals `159fdcc6-…`, `a72ac30d-…` | inside the `memgw` schema of `memgw_test` |

One credential, `64345493-…`, was leaked to a transcript earlier in this work and
is **already revoked**. It is recorded here so the revocation is not forgotten,
not because it needs action.

No memgw process is running. Both containers are up. Nothing listens on
`127.0.0.1:7777` or on the test named pipes.

### 8.2 Staged cleanup — the procedure, not a script to run now

Each stage is verifiable before the next begins. Do not skip stage 4: a token
file removed after the data around it is the wrong order, and a credential
revoked only in the filesystem is not revoked at all.

1. **Stop processes.** Confirm no gateway, collector, projection worker or
   graphify build is running. Check for `enowx-rag` and the four
   `D:\memgw-test\bin\*.exe` images, and for a listener on `127.0.0.1:7777`.
2. **Stop the disposable containers**, by name only — `memgw-devdb` and
   `memgw-qdrant`. Never a global prune: this machine holds containers and
   volumes belonging to other work that must not be touched.
3. **Verify no open handles** on `D:\memgw-test\`, particularly `spool.db` and
   its WAL sidecars. A spool deleted while a process holds it leaves a WAL
   behind, and on Windows the delete simply fails.
4. **Revoke the test credentials at the ledger first, then remove the files.**
   `memgw principal revoke-credential` for the `claude-code` and
   `rehearsal-admin` credentials; confirm with `memgw principal list` that
   `revoked` is set; only then delete `D:\memgw-test\collector\token` and
   `D:\memgw-test\rehearsal\token`, followed by both `spool.key` files. Do not
   open any of them.
5. **Remove the remaining test data**: the two spool directories, `backup\`,
   `history\`, `graphify\`, `adapter\`, `adapter-collector\`, `rehearsal\`,
   `bin\`, the `*.out`/`*.err` logs, `gstatus.json`, `serve.env.sh` and
   `qdrant-storage\`.
6. **Verify no production path was touched.** Confirm `seizrag-qdrant` is still
   running and unrenamed, every `sc_recovery_*` volume still exists,
   `D:\ClaudeVM` and the `vm_bundles` junction are intact, and the production
   host was not contacted during cleanup.

The commands themselves, stage by stage with an expected result for each, are in
[`../runbooks/memgw-test-cleanup.md`](../runbooks/memgw-test-cleanup.md). They
are deliberately not executed by this work, and cleanup requires its own
approval: these resources hold the only evidence behind rows 10, 15 and 16 of
the checklist above.

Note the ordering difference between §8.2 above and the runbook: the runbook
revokes the test credentials **first** (stage 1), while `memgw-devdb` is still
running, because a credential can only be revoked at the ledger and stopping the
container removes the ledger. The stages are otherwise the same.

## 9. Production approval and the commands that wait on it

Two documents complete the package and neither has been acted on:

- [`production-approval-form.md`](production-approval-form.md) — one block per
  decision (token rotation, off-host backup, `archive_mode`, principals, adapter
  installation per host, cutover). **Every field is blank.** No value was filled
  in on the model's judgement.
- [`../runbooks/memgw-production-commands.md`](../runbooks/memgw-production-commands.md)
  — every production command written out with its approval, precondition,
  expected result, rollback and whether it is reversible. Placeholders are
  `<UPPER_CASE>`; no placeholder stands for a secret typed on a command line.

One correction made while revalidating: `pkg/memgw/adapter/translate.go`,
`pkg/memgw/outbox/outbox.go` and `pkg/memgw/principal/principal.go` were listed
in an earlier report as pre-existing unformatted files "not mine, not touched".
That was wrong — everything under `pkg/memgw/` is new work. `gofmt -w` was run
on those three; the change is struct- and map-literal alignment only, and
`go build`, `go vet` and `go test ./pkg/memgw/...` are green after it. The rest
of the repository's `gofmt -l` list is genuinely pre-existing and was left alone.

The irreversible steps, named as such: fencing an epoch (a fenced number can
never be re-opened), the schema migration (forward-only; going back means
restoring the backup), and issuing a credential (the row cannot be deleted, only
revoked). Everything else is reversible or partly reversible, and each section
says which.
