#!/bin/sh
set -eu

adapter=contrib/codex/report-agent-session.sh
fake_binary=$(pwd)/scripts/testdata/capture-agent-report.sh
context_id=123e4567-e89b-12d3-a456-426614174000
session_id=01a04a4b-7fb9-7a90-8ace-51f7ae68e0ee
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT HUP INT TERM
output=$test_root/report.json

run_managed() {
	env \
		HERDR_ENV=1 \
		SWAY_SESSION_CONTEXT_ID="$context_id" \
		HERDR_PANE_ID=work:p1 \
		CODEX_THREAD_ID="${test_thread_id:-$session_id}" \
		CODEX_HOOK_TEST_OUTPUT="$output" \
		"$adapter" "$fake_binary"
}

assert_not_invoked() {
	if [ -e "$output" ]; then
		echo 'Codex hook unexpectedly invoked sway-session' >&2
		exit 1
	fi
}

printf '{"hook_event_name":"SessionStart","session_id":"%s","transcript_path":"/secret/history","command":["sh","-c","danger"]}\n' "$session_id" |
	run_managed
printf '{"agent":"codex","agent_session_id":"%s"}\n' "$session_id" >"$test_root/expected.json"
cmp "$test_root/expected.json" "$output"

rm -f "$output"
printf '{"hook_event_name":"SessionStart","session_id":"%s"}\n' "$session_id" |
	env -u HERDR_ENV -u CODEX_THREAD_ID CODEX_HOOK_TEST_OUTPUT="$output" "$adapter" "$fake_binary"
assert_not_invoked

printf '{"hook_event_name":"AfterAgent","session_id":"%s"}\n' "$session_id" |
	run_managed
assert_not_invoked

if printf '{"hook_event_name":"SessionStart","session_id":"%s"}\n' "$session_id" |
	env \
		HERDR_ENV=1 \
		SWAY_SESSION_CONTEXT_ID="$context_id" \
		HERDR_PANE_ID=work:p1 \
		CODEX_THREAD_ID=223e4567-e89b-42d3-a456-426614174000 \
		CODEX_HOOK_TEST_OUTPUT="$output" \
		"$adapter" "$fake_binary" 2>/dev/null; then
	echo 'Codex hook accepted a CODEX_THREAD_ID mismatch' >&2
	exit 1
fi
assert_not_invoked

for invalid_id in \
	00000000-0000-0000-0000-000000000000 \
	01A04A4B-7FB9-7A90-8ACE-51F7AE68E0EE \
	not-a-uuid; do
	if printf '{"hook_event_name":"SessionStart","session_id":"%s"}\n' "$invalid_id" |
		test_thread_id=$invalid_id run_managed 2>/dev/null; then
		echo "Codex hook accepted invalid session ID: $invalid_id" >&2
		exit 1
	fi
	assert_not_invoked
done

if {
	printf '{"hook_event_name":"SessionStart","session_id":"%s","padding":"' "$session_id"
	dd if=/dev/zero bs=16384 count=1 2>/dev/null | tr '\0' x
	printf '"}\n'
} | run_managed 2>/dev/null; then
	echo 'Codex hook accepted oversized input' >&2
	exit 1
fi
assert_not_invoked

if printf '{"hook_event_name":"SessionStart","session_id":"%s"}\n{}\n' "$session_id" |
	run_managed 2>/dev/null; then
	echo 'Codex hook accepted multiple JSON values' >&2
	exit 1
fi
assert_not_invoked

if printf '{"hook_event_name":"SessionStart","session_id":"%s"}\n' "$session_id" |
	env HERDR_ENV=1 CODEX_HOOK_TEST_OUTPUT="$output" "$adapter" relative/sway-session 2>/dev/null; then
	echo 'Codex hook accepted a relative sway-session path' >&2
	exit 1
fi
assert_not_invoked
