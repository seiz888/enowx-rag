#!/bin/bash
# memgw migrate rehearsal -- prove end-to-end that a dump taken from the
# authoritative ledger restores into this cluster as the SAME ledger.
#
# This is the gate before the real cutover. It does the whole transfer in one
# shot and compares content, not counts:
#
#   source ledger --pg_dump--> archive --pg_restore--> NEW database
#                          |                                   |
#                          +---- ledger-digest.sql ----------- +
#                                        |
#                                  must be identical
#
# Both digests are computed with the SAME file (`ledger-digest.sql`), so the
# algorithm cannot silently differ between the two sides -- the failure mode
# where a verification harness agrees with itself because the two sides were
# hashed differently.
#
# Why counts are not enough: during this migration the staging ledger held 135
# events / 135 receipts / max_seq 135, identical to the source, and was a
# different ledger -- a probe occupied seq 135 where the source had a real
# `omp.compact`. Counts pass that case; this comparison does not.
#
# The source keeps committing, so a dump is a moment, not a state. The script
# therefore reports the source's `max(seq)` before and after the transfer. If
# the ledger advanced during the window, the digest comparison will legitimately
# differ by exactly those events and the script says so explicitly rather than
# reporting a false failure -- and re-running is the fix. That is also why this
# must be run against a QUIESCED source at real cutover: only then is the
# equality exact.
#
# Usage:
#   migrate-rehearsal.sh <dump-or-'-for-fresh-dump'> [--target <db>] [--keep-target]
#   migrate-rehearsal.sh <dump> --source-digest <file> [--keep-target]
#
# `--source-digest` exists because the authoritative ledger is not always on
# this host. During the rehearsal the source is the workstation's ledger and the
# dump was transferred here, so the digest to match against must be the one
# computed on the workstation -- computing a "source" digest from this host's
# staging database would compare the restore against the wrong ledger, and it
# would pass while proving nothing. At real cutover the source IS here and the
# file is omitted, so the script hashes it itself.
#
# Run on the VPS. Requires `sudo -u postgres`. Prints no secret.

set -euo pipefail

PORT="${MEMGW_PORT:-5433}"
SRC_DB="${MEMGW_SRC_DB:-memgw}"
TARGET=""
KEEP_TARGET=0
DUMP=""
SOURCE_DIGEST=""

while [ $# -gt 0 ]; do
  case "$1" in
    --target)      TARGET="$2"; shift 2 ;;
    --source-digest) SOURCE_DIGEST="$2"; shift 2 ;;
    --keep-target) KEEP_TARGET=1; shift ;;
    -*)            echo "unknown option: $1" >&2; exit 2 ;;
    *)             DUMP="$1"; shift ;;
  esac
done

log()  { echo "$(date -Is) $*"; }
fail() { echo "$(date -Is) FAILED: $*" >&2; exit 1; }
psql_() { sudo -u postgres psql -p "$PORT" -d "$1" -tA "$2"; }

DIGEST_SQL=/opt/memgw/bin/ledger-digest.sql
RESTORE=/opt/memgw/bin/restore-memgw-ledger.sh
[ -f "$DIGEST_SQL" ] || fail "digest SQL not found at $DIGEST_SQL"
[ -x "$RESTORE" ]    || fail "restore script not found at $RESTORE"

# --- freeze-point observation ------------------------------------------------
# The digest the restore must reproduce is computed with the SAME SQL that runs
# against the restore, so the two reports are comparable line for line.
#
# When `--source-digest` is given the authoritative ledger is NOT on this host
# (it is still the workstation's), so nothing here is queried: reporting
# `max_seq` from this host's staging database would describe the wrong ledger
# and the comparison would be against itself.
WORK=$(mktemp -d /tmp/memgw-rehearsal-XXXXXX)
chmod 0711 "$WORK"
trap 'rm -rf "$WORK"' EXIT

if [ -n "$SOURCE_DIGEST" ]; then
  SEQ_BEFORE=$(sed -n 's/^chain events rows=\([0-9]*\).*/\1/p' "$SOURCE_DIGEST" 2>/dev/null | head -1)
  log "external source digest: $(basename "$SOURCE_DIGEST") (source holds ${SEQ_BEFORE:-?} events)"
else
  SEQ_BEFORE=$(psql_ "$SRC_DB" "SELECT COALESCE(max(seq),0) FROM memgw.events")
  log "source $SRC_DB at max_seq=$SEQ_BEFORE before transfer"
fi

# --- dump --------------------------------------------------------------------
if [ "$DUMP" = "-" ]; then
  # Streamed, matching the backup script: never materialise an uncompressed copy.
  DUMP="$WORK/source.dump.gz"
  sudo -u postgres pg_dump -p "$PORT" -Fc -d "$SRC_DB" | gzip -1 > "$DUMP.part" \
    || fail "pg_dump failed"
  mv "$DUMP.part" "$DUMP"
  log "dumped $SRC_DB -> $(basename "$DUMP") ($(stat -c %s "$DUMP") bytes)"
else
  [ -f "$DUMP" ] || fail "dump not found: $DUMP"
  log "using existing dump $DUMP"
fi

# --- source digest (post-dump, so it brackets the transfer) ------------------
if [ -n "$SOURCE_DIGEST" ]; then
  [ -f "$SOURCE_DIGEST" ] || fail "source digest not found: $SOURCE_DIGEST"
  # Strip carriage returns. A digest produced on Windows arrives CRLF-terminated,
  # and comparing it against this host's LF output makes every single line
  # "differ" while the ledgers are in fact identical -- a false failure in the
  # one check whose whole job is to be believed. Normalising here means the
  # comparison depends on content, not on which OS wrote the file.
  tr -d '\r' < "$SOURCE_DIGEST" > "$WORK/source.digest"
  SEQ_AFTER="$SEQ_BEFORE"
  log "using the provided source digest from $(basename "$SOURCE_DIGEST")"
else
  sudo -u postgres psql -p "$PORT" -d "$SRC_DB" -tA -f "$DIGEST_SQL" > "$WORK/source.digest" 2>&1 \
    || fail "source digest failed"
  sed -i 's/\r$//' "$WORK/source.digest"
  SEQ_AFTER=$(psql_ "$SRC_DB" "SELECT COALESCE(max(seq),0) FROM memgw.events")
  log "source digest taken at max_seq=$SEQ_AFTER"
  if [ "$SEQ_BEFORE" != "$SEQ_AFTER" ]; then
    log "NOTE: the source advanced $SEQ_BEFORE -> $SEQ_AFTER during the transfer window;"
    log "      the comparison below is valid for everything up to $SEQ_AFTER, and the"
    log "      real cutover must quiesce the source first."
  fi
fi

# --- restore -----------------------------------------------------------------
if [ -z "$TARGET" ]; then
  TARGET="${SRC_DB}_rehearsal_$(date -u +%Y%m%d%H%M%S)"
fi
sudo -u postgres dropdb -p "$PORT" --if-exists "$TARGET" 2>/dev/null || true
log "restoring into $TARGET"
RESTORED=$(sudo "$RESTORE" "$DUMP" --target "$TARGET" 2>&1 | tail -1) || {
  echo "$RESTORED" >&2; fail "restore failed"
}
[ "$RESTORED" = "$TARGET" ] || fail "restore did not report the target cleanly: $RESTORED"

sudo -u postgres psql -p "$PORT" -d "$TARGET" -tA -f "$DIGEST_SQL" > "$WORK/restored.digest" 2>&1 \
  || fail "restored digest failed"
sed -i 's/\r$//' "$WORK/restored.digest"

# Both reports contain blank lines and psql notices; compare only the report
# lines themselves, so an unrelated notice cannot masquerade as a difference.
grep -E '^(table |chain |chain-position |ledger )' "$WORK/source.digest"   > "$WORK/source.report"   || true
grep -E '^(table |chain |chain-position |ledger )' "$WORK/restored.digest" > "$WORK/restored.report" || true

# --- compare, line by line ---------------------------------------------------
# `diff` on the two reports is the whole verification: every table, the two
# order-sensitive chain digests, and the rollup. A single differing byte shows.
#
# A divergence in `table X` localises WHICH state is wrong; a divergence in
# `chain events` says the append-only history itself differs, which is the
# serious one; a divergence in `chain-position events seq=N` names the exact
# sequence number where the two ledgers part company -- the check that turns
# "these differ" into "they fork at 135".
echo
echo "================ digest comparison ================"
if diff -u "$WORK/source.report" "$WORK/restored.report" > "$WORK/diff.txt"; then
  echo "IDENTICAL: every table, both chain digests and the rollup match"
  grep '^ledger ' "$WORK/restored.report" | sed 's/^/  /'
  SAME=1
else
  echo "DIFFERENCES:"
  sed 's/^/  /' "$WORK/diff.txt"
  SAME=0
fi

# Restrict attention to the rows that a source-side commit can explain. The
# comparison above is the honest one; this classifies its result so a reader can
# tell "the source moved" from "the restore is wrong".
if [ "$SAME" = "0" ] && [ "$SEQ_BEFORE" != "$SEQ_AFTER" ]; then
  echo
  echo "The source moved during the window ($SEQ_BEFORE -> $SEQ_AFTER)."
  echo "Rows that differ ONLY by count on the source side:"
  diff "$WORK/source.report" "$WORK/restored.report" | grep -E '^[<>]' | grep -E 'rows=' | sed 's/^/  /' || true
  echo
  echo "Structural tables that MUST still match despite a moving source:"
  for t in deployment_identity writer_epochs schema_migrations principals grants \
           projects workspaces project_repos; do
    a=$(grep "^table $t " "$WORK/source.report" || true)
    b=$(grep "^table $t " "$WORK/restored.report" || true)
    if [ "$a" = "$b" ] && [ -n "$a" ]; then echo "  MATCH  $t"; else echo "  DIFFER $t"; fi
  done
fi

if [ "$KEEP_TARGET" = "0" ]; then
  sudo -u postgres dropdb -p "$PORT" "$TARGET" || true
  log "dropped rehearsal database $TARGET"
else
  log "kept rehearsal database $TARGET for inspection"
fi

[ "$SAME" = "1" ] || fail "the restored ledger is not identical to the source; do not cut over"
log "REHEARSAL OK: restore reproduces the source ledger exactly"
