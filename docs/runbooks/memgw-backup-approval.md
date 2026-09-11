# Approval sheet — backup, RPO/RTO and off-host destination

Every field below is a **decision the owner has to make**. Nothing here has been
chosen, provisioned or purchased. No provider was selected, no bucket created,
no backup uploaded, no credential issued.

Fill this in, then the cutover runbook's precondition 2 can move from
`blocked_by_operator` to `verified`. Until then the honest statement is:

> **RPO = the last on-host backup. RTO = unbounded on host loss.**

Measured facts this sheet is built on are in the main plan §4: Qdrant snapshots
daily 03:17 UTC keeping 7, on-host only, restore proven 2026-09-09; app bundles
daily 03:20 UTC keeping 7, GPG-encrypted, on-host only, restore **not** proven;
PostgreSQL has **no backup of any kind**.

---

## 1. Off-host destination

| Field | Value | Notes |
|---|---|---|
| Destination kind | `______` | object storage / second host / pull to Windows `D:` / other |
| Provider or hostname | `______` | name only; no credentials in this file |
| Region or physical location | `______` | must not share a failure domain with the VPS |
| Path or bucket prefix | `______` | |
| Transport | `______` | e.g. rsync over SSH, rclone, provider CLI |
| Push or pull | `______` | a pull from the destination survives a compromised source host; a push does not |
| Bandwidth/cost ceiling | `______` | |
| Approved by | `______` | |
| Date | `______` | |

Nothing was chosen here on the model's judgement, because the choice carries
cost and a data-residency consequence.

## 2. Encryption

| Field | Value | Notes |
|---|---|---|
| At-rest encryption | `______` | provider-side, GPG, age, other |
| Key owner (person) | `______` | not a role, a person |
| Key source | `______` | where the key material lives — the vault, a smartcard, a KMS |
| Key escrow / second holder | `______` | a backup only one person can decrypt is a single point of failure |
| Key rotation interval | `______` | |
| Restore requires which key | `______` | old bundles need the key they were made with |

**Do not put key material, passphrases or credentials in this file, in the
repository, or in the shared RAG.** Name where it lives; nothing more.

## 3. RPO — how much may be lost

| Field | Value | Notes |
|---|---|---|
| Target RPO, ledger | `______` | proposal was ~5 minutes, which requires `archive_mode=on` + WAL archiving |
| Target RPO, vector index | `______` | derived state; it can be rebuilt from the ledger, so a longer RPO is defensible |
| Target RPO, app bundle | `______` | |
| Accepted worst case on host loss | `______` | the number that is true today is "everything since the last on-host dump, and even that is on the lost disk" |
| `archive_mode` change approved | `______` | required for any RPO shorter than the dump interval; needs a maintenance window |
| Maintenance window for it | `______` | |

## 4. RTO — how long recovery may take

| Field | Value | Notes |
|---|---|---|
| Target RTO, single-database restore | `______` | proposal was ~30 minutes |
| Target RTO, full host loss | `______` | includes provisioning a new host; currently unbounded |
| Who declares a disaster | `______` | |
| Where the procedure lives | `docs/runbooks/memgw-backup-restore.md` | already written |

## 5. Retention

| Field | Value | Notes |
|---|---|---|
| Ledger dumps kept | `______` | count and/or age |
| WAL spool kept | `______` | |
| Qdrant snapshots kept | 7 today | change? `______` |
| App bundles kept | 7 today | these still contain the **pre-rotation** admin token |
| Legal or policy floor | `______` | |
| Purge of pre-rotation bundles | `______` | purge early, or let retention age them out — purging destroys recovery material |

## 6. Restore ownership and rehearsal

| Field | Value | Notes |
|---|---|---|
| Restore operator (primary) | `______` | |
| Restore operator (backup) | `______` | |
| Rehearsal cadence | `______` | a backup nobody has restored is a hypothesis |
| Next rehearsal window | `______` | |
| Rehearsal scope | `______` | dump/restore only, or full cross-machine |

## 7. Cross-machine restore test — result

Not performed. This is the test that would retire the largest untested claim in
the whole package.

| Field | Value |
|---|---|
| Date performed | `______` |
| Source backup (name, size, checksum) | `______` |
| Target machine | `______` |
| Target PostgreSQL version | `______` |
| `pg_restore` exit code | `______` |
| Reconciliation: counts match | `______` |
| Reconciliation: `receipts_without_event` = 0 | `______` |
| Reconciliation: `events_without_receipt` = 0 | `______` |
| Event chain checksum matches | `______` |
| Receipt chain checksum matches | `______` |
| `memgw history replay` — newly marked | `______` |
| `memgw projection rebuild` — rows restored | `______` |
| Wall-clock time to first correct query | `______` (this is the measured RTO) |
| Verdict | `______` |

The rehearsal already run on the test machine restored into a **second database
on the same server**, from a dump sitting on the **same disk**. It proved the
schema and contents survive a dump/restore cycle and that the copy reconciles.
It proved nothing about surviving the loss of that disk, and it must not be
reported as off-host durability.

## 8. What is deliberately not decided here

- No provider was selected and no account exists.
- No bucket, share or remote path was created.
- No backup was uploaded anywhere.
- No credential was created for any destination.
- `archive_mode` was not changed and no maintenance window was booked.

All five are the owner's to do, and each is on the forbidden list for unattended
work.
