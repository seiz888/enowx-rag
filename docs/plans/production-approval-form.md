# Production approval form — shared memory gateway

Nothing in this file has been decided. Every blank is a decision the owner has
to make and record here before the corresponding action may be taken. No value
below was filled in on the model's judgement, and no blank is to be guessed at
later: an empty field means the approval does not exist.

**No secret belongs in this file.** Name people, destinations, windows and
policies. Do not write a token, a password, a key, a passphrase or a connection
string with credentials in it. This file is tracked in git and will be readable
by anyone with the repository.

## How to use it

1. Fill in one block. Leave the others alone.
2. Record the approver by name and the date.
3. Only then run the corresponding section of
   [`../runbooks/memgw-production-commands.md`](../runbooks/memgw-production-commands.md),
   which has the exact commands, their preconditions, their expected output and
   their rollback.
4. Mark the matching row in the readiness checklist
   (`shared-memory-gateway-implementation.md` §6) — and only on runtime
   evidence, never on the approval alone. An approval permits an action; it is
   not proof the action succeeded.

The blocks are ordered by dependency. Token rotation is first because the
current token is compromised and every later step would otherwise be carried out
under a credential that must be assumed known to a third party.

---

## The form

```text
RAG_ADMIN_TOKEN rotation:
  operator:
  approved:
  maintenance window:
  rollback owner:

Off-host backup:
  provider/destination:
  encryption owner:
  RPO:
  RTO:
  retention:
  restore operator:
  cross-machine restore result:

PostgreSQL archive_mode:
  approved:
  maintenance window:
  axonhub impact owner:
  rollback procedure:

Production principals:
  issuer:
  scopes:
  expiry/rotation policy:
  approved:

Adapter installation:
  OMP:
  Claude Code:
  Droid:
  OpenCode:
  Codex:
  Hermes:

Production cutover:
  approved:
  fence epoch:
  drain watermark:
  rollback owner:
```

---

## What each block commits you to

These notes exist so a blank is filled in knowingly. They add nothing to the
form above and change none of its fields.

### `RAG_ADMIN_TOKEN` rotation

The token is compromised: it sits in cleartext in two nginx site configurations
and was rendered into a transcript on 2026-09-09. Thirteen consumers are
enumerated by path in
[`../runbooks/rag-admin-token-rotation.md`](../runbooks/rag-admin-token-rotation.md)
§1. One of them, `~/.enowx-rag/config.yaml` on this Windows host, has never been
read and may or may not carry the value; the operator has to look.

- *Maintenance window* matters because the server is updated before its
  consumers, so every agent host is unauthenticated for the length of the
  rotation.
- *Rollback owner* matters because rolling back puts the compromised token back
  in service. It buys hours, not days.
- The daily app bundles under `/opt/rag-backup/app/` keep the **old** token for
  as long as retention holds them. That is a retention decision, in the backup
  block, not something rotation fixes.

### Off-host backup

The detailed sheet is
[`../runbooks/memgw-backup-approval.md`](../runbooks/memgw-backup-approval.md);
fill that in and copy the summary here. What is true today, and what the blank
replaces:

> **RPO = the last on-host backup. RTO = unbounded on host loss.** PostgreSQL
> has no backup of any kind. Qdrant snapshots and app bundles are daily,
> retained 7, and live on the same host they protect.

*Cross-machine restore result* stays blank until the test in that sheet's §7 has
actually been run. It is the single test that would retire the largest untested
claim in this package.

### PostgreSQL `archive_mode`

Measured `off` on the production cluster. Turning it on requires a **restart**,
not a reload, so this block needs a real window and it touches every other
database on that cluster.

*axonhub impact owner* is a separate line because axonhub shares the cluster:
the restart is not confined to the gateway's data, and whoever owns that service
has to agree to the outage rather than discover it.

### Production principals

One principal per host, granted the least role that admits the event types that
host actually emits. Roles available:
`history_read work_read checkpoint_write candidate_write fact_promote
projection_worker admin`.

Two facts from the rehearsal belong in the *scopes* decision:

- Roles overlap. Revoking `checkpoint_write` alone did **not** stop
  `session.started`, because `work_read` also admits it. Grant and revoke role
  sets, not single roles.
- `memgw principal issue` prints the secret **once, on stdout**, and it is not
  recoverable. It must be redirected straight into a `0600` file; if it is ever
  echoed, treat it as compromised and revoke that credential id.

*expiry/rotation policy* is a real field: `grant` and `issue` both take
`--expires`, and leaving it empty means the credential never expires on its own.

### Adapter installation

Per host, because the hosts are not equivalent and one blanket approval would
hide that:

- **Codex** has never been observed firing a hook on this machine; its event
  vocabulary is inferred from binary strings. Approving it approves an untested
  integration.
- **Hermes** runs on a Linux VPS and has **no offline durability today**: it
  submits straight to the gateway, so events during an outage are lost, not
  queued. This is no longer a platform property -- a Linux collector exists and
  was proven on a disposable container -- but nothing is installed on the Hermes
  host, and installing it needs a systemd unit with an encrypted credential.
  Until that is done the statement above holds and belongs in any cutover
  announcement.
- **OpenCode**'s plugin API announces no exit event, so `session.ended` is never
  recorded for it and nothing fabricates one from idleness.
- OMP, Claude Code and Droid have had real payload shapes committed through the
  real collector, but by hand. No fragment has been loaded by a live host.

Installing a fragment changes a **global** agent configuration on this machine.
That is why it is a per-host approval and not part of cutover.

### Production cutover

*fence epoch* is the number of the epoch being closed, not the one being opened.
The order is fixed and is not negotiable: **open the new epoch, move the hosts,
then fence the old one.** Fencing first strands every host that has not moved.

*drain watermark* may be left to default. `writer_epoch.fenced` defaults it to
the fencing event's own seq, which is correct by construction — nothing stamped
with the old epoch can commit after the fence. Write a number here only if you
intend something other than that.

*rollback owner* is the person who decides to open a new epoch and move the
hosts back. Note that a fenced epoch number can never be re-opened; rolling back
means opening the next number, not the previous one.

### One thing this form cannot approve

The binary as built **refuses to migrate a production database**, and no field
here changes that. `pgstore.VerifyNonProduction` requires `MEMGW_ENV` to be
`development` or `test`, requires the database name to match a disposable
pattern, and requires a loopback target; the migration runner and
`memgw history apply` both call it. Making production a legitimate target is a
**code change**, reviewed and committed on its own, not a flag to be set during
a maintenance window. See `memgw-production-commands.md` §M0 and readiness row
19.
