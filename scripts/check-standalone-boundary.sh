#!/bin/sh
set -eu

module=github.com/marang/sway-session

test "$(go list -m)" = "$module"
test ! -e cmd/sway-title-animator
test ! -e config.example.toml
test ! -e docs/sound-presets-plan.md
test ! -e docs/assets/logo.svg

if grep -R -n --include='*.go' 'github.com/marang/sway-title-animator' .; then
	echo 'Retained Go code still imports the former combined module.' >&2
	exit 1
fi

if find cmd -mindepth 1 -maxdepth 1 -type d ! -name sway-session -print | grep .; then
	echo 'A non-session command remains in the standalone repository.' >&2
	exit 1
fi

packages=$(go list ./...)
if printf '%s\n' "$packages" | grep -F '/cmd/sway-title-animator' >/dev/null; then
	echo 'The animator command remains in the Go package graph.' >&2
	exit 1
fi

for package in \
	internal/agentreport \
	internal/diagnostic \
	internal/herdrinit \
	internal/session \
	internal/sessionrequest \
	internal/statefile \
	internal/swayipc \
	internal/titleindicator; do
	printf '%s\n' "$packages" | grep -Fx "$module/$package" >/dev/null || {
		echo "Required standalone package is missing: $package" >&2
		exit 1
	}
done

test -f internal/titleindicator/testdata/v1.json
test -f internal/titleindicator/wire_contract_test.go
