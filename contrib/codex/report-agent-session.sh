#!/bin/sh
set -eu

max_hook_payload=16384

fail() {
	printf '%s\n' "sway-session Codex hook: $*" >&2
	exit 1
}

if [ "$#" -ne 1 ]; then
	fail 'expected one absolute sway-session binary path'
fi

session_binary=$1
case $session_binary in
	/*) ;;
	*) fail 'sway-session binary path must be absolute' ;;
esac

command -v jq >/dev/null 2>&1 || fail 'jq is required by the Codex hook integration'

umask 077
input_file=$(mktemp /tmp/sway-session-codex-hook.XXXXXX) || fail 'could not create bounded input buffer'
trap 'rm -f "$input_file"' EXIT HUP INT TERM

if ! dd bs=1 count=$((max_hook_payload + 1)) of="$input_file" 2>/dev/null; then
	fail 'could not read Codex hook input'
fi
input_size=$(wc -c <"$input_file")
if [ "$input_size" -gt "$max_hook_payload" ]; then
	fail "hook input exceeds $max_hook_payload bytes"
fi

# Slurping lets the adapter reject trailing JSON values. Unknown object fields
# remain provider-owned and are deliberately discarded at this edge.
if ! parsed=$(jq -er --slurp '
  if length != 1 then error("expected one JSON value")
  elif (.[0] != null and (.[0] | type) != "object") then error("expected a JSON object")
  elif (((.[0].hook_event_name // "") | type) != "string") then error("hook_event_name must be a string")
  elif (((.[0].session_id // "") | type) != "string") then error("session_id must be a string")
  else [(.[0].hook_event_name // ""), (.[0].session_id // "")] | @tsv
  end
' "$input_file"); then
	fail 'invalid Codex hook JSON'
fi

tab=$(printf '\t')
hook_event=${parsed%%"$tab"*}
session_id=${parsed#*"$tab"}

# Codex can run this installed hook outside a Herdr-managed pane. Matching the
# former boundary, unrelated events and unmanaged sessions are silent no-ops.
[ "$hook_event" = SessionStart ] || exit 0
[ "${HERDR_ENV:-}" = 1 ] || exit 0

if ! printf '%s\n' "$session_id" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'; then
	fail 'session_id must be a canonical lowercase UUID'
fi
if [ "$session_id" = 00000000-0000-0000-0000-000000000000 ]; then
	fail 'session_id must not be the nil UUID'
fi
if [ -n "${CODEX_THREAD_ID:-}" ] && [ "$CODEX_THREAD_ID" != "$session_id" ]; then
	fail 'session_id does not match CODEX_THREAD_ID'
fi
if [ ! -x "$session_binary" ]; then
	fail "sway-session binary is not executable: $session_binary"
fi

jq -cn --arg session_id "$session_id" \
	'{agent:"codex",agent_session_id:$session_id}' |
	"$session_binary" report-agent-session
