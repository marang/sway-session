#!/bin/sh
set -eu

policy=${1:-contrib/apparmor/codex-home-guard}

if command -v apparmor_parser >/dev/null 2>&1; then
  apparmor_parser -Q -T "$policy"
fi

require_rule() {
  if ! grep -F "$1" "$policy" >/dev/null; then
    echo "Missing AppArmor boundary rule: $1" >&2
    exit 1
  fi
}

require_count() {
  expected=$1
  rule=$2
  actual=$(grep -F -c "$rule" "$policy" || true)
  if [ "$actual" -ne "$expected" ]; then
    echo "Unexpected AppArmor rule count for '$rule': got $actual, want $expected" >&2
    exit 1
  fi
}

reject_rule() {
  if grep -F "$1" "$policy" >/dev/null; then
    echo "Forbidden AppArmor rule fragment: $1" >&2
    exit 1
  fi
}

require_rule 'audit deny @{HOME}/.config/herdr/{,**} mrwkl,'
require_rule 'audit deny @{HOME}/.local/ w,'
require_rule 'audit deny @{HOME}/.local/state/ w,'
require_rule 'audit deny @{HOME}/.local/state/sway-session/{,**} mrwkl,'
require_rule 'audit deny @{run}user/[0-9]*/sway-ipc.*.sock mrwkl,'
require_rule 'audit deny @{run}user/[0-9]*/sway-session/ wkl,'
require_rule 'audit deny @{run}user/[0-9]*/sway-session/app-operations/{,**} mrwkl,'
require_rule 'audit deny @{run}user/[0-9]*/sway-session/codex-report.sock klm,'
require_rule 'owner @{run}user/[0-9]*/sway-session/codex-report.sock rw,'
require_rule 'audit deny @{run}user/[0-9]*/sway-session/agent-report.sock klm,'
require_rule 'owner @{run}user/[0-9]*/sway-session/agent-report.sock rw,'
require_rule 'audit deny @{run}user/[0-9]*/sway-session/session-start.sock klm,'
require_rule 'owner @{run}user/[0-9]*/sway-session/session-start.sock rw,'
reject_rule 'sway-herdr-init'
require_count 0 'audit deny @{HOME}/.config/gh/{,**} mrwkl,'
reject_rule '@{run}/'
