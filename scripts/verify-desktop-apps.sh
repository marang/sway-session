#!/bin/sh
# Explicit real desktop acceptance; ordinary verification never starts apps.
set -eu

if [ "$#" -ne 1 ]; then
  echo 'usage: sh scripts/verify-desktop-apps.sh --chrome|--slack|--all' >&2
  exit 2
fi
case "$1" in
  --chrome) cases='ChromePrivate' ;;
  --slack) cases='SlackFlatpakPrivate' ;;
  --all) cases='(ChromePrivate|SlackFlatpakPrivate)' ;;
  *) echo 'usage: sh scripts/verify-desktop-apps.sh --chrome|--slack|--all' >&2; exit 2 ;;
esac

cd "$(dirname "$0")/.."
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.26.5}"
export SWAY_SESSION_DESKTOP_ACCEPTANCE=1
export SWAY_SESSION_HEADLESS_INTEGRATION=0
go test -race -p 1 ./cmd/sway-session \
  -run "^TestDesktopAcceptance${cases}\$" -count=1 -timeout=10m -v
