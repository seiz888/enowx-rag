# Runbook — fencing a writer, and rolling authority back

Two ways to stop a host writing, for two different problems:

- **Fencing an epoch** stops *a host generation* — the machine was replaced, the
  cutover happened, the process is presumed gone. It is per epoch, not per
  principal, and it is how a host that comes back from being offline is refused.
- **Revoking grants** stops *a principal* from writing where it should not. It
  is per principal and per project, and it takes effect on the next event.

Both leave the events themselves on disk in the collector's spool. Neither
deletes anything canonical: the log only ever grows.

Exercised end to end against a real collector process, a real named pipe and a
live test gateway. Not run against production; production fencing and cutover
are not authorised.

---

## 1. Fencing an epoch

Fencing is an **admin** action, submitted as an event like anything else — not a
hand-written `UPDATE`. A committed receipt for `writer_epoch.fenced` means the
epoch is fenced, because the event is what changes the table.

Payload:

```json
{"epoch": 1, "reason_class": "cutover", "drain_watermark_seq": 22}
```

- `reason_class` is a closed vocabulary: `cutover`, `host_replaced`,
  `credential_rotation`, `incident`, `maintenance`.
- `drain_watermark_seq` is optional and defaults to the fencing event's **own
  seq**, which is correct by construction: nothing stamped with the old epoch
  can commit after the fence.
- `epoch` must be positive. Zero is what an omitted field decodes to, and
  admitting it would fence whatever epoch is numbered zero.

Refusals, all `policy_rejected` and all deliberate:

- Opening an epoch that is already open — two opens mean two hosts believe they
  were admitted separately, and every event already stamped with that epoch
  points at the row a second open would move.
- Fencing an epoch nobody opened — creating the row here would *admit* an epoch
  by fencing it, the opposite of what was asked.
- A second fence of an already-fenced epoch is a no-op that keeps the **first**
  timestamp, reason and watermark. When the epoch stopped accepting writes is
  evidence, and a retry during an incident must not rewrite it.

A non-admin attempting to fence gets `scope_denied` and the epoch stays open. A
host that could fence would be able to lock every other writer out of the ledger.

### Order of operations

**Open the new epoch first, move the hosts, fence the old one last.**

```
writer_epoch.opened  {epoch: N+1, reason_class: cutover}
  → update each host's adapter config to writer_epoch N+1
writer_epoch.fenced  {epoch: N,   reason_class: cutover}
```

Fencing first strands every host that has not moved yet, and the fencing event
itself is stamped with an epoch — fence the epoch you are submitting under and
the next admin event is refused before it reaches the domain.

### What the fenced host sees

Verified: a collector holding an undelivered event stamped `writer_epoch 1`
reconnected after the fence, was refused, and **quarantined** the row rather than
retrying forever:

```
seq 18  state quarantined  type session.started  attempts 4
fail_class  writer_epoch_fenced
fail_reason "the gateway refused this event: this writer has been fenced"
```

Rolling that host forward means setting `writer_epoch` in its adapter config and
firing the hook again. It will derive the **same idempotency key** and therefore
deduplicate against the held row — the collector answers class `held`, not
"accepted". Purge the held row (§3), then the corrected event commits.

## 2. Rolling authority back

```
enowx-rag memgw principal list <principal-id>          # see grants and credentials
enowx-rag memgw principal revoke-credential <cred-id>  # one way of being it
enowx-rag memgw principal revoke <principal-id>        # every way, at once
```

Revoking a *principal* revokes every credential it holds, immediately.

**Revoke the whole role set, not one grant.** Verified the hard way: revoking
only `checkpoint_write` changed nothing, because `session.started` also accepts
`work_read`, and the next event committed normally. An operator who revoked one
grant and walked away would believe a rollback had taken effect that had not.
Check the role table for the event types you actually want to stop before
deciding which grants to pull.

With both grants revoked, the next hook was accepted by the collector, refused by
the gateway, and quarantined after **one** attempt with `fail_class scope_denied`
("this principal may not write here"). It was held, not lost, and the ledger
gained nothing while the rollback was in force.

## 3. Rolling forward

Restore the grants (`memgw principal grant …`), then release the held rows:

```
enowx-rag memgw collector held    --dir <dir> --token-file <file>
enowx-rag memgw collector release --seq N
```

Verified: restoring both grants and releasing the held row delivered that exact
event. **The held row is the work** — the host does not need to redo anything.

Use `purge --seq N` instead when the *event* was wrong and the host will produce
a corrected one deriving the same key; while the held row exists the correction
deduplicates against it and can never be queued. See `memgw-collector.md` §4 for
release vs discard vs purge.

## 4. Verify the rollback actually held

A control that quietly does nothing is worse than none. After either operation:

- `memgw collector stats` — the refused events are in `quarantined`, not `sent`.
- The ledger's `max(seq)` did not move while the rollback was in force.
- The chain checksum over the range that existed before is unchanged: nothing was
  rewritten, only refused. Verified — the chain over `seq <= 20` was identical
  before and after the whole fencing and rollback rehearsal.
- `SELECT epoch, fenced_at, reason_class, drain_watermark_seq FROM writer_epochs`
  reads the way you asked for it.

## 5. Limits

Everything above ran on disposable test resources with a disposable test
credential. Production fencing and cutover are not authorised, and no production
host has a memgw credential yet.
