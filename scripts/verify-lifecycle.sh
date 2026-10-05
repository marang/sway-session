#!/bin/sh
# Development-only matrix; every fixture owns its disposable state/processes.
set -eu

case "${1-}" in
  '') headless=0 ;;
  --headless) headless=1 ;;
  *) echo 'usage: sh scripts/verify-lifecycle.sh [--headless]' >&2; exit 2 ;;
esac
if [ "$#" -gt 1 ]; then
  echo 'usage: sh scripts/verify-lifecycle.sh [--headless]' >&2
  exit 2
fi
cd "$(dirname "$0")/.."
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.26.5}"
# Do not accidentally enable private compositor checks in the simulated stage.
export SWAY_SESSION_HEADLESS_INTEGRATION=0

if [ "$headless" -eq 1 ]; then
  for tool in go sway alacritty sleep herdr bash; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "required private-compositor tool unavailable: $tool" >&2
      exit 1
    fi
  done
fi

printf '%s\n' 'Lifecycle matrix: deterministic events and disposable SQLite/process fixtures'
go test -race -p 1 ./cmd/sway-session \
  -run '^Test(Scratchpad|ObservedTerminalClose|FollowApplication|ApplicationClose|ApplicationLaunch|RuntimeAdoption|RuntimeRetriesFailedAdoption|RuntimeFreshCloseConfirmation|RuntimeCancelledCleanup|SessionRuntime|LifecycleFeedback|LifecycleSuspended|LifecycleReservation|DaemonPurge|TerminalCommand|RestorePreview|RestoreReusesExisting|RestoreLaunchesMissing)' \
  -count=1 -timeout=5m
go test -race -p 1 ./internal/session \
  -run '^Test(Scratchpad|LayoutMigration|ApplicationAdoption|Adopted|PersistedAdoption|RejectedRetryRearmsAdoption|LifecycleCrash|TerminalPurge|LayoutAcceptance|RestorePolicyMatches|RegistryWithTerminalCreation|TerminalContextAndCreation|EphemeralTerminalDoesNotExpose|PersistentTerminalDoesNotExpose|ExecProcessStarter)' \
  -count=1 -timeout=5m
go test -race -p 1 ./internal/shutdownwatch ./internal/swayipc \
  -count=1 -timeout=2m

if [ "$headless" -eq 1 ]; then
  printf '%s\n' 'Lifecycle matrix: real private Sway IPC/processes, workspaces 98+'
  SWAY_SESSION_HEADLESS_INTEGRATION=1 go test -race -p 1 ./cmd/sway-session \
    -run '^Test(ApplicationAdoptionDaemonRestartHeadless|SessionRuntime(Scratchpad|RestoreFocus|RestoreColdStartFocus|RestoreCleanup|LayoutShapes|LateApplication|ApplicationLaunch|StartupPrompt|StartupPromptApplication)Headless|FollowApplicationShutdownHeadless|LifecycleHeadlessApplicationRecovery|DaemonExecutableReplacementPreservesWorkHeadless|TerminalLifecycle.*Headless|SessionStart.*Headless)$' \
    -count=1 -timeout=10m -v
else
  printf '%s\n' 'Private compositor: not run (use --headless)'
fi
printf '%s\n' 'Actual VM reboot/logind ordering: not run by this runner; see docs/follow-application-vm-check.md'
