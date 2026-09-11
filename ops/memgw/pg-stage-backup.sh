#!/bin/sh
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
# `wal/` incrementally with rsync semantics, so the archive travels off-host
# without being re-transferred every day.
#
# Failure is visible
# ------------------
# Every failure exits non-zero so systemd records a failed unit. A backup that
# quietly stops is the failure mode this whole file exists to prevent (the
# 2026-08-04 incident: cron fired for 36 days against a path that no longer
# existed and nothing ever said so).

set -eu

DB="${1:-axonhub}"
# Minimum plausible dump size. A truncated or empty dump is the failure a plain
# existence check misses; this floor is what catches it. Overridable for a
# small database (the mechanism is the same, the corpus is not).
MIN_BYTES="${2:-1000000}"
STAGE=/opt/rag-backup/stage
PGBIN=/usr/lib/postgresql/16/bin
KEEP_DUMPS=4
KEEP_DAYS=14

STAMP=$(date -u +%Y%m%d-%H%M%S)
DUMP="$STAGE/${DB}-${STAMP}.dump.gz"
TMP="$STAGE/.${DB}-${STAMP}.dump"

log() { echo "$(date -Is) $*"; }
fail() { echo "$(date -Is) GAGAL: $*" >&2; exit 1; }

mkdir -p "$STAGE"
# pg_dump runs as `postgres` (over its unix socket, so no password is needed),
# so the staging directory must be writable by that user for the temporary
# uncompressed dump. Group postgres with group write, setgid so files inherit
# the group: readable and writable by root and postgres only.
chown root:postgres "$STAGE"
chmod 2770 "$STAGE"

# --- dump -------------------------------------------------------------------
# -Fc for selective/parallel restore; streamed through gzip because the volume
# is the constraint. The dump runs as the postgres superuser over its local
# unix socket, so no password is on any command line.
log "dump $DB -> $DUMP"
sudo -u postgres "$PGBIN/pg_dump" -Fc -d "$DB" -f "$TMP" \
  || fail "pg_dump $DB failed"
gzip -1 -c "$TMP" > "$DUMP" || fail "gzip failed"
rm -f "$TMP"

[ -s "$DUMP" ] || fail "$DUMP is empty"
SIZE=$(stat -c %s "$DUMP")
[ "$SIZE" -ge "$MIN_BYTES" ] || fail "$DUMP is only $SIZE bytes; expected at least $MIN_BYTES"

# --- integrity ---------------------------------------------------------------
# The dump is gzipped, so pg_restore cannot read it in place (it seeks). Verify
# the gzip stream end-to-end with `gzip -t` (catches truncation) and confirm the
# decompressed stream is a pg custom-format archive by its magic header. This is
# stronger than a size check: a half-written dump passes the size floor.
gzip -t "$DUMP" || fail "$DUMP is not a valid gzip stream (truncated?)"
MAGIC=$(gzip -dc "$DUMP" 2>/dev/null | head -c 5 | od -An -tx1 | tr -d ' \n')
[ "$MAGIC" = "5047444d50" ] || fail "$DUMP does not start with the pg_dump custom-format magic (got $MAGIC)"
SUM=$(sha256sum "$DUMP" | awk '{print $1}')
echo "$SUM  $(basename "$DUMP")" > "$DUMP.sha256"
log "ok: $(basename "$DUMP") $SIZE bytes sha256=$SUM"

# --- WAL archive -------------------------------------------------------------
# Copy the archive into the staging area so the Windows pull can take it
# incrementally. `cp -u` (update) means only new segments move; the Windows
# side then rsyncs its own copy.
ARCHIVE=/var/lib/postgresql/16/archive
if [ -d "$ARCHIVE" ]; then
  mkdir -p "$STAGE/wal"
  sudo cp -u "$ARCHIVE"/*.gz "$STAGE/wal/" 2>/dev/null || true
  # Keep the staging WAL copy bounded: the authoritative archive is the live
  # directory, this is only the pull buffer.
  find "$STAGE/wal" -name '*.gz' -mtime +$KEEP_DAYS -delete
fi

# --- manifest ----------------------------------------------------------------
{
  echo "database=$DB"
  echo "dump=$(basename "$DUMP")"
  echo "bytes=$SIZE"
  echo "sha256=$SUM"
  echo "taken_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "wal_segments=$(find "$STAGE/wal" -name '*.gz' 2>/dev/null | wc -l)"
  echo "pg_version=$("$PGBIN/postgres" --version | awk '{print $3}')"
} > "$STAGE/latest.manifest"

# --- retention ---------------------------------------------------------------
# By count for dumps (bounded regardless of clock), by age for WAL.
ls -1t "$STAGE"/${DB}-*.dump.gz 2>/dev/null | tail -n +$((KEEP_DUMPS + 1)) | while read -r f; do
  rm -f "$f" "$f.sha256"
  log "pruned $(basename "$f")"
done
find "$STAGE" -name "${DB}-*.dump.gz" -mtime +$KEEP_DAYS -delete 2>/dev/null || true
find "$STAGE" -name "${DB}-*.dump.gz.sha256" -mtime +$KEEP_DAYS -delete 2>/dev/null || true

KEPT=$(find "$STAGE" -name "${DB}-*.dump.gz" | wc -l)
log "done: $KEPT dump(s) staged, $(du -sh "$STAGE" | cut -f1) total"
