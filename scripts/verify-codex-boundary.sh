#!/bin/sh
set -eu

# The live boundary test must not trust user-writable command resolution.
PATH=/usr/bin
export PATH

if [ "$#" -ne 6 ]; then
  echo "Usage: $0 CONTEXT_UUID PANE_ID CODEX_SESSION_UUID HERDR_HISTORY STATE_FILE HERDR_SOCKET" >&2
  exit 2
fi

context_id=$1
pane_id=$2
codex_session_id=$3
history_file=$4
state_file=$5
herdr_socket=$6
profile=${AGENT_APPARMOR_PROFILE:-${CODEX_APPARMOR_PROFILE:-agent-home-guard}}
session_binary=/usr/bin/sway-session
hook_binary=/usr/lib/sway-session/codex-report-agent-session

require_packaged_binary() {
  binary=$1

  if [ ! -f "$binary" ] || [ -L "$binary" ]; then
    echo "Required packaged binary is not a regular non-symlink file: $binary" >&2
    exit 1
  fi
  metadata=$(stat -Lc '%u:%g:%a' "$binary")
  if [ "$metadata" != '0:0:755' ]; then
    echo "Required packaged binary has unsafe ownership or mode: $binary ($metadata, want 0:0:755)" >&2
    exit 1
  fi

  if command -v pacman >/dev/null 2>&1; then
    package=$(pacman -Qqo -- "$binary" 2>/dev/null || true)
  elif command -v dpkg-query >/dev/null 2>&1; then
    package=$(dpkg-query -S "$binary" 2>/dev/null | awk -F: 'NR == 1 { print $1 }' || true)
  elif command -v rpm >/dev/null 2>&1; then
    package=$(rpm -qf --qf '%{NAME}\n' "$binary" 2>/dev/null || true)
  else
    echo "Cannot verify package ownership for $binary: no supported package database" >&2
    exit 1
  fi
  if [ "$package" != 'sway-session' ]; then
    echo "Required binary is not owned by the sway-session package: $binary" >&2
    exit 1
  fi
}

command -v aa-exec >/dev/null 2>&1
command -v python3 >/dev/null 2>&1
test -r "$history_file"
test -r "$state_file"
test -S "$herdr_socket"
require_packaged_binary "$session_binary"
require_packaged_binary "$hook_binary"

# Positive path: the confined hook can reach only the narrow broker.
printf '{"hook_event_name":"SessionStart","session_id":"%s"}\n' "$codex_session_id" |
  aa-exec -p "$profile" -- env \
    HERDR_ENV=1 \
    SWAY_SESSION_CONTEXT_ID="$context_id" \
    HERDR_PANE_ID="$pane_id" \
    CODEX_THREAD_ID="$codex_session_id" \
    "$hook_binary" "$session_binary"

# Negative file paths: neither read nor write may succeed in confinement.
if aa-exec -p "$profile" -- sh -c 'exec 3<"$1"' sh "$history_file" 2>/dev/null; then
  echo "Codex could read Herdr pane history" >&2
  exit 1
fi
if aa-exec -p "$profile" -- sh -c 'exec 3<"$1"' sh "$state_file" 2>/dev/null; then
  echo "Codex could read sway-session state" >&2
  exit 1
fi
history_probe="$history_file.apparmor-probe-$$"
if aa-exec -p "$profile" -- sh -c 'printf probe > "$1"' sh "$history_probe" 2>/dev/null; then
  rm -f "$history_probe"
  echo "Codex could create files beside Herdr pane history" >&2
  exit 1
fi
state_probe="$state_file.apparmor-probe-$$"
if aa-exec -p "$profile" -- sh -c 'printf probe > "$1"' sh "$state_probe" 2>/dev/null; then
  rm -f "$state_probe"
  echo "Codex could create files beside sway-session state" >&2
  exit 1
fi

runtime_probe="${XDG_RUNTIME_DIR:?}/sway-session/.apparmor-probe-$$"
if aa-exec -p "$profile" -- sh -c 'printf probe > "$1"' sh "$runtime_probe" 2>/dev/null; then
  rm -f "$runtime_probe"
  echo "Codex could create a broker-runtime sibling; runtime pathname mutation remains an unresolved boundary requirement" >&2
  exit 1
fi

# Require an AppArmor denial, not merely an idle or absent Herdr server.
aa-exec -p "$profile" -- python3 - "$herdr_socket" <<'PY'
import errno
import socket
import sys

client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
try:
    client.connect(sys.argv[1])
except OSError as error:
    if error.errno in (errno.EACCES, errno.EPERM):
        raise SystemExit(0)
    raise
else:
    raise SystemExit("Codex connected to the general Herdr socket; pathname connect mediation remains an unresolved LAB-89 requirement")
finally:
    client.close()
PY
