#!/bin/sh
set -eu

[ "$#" -eq 1 ]
[ "$1" = report-agent-session ]
: "${CODEX_HOOK_TEST_OUTPUT:?}"

cat >"$CODEX_HOOK_TEST_OUTPUT"
