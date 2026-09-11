#!/bin/bash
# Bounded PostgreSQL backup for the memgw/axonhub cluster, staged for an
# off-host pull.
#
# Why this exists
# ---------------
# The cluster had no organised backup at all, and the one full dump taken by
# hand sat on the same disk as the database it protected. This script is the
# recurring half: it writes a compressed logical dump plus a checksum and a
# manifest into a staging directory, and it prunes by both age and count so the
# `/` volume (historically the tight one) cannot fill. The off-host half is a
# Windows Scheduled Task that pulls this staging directory, verifies the
# checksum, seals it with DPAPI and applies its own retention.
#
# What is and is not synced
# -------------------------
# The dump and the WAL archive are both staged. WAL is what makes the dump a
# point-in-time recoverable base rather than a snapshot: the pull copies
# `wal/` incrementally, so the archive travels off-host without being
# re-transferred every day.
#
# Streaming, not a temp file
# --------------------------
# `pg_dump -Fc` is piped straight into `gzip`, so the staging directory never
# holds an uncompressed copy. Two failures disappear: `pg_dump` (which runs as
# `postgres`) no longer needs write access to the staging directory at all, and
# a run no longer needs twice the dump size in free space. The pipe is why this
# is `bash` under `set -o pipefail` and not `sh`: if `pg_dump` dies mid-stream,
# `gzip` would still exit 0 and the run would report a successful backup of a
# truncated file.
#
# Bounded for the real disk
# -------------------------
# The `/` volume has ~15 GB free and an axonhub dump is ~4.7 GB, so retention
# is two dumps per database, pruned *before* the new dump is written: peak
# usage is two dumps, not three. Retention is per database (`${DB}-*`), so a
# small database cannot evict the large one.
#
# Failure is visible
# ------------------
# Every failure exits non-zero so systemd records a failed unit, and the exit
# trap deletes a partial dump rather than leaving it behind looking like a
# backup. A backup that quietly stops is the failure mode this whole file
# exists to prevent (the 2026-08-04 incident: cron fired for 36 days against a
# path that no longer existed and nothing ever said so).

set -euo pipefail

DB="${1:-axonhub}"
# Absolute minimum plausible dump size in bytes; overridable as $2. A dump
# below the effective floor is treated as truncated or empty. Once one good
# dump exists the relative floor below usually dominates.
ABS_FLOOR="${2:-1048576}"
STAGE=/opt/rag-backup/stage
PGBIN=/usr/lib/postgresql/16/bin
KEEP_DUMPS="${KEEP_DUMPS:-2}"
KEEP_DAYS=14

STAMP=$(date -u +%Y%m%d-%H%M%S)
DUMP="$STAGE/${DB}-${STAMP}.dump.gz"

log() { echo "$(date -Is) $*"; }
fail() { echo "$(date -Is) GAGAL: $*" >&2; exit 1; }

DONE=0
cleanup() { if [ "$DONE" != 1 ]; then rm -f "$DUMP"; fi; return 0; }
trap cleanup EXIT

mkdir -p "$STAGE"
# Group postgres, group write, setgid: staged files are readable by the database
# owner for a manual restore, and by nobody else.
chown root:postgres "$STAGE"
chmod 2770 "$STAGE"
mkdir -p "$STAGE/wal"
chown root:postgres "$STAGE/wal"

# --- prune before write ------------------------------------------------------
# Keep only one older dump during the run so peak usage is KEEP_DUMPS, not
# KEEP_DUMPS + 1. Count first (bounded regardless of clock), age afterwards.
PRE_KEEP=$(( KEEP_DUMPS - 1 ))
if [ "$PRE_KEEP" -lt 1 ]; then PRE_KEEP=1; fi
ls -1t "$STAGE/${DB}-"*.dump.gz 2>/dev/null | tail -n "+$((PRE_KEEP + 1))" | while read -r old; do
  rm -f "$old" "$old.sha256" "$old.manifest"
  log "pruned $(basename "$old")"
done || true

# --- floors and free space ---------------------------------------------------
# The strongest cheap check is relative: a dump smaller than 60% of the last
# good dump is far more likely truncated than genuinely smaller, and a fixed
# byte floor cannot see that. The absolute floor covers the first-ever run.
MIN_BYTES=$ABS_FLOOR
NEED_FREE=1048576
PREV=$(ls -1t "$STAGE/${DB}-"*.dump.gz 2>/dev/null | head -n1 || true)
if [ -n "$PREV" ]; then
  PREV_SIZE=$(stat -c %s "$PREV")
  REL_FLOOR=$(( PREV_SIZE * 60 / 100 ))
  if [ "$REL_FLOOR" -gt "$MIN_BYTES" ]; then MIN_BYTES=$REL_FLOOR; fi
  NEED_FREE=$(( PREV_SIZE * 13 / 10 ))
fi
FREE=$(df -Pk "$STAGE" | awk 'NR==2 {print $4 * 1024}')
[ -n "${FREE:-}" ] || fail "cannot determine free space on $STAGE"
if [ "$FREE" -lt "$NEED_FREE" ]; then
  fail "only $FREE bytes free in $STAGE; need $NEED_FREE for a $DB dump"
fi

# --- dump --------------------------------------------------------------------
# -Fc for selective/parallel restore; streamed through gzip because the volume
# is the constraint. The dump runs as the postgres superuser over its local
# unix socket, so no password is on any command line. pipefail is what makes a
# `pg_dump` failure fail the run instead of being masked by gzip's exit 0.
log "dump $DB -> $DUMP"
sudo -u postgres "$PGBIN/pg_dump" -Fc -d "$DB" | gzip -1 > "$DUMP"

[ -s "$DUMP" ] || fail "$DUMP is empty"
SIZE=$(stat -c %s "$DUMP")
[ "$SIZE" -ge "$MIN_BYTES" ] || fail "$DUMP is only $SIZE bytes; expected at least $MIN_BYTES"

# --- integrity ---------------------------------------------------------------
# The dump is gzipped, so pg_restore cannot read it in place (it seeks). Verify
# the gzip stream end to end with `gzip -t` (catches truncation) and confirm the
# decompressed stream is a pg custom-format archive by its magic header. A
# half-written dump passes a size floor; it does not pass this.
gzip -t "$DUMP" || fail "$DUMP is not a valid gzip stream (truncated?)"
# `head` closes the pipe after five bytes, so gzip is killed by SIGPIPE; the
# subshell turns pipefail off for this probe alone so that is not read as a
# failure of the backup.
MAGIC=$(set +o pipefail; gzip -dc "$DUMP" 2>/dev/null | head -c 5 | od -An -tx1 | tr -d ' \n')
[ "$MAGIC" = "5047444d50" ] || fail "$DUMP does not start with the pg_dump custom-format magic (got $MAGIC)"
SUM=$(sha256sum "$DUMP" | awk '{print $1}')
echo "$SUM  $(basename "$DUMP")" > "$DUMP.sha256"
DONE=1
log "ok: $(basename "$DUMP") $SIZE bytes sha256=$SUM"

# --- WAL archive -------------------------------------------------------------
# Copy the archive into the staging area so the Windows pull can take it
# incrementally. `cp -u` means only new segments move. Keep the staging copy
# bounded: the authoritative archive is the live directory, this is a buffer.
ARCHIVE=/var/lib/postgresql/16/archive
if [ -d "$ARCHIVE" ]; then
  cp -u "$ARCHIVE"/*.gz "$STAGE/wal/" 2>/dev/null || true
  find "$STAGE/wal" -name '*.gz' -mtime "+$KEEP_DAYS" -delete 2>/dev/null || true
fi

# --- manifest ----------------------------------------------------------------
# Two copies: one naming this dump exactly, so a sealed artefact carries its own
# provenance, and the `latest` pointer the pull uses as a convenience.
{
  echo "database=$DB"
  echo "dump=$(basename "$DUMP")"
  echo "bytes=$SIZE"
  echo "sha256=$SUM"
  echo "taken_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "wal_segments=$(find "$STAGE/wal" -name '*.gz' 2>/dev/null | wc -l)"
  echo "pg_version=$("$PGBIN/postgres" --version | awk '{print $3}')"
} > "$DUMP.manifest"
cp -f "$DUMP.manifest" "$STAGE/latest.manifest"

# --- retention (age) ---------------------------------------------------------
find "$STAGE" -name "${DB}-*.dump.gz" -mtime "+$KEEP_DAYS" -delete 2>/dev/null || true
find "$STAGE" -name "${DB}-*.dump.gz.sha256" -mtime "+$KEEP_DAYS" -delete 2>/dev/null || true

KEPT=$(find "$STAGE" -name "${DB}-*.dump.gz" | wc -l)
log "done: $KEPT dump(s) staged, $(du -sh "$STAGE" | cut -f1) total"
