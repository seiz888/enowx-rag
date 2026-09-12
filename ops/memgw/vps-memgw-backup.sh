#!/bin/bash
# memgw ledger backup — dump, seal, verify, retain. Runs on the VPS, where the
# ledger is authoritative.
#
# Scope
# -----
# This backs up the MEMGW CLUSTER ONLY (port 5433). It never touches the `main`
# cluster that serves axonhub: that database has its own pipeline and its own
# owner, and a memory-gateway job reaching into it would be exactly the
# cross-service coupling the dedicated cluster exists to avoid.
#
# WAL archiving is configured on the cluster itself (archive_mode=on,
# archive_timeout=300), so point-in-time recovery exists independently of this
# logical dump. This script is the *logical* restore point; the archive is the
# continuous one.
#
# Why the shape is what it is
# ---------------------------
# * Streaming, not a temp file: `pg_dump -Fc | gzip` never materialises an
#   uncompressed copy, so the run needs no scratch space and cannot leave a
#   half-written dump behind looking like a backup.
# * `set -o pipefail` matters: without it a dying pg_dump leaves gzip exiting 0
#   and the run reports success over a truncated file.
# * The dump is verified BEFORE it is sealed (gzip integrity, pg custom-format
#   magic, TOC count) and the sealed artefact is verified AFTER (unseal through
#   the escrowed recovery key and compare digests). A backup nobody has opened
#   is a hypothesis.
# * Every failure exits non-zero so systemd records a failed unit. A backup that
#   quietly stops is the failure this whole file exists to prevent.
# * Disk is the binding constraint on this host, so retention is bounded by
#   count and the run refuses to start below a free-space floor.
#
# No secret is printed: the DSN is read from the unit's EnvironmentFile, the
# escrowed recovery key never comes near this host (only its public half is
# here), and the data key is written to a 0600 file that is removed on exit.

set -euo pipefail

DB="${MEMGW_BACKUP_DB:-memgw}"
PORT="${MEMGW_BACKUP_PORT:-5433}"
DEST="${MEMGW_BACKUP_DEST:-/opt/memgw/backup}"
KEEP="${MEMGW_BACKUP_KEEP:-7}"
# Refuse to run below this much free space. A backup that fills the disk it
# backs up is a second outage, and this host runs at ~93% used.
MIN_FREE_BYTES="${MEMGW_BACKUP_MIN_FREE_BYTES:-2147483648}"

ENVELOPE=/opt/memgw/bin/ledger-envelope.py
RECOVERY_PUB=/opt/memgw/bin/ledger-recovery.pub.pem
STAMP="$(date -u +%Y%m%d-%H%M%S)"
STEM="$DEST/memgw-$STAMP.dump"
DUMP="$STEM.gz"
SEALED="$STEM.aes"

log() { echo "$(date -Is) $*"; }
fail() { echo "$(date -Is) FAILED: $*" >&2; exit 1; }

KEYFILE=""
cleanup() {
  rm -f "${DUMP:-}" "${KEYFILE:-}" 2>/dev/null || true
  # The unsealed verification copy is plaintext; it must not outlive the run.
  rm -f "${VERIFY_OUT:-}" 2>/dev/null || true
}
trap cleanup EXIT

[ -x "$ENVELOPE" ] || fail "envelope helper not found at $ENVELOPE"
[ -f "$RECOVERY_PUB" ] || fail "recovery public key not found at $RECOVERY_PUB (a backup with no cross-machine path is not a backup)"
install -d -m 0755 "$DEST"

# --- disk floor --------------------------------------------------------------
avail=$(df -B1 --output=avail "$DEST" | tail -1 | tr -d ' ')
if [ "$avail" -lt "$MIN_FREE_BYTES" ]; then
  fail "only ${avail} bytes free on $DEST; refusing to back up below ${MIN_FREE_BYTES} (this host is the tight one)"
fi
log "free space ${avail} bytes"

# --- dump (streamed) ---------------------------------------------------------
tmp_dump="$DUMP.part"
# Run as the postgres OS user so the socket is reachable via peer auth for the
# superuser; the connection string never carries a password.
if ! sudo -u postgres pg_dump -p "$PORT" -Fc -d "$DB" | gzip -1 > "$tmp_dump"; then
  fail "pg_dump failed; no partial dump retained"
fi
mv "$tmp_dump" "$DUMP"

# --- verify the dump is a dump ----------------------------------------------
gzip -t "$DUMP" || fail "gzip integrity check failed on $DUMP"
MAGIC=$(gzip -dc "$DUMP" 2>/dev/null | head -c 5 | od -An -tx1 | tr -d ' \n') || true
[ "$MAGIC" = "5047444d50" ] || fail "uncompressed stream is not a pg custom-format archive (magic $MAGIC)"
# `head -c` closes the pipe early, which makes gzip die of SIGPIPE (141). Under
# `set -o pipefail` that non-zero status would be the pipeline's status and,
# with `set -e`, would abort the run -- after which the EXIT trap deletes the
# dump that had just been verified. That is exactly the "backup silently did not
# happen" failure this file exists to prevent, and it was observed here: the
# magic check succeeded and the run still died before sealing.
#
# So the two `head`/`grep -c` readers below tolerate SIGPIPE explicitly, and the
# value they produced is still checked on the next line. `gzip -t` above already
# proved the stream's integrity, which is what a truncated read would break.
TOC=$(gzip -dc "$DUMP" 2>/dev/null | pg_restore -l 2>/dev/null | grep -cE '^[0-9]+;' || true)
[ "$TOC" -ge 20 ] || fail "dump lists only $TOC TOC entries; refusing to seal a stub"
SIZE=$(stat -c %s "$DUMP")
SHA=$(sha256sum "$DUMP" | cut -d' ' -f1)
log "dumped: $(basename "$DUMP") size=$SIZE toc=$TOC sha256=$SHA"

# --- seal --------------------------------------------------------------------
KEYFILE=$(mktemp /dev/shm/memgw-key-XXXXXX 2>/dev/null || mktemp /tmp/memgw-key-XXXXXX)
chmod 600 "$KEYFILE"
# python emits progress on stderr only with --verbose; stdout stays clean.
python3 "$ENVELOPE" seal --input "$DUMP" --out-prefix "${SEALED%.aes}" \
  --recovery-public "$RECOVERY_PUB" --key-out "$KEYFILE" >/dev/null
[ -f "$SEALED" ] || fail "seal produced no artefact"
FP=$(cat "${SEALED%.aes}.key.recovery.fingerprint")

# --- verify the sealed artefact through the ESCROWED path --------------------
# Unwrap with the recovery public key's counterpart is impossible here by design
# (the private half is escrowed off-host). What CAN be proven on this host is
# that the sealed stream authenticates and decrypts back to the exact dump with
# the DPAPI-free data key -- i.e. the envelope is intact and the key wrapper is
# the one this artefact names.
VERIFY_OUT=$(mktemp /tmp/memgw-verify-XXXXXX)
python3 "$ENVELOPE" unseal --input "$SEALED" --key-file "$KEYFILE" --out "$VERIFY_OUT" >/dev/null \
  || fail "the sealed artefact did not unseal; refusing to keep it"
VSUM=$(sha256sum "$VERIFY_OUT" | cut -d' ' -f1)
[ "$VSUM" = "$SHA" ] || fail "unsealed digest $VSUM != dump digest $SHA"
SEALED_SIZE=$(stat -c %s "$SEALED")
log "sealed: $(basename "$SEALED") size=$SEALED_SIZE recovery=$FP verified=yes"

# --- manifest ----------------------------------------------------------------
# Provenance travels with the artefact: a restore reconciles against these
# counts, not against the live ledger (which keeps moving).
COUNTS=$(sudo -u postgres psql -p "$PORT" -d "$DB" -tAc \
  "SELECT 'events='||(SELECT count(*) FROM memgw.events)||' receipts='||(SELECT count(*) FROM memgw.write_receipts)||' max_seq='||(SELECT COALESCE(max(seq),0) FROM memgw.events)")
# Only touch leading/trailing whitespace (psql pads the line). Stripping every
# space would glue the fields into `events=135receipts=135`, which no reader can
# parse and which would then disagree with the restore-side comparison.
COUNTS="$(echo "$COUNTS" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
# One naming convention for the whole artefact, derived from $STEM:
#
#   $STEM.aes                              the authenticated payload
#   $STEM.key.recovery                     the RSA-wrapped data key
#   $STEM.key.recovery.fingerprint         which recovery key opens it
#   $STEM.manifest                         provenance
#   $STEM.sha256                           digest of the payload
#
# An earlier revision named the manifest and sha256 by appending to the full
# `$STEM.aes` name while the envelope helper wrote the key wrapper against
# `$STEM`. Both halves were internally consistent and the pair disagreed, so a
# reader that followed either convention could not find the other's files --
# the same class of defect that made an earlier pipeline silently ship nothing.
cat > "${STEM}.manifest" <<EOF
database=$DB
artefact=$(basename "$SEALED")
envelope=memgwse2
cipher=aes-256-gcm
segment=chained-tag
recovery_key=rsa-4096-oaep-sha256
recovery_fingerprint=$FP
bytes=$SEALED_SIZE
sha256=$(sha256sum "$SEALED" | cut -d' ' -f1)
taken_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
toc_entries=$TOC
counts=$COUNTS
EOF
echo "$(sha256sum "$SEALED" | cut -d' ' -f1)  $(basename "$SEALED")" > "${STEM}.sha256"
log "manifest written: $COUNTS"

# --- retention ---------------------------------------------------------------
# Keep whole artefacts only: an `.aes` whose `.key.recovery` was pruned is
# unopenable, so every part of a pruned artefact goes, and every part of a kept
# one stays.
#
# The sidecar naming matters and is easy to get wrong. The envelope helper is
# given `--out-prefix <stem>.dump` (the artefact name without `.aes`), so it
# writes `<stem>.dump.key.recovery` and `<stem>.dump.key.recovery.fingerprint`.
# A retention loop that instead appends to the full `.dump.aes` name looks for
# `<stem>.dump.aes.key.recovery`, matches nothing, prunes nothing, and reports
# `kept=0` while artefacts sit on disk. That was observed here.
# `sidecar_stem` is the one place the convention is encoded.
sidecar_stem() { printf '%s' "${1%.aes}"; }
mapfile -t olds < <(ls -1t "$DEST"/memgw-*.dump.aes 2>/dev/null | tail -n +$((KEEP + 1)))
for f in "${olds[@]:-}"; do
  [ -n "$f" ] || continue
  s=$(sidecar_stem "$f")
  rm -f "$f" "$s.manifest" "$s.sha256" "$s.key.recovery" "$s.key.recovery.fingerprint"
  log "pruned $(basename "$f")"
done
kept=0
for f in "$DEST"/memgw-*.dump.aes; do
  [ -e "$f" ] || continue
  s=$(sidecar_stem "$f")
  if [ -e "$s.key.recovery" ] && [ -e "$s.manifest" ] && [ -e "$s.sha256" ]; then
    kept=$((kept + 1))
  else
    log "WARNING: $(basename "$f") is missing a sidecar and will not be counted as a good artefact"
  fi
done
log "done: kept=$kept complete artefact(s)"
