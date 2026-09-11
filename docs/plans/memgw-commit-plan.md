# Commit plan — memgw working tree

**Nothing here has been committed, staged, reverted or cleaned.** This document
is an inventory and a proposed order. It exists because the working tree holds
two unrelated bodies of work — the user's own pre-Phase-0.5 changes and the
memory-gateway phases — and five files contain hunks from both.

Branch `local/write-guard`, HEAD `0e6d55cf`. Author every commit as
`seisseiz78@gmail.com`.

---

## 0. How the split was established

Not by guessing. Three independent sources agree:

1. **The Phase 1 audit**, recorded in the main plan §1.1, counted the tree
   *before* any gateway work: 21 modified tracked files (14 Go, `CHANGELOG.md`,
   `SECURITY.md`, 5 web) and, untracked, 4 Go test files, 1 web asset and
   `docs/plans/`.
2. **Modification times.** Everything the user changed is stamped
   `2026-09-09 20:33–20:49`; the gateway work begins at `22:19` with
   `pkg/buildinfo/buildinfo.go` and runs to `2026-09-10 07:44`. The boundary is
   clean — no file falls between 20:49 and 22:19.
3. **Diff content.** Each ambiguous file was read: `buildinfo`/`memgw`/
   `lifecycle` markers identify gateway hunks, `offset`/`ExportProject`/paging
   markers identify the user's.

Where the three disagreed, the file is listed as **mixed** and needs hunk-level
staging (`git add -p`), never a whole-file `git add`.

## 1. Group A — pre-existing changes (INVENTORY ONLY, do not commit automatically)

The user's own work, present before Phase 0.5 and untouched by every phase
since. **Do not fold these into a gateway commit.** Whether they are finished,
and whether `local/write-guard` is their home, is the user's call.

| File | Change |
|---|---|
| `SECURITY.md` | 8 added lines |
| `mcp-server/pkg/config/config.go` | 7+/4- |
| `mcp-server/pkg/core/service.go` | 39+/4- — point paging (`ListPointsPage`), search bounds |
| `mcp-server/pkg/core/sqlite_metrics.go` | 34+/7- — metrics schema versioning |
| `mcp-server/pkg/core/writeguard_test.go` | 65+/7- |
| `mcp-server/pkg/httpapi/auth.go` | 10+/2- — fail-closed auth |
| `mcp-server/pkg/httpapi/setup_config.go` | 37+/27- |
| `mcp-server/pkg/migrate/migrate.go` | 4+/0- |
| `mcp-server/pkg/migrate/migrate_test.go` | 24+/4- |
| `mcp-server/pkg/core/metrics_e2e_test.go` | untracked, new |
| `mcp-server/pkg/httpapi/auth_failclosed_test.go` | untracked, new |
| `mcp-server/pkg/httpapi/export_test.go` | untracked, new |
| `mcp-server/pkg/rag/qdrant_paging_test.go` | untracked, new |
| `mcp-server/web/src/pages/Overview.tsx` | 15+/4- |
| `mcp-server/web/src/pages/Playground.tsx` | 1+/1- |
| `mcp-server/web/dist/index.html` | 19+/19- — generated |
| `mcp-server/web/dist/assets/index-CtrG2dQ9.js` | untracked, generated |
| `mcp-server/web/dist/assets/index-CSmqwHNY.js` | **deleted**, generated |
| `mcp-server/web/dist/assets/index-DGQfgDOb.css` | line-ending change only (empty diff) |

Plus the **pre-existing halves** of the five mixed files in §7.

- **Dependency**: none on the gateway work. It builds and tests on its own.
- **Risk**: the `/export` route in this group is *already live in production*
  (main plan §2, §2.2): production runs this uncommitted code. Committing it
  changes nothing operationally; **not** committing it leaves production running
  code absent from any commit.
- **Verification**: `go build ./... && go test ./... && go vet ./...`
- **Safe to commit separately**: yes.
- **Needs user review**: **yes** — this is the one group that must not be
  committed on the model's judgement.

## 2. Group B — build identity (Phase 0.5)

| File | Status |
|---|---|
| `mcp-server/pkg/buildinfo/buildinfo.go` | new |
| `mcp-server/pkg/httpapi/version_test.go` | new |
| `.goreleaser.yaml` | 4+/1- |
| `Makefile` | 9+/1- — `VERSION` ldflags, `-trimpath` note |
| `mcp-server/cmd/mcp-server/main.go` | **mixed** — take only the `buildinfo`/`resolvedVersion` hunks |
| `mcp-server/pkg/httpapi/handlers.go` | **mixed** — take only the `Version` handler and its import |
| `mcp-server/pkg/httpapi/server.go` | **mixed** — take only `r.Get("/version", h.Version)` |
| `mcp-server/pkg/httpapi/docs.go` | **mixed** — the version-related doc text |
| `CHANGELOG.md` | **mixed** — the "Build identity" block only |

- **Dependency**: none. It compiles against the tree as it stands.
- **Risk**: low, but it is the group with the most operational value: it is what
  makes "which build is running" answerable, and it is a named precondition in
  `docs/runbooks/memgw-production-cutover.md` §1.
- **Verification**: `go test ./mcp-server/pkg/httpapi/ -run Version` and
  `go run ./cmd/mcp-server version` (expect `dirty_tree: true` from a modified
  tree — that is the feature, not a defect).
- **Safe to commit separately**: yes, but only after the mixed files are split.
- **Needs user review**: the hunk split, yes.

## 3. Group C — domain contracts (Phase 2)

| Path | Files |
|---|---|
| `mcp-server/pkg/memgw/contract/` | `contract_test.go`, `doc.go`, `testdata/contract.json`, `testdata/fixtures/*.json` (15) |
| `mcp-server/pkg/memgw/errclass/` | `errclass.go` |
| `mcp-server/pkg/memgw/domain/` | `apply.go`, `domain.go`, `facts.go`, `registry.go`, `registry_test.go`, `validate.go` |

- **Dependency**: **`domain` imports `pkg/memgw/pgstore`**, which lives in group
  D. Verified with `go list`. So this group **does not compile on its own** —
  either move `pgstore/` up into it, or commit C and D back to back and verify
  at D. Recommended: move `pkg/memgw/pgstore/` into this group; it is a target
  guard and a pool, it depends on nothing of ours, and `domain`, `migrations`,
  `principal` and `outbox` all need it.
- **Risk**: low. Additive; no existing file is edited.
- **Verification**: `go test ./pkg/memgw/contract/ ./pkg/memgw/domain/ ./pkg/memgw/pgstore/`
- **Safe to commit separately**: only with `pgstore` included.
- **Needs user review**: no.

## 4. Group D — ledger foundation (Phase 3)

| Path | Files |
|---|---|
| `mcp-server/pkg/memgw/migrations/` | `runner.go`, `runner_test.go`, `integration_test.go`, `sql/0001…`, `sql/0002…`, `sql/0003…` |
| `mcp-server/pkg/memgw/pgstore/` | if not already in group C |
| `mcp-server/pkg/memgw/principal/` | `principal.go`, `credential.go`, `principal_test.go`, `credential_test.go`, `scope_test.go` |
| `mcp-server/pkg/memgw/ledger/` | `canonical.go`, `errors.go`, `submit.go` + 6 test files |
| `mcp-server/pkg/memgw/memgwtest/` | `memgwtest.go` |
| `mcp-server/pkg/httpapi/lifecycle.go`, `lifecycle_test.go` | new — bounded server, graceful shutdown |
| `mcp-server/pkg/core/writeguard.go` | 35+/0- — one additive export, `ContainsCredential` |
| `mcp-server/cmd/mcp-server/main.go` | **mixed** — the shutdown/metrics-flush hunks |

- **Dependency**: group C. `ledger` imports `pkg/core` for `ContainsCredential`,
  so `writeguard.go` must be in this commit or earlier.
  `principal/credential.go` was actually written in Phase 4 Section B; it is
  placed here because it compiles standalone and splitting one package across
  two commits buys nothing.
- **Risk**: `sql/0001–0003` are the schema. Once committed they are a contract:
  changing a released migration in place is how two databases silently diverge.
- **Verification**: `MEMGW_TEST_DSN=… go test ./pkg/memgw/migrations/ ./pkg/memgw/ledger/ ./pkg/memgw/principal/ ./pkg/httpapi/`
  (DB-backed tests skip without the DSN — a skip is not a pass).
- **Safe to commit separately**: yes, after C.
- **Needs user review**: no, but the migration files deserve a read.

## 5. Group E — remaining implementation (Phase 4 sections B–H)

| Path | Files |
|---|---|
| `mcp-server/pkg/memgw/gateway/` | 7 files |
| `mcp-server/pkg/memgw/collector/` | 15 files |
| `mcp-server/pkg/memgw/outbox/` | 5 files |
| `mcp-server/pkg/memgw/projection/` | 11 files |
| `mcp-server/pkg/memgw/adapter/` | 12 files |
| `mcp-server/pkg/memgw/graphify/` | 9 files |
| `mcp-server/pkg/memgw/migrate/` | 8 files (incl. `replay.go`, `replay_test.go`) |
| `mcp-server/pkg/memgw/shadow/` | 4 files + 2 datasets |
| `mcp-server/cmd/mcp-server/` | `memgw.go`, `memgw_adapter.go`, `memgw_collector_windows.go`, `memgw_collector_other.go`, `memgw_graphify.go`, `memgw_history.go`, `memgw_principal.go`, `memgw_projection.go`, `memgw_shadow.go` |
| `adapters/` | `README.md`, `adapter.example.json`, `install-sandbox.ps1`, and one fragment per host |
| `mcp-server/pkg/httpapi/server.go` | **mixed** — the `memgwHandler` parameter and the `/memgw` mount |
| `mcp-server/pkg/httpapi/handlers_test.go`, `setup_test.go` | 2+/2- each — the `NewRouter` signature. **Must be in the same commit as `server.go`**, or the package will not build |
| `mcp-server/cmd/mcp-server/main.go` | **mixed** — the memgw wiring hunks |

- **Dependency**: groups C and D. `adapter` imports `collector`; `projection`
  imports `outbox`, `domain` and `pkg/rag`; `shadow` imports `migrate`.
- **Risk**: the largest group by far. It is also the one where a partial commit
  breaks the build, because the router signature change and its two test call
  sites must move together.
- **Verification**: `go build ./... && go vet ./... && MEMGW_TEST_DSN=… MEMGW_TEST_QDRANT_URL=… go test ./...`
- **Safe to commit separately**: yes, as one commit. Splitting it further is
  possible (gateway / collector+adapter / projection+outbox / graphify /
  migrate+shadow) and is a matter of taste; each sub-split still needs C and D.
- **Needs user review**: no.

## 6. Group F — the Qdrant deletion fix (independent)

`mcp-server/pkg/rag/qdrant.go` — the `DeletePoints` hunk only — plus
`mcp-server/pkg/rag/qdrant_delete_test.go` (new).

`Index` stores a document under `pointID(d.ID)` while `DeletePoints` sent raw
ids, so a deletion addressed by document id could never reach the point that was
written. This is a defect in the shared provider and affects any caller deleting
by document id, memgw or not.

- **Dependency**: none. It touches no memgw package.
- **Risk**: low and well covered. `pointID` returns a valid UUID unchanged, so
  callers already holding Qdrant ids are unaffected;
  `TestDeletePointsUsesTheSameIDMappingAsUpsert` pins both directions.
- **Verification**: `go test ./pkg/rag/`
- **Safe to commit separately**: **yes, and it should be** — it is the one fix
  worth landing on its own, ahead of the gateway work.
- **Needs user review**: no, but note the file is **mixed**: the rest of
  `qdrant.go`'s diff is the user's paging work (group A). Stage the
  `DeletePoints` hunk only.

## 7. Mixed files — hunk-level staging required

Five files carry hunks from more than one group. `git add -p` each; do not
`git add` the whole file.

| File | Hunks belong to |
|---|---|
| `CHANGELOG.md` | A (the "Fixed" block) + B (the "Build identity" block) |
| `mcp-server/cmd/mcp-server/main.go` | B (buildinfo) + D (shutdown, metrics flush) + E (memgw wiring) |
| `mcp-server/pkg/httpapi/handlers.go` | A (paging, `ExportProject`) + B (`Version`) |
| `mcp-server/pkg/httpapi/server.go` | A (`/projects/{id}/export` route) + B (`/version` route) + E (`memgwHandler`, `/memgw` mount) |
| `mcp-server/pkg/rag/qdrant.go` | A (`ListPointsPage`, scroll bounds) + F (`DeletePoints`) |
| `mcp-server/pkg/httpapi/docs.go` | A + B (documentation text for both) |

If splitting these is more trouble than it is worth, the honest alternative is a
single commit that says so in its message — not a commit that claims a clean
separation it does not have.

## 8. Group G — documentation

`docs/adr/` (1 file), `docs/architecture/` (1 file), `docs/plans/` (3 files —
`shared-memory-gateway-implementation.md`, this file, and
`production-approval-form.md`), and `docs/runbooks/` (10 files):

    README.md
    memgw-backup-approval.md
    memgw-backup-restore.md
    memgw-collector.md
    memgw-production-commands.md
    memgw-production-cutover.md
    memgw-projection-rebuild.md
    memgw-rollback-and-fencing.md
    memgw-test-cleanup.md
    rag-admin-token-rotation.md

`production-approval-form.md` is committed **with every field blank**. That is
its correct state: an empty field means the approval does not exist, and filling
one in is a separate, deliberate commit by the person who approved it.

- **Dependency**: none technically; it describes A–F.
- **Risk**: none to the build. Read the runbooks once more before committing —
  they are what an operator will follow at 3am.
- **Verification**: none automatable beyond `git diff --check`.
- **Safe to commit separately**: yes. Last.
- **Needs user review**: worth a read, not a blocker.

## 9. Files that must NOT be deleted or "cleaned"

- `enowx-rag-arm64` — tracked build artifact, 17,236,130 bytes, stale, matches
  nothing running. Removing it is a repository change awaiting the user's
  decision (main plan §6 item 5). Leave it exactly as it is.
- `mcp-server/web/dist/**` — generated, but tracked and currently deployed.
- `mcp-server/pkg/memgw/shadow/datasets/history-classification.draft.json` —
  the editable source beside the frozen dataset; deliberate, see main plan §5.8.
- Everything under `docs/` — the only record of what was verified and what was
  not.
- `mcp-server/cmd/mcp-server/setup.go` and
  `mcp-server/web/dist/assets/index-DGQfgDOb.css` show as modified but have an
  **empty content diff**: the change is line endings only. Do not commit them;
  do not "fix" them either, since touching them rewrites the whole file.

## 10. Suggested order

1. **F** — the Qdrant deletion fix (independent, smallest, real defect).
2. **A** — pre-existing changes, *after the user reviews them*.
3. **B** — build identity.
4. **C** — domain contracts (with `pgstore`).
5. **D** — ledger foundation.
6. **E** — remaining implementation.
7. **G** — documentation.

Run `go build ./... && go vet ./... && go test ./...` after each. The tree is
green today, so any red after a commit is that commit's fault and nothing else's.
