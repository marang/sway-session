#!/bin/sh
set -eu

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
profile=${CODEX_APPARMOR_PROFILE:-codex-home-guard}

command -v aa-exec >/dev/null 2>&1
command -v sway-session >/dev/null 2>&1
command -v python3 >/dev/null 2>&1
test -r "$history_file"
test -r "$state_file"
test -S "$herdr_socket"

# Positive path: the confined hook can reach only the narrow broker.
printf '{"hook_event_name":"SessionStart","session_id":"%s"}\n' "$codex_session_id" |
  aa-exec -p "$profile" -- env \
    HERDR_ENV=1 \
    SWAY_SESSION_CONTEXT_ID="$context_id" \
    HERDR_PANE_ID="$pane_id" \
    CODEX_THREAD_ID="$codex_session_id" \
    sway-session report-codex-session

# Negative file paths: neither read nor write may succeed in confinement.
if aa-exec -p "$profile" -- test -r "$history_file"; then
  echo "Codex could read Herdr pane history" >&2
  exit 1
fi
if aa-exec -p "$profile" -- test -r "$state_file"; then
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
  echo "Codex could mutate the broker runtime directory" >&2
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
    raise SystemExit("Codex connected to the general Herdr socket")
finally:
    client.close()
PY
