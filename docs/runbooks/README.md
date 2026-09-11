# Runbooks — shared memory gateway

Operational procedures for the memgw components. Each one records what was
actually exercised and, separately, what was not — a runbook that reads as if
everything were proven would be worse than none.

| Runbook | Covers |
|---|---|
| [memgw-collector.md](memgw-collector.md) | Collector recovery, the quarantine, release/discard/purge, the DPAPI spool key, the credential file |
| [memgw-projection-rebuild.md](memgw-projection-rebuild.md) | Projection status, draining, re-queuing from the ledger, dead letters |
| [memgw-backup-restore.md](memgw-backup-restore.md) | Dump, restore, the four reconciliation checks, the mandatory tombstone replay |
| [memgw-rollback-and-fencing.md](memgw-rollback-and-fencing.md) | Fencing a writer epoch, revoking authority, rolling forward without redoing work |
| [memgw-production-cutover.md](memgw-production-cutover.md) | The cutover procedure and the preconditions that are still open |
| [rag-admin-token-rotation.md](rag-admin-token-rotation.md) | Rotating the compromised shared bearer: consumers, order, tests that never print a secret |
| [memgw-backup-approval.md](memgw-backup-approval.md) | The approval sheet for the off-host destination, encryption owner, RPO/RTO, retention and the cross-machine restore test |
| [memgw-production-commands.md](memgw-production-commands.md) | Every production command, written out with precondition, expected result, rollback and whether it is reversible. **None has been run.** |
| [memgw-test-cleanup.md](memgw-test-cleanup.md) | Staged removal of the disposable test resources — revoke first, prune never, verify the protected resources last |
| [memgw-pilot-persistence.md](memgw-pilot-persistence.md) | Keeping the local gateway and collectors alive across logoff/reboot: install, status, uninstall, and how to verify without rebooting |

The decisions those procedures wait on are collected in one place:
[`../plans/production-approval-form.md`](../plans/production-approval-form.md).
Every field in it is blank.

Design and rationale live in
[`../architecture/shared-memory-gateway.md`](../architecture/shared-memory-gateway.md);
implementation state and evidence in
[`../plans/shared-memory-gateway-implementation.md`](../plans/shared-memory-gateway-implementation.md).

**None of this has run against production.** Production has no collector, no
projection worker and no issued credentials; the open preconditions are listed in
`memgw-production-cutover.md` §1.

Two rules that appear in more than one runbook because getting them wrong is
silent:

- **Replay after every import and every restore, before anything queries.** A
  mapping row that arrives after a deletion was ordered is readable again until
  the journal is re-applied.
- **Open the new epoch, move the hosts, fence the old one — in that order.**
