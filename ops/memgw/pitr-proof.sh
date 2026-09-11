#!/bin/bash
# PITR proof on a disposable cluster.
#
# Proves, without a spare physical host and without touching production:
#   initdb -> archive_mode=on -> write MARKER -> pg_basebackup -> write AFTER
#   -> recover the base to the MARKER's LSN -> MARKER present, AFTER absent.
#
# This is the real mechanism (base backup + WAL replay to a named LSN), not a
# pg_dump restore. The same procedure recovers the production cluster from
# /opt/rag-backup/pg-basebackup + /var/lib/postgresql/16/archive.
#
# Everything it creates is under /tmp/pitr-proof and is removed on exit unless
# KEEP=1. It touches no existing cluster, database or protected resource.

set -eu

PGBIN=/usr/lib/postgresql/16/bin
ROOT=/tmp/pitr-proof
SRC=$ROOT/src            # the "production" disposable cluster
ARCH=$ROOT/archive       # its WAL archive
BASE=$ROOT/base          # pg_basebackup output
REST=$ROOT/restored      # recovered cluster
PORT=55999
LOG=$ROOT/proof.log

KEEP="${KEEP:-0}"

log() { echo "$(date -Is) $*" | tee -a "$LOG"; }
fail() { echo "$(date -Is) GAGAL: $*" | tee -a "$LOG" >&2; exit 1; }

cleanup() {
  [ "$KEEP" = "1" ] && { log "KEEP=1: leaving $ROOT"; return; }
  "$PGBIN/pg_ctl" -D "$REST" stop -m immediate >/dev/null 2>&1 || true
  "$PGBIN/pg_ctl" -D "$SRC"  stop -m immediate >/dev/null 2>&1 || true
  rm -rf "$ROOT"
}
trap cleanup EXIT

rm -rf "$ROOT"
# initdb requires its target directory to be empty, so create only the parents
# here; the cluster dir itself is made by initdb.
mkdir -p "$ROOT" "$ARCH" "$BASE" "$REST"
chmod 700 "$ARCH"

log "=== 1. initdb a disposable source cluster ==="
"$PGBIN/initdb" -D "$SRC" -U postgres --auth=trust >/dev/null 2>&1 || fail "initdb"

# archive_mode=on with an archive_command writing to $ARCH. Archive each WAL
# segment as it completes; recovery will copy them back into pg_wal.
cat >> "$SRC/postgresql.conf" <<CONF
port = $PORT
listen_addresses = ''
unix_socket_directories = '$ROOT'
wal_level = replica
archive_mode = on
archive_command = 'test ! -f $ARCH/%f && cp %p $ARCH/%f'
archive_timeout = 2
full_page_writes = on
CONF

"$PGBIN/pg_ctl" -D "$SRC" -l "$SRC/server.log" start -w -t 30 >/dev/null || fail "start source"
log "source cluster up on port $PORT"

PSQL="$PGBIN/psql -h $ROOT -p $PORT -U postgres -d postgres -tA"

log "=== 2. create the table, then take the BASE backup (the fixed start point) ==="
# Order matters: the base backup is the start of recoverable history, so it is
# taken BEFORE the marker. Recovering the base forward to the marker's LSN then
# contains the marker and excludes everything written after it.
$PSQL -c "CREATE TABLE events (id int primary key, note text);" >/dev/null
$PSQL -c "SELECT pg_switch_wal();" >/dev/null
sleep 2
"$PGBIN/pg_basebackup" -h "$ROOT" -p $PORT -U postgres -D "$BASE" -Ft -z -X stream >/dev/null 2>&1 \
  || fail "pg_basebackup"
log "base backup taken"

log "=== 3. write the MARKER row and record its LSN ==="
$PSQL -c "INSERT INTO events VALUES (1, 'MARKER: before the recovery target');" >/dev/null
MARKER_LSN=$($PSQL -c "SELECT pg_current_wal_lsn();" | tr -d '[:space:]')
log "marker LSN = $MARKER_LSN"

# Give the archive a moment to flush the segment containing the marker.
$PSQL -c "SELECT pg_switch_wal();" >/dev/null
sleep 3

log "=== 4. write the AFTER row (must NOT survive recovery to the marker) ==="
$PSQL -c "INSERT INTO events VALUES (2, 'AFTER: written past the recovery target');" >/dev/null
$PSQL -c "SELECT pg_switch_wal();" >/dev/null
sleep 3

BEFORE_COUNT=$($PSQL -c "SELECT count(*) FROM events;" | tr -d '[:space:]')
log "source now holds $BEFORE_COUNT rows (marker + after)"

log "=== 5. build the recovery cluster from base + WAL to the marker LSN ==="
tar -xzf "$BASE/base.tar.gz" -C "$REST" || fail "untar base"
# PostgreSQL refuses a data directory that is group/world accessible; the tar
# extract does not preserve the 0700 the base had.
chmod 700 "$REST"
# recovery.signal = archive recovery; recovery_target_lsn stops AT the marker.
cat >> "$REST/postgresql.conf" <<CONF
port = $((PORT + 1))
listen_addresses = ''
unix_socket_directories = '$ROOT'
archive_mode = off
restore_command = 'cp $ARCH/%f %p'
recovery_target_lsn = '$MARKER_LSN'
recovery_target_action = 'promote'
CONF
touch "$REST/recovery.signal"

"$PGBIN/pg_ctl" -D "$REST" -l "$REST/recovery.log" start -w -t 60 >/dev/null || {
  cat "$REST/recovery.log" | tail -20 | tee -a "$LOG"; fail "recovery start"
}

RPSQL="$PGBIN/psql -h $ROOT -p $((PORT + 1)) -U postgres -d postgres -tA"
sleep 2
IN_RECOVERY=$($RPSQL -c "SELECT pg_is_in_recovery();" | tr -d '[:space:]')
ROWS=$($RPSQL -c "SELECT count(*) FROM events;" | tr -d '[:space:]')
HAS_MARKER=$($RPSQL -c "SELECT count(*) FROM events WHERE note LIKE 'MARKER%';" | tr -d '[:space:]')
HAS_AFTER=$($RPSQL -c "SELECT count(*) FROM events WHERE note LIKE 'AFTER%';" | tr -d '[:space:]')

log "recovered: in_recovery=$IN_RECOVERY rows=$ROWS marker=$HAS_MARKER after=$HAS_AFTER"

FAILED=0
[ "$HAS_MARKER" = "1" ] || { log "FAIL: the marker row did not survive recovery"; FAILED=1; }
[ "$HAS_AFTER" = "0" ]  || { log "FAIL: the AFTER row survived; recovery did not stop at the target"; FAILED=1; }
[ "$IN_RECOVERY" = "f" ] || { log "FAIL: cluster is still in recovery (did not reach a consistent, promoted state)"; FAILED=1; }

if [ "$FAILED" = "0" ]; then
  log "PITR PROOF PASS: recovered to LSN $MARKER_LSN; marker present, later write absent"
  exit 0
fi
log "PITR PROOF FAIL"
exit 1
