#!/bin/sh
# memgw lifecycle adapter for Hermes Agent.
#
# Hermes hooks are shell scripts, so this fragment is a shell script. It builds
# the canonical hook object and pipes it to the binary, which does everything
# else. It adds no dependency: sh, and the binary.
#
# Install: copy this file where the Hermes configuration points its
# on_session_start / on_session_end hooks, and make it executable. Hermes keys
# its approval allowlist on the script's mtime, so re-approve after any edit.
# Nothing here upgrades or patches Hermes; the v0.18.0 pin protecting the local
# adapter.py patch is untouched.
#
# Hermes runs on Linux, and the collector is a Windows service. There is
# therefore NO local durable queue on this host: events go straight to the
# gateway, and a lifecycle event that fires while the gateway is unreachable is
# lost. That is a real gap, not a configuration mistake.
#
# Usage, from the Hermes hook that fires:
#   memgw-hook.sh session_start  "$SESSION_ID"
#   memgw-hook.sh session_end    "$SESSION_ID"
#   memgw-hook.sh session_compact "$SESSION_ID"
#
# Kill switch: MEMGW_ADAPTER=0. Configuration: MEMGW_ADAPTER_CONFIG.

set -eu

[ "${MEMGW_ADAPTER:-1}" = "0" ] && exit 0
[ -n "${MEMGW_ADAPTER_CONFIG:-}" ] || exit 0

BIN="${MEMGW_BIN:-enowx-rag}"
EVENT="${1:-}"
SESSION="${2:-}"

[ -n "$EVENT" ] || { echo "memgw adapter: no event named" >&2; exit 2; }
[ -n "$SESSION" ] || { echo "memgw adapter: no session id" >&2; exit 2; }

case "$EVENT" in
  session_start|session_resume|session_compact|session_end) ;;
  *) echo "memgw adapter: $EVENT is not a lifecycle this fragment sends" >&2; exit 2 ;;
esac

# json_escape keeps a session id with a quote or a backslash in it from
# producing a payload the adapter would refuse as malformed.
json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

payload=$(printf '{"host":"hermes","native_event":"%s","event":"%s","session_id":"%s","cwd":"%s"}' \
  "$(json_escape "$EVENT")" \
  "$(json_escape "$EVENT")" \
  "$(json_escape "$SESSION")" \
  "$(json_escape "$PWD")")

# A hook must not hang a session. The binary has its own delivery timeout; this
# is the backstop for the case where it cannot start at all.
printf '%s' "$payload" | "$BIN" memgw adapter --host hermes || {
  echo "memgw adapter: this lifecycle event was not recorded" >&2
  exit 0
}
