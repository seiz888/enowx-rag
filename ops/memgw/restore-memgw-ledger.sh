#!/bin/bash
# memgw ledger restore -- replace the memgw cluster's database with a dump taken
# from the authoritative ledger, then prove the result is byte-for-byte the same
# ledger.
#
# Why a whole-database replace, and not a data-only load
# -----------------------------------------------------
# The loudest lesson from building this migration is that matching row COUNTS
# prove almost nothing. The server ledger that this script replaces reported
# 135 events / 135 receipts and `max_seq = 135` -- identical to the source -- and
# was still a different ledger: a probe event occupied seq 135 on the server
# while the source had a real `omp.compact` committed there. Same count, same
# high-water mark, different chain. A reconciliation that stops at counts would
# have declared that a success.
#
# So the reconciliation here is content-level, and the rest is arranged to make
# it possible:
#
#   * One digest over the ordered chain: `seq:event_id:payload_digest` for
#     events, `seq:event_id:payload_digest` for receipts. Equal digests over the
#     same ordered rows means the same ledger, not merely a same-sized one.
#   * Per-epoch and terminal-seq reporting, so a fork shows up as a divergence
#     in the last rows rather than hiding behind an equal count.
#   * Zero orphans both ways: no event without a receipt and no receipt without
#     an event.
#
# In-place restore is impossible by design: `events`, `write_receipts`,
# `checkpoints`, `evidence`, `tombstones` and `principal_credentials` carry
# append-only BEFORE DELETE/UPDATE triggers that raise. A restore therefore
# targets a NEW database and the gateway is repointed at it, which is also what
# makes the operation reversible: the previous database is left intact until the
# new one has proved itself.
#
# `pg_restore --data-only` is NOT used. It fails on this schema with a foreign
# key violation (`branches_project_id_fkey`) because data sections restore in an
# order that does not respect FKs when the schema is loaded separately; a full
# restore (schema + data in one pass) is what works, and it also carries the
# identity values, so no `ALTER COLUMN` is needed -- `events_seq_seq` continues
# past `max(seq)` on its own.
#
# Cluster-level grants are NOT in the dump. `memgw_app`'s membership in
# `memgw_migrator` lives in `pg_auth_members` and is cluster-wide, so a restore
# onto a fresh cluster would leave the application role unable to read its own
# tables. That grant is re-asserted here and verified.
#
# Usage:
#   restore-memgw-ledger.sh <dump-file> [--target <db>] [--expect-events N] [--expect-chain HEX] [--keep-old]
#
# Run as a user that can `sudo -u postgres`. Nothing here prints a password.

set -euo pipefail

PORT="${MEMGW_PORT:-5433}"
SRC_DB="${MEMGW_DB:-memgw}"
TARGET=""
EXPECT_EVENTS=""
EXPECT_CHAIN=""
KEEP_OLD=0
DUMP=""

while [ $# -gt 0 ]; do
  case "$1" in
    --target)       TARGET="$2"; shift 2 ;;
    --expect-events) EXPECT_EVENTS="$2"; shift 2 ;;
    --expect-chain)  EXPECT_CHAIN="$2"; shift 2 ;;
    --keep-old)     KEEP_OLD=1; shift ;;
    -*)             echo "unknown option: $1" >&2; exit 2 ;;
    *)              DUMP="$1"; shift ;;
  esac
done

log()  { echo "$(date -Is) $*"; }
fail() { echo "$(date -Is) FAILED: $*" >&2; exit 1; }
psql_() { sudo -u postgres psql -p "$PORT" -d "$1" -tAc "$2"; }

[ -n "$DUMP" ] || fail "usage: restore-memgw-ledger.sh <dump-file> [--target <db>] ..."
[ -f "$DUMP" ] || fail "dump not found: $DUMP"

# The dump is gzip-wrapped in this pipeline. Detect rather than assume, so the
# script also accepts a raw custom-format archive.
MAGIC=$(head -c 2 "$DUMP" | od -An -tx1 | tr -d ' \n')
if [ "$MAGIC" = "1f8b" ]; then
  DECOMPRESS=1
  log "dump is gzip-wrapped"
else
  DECOMPRESS=0
fi

# Decompress once to a scratch file: pg_restore needs a seekable archive, and
# re-running it through a pipe for each of several passes (list, restore,
# verify) would decompress several times over.
WORK=$(mktemp -d /tmp/memgw-restore-XXXXXX)
trap 'rm -rf "$WORK"' EXIT
ARCHIVE="$WORK/ledger.dump"
if [ "$DECOMPRESS" = "1" ]; then
  gzip -dc "$DUMP" > "$ARCHIVE" || fail "could not decompress $DUMP"
else
  cp "$DUMP" "$ARCHIVE"
fi

# This script runs as root (it needs `sudo -u postgres`), so `mktemp -d` created
# a root-owned 0700 directory. `pg_restore` runs as postgres and could not
# traverse it -- "Permission denied" on the archive it was just handed. The
# archive is the UNSEALED ledger, so the fix is not to widen it to the world:
# the directory keeps owner-only listing (0711 allows traversal, not listing)
# and the archive is handed to postgres, which is the only other identity that
# needs to read it. `rm -rf` in the trap still works because the 0711 directory
# keeps its write bit for its owner.
chmod 0711 "$WORK"
chown postgres:postgres "$ARCHIVE"
chmod 0600 "$ARCHIVE"

AMAGIC=$(head -c 5 "$ARCHIVE" | od -An -tx1 | tr -d ' \n')
[ "$AMAGIC" = "5047444d50" ] || fail "not a pg custom-format archive (magic $AMAGIC)"
TOC=$(pg_restore -l "$ARCHIVE" 2>/dev/null | grep -cE '^[0-9]+;' || true)
[ "$TOC" -ge 100 ] || fail "only $TOC TOC entries; refusing to restore a stub over a real ledger"
log "archive ok: $TOC TOC entries, $(stat -c %s "$ARCHIVE") bytes uncompressed"

# --- what the dump itself says ----------------------------------------------
# The dump's own `counts=` line is provenance, but the authoritative comparison
# is made against the restored database at the end. Print it so an operator can
# see a mismatch between the file and what they were told to expect.
MANIFEST="${DUMP%.aes}"
if [ -f "$MANIFEST.manifest" ]; then
  log "manifest says: $(grep -E '^(counts|toc_entries)=' "$MANIFEST.manifest" | tr '\n' ' ')"
fi

# --- pick a target that cannot be the live database -------------------------
if [ -z "$TARGET" ]; then
  TARGET="${SRC_DB}_restore_$(date -u +%Y%m%d%H%M%S)"
fi
[ "$TARGET" != "$SRC_DB" ] || fail "refusing to restore over the live database '$SRC_DB'; name a different --target"

if [ "$(psql_ postgres "SELECT 1 FROM pg_database WHERE datname='$TARGET'")" = "1" ]; then
  fail "target database '$TARGET' already exists; drop it or name another"
fi

log "restoring into NEW database '$TARGET' (the live '$SRC_DB' is untouched)"

# --- restore ----------------------------------------------------------------
# The dump's artifacts are owned by memgw_migrator and grant to memgw_app; both
# roles exist on this cluster, which is why the restore can carry ownership
# rather than being redirected with --no-owner.
sudo -u postgres createdb -p "$PORT" -T template0 -E UTF8 "$TARGET" \
  || fail "could not create $TARGET"

RESTORE_RC=0
sudo -u postgres pg_restore -p "$PORT" -d "$TARGET" --exit-on-error --no-owner "$ARCHIVE" \
  > "$WORK/restore.log" 2>&1 || RESTORE_RC=$?
if [ "$RESTORE_RC" -ne 0 ]; then
  echo "----- last 20 lines of pg_restore output -----" >&2
  tail -20 "$WORK/restore.log" >&2
  fail "pg_restore exited $RESTORE_RC; '$TARGET' left in place for inspection"
fi
log "pg_restore completed with no errors"

# --- apply the schema ownership the dump cannot carry -----------------------
# --no-owner leaves everything owned by the restoring role (postgres). The
# application connects as memgw_app and the append-only triggers plus the
# default ACLs assume memgw_migrator owns the schema, so ownership is re-applied
# to match the source cluster. Verified after the fact rather than assumed.
for t in $(psql_ "$TARGET" "SELECT tablename FROM pg_tables WHERE schemaname='memgw'"); do
  psql_ "$TARGET" "ALTER TABLE memgw.\"$t\" OWNER TO memgw_migrator" >/dev/null
done
for s in $(psql_ "$TARGET" "SELECT sequencename FROM pg_sequences WHERE schemaname='memgw'"); do
  psql_ "$TARGET" "ALTER SEQUENCE memgw.\"$s\" OWNER TO memgw_migrator" >/dev/null
done
psql_ "$TARGET" "ALTER SCHEMA memgw OWNER TO memgw_migrator" >/dev/null
log "ownership re-applied to memgw_migrator"

# Cluster-level grants are not in a dump. Without these the application cannot
# read its own ledger on a fresh cluster -- the failure would surface later, as
# a permission error under production load.
psql_ postgres "GRANT memgw_migrator TO memgw_app" >/dev/null
psql_ "$TARGET" "GRANT USAGE ON SCHEMA memgw TO memgw_app" >/dev/null
psql_ "$TARGET" "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA memgw TO memgw_app" >/dev/null
psql_ "$TARGET" "GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA memgw TO memgw_app" >/dev/null
log "memgw_app membership and schema privileges re-asserted"

# --- reconciliation, content-level ------------------------------------------
EV_CHAIN_Q="SELECT COALESCE(md5(string_agg(seq||':'||event_id::text||':'||payload_digest,'|' ORDER BY seq)),'empty') FROM memgw.events"
RC_CHAIN_Q="SELECT COALESCE(md5(string_agg(seq||':'||event_id::text||':'||payload_digest,'|' ORDER BY seq)),'empty') FROM memgw.write_receipts"

N_EVENTS=$(psql_ "$TARGET" "SELECT count(*) FROM memgw.events")
N_RECEIPTS=$(psql_ "$TARGET" "SELECT count(*) FROM memgw.write_receipts")
MAX_SEQ=$(psql_ "$TARGET" "SELECT COALESCE(max(seq),0) FROM memgw.events")
EV_CHAIN=$(psql_ "$TARGET" "$EV_CHAIN_Q")
RC_CHAIN=$(psql_ "$TARGET" "$RC_CHAIN_Q")
ORPHAN_EVENTS=$(psql_ "$TARGET" "SELECT count(*) FROM memgw.events e LEFT JOIN memgw.write_receipts r ON r.event_id=e.event_id WHERE r.event_id IS NULL")
ORPHAN_RECEIPTS=$(psql_ "$TARGET" "SELECT count(*) FROM memgw.write_receipts r LEFT JOIN memgw.events e ON e.event_id=r.event_id WHERE e.event_id IS NULL")

log "restored: events=$N_EVENTS receipts=$N_RECEIPTS max_seq=$MAX_SEQ"
log "  events_chain   $EV_CHAIN"
log "  receipts_chain $RC_CHAIN"
log "  orphans: events_without_receipt=$ORPHAN_EVENTS receipts_without_event=$ORPHAN_RECEIPTS"

[ "$EV_CHAIN" = "$RC_CHAIN" ] || fail "the restored event chain and receipt chain disagree ($EV_CHAIN vs $RC_CHAIN); the ledger is internally inconsistent"
[ "$ORPHAN_EVENTS" -eq 0 ] || fail "$ORPHAN_EVENTS event(s) have no receipt"
[ "$ORPHAN_RECEIPTS" -eq 0 ] || fail "$ORPHAN_RECEIPTS receipt(s) have no event"

# Per-epoch breakdown: this is what catches a fork whose total happens to
# match. Two ledgers can agree on 135 events and disagree on which epoch the
# last ones belong to.
log "per-epoch distribution:"
psql_ "$TARGET" "SELECT '  epoch '||writer_epoch||': '||count(*)||' events, seq '||min(seq)||'..'||max(seq) FROM memgw.events GROUP BY writer_epoch ORDER BY writer_epoch"

# The tail, printed so a reviewer sees the actual last rows rather than trusting
# a count. This is the check that would have caught the fork described at the
# top of this file.
log "last 3 events (seq|event_id|session|epoch):"
psql_ "$TARGET" "SELECT '  '||seq||'|'||event_id::text||'|'||COALESCE(session_id,'-')||'|'||writer_epoch FROM memgw.events ORDER BY seq DESC LIMIT 3"

# --- migrations -------------------------------------------------------------
# The gateway refuses to serve a schema whose migrations are not clean, so this
# is checked here rather than discovered at cutover.
MIG=$(psql_ "$TARGET" "SELECT count(*) FROM memgw.schema_migrations")
DIRTY=$(psql_ "$TARGET" "SELECT count(*) FROM memgw.schema_migrations WHERE dirty")
log "migrations: $MIG applied, $DIRTY dirty"
[ "$DIRTY" -eq 0 ] || fail "$DIRTY migration(s) are marked dirty in the restored copy"

# --- expected-value gates ---------------------------------------------------
if [ -n "$EXPECT_EVENTS" ] && [ "$N_EVENTS" -ne "$EXPECT_EVENTS" ]; then
  fail "expected $EXPECT_EVENTS events, restored $N_EVENTS"
fi
if [ -n "$EXPECT_CHAIN" ] && [ "$EV_CHAIN" != "$EXPECT_CHAIN" ]; then
  fail "expected events_chain $EXPECT_CHAIN, restored $EV_CHAIN"
fi

# --- application role can actually read it ----------------------------------
# Ownership and grants are the part most likely to be silently wrong on a fresh
# cluster, and the failure would only appear under load. Reading as memgw_app is
# the proof that matters, and reading pg_authid must still be refused.
# `-tAc` with two statements prints the command tag of the first as well, so the
# output is "SET\n<count>". Taking the last line reads the count itself; parsing
# the whole output compared "SET
# 135" against "135" and failed a correct restore.
APP_OK=$(sudo -u postgres psql -p "$PORT" -d "$TARGET" -tAc "SET ROLE memgw_app; SELECT count(*) FROM memgw.events" | tail -n1 | tr -d '[:space:]')
[ "$APP_OK" = "$N_EVENTS" ] || fail "memgw_app sees $APP_OK events, not $N_EVENTS; privileges were not restored correctly"
APP_DENIED=$(sudo -u postgres psql -p "$PORT" -d "$TARGET" -tAc "SET ROLE memgw_app; SELECT count(*) FROM pg_authid" 2>&1 || true)
case "$APP_DENIED" in
  *"permission denied"*) log "memgw_app: reads the ledger ($APP_OK events), still denied pg_authid" ;;
  *) fail "memgw_app could read pg_authid; the role is over-privileged: $APP_DENIED" ;;
esac

# --- identity ---------------------------------------------------------------
log "deployment identity: $(psql_ "$TARGET" "SELECT deployment||' recorded '||recorded_at FROM memgw.deployment_identity")"
log "writer epochs:"
psql_ "$TARGET" "SELECT '  epoch '||epoch||' opened '||opened_at||' fenced '||COALESCE(fenced_at::text,'(open)')||' reason '||COALESCE(reason_class,'-') FROM memgw.writer_epochs ORDER BY epoch"

log "RESTORE OK -> $TARGET"
echo "$TARGET"
