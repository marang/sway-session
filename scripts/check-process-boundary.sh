#!/bin/sh
set -eu

animator=./cmd/sway-title-animator
module=github.com/marang/sway-title-animator

if [ -e ./cmd/sway-herdr-init ]; then
	echo 'Standalone sway-herdr-init command must remain retired' >&2
	exit 1
fi

for removed in session_state.go session_state_test.go codex_broker.go codex_broker_test.go; do
	if [ -e "$animator/$removed" ]; then
		echo "Session runtime remains under sway-title-animator: $animator/$removed" >&2
		exit 1
	fi
done

for package in internal/session internal/sessionrequest internal/codexreport internal/statefile internal/herdrinit; do
	if go list -deps "$animator" | grep -Fqx "$module/$package"; then
		echo "sway-title-animator depends on forbidden session package: $package" >&2
		exit 1
	fi
done

if find "$animator" -type f -name '*.go' ! -name '*_test.go' -print0 |
	xargs -0 grep -En 'contexts\.json|layout\.json|session-start\.sock|codex-report\.sock|internal/(session|sessionrequest|codexreport|statefile|herdrinit)' >/dev/null; then
	echo "sway-title-animator production code names a session state file, socket, or package" >&2
	exit 1
fi

for moved in daemon.go daemon_runtime.go codex_broker.go; do
	if [ ! -f "cmd/sway-session/$moved" ]; then
		echo "Dedicated sway-session runtime component is missing: cmd/sway-session/$moved" >&2
		exit 1
	fi
done

grep -Fq '"daemon"' cmd/sway-session/main.go || {
	echo 'sway-session daemon command is not wired into the CLI' >&2
	exit 1
}

grep -Fq 'resolveProgram:  sessionstate.ResolveRootOwnedSystemExecutable' cmd/sway-session/broker.go || {
	echo 'session request broker must resolve Herdr from root-owned system paths' >&2
	exit 1
}
