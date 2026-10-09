# Sway Session verification

This document separates the repeatable automated gate from live compositor,
packaging, migration, and security checks. Never report a live check that was
not run.

## Automated gate

Run:

~~~sh
make verify
~~~

The gate covers:

- gofmt;
- uncached unit tests;
- race-detector tests;
- go vet and staticcheck;
- CGO-disabled production build;
- AppArmor policy structure;
- Bash, Zsh, and Fish completion behavior;
- standalone source and package boundaries;
- GoReleaser, Arch, install-path, and release-workflow metadata;
- exact release-tag and publication-workflow regressions;
- git whitespace checks; and
- the version-1 title-indicator golden wire fixture.

CI additionally runs GoReleaser configuration validation. A release candidate
must also run a clean GoReleaser snapshot and inspect every archive, DEB, and
RPM so only sway-session and the documented integration assets are present.

`make packaging-check` runs the complete Makefile install target in disposable
build directories with private installation prefixes. It covers plain paths,
prefixes containing spaces, and an independently overridden `DOC_ROOT` with
spaces. Source inputs are linked from the checkout; generated binaries
and installed files stay in the disposable roots. The checks compare every
installed file, its contents and mode, all destination directories, and the
absence of paths accidentally created by shell word splitting. They use no
privilege escalation or production installation directory.

`make release-gate-check` runs isolated local Git fixtures, cross-workflow
publication contracts, and the actual GitHub release rerun script against a
mock API. The actual AUR version guards, publication step and standalone
metadata-sync step also run against private file-only Git remotes and a fake
GitHub CLI. They check stale reruns, unchanged reruns, accepted version and
package-revision updates, empty bootstrap repositories and malformed metadata;
PKGBUILD remains unexecuted input data. These developer checks require Python 3,
PyYAML (`python-yaml` on Arch, `python3-yaml` on Ubuntu), and Node.js. They do not
publish or access live session state. CI installs PyYAML and uses the hosted
runner's Node.js.

LAB-281 additionally exercised the production `needs`/commit comparisons on
GitHub with read-only placeholder jobs: failed, skipped, missing and wrong-SHA
verification blocked both publishers; an accepted SHA allowed both. While
verification was running neither publisher started, and cancelling the run
left both unexecuted. Full reruns retained those outcomes. A separate disabled
workflow rejected full, failed-job and single-job reruns with HTTP 403. These
are GitHub orchestration checks, not real release or AUR publication evidence.
The historical publisher files remain as inert retirement placeholders so
their workflow IDs can stay explicitly disabled. GitHub's `deleted` state did
not reject an isolated, skipped historical job rerun; deleting workflow files
therefore does not establish the rerun barrier. Validate the retired IDs with
new placeholder runs whose jobs cannot execute, as described in
[the publication gate](releasing.md#automated-publication-gate).

### Integrated review-fix acceptance (LAB-278, LAB-284)

On 2026-10-09, the five review fixes and the three confirmed checkpoint
follow-ups were tested together at source commit
`edb2f34fb9caece9a756ab7969438b2775123b6d`. The environment was Arch Linux
x86_64, kernel `7.2.8-arch1-2`, Go `1.26.5` (module `go 1.26.0`, toolchain
`go1.26.5`) and private Sway `1.12` where required. The independently exercised
CGO-free CLI identified itself as `dev`, that exact commit and
`modified=false`; its SHA-256 was
`ed7e1c41f52114bbd0d65523a7e1da43e864f35671ad0143ca159a892bf64843`.

| Scenario | Observed evidence | Status |
| --- | --- | --- |
| R1 and initialization follow-up | Real management, restore and initialization subprocesses with pipe-holding descendants, plus persistent starter lifecycle, passed five race-detector repetitions. Owned supervisors cleaned up their descendants; captured-pipe bounds did not become process-group termination. | Passed |
| R2 established Unix exchanges | Session-start, agent-report and Herdr endpoint cases passed 20 race-detector repetitions per package, including accepted mutations, early cancel, deadlines, complete success, ordinary transport failures, response guards and response/cancel races. No automatic second request was sent. CLI SIGINT/SIGTERM cases passed ten repetitions. | Passed |
| R3 publication, explicit tag and stale metadata | The actual local resolver, source transformation, published-release rerun guard, AUR version guard and metadata-sync scripts passed all 51 release-gate tests with private Git remotes/mock APIs. A fresh actual GoReleaser 2.18.2 build with Go 1.26.5 in a private dual-tag fixture reproduced default tag v1.2.4 and selected v1.2.3 when the production override was supplied; binary and metadata identities agreed. Fresh GitHub scheduler and retired-ID observations are recorded below. | Passed |
| R4 installation | Three actual private `make install` cases passed: ordinary prefix, prefix with spaces and separate documentation root with spaces. Every installed byte, file/directory mode and destination matched the complete manifest; no split paths appeared. | Passed |
| R5 repair guidance | An independent validator ran ten actual text/JSON CLI scenarios and real NO_COLOR PTYs at 80×24 and 48×16 against private Sway, workspace 98, using ordinary multi-file configuration with inert startup declarations. Preview, confirmation, original 0600 backups, fall-dependent guidance, full result scrolling and recheck passed. Exec traces, loaded-config facts and private runtime/state comparisons showed no daemon/restore execution or compositor reload. | Passed |

The complete `GOTOOLCHAIN=go1.26.5 make verify` gate passed at that source.
[Main push CI](https://github.com/marang/sway-session/actions/runs/37931503241)
also passed and identified `verify.yml` at the same SHA as its reusable
workflow. Exact checkout, full verification, GoReleaser configuration check and
verified-commit output steps all succeeded. Separate repetition commands were:

```sh
GOTOOLCHAIN=go1.26.5 go test -race ./internal/session ./internal/sessionrequest ./internal/herdrinit \
  -run '^Test(ExecCommandRunnerProcess|ExecRestoreRunnerProcess|ExecRunnerProcess|ExecProcessStarterLifecycle)$' -count=5 -timeout=4m
GOTOOLCHAIN=go1.26.5 go test -race ./internal/sessionrequest ./internal/agentreport ./internal/session \
  -run '^Test(Send|HerdrEndpoint)' -count=20 -timeout=4m
GOTOOLCHAIN=go1.26.5 go test -race ./cmd/sway-session \
  -run '^TestRequestStartSignalCancellation$' -count=10 -timeout=3m
GOTOOLCHAIN=go1.26.5 python3 -B scripts/test_install.py -v
python3 -B -m unittest discover -s scripts/release-gate -p 'test_*.py' -v
```

Fresh GitHub probes copied production `needs`/`if` and both reusable verifier
output mappings from the named source into credential-free marker workflows
at fixture commit `2df5cb93c0254abf4177b0bcf6ad0e50847547d3`.
[The matrix](https://github.com/marang/sway-session/actions/runs/37932261654)
passed its assertions in attempts 1 and 2: successful exact-SHA verification
ran all four build/publish/sync markers; failed, skipped, missing-output and
wrong-SHA verification left every downstream job skipped with no executed
steps. [The hold probe](https://github.com/marang/sway-session/actions/runs/37932261632)
twice showed no downstream start while both verifiers ran. Unique job-level
concurrency kickers cancelled only those verifier jobs; assertions still ran
and all four downstream jobs stayed skipped. Synthetic failed/cancelled jobs
make aggregate probe conclusions red/cancelled by design; the exact job and
step observations establish the expected barriers, not an aggregate green run.
These probes exercised GitHub scheduling, not actual publication.

At `2026-10-09T12:51:59Z`, fresh observations of the unchanged historical inert
artifacts (`add83a40b36e2f7ab13562acde99621e17c179bd`) found both original
publisher IDs `disabled_manually`, all six full/failed-job/single-job rerun
requests rejected with HTTP 403, and no queued or running executions. The
original inert artifact identity remains separate from this acceptance source.

The required current-state architecture checkpoint sampled these transport,
process, state/transaction, lifecycle and Doctor boundaries plus publication
workflows. It found no further evidence-backed defect; independent focused
race checks of 29 transport/lock/repair groups also passed. This was a bounded
checkpoint, not an exhaustive repository audit.

Not run: production publication/AUR writes, workstation installation or login,
physical shortcut execution, real daemon/restore startup, production Chrome/
Slack, loaded AppArmor enforcement, or a new release-candidate archive/DEB/RPM
and package-manager transition matrix. They are outside this review package;
earlier release, login and VM evidence retains its own source/artifact identity.
Private install checks built disposable binaries; the named candidate above
belongs to the independent executable acceptance. Client cancellation does
not establish that an accepted remote mutation was undone. The parent ticket
[LAB-278](https://linear.app/riotbox/issue/LAB-278/codebase-review-2026-10-07-abbruchverhalten-release-gates-und)
owns the consolidated result and the deliberately excluded checks.

## Management subprocess pipe draining (LAB-279, LAB-315)

The production Herdr command runner, fixed Herdr initialization runner and
trusted restore subprocess allow 250 ms for output-pipe draining after context
cancellation or direct-process exit. Their existing output limits remain
separate memory bounds. A descendant holding an inherited pipe cannot keep an
otherwise finished command waiting until that descendant exits.

The runners retain `os/exec` error classification: a killed or unsuccessful
direct process normally returns `*exec.ExitError`; a successful direct process
whose pipes exceed the drain budget returns `exec.ErrWaitDelay`. Existing
wrappers preserve these errors. Closing the pipes does not establish whether
the requested management action took effect, so it does not trigger a retry.

`CommandContext` owns its direct process. The runners do not signal process
groups or reap arbitrary descendants; a descendant writing afterward may
encounter a closed pipe. Detached terminal and desktop starts continue to use
the separate asynchronous `ExecProcessStarter` lifetime.

```sh
GOTOOLCHAIN=go1.26.5 go test -race ./internal/session ./internal/sessionrequest ./internal/herdrinit \
  -run '^Test(ExecCommandRunnerProcess|ExecRestoreRunnerProcess|ExecRunnerProcess|ExecProcessStarterLifecycle)$' \
  -count=1
```

These tests exercise the production runners with real pipe-holding descendants,
early cancellation, deadlines, successful parent exit, ordinary success and
failed exit, and output limits. Isolated helper supervisors clean up and reap
only their owned descendants. The launcher check also verifies detached
process survival after the short-lived CLI returns.

## Established Unix exchange cancellation (LAB-280)

Session-start, agent-report and Herdr API clients close their established
connection when the request context is canceled. The cancellation callback is
unregistered when the exchange finishes. Cancellation-induced I/O errors retain
`context.Canceled`; network deadlines derived from the context retain
`context.DeadlineExceeded` even if the socket timer fires first. Ordinary
transport and response-validation errors retain their existing handling.

A fully received and validated response can win a race with cancellation.
Closing a client connection does not prove that an accepted server action did
not happen. These clients do not repeat an exchange automatically and do not
introduce a remote cancellation protocol.

```sh
GOTOOLCHAIN=go1.26.5 go test -race \
  ./internal/sessionrequest ./internal/agentreport ./internal/session ./cmd/sway-session \
  -run '^Test(Send|HerdrEndpoint|RequestStartSignalCancellation)' -count=1
```

Private Unix servers exercise cancellation after accepting a mutating request,
deadlines, success, ordinary disconnects, response guards and response/cancel
races. Each canceled accepted request is sent once. An isolated CLI helper runs
the actual `main` signal context and verifies both SIGINT and SIGTERM return the
existing operational-failure exit code while the broker holds its response,
instead of waiting for the 100-second default deadline. Test sockets and
processes use disposable roots; no production broker or Herdr session is used.

## Executable lifecycle scenario matrix (LAB-134)

Run the deterministic matrix with the declared toolchain:

```sh
GOTOOLCHAIN=go1.26.5 make lifecycle-check
```

The development-only [runner](../scripts/verify-lifecycle.sh) reuses existing
runtime, storage, transport and shutdownwatch suites under the race detector.
It runs each package with bounded deadlines and does not need Sway or Herdr.
`make verify` remains the full required gate; this focused runner complements it.

To include actual private-compositor/process checks:

```sh
GOTOOLCHAIN=go1.26.5 sh scripts/verify-lifecycle.sh --headless
```

The explicit headless mode requires `go`, `sway`, `alacritty` and `sleep`; a
missing tool fails the runner rather than silently satisfying live acceptance.
The shared-workspace broker cases also require `herdr` and `bash`. These are
development tools, not new runtime package dependencies. Each live
fixture starts its own compositor and uses disposable XDG/config/state roots,
an environment allowlist and workspaces 98 or higher. Cleanup signals only
fixture-owned processes. No production daemon, Herdr server, provider session
or workstation compositor is stopped.

Test names below are the evidence sources, not claims of VM reboot coverage.
Most runtime tests are in `cmd/sway-session`; storage tests are in
`internal/session`, and inhibitor tests are in `internal/shutdownwatch`.

| Scenario | Required observable invariant | Automated evidence |
| --- | --- | --- |
| Create, map and reuse | Stable context is mapped once; an occupied agent is not restarted | `TestTerminalCommandCreatesThenReusesOneAgentAddressableDefault`, `TestTerminalCommandProjectRecoversMissingWindowWithoutRestartingOccupiedAgent`, `TestRestoreLaunchesMissingContextWithTypedArgumentsAndWaitsForMapping` (injected process boundary) |
| Pending process transition after one accepted start | Fresh process observations must settle before window effects; persistent/pre-start ambiguity and cancellation preserve recovery identity without another launch | `TestTerminalManagerWaitsForTransientPendingAmbiguityAfterStart`, `TestTerminalManagerPendingAmbiguityPreservesContextWithoutEffects`, `TestTerminalManagerRejectsPendingAmbiguityBeforeOwnStart`, `TestTerminalManagerDoesNotRestartWhenAmbiguousPendingProcessesDisappear` (Go-only injected whole-open observations) |
| Healthy close and reopen | Fresh absence after grace archives; explicit activation and a new map reuse the context | Private-Sway `TestTerminalLifecycleHealthyCloseReopenHeadless`; ordinary-close assertions in `TestObservedTerminalCloseArchivesAfterGraceAndFreshAbsence` |
| New window during grace | Fresh presence cancels pending archival, including ambiguous duplicate identity | Private-Sway terminal lifecycle cases; `TestObservedTerminalCloseDropsReopenedTerminal`, `TestObservedTerminalCloseDropsAmbiguousReopenedIdentity` |
| Daemon replacement and restart | Existing container IDs, adapter PIDs, foreground process start identities and eligibility survive; second daemon reports lock conflict; repeated restore reuses mapped windows | Private-Sway `TestDaemonExecutableReplacementPreservesWorkHeadless` |
| Already missing at startup | Absence alone does not archive an active context | Private-Sway terminal lifecycle and executable-replacement cases; `TestSessionRuntimeStartupAndShutdownGuardsKeepPreviousLayout` checks layout preservation |
| Disconnect and shutdown | Old close evidence is invalidated; partial observations do not authorize mutations | `TestObservedTerminalCloseDropsOnStreamDisconnectAndShutdown`, `TestSessionRuntimeStopsMutatingWhenStreamDisconnectsDuringReconcile`; `internal/swayipc` event-stream transport cases |
| Preparation and inhibitor lifetime | Unsafe state is visible before inhibitor release; interrupted acquisition cannot re-enable the guard | `TestPreparationSignalsDisableBeforeReleasingInhibitor`, `TestPreparationInterruptsInFlightRearmAndReleasesNewInhibitor`, `TestSignalDuringStartupCannotEnableMonitor` (injected logind) |
| Logind identity resolution during startup | Unrelated logouts do not lose the guard; own-session removal, untrusted signals and excessive churn reject startup without leaking resources | `TestSessionRemovalDuringStartupIsAttributedToOwnSession`, `TestStartupSessionRemovalTrackingIsBounded`, `TestStartupSessionRemovalRejectsUntrustedSignals`, `TestConcurrentSessionRemovalAndStartupIdentityResolution`; KVM acceptance also requires the actual daemon PID's delay inhibitor after both reboots |
| Follow absence during unsafe lifecycle | Saved desired-open intent survives uncertain/shutdown absence; a new healthy close remains authoritative | `TestFollowApplicationShutdownPreservesDesiredOpen`, `TestFollowApplicationUncertainLifecycleRequiresNewHealthyPresence`; private-Sway `TestFollowApplicationShutdownHeadless` |
| App appears during Prepare | Fresh presence prevents a duplicate start and attempt; an absent control still launches | `TestApplicationLaunchMapsDuringPreparation`, `TestApplicationLaunchMapsWhileWaitingForRegistryLock`; private-Sway `TestSessionRuntimeApplicationLaunchHeadless` |
| Multi-pass restore and own focus | Own move/focus feedback does not cancel remaining work; restored structure and focus converge across workspaces | Deterministic `TestSessionRuntimeLifecycleFeedbackSequence` cases and `TestSessionRuntimeOwnStagingFocusContinuesReconstruction`; private-Sway `TestSessionRuntimeRestoreFocusHeadless` |
| User cancellation and staged cleanup | Return only owned staged windows, remove temporary marks, retain later user placement and retry after reconnect | Deterministic feedback scenarios; `TestSessionRuntimeCleanupPreservesUserMovedWindow`, `TestSessionRuntimeCleanupWaitsForFreshConnection`; private-Sway `TestSessionRuntimeRestoreCleanupHeadless` |
| Committed snapshot restart | Fresh store/runtime observation uses committed eligibility and layouts; archived contexts remain ineligible | `TestLayoutAcceptanceSelectsRestoreFromReloadedExactSnapshot`, `TestLayoutAcceptanceMixedArchivedWindowKeepsPlacementOnlyAfterReload`, `TestRestorePolicyMatchesActiveLayoutMembership`; executable-replacement scenario |
| Interrupted external effects | Process death or lost acknowledgement leaves durable intent that a later exact retry completes without guessing | `TestLifecycleCrashApplicationEffectsSurviveProcessDeath`, `TestLifecycleCrashLostMarkAcknowledgementsAreObserved`, `TestLifecycleCrashConcurrentProcessRetries`, `TestTerminalPurgeProcessDeathRecovery`, `TestTerminalPurgeRetriesNilUnavailableBusyAndLostAck`; private-Sway `TestLifecycleHeadlessApplicationRecovery` |
| Fixture publication and child cleanup | Readers only see complete owner-only JSON; launcher children are reaped | Terminal environment tests use temp-file/rename publication; focused `TestExecProcessStarter*` evidence belongs to LAB-139 |

The deterministic cleanup compositor applies staging/move/mark effects to its
observed tree and queues move, implicit focus and tick-barrier events for
`HandleEvent`. It is not a general Sway emulator: complete structural
reconstruction, nested/floating shapes and real command-generated events are
also checked by the existing private-Sway focus/layout harnesses. A requester
that only records commands is not cited as feedback-loop evidence.

The executable replacement scenario builds two differently stamped copies of
the current source and atomically replaces their pathname. It verifies the old
running inode through `/proc/PID/exe`, then requires a fresh automatic restore
run after explicit restart. It is not a historical-release compatibility test.
Alacritty runs `sleep` as a synthetic foreground agent; PID plus process start
time and non-zombie state verify continuity. This is process-survival evidence,
not Herdr pane history or Codex/Claude conversation-resume evidence. The
executable scenario uses a deliberately absent private system bus; healthy
close and inhibitor ordering have their separate injected-guard tests.

Sway cannot reliably distinguish an ordinary client crash from an intentional
window close when both produce the same healthy close/absence sequence. The
healthy-close assertions document that limitation; shutdown/disconnect/unsafe
guard cases test separate conservative behavior. Do not infer user intent from
startup absence or claim injected shutdown signals prove a real reboot.

At LAB-134 delivery, actual VM reboot and real logind ordering were **not run**
because no disposable VM was supplied. The later
[automated KVM runner](kvm-reboot-verification.md) adds a repeatable layout reboot
scenario; its actual result must be recorded per candidate. The existing opt-in
[guest reboot procedure](https://github.com/marang/sway-session/blob/03e9cfdf47fa40965eb73b391285ec80eb66feb1/docs/follow-application-vm-check.md) records guest boot IDs,
pre-daemon eligible state, healthy-close controls and real inhibitor evidence.
A private compositor, forced reset or simulated logind event does not satisfy
that evidence class. Later delivery closed the durable saga and adoption/restart
gaps in LAB-116 and LAB-211; the scenario table above names their executable
coverage. The
[desktop acceptance closeout](https://github.com/marang/sway-session/blob/main/docs/desktop-application-acceptance.md)
separates that evidence from real Chrome/Slack and full guest-reboot acceptance.
The original LAB-134 delivery did not itself implement those later fixes.

## Doctor checks

Run the focused shared-service, CLI, and TUI tests before the full gate:

~~~sh
GOTOOLCHAIN=go1.26.5 go test ./internal/doctor ./cmd/sway-session -run Doctor
GOTOOLCHAIN=go1.26.5 go test -race ./internal/doctor ./cmd/sway-session
~~~

Use disposable configuration and XDG roots for end-to-end repair checks.
Preview new setup with `--fix sway.integration --adopt-standard` and confirm no
file changed or was created. New setup must produce startup-only; explicitly
select `--shortcuts default` to add both bindings. Preserve an existing profile
when no selection is supplied; exercise explicit switches in both directions.
Apply only to the fixture. Verify private `0600` original backups, unchanged
foreign content, atomic writes and exactly one newly appended direct include.
Cancelled, stale and concurrent-writer plans must preserve intervening edits.

Both main file and snippet are protected by file/ownership checks. Historical
partial profiles and manual edits require migration and must never be expanded
or replaced. Missing main files are not created. Missing snippets or unobserved
direct includes require explicit adoption. Cover CLI option restrictions,
text/JSON status and stable wire shape, read-only inspection and independent
runtime findings. No production compositor reload is part of Doctor.

Check the TUI at 80×24 and 48×16 with NO_COLOR, long evidence/preview/result,
a filtered list and reordered results after refresh. Select by stable check
identity. Adoption, profile selection, preview and application are distinct
steps; cancellation at each step must leave files untouched. All details and
first-start guidance remain reachable by scrolling with labelled quit/cancel
keys. Runtime findings and source setup must be distinguishable.

## Standard Doctor integration acceptance

`internal/doctor/testdata/workstation` is sanitized foreign configuration with
output/input/bar/resize-mode blocks, colors, shell expressions, continuations
and other includes. These files are opaque to the standard integration check.
Through the public report, verify that their content does not alter the result
for a supported owned snippet and an observed direct include.

Cover both supported profiles, missing standard files, literal direct and
repeated includes, unobserved indirect/variable/glob includes, protected legacy
partial and edited files. A working indirect include in Sway still produces
unobserved-direct-include evidence in Doctor. Source evidence must not claim
effective load order, executed bindings or successful next login. The bounded
recognizer's fuzz seeds cover quoting, continuations, comments and block scope;
fuzz campaigns must be explicitly time-bounded.

Run `sh scripts/check-completions.sh` with Bash, Zsh and Fish installed, and
`sh scripts/check-packaging.sh`. Check fix-only adoption and shortcut values,
option terminators, path arguments and absence of CLI execution during Doctor
completion. The shipped template has active daemon/restore starts and commented
terminal shortcuts. Removing owned shortcuts must not claim restored previous
bindings. The default-shortcut choice must remind users to define `$mod` before the first
include; it does not verify that definition.

The automated private-compositor acceptance uses:

~~~sh
SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 \
  go test ./cmd/sway-session -run '^TestDoctor(ConfigurationHeadless|StandardStartupHeadless)$' \
  -count=1 -v
~~~

Use a private headless Sway instance, disposable XDG roots and workspace 98 or
higher. Startup declarations target an inert own executable named sway-session.
For both profiles, count one daemon and one restore invocation at startup and
no additional invocation after reload. This validates the declarations, not a
production daemon or restore implementation. Validate source fixtures with
`sway -C`; GET_CONFIG and GET_BINDING_STATE establish only loaded main source
and current mode. Text/JSON reports and rendered TUI use the same cases.
Inspection, previews and rejected stale applications must leave files unchanged.
No interactive keyboard or production-workstation/reboot behavior is implied.

Run the real executable CLI and PTY acceptance separately:

~~~sh
mkdir -p /tmp/lab322-candidate
go build -o /tmp/lab322-candidate/sway-session ./cmd/sway-session
python3 scripts/doctor-standard-acceptance.py --binary /tmp/lab322-candidate/sway-session
~~~

This exercises private Sway, actual CLI flows and NO_COLOR terminal sessions at
80×24 and 48×16. See `docs/doctor-standard-acceptance.md` for measured outcomes
and coverage limits. These behavioral checks complement `make verify` and the
independent code and architecture reviews for LAB-322.

## Shared-workspace session-start broker (LAB-270)

The deterministic service/socket regressions cover three independent contexts
on an occupied destination, exact-window focus on reuse, concurrent distinct
and repeated requests, nested/floating windows, workspace-number ambiguity,
incorrect live/saved placement, duplicate identities, focus/placement changes,
partial restore/initialization, and preservation of the original retry identity.
Transport/CLI checks verify actionable allowlisted diagnostics, context UUID
recovery, legacy protocol-v1 compatibility, and private-detail redaction.

Run the opt-in real broker/compositor cases explicitly:

```sh
GOTOOLCHAIN=go1.26.5 SWAY_SESSION_HEADLESS_INTEGRATION=1 \
  go test -race ./cmd/sway-session -run '^TestSessionStart.*Headless$' \
  -count=1 -timeout=2m -v
```

The tests use private Sway, owner-only broker sockets, disposable SQLite and
workspaces 98/99. Three distinct requests coexist with unrelated tiled and
floating windows; concurrent exact retries preserve one registration/window
per context and focus the selected leaf. Real Sway window events and fresh
observations reject unrelated moves/closes. Separate named Herdr sessions
retain their custom split directions, ratios, pane IDs, and idle interactive
project shells on retry. Terminal resize geometry is intentionally excluded
from the stable Herdr topology comparison.

The restore launcher and empty-session initializer are injected boundaries in
these compositor tests. Fixed logical `codex`/`shell` roles are checked, but
authenticated Codex, the production restore CLI, AppArmor enforcement and OS
reboot ordering are not exercised. The independent opt-in
`TestHerdrLiveInitialization` checks the real Herdr initializer with a harmless
Codex stand-in, including an unchanged second initialization. Keep those proof
boundaries explicit rather than treating shell-only fixtures as real agents.
The ordinary automated gate skips compositor/Herdr opt-ins.

## Standalone extraction checks

Confirm the Go package graph contains the sway-session command and exactly the
retained internal responsibilities, with no animator command or old module
imports:

~~~sh
sh scripts/check-standalone-boundary.sh
go list ./...
rg 'github\.com/marang/sway-title-animator' --glob '*.go'
test ! -e cmd/sway-title-animator
~~~

The last rg command must produce no output. User-facing historical references
may name sway-title-animator only to explain independence, shared mark
compatibility, preserved Git history, or the package-ownership transition.

The v1 presentation mark fixture must stay byte-identical in both sibling
repositories:

~~~sh
cmp ../sway-session/internal/titleindicator/testdata/v1.json \
  ../sway-title-animator/internal/titleindicator/testdata/v1.json
go test ./internal/titleindicator
~~~

The fixture is authoritative. Do not normalize or regenerate it independently
on one side.

## State and migration checks

All probes use disposable XDG roots. Never point a test command at live user
state.

The terminal-creation registry regression checks that successful mutations
return the full committed registry for both activity-recording branches, with
new and existing state. It also verifies creation timestamps and the existing
rollback behavior when an activity insert fails:

```sh
GOTOOLCHAIN=go1.26.5 go test ./internal/session \
  -run '^(TestRegistryWithTerminalCreationReturnsCommittedRegistry|TestTerminalContextAndCreationActivityCommitAtomically)$' \
  -count=1
```

These tests use disposable SQLite roots and require no compositor.

Registry snapshot commits also retain `context.Canceled` or
`context.DeadlineExceeded` when `database/sql` claims its automatic rollback
before `Commit`. A SQLite driver wrapper triggers cancellation after the final
rows close and waits for that ordering; it does not fabricate a commit error.
The existing read-commit test checks that a closed transaction with a live
context still reports `sql.ErrTxDone`. The terminal-close deadline test verifies
that an expired observation retains the active context and pending candidate,
then archives it only after a fresh observation on retry. The 257-context test
checks eventual completion across bounded batches:

```sh
GOTOOLCHAIN=go1.26.5 go test -race ./internal/session \
  -run '^(TestStoredRegistrySnapshotCancellationAfterRowsClose|TestReadCommitCancellationRetainsItsCauseAfterRollback)$' -count=10
GOTOOLCHAIN=go1.26.5 go test -race ./cmd/sway-session \
  -run '^(TestObservedTerminalCloseDeadlineRetainsCandidateAndArchivesOnRetry|TestObservedTerminalCloseArchivesEveryCandidateBeyondOneBatch)$' -count=10
```

These checks use disposable state and a fake compositor. They do not establish
physical reboot or live compositor behavior.

Create a fresh schema-1 database:

~~~sh
probe=$(mktemp -d)
XDG_STATE_HOME="$probe" sway-session register \
  --id 88888888-8888-4888-8888-888888888888 \
  --session fresh-schema-probe --cwd /tmp --label "Fresh schema probe"
test -f "$probe/sway-session/state.sqlite3"
test "$(sqlite3 "$probe/sway-session/state.sqlite3" \
  'PRAGMA user_version')" -eq 1
sqlite3 "$probe/sway-session/state.sqlite3" \
  'PRAGMA journal_mode; PRAGMA foreign_key_check; PRAGMA quick_check;'
~~~

Expect wal, no foreign-key rows, and ok. Verify the database and every present
WAL, SHM, or journal sidecar are regular, single-link, current-owner files with
mode 600. Stop every process using the disposable root before removing only
that root.

The pre-release JSON migration remains source-preserving. In a second
disposable root, prepare recognized legacy contexts.json, layout.json,
application-runtime/application-session.json, and
terminal-runtime/terminal-activity.json fixtures. Confirm ordinary state
opening fails closed while the database is absent, run terminal manage action
m, then verify:

- all valid rows were imported in one transaction;
- stale activity or launch rows whose context is absent were reported and
  skipped;
- every source JSON file is unchanged;
- repeating migration returns verified success without duplication; and
- once state.sqlite3 exists, the legacy files are ignored and never recreated.

LAB-119 introduces no path, schema, or migration change. Existing live state
must not be opened or rewritten merely to verify the repository split.

### Backup and recovery CLI checks

Run the CLI tests and completion checks with the declared toolchain:

~~~sh
GOTOOLCHAIN=go1.26.5 go test ./cmd/sway-session -run '^TestState' -count=1
sh scripts/check-completions.sh
~~~

The CLI fixtures use temporary state/runtime directories and injected core
operations. They check required and invalid arguments, typed JSON results and
diagnostics, cancellation forwarding, and recovery's preview default even when
stdin contains confirmation text. Preview must neither prepare a state root
nor create a runtime daemon lock. Recovery apply must refuse an already held
daemon lock or invalid runtime, hold the lock throughout the core operation,
and release it on both success and failure. Busy and pending recovery errors
must retain distinct diagnostic codes. Human output must distinguish a valid
rollback database from an unvalidated raw bundle and explain preview limits.

Completion probes cover `state`, `backup --output`, `recover --from`, apply-only
`--yes`, file paths with spaces, and option terminators. They must not invoke a
state command or request private context candidates. Bash always runs; Zsh and
Fish checks run when their interpreters are installed.

Core recovery checks additionally use disposable database fixtures to prove
writer exclusion, backup validation, preservation of healthy and corrupt
previous state, and resumption after interruption. No backup/recovery test may
read real user state, stop a real daemon, signal a process, or use the live
compositor. The full integration gate remains `make verify`.

## Real Sway and Herdr check

Use a private compositor/socket, disposable XDG config, state, and runtime
roots, and workspace 98 or higher. Never create, move, close, restore, or purge
a test window on a single-digit workspace. Enable pane history in the
disposable Herdr config and use a short enough root for Herdr Unix socket path
limits.

The fixed role initializer has a separate real-Herdr check that uses a private
Herdr server and a harmless Codex fixture, without contacting the user's Herdr
server or opening a Sway window:

```sh
SWAY_SESSION_HERDR_INTEGRATION=1 go test ./internal/herdrinit \
  -run '^TestHerdrLiveInitialization$' -count=1 -v
```

The scheduled `Herdr compatibility` workflow runs this check against the latest
stable Linux x86_64 release every week and can also be started manually. It
detects a new incompatible Herdr wire contract; it does not update the user's
installed Herdr package or promise compatibility before the check passes.

1. Build the candidate sway-session binary.
2. Start the daemon against the private Sway socket.
3. Create a fresh persistent terminal with terminal --new and record only its
   disposable context UUID.
4. Confirm one typed terminal maps and receives the stable context mark.
5. Let the daemon capture workspace and layout into state.sqlite3.
6. Stop the daemon, close only that exact test window, and run one-shot restore.
   Confirm the terminal maps again with the same UUID and Herdr session.
7. Start the daemon and confirm it places the window on the saved high-numbered
   workspace and converges its outer layout.
8. Archive/activate and then exact-ID purge the disposable context.
9. Confirm the registry is empty, stop every test process, and remove only the
   disposable roots and high-numbered workspaces.

One-shot restore proves launch or mapping. Saved workspace placement and layout
require the daemon; never claim placement from a daemon-free mapping test.

The LAB-177 layout matrix exercises capture of stacked, split, nested tabbed,
and floating workspaces against a persisted placement-only snapshot containing
an archived context. It checks SQLite reload, exact restore selection, safe
mixed-tree degradation, archived registry retention, and repeated merge
idempotence:

```sh
go test ./internal/session -run '^TestLayoutAcceptance' -count=1
```

The private-Sway delayed-application test also keeps archived and deliberately
closed application records beside a desired-open application. Only the latter
may launch; the inactive placements must disappear from the new layout while
their registry records remain. A second private-Sway regression reconstructs
vertical splits, nested tabs, and floating windows from newly mapped terminals,
checks the live structure and durable capture, then verifies steady-state
idempotence:

```sh
SWAY_SESSION_HEADLESS_INTEGRATION=1 go test -race ./cmd/sway-session \
  -run '^TestSessionRuntime(LayoutShapes|LateApplication)Headless$' -count=1 -v
```

The automated [KVM reboot acceptance](kvm-reboot-verification.md) complements
these checks with two regular guest reboots, persistent guest state and the
normal Sway login/autostart path. It establishes VM reboot behavior; a user
workstation result remains separate evidence.

Also exercise:

- absent → mapped → absent during both terminal stability checks;
- reuse after closing the outer adapter without restarting an occupied agent;
- role initialization failure followed by idempotent exact-context retry;
- rejection of a conflicting persisted adapter or working directory;
- lifecycle lock serialization across terminal, restore, and broker entry
  points;
- more than 128 stored contexts without a registry-capacity error;
- rotating placement/indicator batches after a completely rejected batch;
- application preflight rotation beyond its two-candidate pass bound; and
- layout re-observation after every mutation and after bounded yield.

### Terminal activity observations

Run the focused observer, inventory and manager tests before the full gate:

~~~sh
go test ./internal/session -run 'Herdr.*Observation|ObserveHerdrSessions'
go test ./cmd/sway-session -run 'TerminalInventory|TerminalCleanup|TerminalManageActivity'
~~~

These checks separate window presence from manager state and live agent
evidence, exercise unknown/unavailable results, and ensure one bulk discovery
per refresh. Manager tests cover cancellation, superseded generations,
archived/filtered entries, deletion-preview freshness and narrow layouts.
No stored association is sufficient proof of a running agent. An isolated
Herdr check must preserve HOME, redirect all XDG roots and Herdr config, and
stop only the disposable named server it created. It needs no real agent
credentials or user session state.

~~~sh
SWAY_SESSION_HERDR_OBSERVATION_INTEGRATION=1 go test ./internal/session \
  -run '^TestObserveHerdrSessionsLiveIsolated$' -v -count=1
~~~

LAB-131 acceptance on 2026-09-30 passed with Herdr 0.9.2: running shell,
pane-directory hint, missing session, and stopped session. HOME was preserved;
all XDG roots and the named server were disposable. Agent process matching,
stale associations, cancellation and concurrency bounds are covered by the
API fixture tests; this smoke does not launch a real Codex agent.

### Cancelled layout restore cleanup

The runtime regression tests exercise a successful staging move, cancellation,
and the subsequent move/focus/tick events before further reconciliation. They
verify that cleanup returns only windows still owned by that operation and
still in staging. A window moved to another workspace by the user stays there.
The suite also covers connection loss and fresh reconnection, rejected and
ambiguous return moves, retaining the saved layout during incomplete cleanup,
and daemon restart followed by cancellation before startup selection.

Cleanup retains attempted staging moves and temporary marks independently of
the structural restore cursor. Every action is planned from a fresh tree and
uses the observed container identity. A missing reply is reobserved before any
retry. Each reconciliation issues at most one cleanup mutation; observation
continues after failures. Attempts rotate across pending windows and marks so
one rejected return cannot starve the others. Structural completion does not
release a rejected unmark: ownership ends only after observation confirms the
effect is gone. Layout persistence waits until cleanup finishes.
After a daemon restart, the saved layout, active identities, reserved staging
workspace and related deterministic restore marks provide recovery evidence.
No new database schema or recovery sidecar is introduced.

Cleanup remains independent of focus attribution. It prevents cancellation
from abandoning temporary staging effects; it does not complete structural
reconstruction against user intent or repair Herdr agent working-directory
restoration.

Run the optional real-compositor regression with Sway and Alacritty installed:

```sh
SWAY_SESSION_HEADLESS_INTEGRATION=1 go test -race ./cmd/sway-session \
  -run '^TestSessionRuntimeRestoreCleanupHeadless$' -count=1 -v
```

It creates its own headless compositor, configuration, runtime sockets and
state roots. Its windows use workspaces 98 and 99. The test drives actual Sway
focus/move events and verifies cleanup, persistent-mark preservation, retained
user placement and the absence of further structural reconstruction. Ordinary
test runs skip it; a private compositor test is not evidence of a real reboot.

### Restore-generated focus events

The runtime matches its command-generated focus feedback from the fresh tree
and focus stacks for staging/placement moves, explicit focus, and fullscreen
activation. Move-induced feedback is consumed only after the corresponding
expected move event. Expected workspace and window feedback is ordered; each
expectation is consumed once and
expires at that command's tick barrier. Stream generation changes, command
failures, and cancellation invalidate allowances. Placement yields for a new
tree before a second move, so it never predicts from an already changed focus
stack. Missing focus-stack evidence cannot justify an arbitrary workspace switch.

Cold startup is tested with the daemon subscribed before terminals map.
Window focus alone does not cancel saved placement, whether it comes from
mapping, a manual click or automatic refocus after another window closes.
Mapping needs no focus allowance, remembered container or separate tick.
An explicit cancellation still prevents later window adoption from restarting
structural restoration in the same daemon lifetime.

Sway IPC provides no focus-event origin token. Exact matching between command
and barrier is therefore a bounded inference: a concurrent user action with
the identical transition cannot be distinguished by these fields alone.
Bindings and unattributed workspace focus events cancel conflicting
reconstruction. Window command feedback is consumed only to maintain ordered
workspace attribution; unmatched window focus is harmless. There is no
time-based focus suppression window.

Run the full event-driven regression using a private compositor:

```sh
SWAY_SESSION_HEADLESS_INTEGRATION=1 go test -race ./cmd/sway-session \
  -run '^TestSessionRuntimeRestore(Focus|ColdStartFocus|Cleanup)Headless$' -count=1 -v
```

The focus test starts with two split workspaces, restores tabbed and stacked
layouts, consumes real command-generated events between bounded runtime
passes, and verifies original focus, leaf order, stable identity marks, and
the absence of temporary staging state. It also verifies persistence and
steady-state idempotence after restoration. The cleanup tests retain explicit
user cancellation. The cold-start case uses the production per-event
reconciliation order, including queued mapping-focus events after adoption,
and checks both successful tabbed restoration and intervening user binding.
Global fullscreen on a container group is covered by real workspace-only
focus feedback. Unit regressions cover delayed, duplicate, out-of-order,
expired and generation-mismatched events, command failures, nested focus
stacks, and fullscreen transitions. These are isolated compositor checks, not
evidence that a production reboot has passed.

### Startup focus from an unrelated dialog (LAB-275)

Saved windows restore to their saved workspaces and layouts while an unrelated
password dialog remains open. The dialog is not a saved restore target. Its
mapping, repeated focus, movement and closure do not pause reconstruction,
renew startup/report deadlines or add a separate capture guard.

Move and close events affect startup intent only when their payload identifies
a restore-eligible registered context present in the saved layout. The same
identity validators recognize terminal marks/stable IDs and native desktop
identities before adoption. Closed windows need no remembered live-tree entry.
Unrelated windows, including preexisting dialogs, are ignored without a
foreign-window list, focus history, return-focus exception or provider list.
Malformed or ambiguous registered identity evidence interrupts with
`observation_unavailable` rather than guessing ownership.

The late-application layout observer selects restore-eligible registered
terminals and unambiguous desktop-application anchors directly from the fresh
identity observation, including anchors not yet marked by adoption. Unrelated
views never enter the compared structure or geometry, even if they existed
before startup and disappear without any new/close event. Ancestors containing
selected views retain their layout structure. The total view count is used only
to avoid interpreting automatic sibling resizing during mapping or closure as
a user resize; geometry comparison also requires an unchanged selected window
set. Bindings, saved-window move/close, unmatched workspace focus and
saved-workspace layout/resize changes still supersede reconstruction. A manual
click which only changes window focus lets reconstruction continue too.
Stream loss interrupts with `observation_unavailable`; it must not be reported
as `user_cancelled`. Existing startup capture protection and late-application
restore retain saved intent while registered windows are still missing.

Run the isolated regressions, or the complete lifecycle runner above:

```sh
GOTOOLCHAIN=go1.26.5 SWAY_SESSION_HEADLESS_INTEGRATION=1 \
  go test -race ./cmd/sway-session \
  -run '^TestSessionRuntime(Foreign|PreexistingForeign|LateApplicationIdentitySettlement|WindowChangesOnly|StartupPrompt)' -count=1 -v
GOTOOLCHAIN=go1.26.5 go test ./internal/session \
  -run '^TestCaptureLayoutStartupPrompt' -count=1 -v
```

The private-Sway cases observe real new/focus/close/refocus events. The terminal
case verifies saved tabs before the unrelated view closes. The application
case uses two distinct synthetic application identities and four terminals on
workspaces 98–101; it verifies delayed mapping, placement, tabbed layouts,
outcome reports and durable capture before the unrelated view closes. These
are authentication-dialog and desktop-app surrogates, not a real GCR unlock,
Chrome/Slack run or production reboot. Separate capture tests show that a floating foreign prompt preserves a
reconstructible terminal group; a mixed unmanaged tiling sibling retains the
existing workspace-local `placement_only` policy.

Installation on the affected workstation remains a coordinated user action.
Before changing the package, retain the previous verified package and create
an owner-only metadata backup with `sway-session state backup --output PATH`
using an absolute, unused destination. A backup preserves current metadata;
it cannot recover an earlier layout that has already been overwritten. Install
the reviewed package through the package manager, then stop only the identified
sway-session daemon and start the replacement through the established session
launcher. Replacing the executable alone does not update a running daemon.
Preserve the terminal adapters, foreground agents and registered contexts.
Rollback installs the retained package with `pacman -U PATH` and uses the same
controlled daemon restart. Metadata recovery is a separate decision: first
preview `sway-session state recover --from PATH`; apply only with stopped
writers and explicit `--yes`. Do not copy live SQLite/WAL files or manually
rewrite layout tables. See the README's state backup/recovery procedure.

### Terminal close intent

With the candidate daemon and a working logind observer, close one exact
disposable terminal on the private compositor. After the close grace, assert
that the same context is archived, not deleted. Automatic restore must skip it;
activation followed by explicit open must reuse the same identity and session.
Stop the daemon, close that test adapter, and start the daemon again: startup
absence must leave an active context active. Check that the daemon log does not
report unavailable shutdown protection before claiming the automatic-close
positive path passed.

Unit/integration tests inject shutdown, sleep, session termination, disconnect,
reconnect, identity ambiguity, reopened windows, and lock contention. The Sway
transport test must block shutdown event delivery and still observe its stream
guard as disconnected. The logind tests must verify that safety is invalidated
before the inhibitor is released and cannot be re-enabled by stale setup work.
Do not reboot or suspend the user's workstation to drive these tests. A private
Sway close/exit test is not evidence of a real power-cycle test.

### Follow application close intent

For adopted applications, also run:

~~~sh
GOTOOLCHAIN=go1.26.5 go test ./internal/session ./cmd/sway-session \
  -run 'Test.*Adopt|TestRuntime(FreshCloseConfirmation|CancelledCleanup)' -count=1
SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 \
  go test ./cmd/sway-session \
  -run '^TestApplicationAdoptionDaemonRestartHeadless$' -count=1 -v
~~~

These checks distinguish persisted presence adoption from launch attempts,
retain closed pinned applications across daemon restart, and permit startup
again in a new compositor. They cover Follow rearm, launch concurrency, failed
observation persistence, old schema reads, compositor-tagged evidence, conflicts,
context deletion and metadata backup/migration. The private Sway case adopts
and closes a real disposable window on workspace 98, then reconstructs the
runtime from the same SQLite state without launching the application. Its
launcher is a recording boundary; it is not a production application or reboot
test.

Run the bounded runtime regressions with:

~~~sh
GOTOOLCHAIN=go1.26.5 go test ./cmd/sway-session \
  -run 'Test(FollowApplication|ApplicationClose)' -count=1
~~~

The tests start with a previously present Follow application, then inject unsafe
shutdown generations, unavailable or failed monitoring, Sway shutdown, and
subscription loss. Absence beyond the grace must preserve durable desired-open.
They also cover a generation change immediately before the registry mutation,
or during tree acquisition or between both planning passes using the same tree,
fresh absence failure or window reappearance, concurrent archive/policy/identity
changes, lifecycle reservations, initially missing apps, and unchanged pinned
policy. Recovery requires fresh healthy presence and a complete new close grace;
uncertain observation cannot trigger an extra launch or focus change.

The opt-in private compositor test uses a real disposable application window
and injected lifecycle guards. This checks the runtime/Sway boundary but is not
evidence of actual logind ordering or a reboot. For that separate acceptance
check, use the [disposable VM procedure](https://github.com/marang/sway-session/blob/03e9cfdf47fa40965eb73b391285ec80eb66feb1/docs/follow-application-vm-check.md). Never
reboot the workstation or reuse production state to run it.

For the manager, archive consecutive items at the middle and end of the active
section; verify remaining active entries remain convenient to select. Delete,
activate, rename and refresh under a filter that excludes at least one entry.
Check keyboard navigation and the active-filter hint at 80x24.

LAB-123 live evidence (2026-09-05): the source-built candidate passed on a
private headless Sway compositor, workspace 98, disposable XDG roots, Alacritty,
and Herdr. Its real logind observer acquired protection successfully. Closing
the exact test terminal archived it; automatic restore skipped it; activation
and open reused its identity. Daemon restart did not archive a context already
absent at startup. Private Sway exit preserved active state, and after starting
a new private compositor the same context restored. Exact purge emptied the
test registry. No live user context, machine reboot, or system suspend was used.

### LAB-119 extraction evidence

On 2026-09-05, source-built binaries were exercised against Sway 1.12,
Alacritty 0.17.0, and Herdr 0.8.2 on a private headless compositor with
isolated XDG roots and workspace 98. No live user state or single-digit
workspace was used.

- sway-session daemon created the SQLite state and remained alive independently
  of sway-title-animator.
- terminal --new created one marked persistent terminal context.
- With both daemons stopped, one-shot restore remapped the same context and
  Herdr session, proving launch/mapping independence.
- After capture, the daemon was restarted from another high-numbered workspace;
  restore remapped the terminal and daemon reconciliation returned it to saved
  workspace 98 without an animator.
- Exact purge emptied the disposable registry.
- All test processes and disposable contexts were removed.

The isolated harness first exposed test-setup mistakes—a too-long Herdr socket
root and a missing pane-history setting. Retrying with the packaged Herdr
template and a shorter private root passed; neither was a product failure.

The same extraction also passed source-built Arch package transitions from the
combined sway-title-animator 0.9.3 package in disposable pacman roots: both a
single transaction installing animator 0.10.0 plus sway-session 0.1.0 and a
sequential animator upgrade followed by sway-session installation. Neither
required an overwrite flag. Package ownership checks assigned each binary to
its respective package. These were isolated package-manager checks, not a
change to the running workstation's installed packages.

GoReleaser snapshots built Linux amd64 and arm64 archives, DEBs, and RPMs.
Inspected session artifacts contain only the session executable and its
documentation/integration assets; DEB runtime metadata lists only Sway.

## Desktop application check

The process-level launcher regression requires Linux but no compositor or Herdr:

```sh
GOTOOLCHAIN=go1.26.5 go test -race ./internal/session \
  -run '^TestExecProcessStarterLifecycle$' -count=1
```

An isolated helper parent exercises the actual starter with several successful
and nonzero-exit children. It checks bounded reaping while the parent is alive,
prompt return for a running child, synchronous startup errors, another command's
exit-status ownership, and detached child survival after a short-lived CLI
exits. The helper adopts that last child and cleans up only its exact test PIDs.

The delayed-application regression uses a real private Sway compositor and an
Alacritty window with an ordinary desktop application identity. Launch
scheduling and time are injected; no browser or agent session is involved:

```sh
SWAY_SESSION_HEADLESS_INTEGRATION=1 go test -race ./cmd/sway-session \
  -run '^TestSessionRuntimeLateApplicationHeadless$' -count=1 -v
```

It covers arrival before and after the startup timeout in both saved child
orders, real mapping and command-generated events, staging beyond the
application close grace period, tabbed convergence, persisted capture,
steady-state idempotence, and preservation of a user binding or an IPC layout
change before or after the startup timeout. Unit regressions also cover
observed resizing, changed policy/identity, close/move/disconnect, existing marks,
ambiguous groups, mark retry, genuine mixed-workspace degradation, and retiring
that degraded intent before its debounced snapshot is written. This is
not a production reboot or application-internal session restore test.

The application launch freshness regression maps a real matching Alacritty
window during an injected launcher preparation, after the caller observed
absence. It checks the real event subscription and current tree, zero starts
and zero persisted attempts. Its absent control checks exactly one start with
durable intent already visible inside the effect boundary:

```sh
SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 \
  go test -race ./cmd/sway-session \
  -run '^TestSessionRuntimeApplicationLaunchHeadless$' -count=1 -v
```

Runtime tests (`TestApplicationLaunch*`) additionally cover mappings while the
registry lock is held and between candidate preparations, stream replacement
or disconnection during preparation and tree acquisition, missing streams,
unavailable or invalid trees, ambiguous windows, lifecycle reservations, fresh
attempt timestamps, idempotence, bounded rotation and concurrency. A lost
stream after the intent commit blocks the effect while preserving the intent.
All state and windows are disposable; the desktop command is injected, and
these checks do not claim a real browser startup or workstation reboot.

Use a disposable desktop entry and workspace 98 or higher. Never reuse or purge
an unrelated registration.

1. Focus one eligible normal top-level and preview register-focused.
2. Approve it explicitly; verify the exact identity, protected launch snapshot,
   stable context mark, and list/status output.
3. Exercise follow close grace, pin/unpin, archive/activate, rebind, reapprove,
   and exact-ID forget.
4. With two indistinguishable matching windows, verify presence is true but no
   anchor is guessed or moved.
5. Queue a missing desired app and verify launch intent is durable before
   process start; daemon restart must not duplicate it in one compositor
   session.
6. Confirm a user binding, workspace switch or saved-window move invalidates
   stale automatic work; a click that only changes window focus lets it continue.

Treat Chrome, Slack, and similar application-internal restoration as app-owned.
Registered application anchors also support typed scratchpad placement. Native Wayland parent/type limits in Sway
1.12 remain documented rather than guessed around.

### Scratchpad application acceptance

`TestSessionRuntimeScratchpadHeadless` captures hidden and shown registered
application anchors, stops and reaps its private compositor, and restores the
owner-only snapshot in a second private compositor. It exercises native Wayland
and, when XWayland is installed, isolated XWayland windows on workspaces 98 and
99. Real map, focus, move and tick events are processed through the daemon.
The focused unrelated window and an unrelated hidden scratchpad window must
remain unchanged; repeated reconciliation must not relaunch or toggle anchors.
The launcher is injected and creates real Alacritty windows with distinct
application identities. This proves compositor restart and placement, not
browser/Slack startup, a VM reboot, or application-private session restoration.

Run the test directly or as part of `scripts/verify-lifecycle.sh --headless`:

```sh
SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 \
  go test -race ./cmd/sway-session -run '^TestSessionRuntimeScratchpadHeadless$' \
  -count=1 -timeout=3m -v
```

The deterministic scratchpad tests cover absent and ambiguous anchors, command
rejection and uncertain acknowledgements, user cancellation, lifecycle
reservations, read-only v1 layout migration, metadata privacy and restore-report
proof. A mapped window alone cannot complete requested scratchpad placement:
membership and saved visibility, including the workspace when shown, must be
freshly observed. Cycling order and reconstruction of scratchpad groups are
not guaranteed. Showing a fresh hidden leaf uses an absolute hide before the
show command and restores original focus; the Sway tree/command interval is
not an atomic compare-and-swap with user input.
The same real-compositor fixture checks hidden and shown scratchpad arrivals
whose saved intent is a normal layout: show, leave membership with an absolute
floating disable, observe again, then mark. Moving a scratchpad window to a
workspace alone does not remove its scratchpad membership.

## AppArmor and broker check

For agent-report changes, run the real Unix-socket regression tests in
internal/agentreport and the supplied provider-hook translation tests.
Verify multiple agent kinds, non-UUID session tokens, rejected commands and
unknown fields, payload limits, peer credentials, unrelated pane ancestry,
socket replacement, and rejection of the removed report-v1 payloads. Separately
verify that the session-start protocol-v1 contract remains unchanged. These tests use disposable
roots and a fake Herdr API; they do not prove provider-specific hook or resume
behavior in a live Herdr session.

Static policy validation is part of make verify:

~~~sh
sh scripts/check-apparmor-policy.sh
~~~

When the matching profile can be loaded safely, use only package-installed
root-owned binaries and invoke:

~~~sh
/usr/share/doc/sway-session/scripts/verify-codex-boundary.sh \
  CONTEXT_UUID PANE_ID CODEX_SESSION_UUID \
  HERDR_HISTORY STATE_FILE HERDR_SOCKET
~~~

The verifier requires /usr/bin/sway-session to be owned by the sway-session
package. It proves the narrow positive report path and negative history/state
access. A pathname-socket connect mediation gap is a failed or explicitly
unsupported boundary, never a passing result.

## Package build check

Create a temporary source archive from the intended release commit and copy
PKGBUILD to a temporary build directory. Set the intended package version,
point its source at that local archive, replace the source checksum with the
archive's actual SHA-256 and regenerate .SRCINFO. This private build needs no
new release tag; `SKIP` was only the historical pre-v0.1.0 bootstrap state.
Then run:

~~~sh
makepkg --verifysource
makepkg --cleanbuild --clean --noconfirm
pacman -Qlp ./*.pkg.tar.zst
~~~

Inspect that the package contains /usr/bin/sway-session, completions, the
license, README, plan, verification guide, standalone Sway template, Herdr and
sway-session config templates, direct Codex hook template, AppArmor profile, and live
verifier—all below /usr/share/doc/sway-session where appropriate. It must
contain no animator binary, animation/audio asset, parec metadata, optional
dependency metadata, or old documentation root.

For the package split, test a clean install and the ownership transition with
actual built packages in an isolated package-manager root. The old combined
package may own /usr/bin/sway-session. Either upgrade sway-title-animator to a
version that no longer owns that path before installing sway-session, or
install both verified replacement packages in one transaction. Never use
--overwrite to conceal ownership mistakes.

## Release gate

Before tagging:

- make verify passes;
- the current worktree has no generated binary or transient package output;
- a clean GoReleaser snapshot passes and its contents are inspected;
- the temporary local-tarball Arch source package builds and tests;
- title-indicator fixtures match across repositories;
- real Sway/Herdr evidence above is current;
- package ownership transition evidence is recorded;
- the code-review workflow has no unresolved actionable finding;
- CI configuration targets only sway-session;
- GitHub release/AUR secrets and permissions are verified without publishing;
  and
- the release commit is on main.

Only then create the immutable tag for the approved, unused release version.
The AUR workflow computes the verified commit archive's actual checksum,
refuses `SKIP`, verifies and builds the package, publishes exact metadata, and
opens the metadata-sync PR. Do not move a release tag or invent a checksum.


## Next-login policy and transition evidence (LAB-130)

Use disposable registry state to compare `restore --preview` policy against
terminal automatic target selection and application coordinator launch selection.
Cover active and archived terminals, Follow/pinned apps with desired-open true
and false, unavailable compositor evidence, duplicate identities, and legacy
records without lifecycle metadata. Preview must use read-only database access,
issue only GetTree, and leave saved state unchanged; empty state must remain
absent. Explicit archive/activate and grace-confirmed close persist the latest
reason and UTC timestamp with the authoritative state update. Shutdown,
disconnect, and unconfirmed absence must not invent a transition.

Inspect manager rendering at 48x16, 80x24, and a wider terminal. Selection,
filtering, and open/archive controls must remain usable; selected next-login
reason and last-change evidence must survive compact rendering. These automated
checks do not establish an actual machine reboot result.


## Recorded restore outcomes and retry (LAB-132)

Use disposable state to check that accepted launches remain distinct from mapped
windows, and mapping remains distinct from placement and full layout proof.
Exercise partial success across bounded launch waves, concurrent/replaced
attempts, interruption and restart, missing contexts, identity changes, and
report replacement. Verify stable reason codes and UTC timestamps without raw
process output. A report with no history must succeed without creating files.

Layout completion is a read-only observation of the requested structure,
geometry and workspace-local selected descendant. A correct inactive workspace
must complete without acquiring global focus (LAB-277). The observer shares
the planner's identity, exact-layout and presentation checks; missing or
malformed local focus evidence does not prove completion. The requested digest
must still match the saved intent. Capture preservation uses runtime restore
failures rather than diagnostic timeout rows; explicit report retries retain
their pending-token checks.

Retry must use the selected exact context ID, re-read current policy and window
state, refuse archived contexts until explicitly activated, and reuse a mapped
window without launching an adapter or reinitializing a live agent. Exercise
manager focus, filtering, refresh and retry at 48x16, 80x24 and a wider viewport.

Run private-compositor layout checks with disposable state and workspace 98 or
higher. Verify outcome proofs from fresh trees after restore commands, rather
than command acknowledgements. This does not establish a machine reboot result
or application-internal session recovery.

The inactive-workspace report check runs a built daemon against two tab pairs
and a singleton on private workspaces 98–100. It checks complete outcomes,
unchanged captured layouts and no window/workspace focus events through the
real report deadline. An optional `SWAY_SESSION_RESTORE_REPORT_DAEMON` selects
an already-built comparison executable; it is a test-only override.

```sh
GOTOOLCHAIN=go1.26.5 SWAY_SESSION_HEADLESS_INTEGRATION=1 \
  go test ./cmd/sway-session \
  -run '^TestDaemonRestoreReportInactiveWorkspaceFocusHeadless$' -count=1 -v
```

The single-window tab check (LAB-313) captures a real top-level tab container,
replaces its owned window with a flat desktop or terminal window, and restores
the captured parent, proportions and local selection. It verifies complete
outcomes, durable capture, cleanup and steady-state idempotence. Other
single-child split, stacked, nested and floating groups retain conservative
placement fallback; unexpected managed or unmanaged tiling neighbors prevent
reconstruction.

```sh
GOTOOLCHAIN=go1.26.5 SWAY_SESSION_HEADLESS_INTEGRATION=1 \
  go test ./cmd/sway-session \
  -run '^TestSessionRuntimeSingleWindowTabHeadless$' -count=1 -v
```

## Durable terminal purge (LAB-208)

The deterministic suite uses independent helper processes to interrupt durable
intent, native stop/delete effects and final journal retirement. It also covers
lost acknowledgements, repeated recovery, concurrent retries, SQLite write
contention, immutable directory evidence, unsafe paths, identity reservations,
backup/recovery and forward-only purge cancellation. CLI/daemon adapter tests
cover pending output, retry backoff, absence completion and a changed Herdr
configuration after intent was recorded.

The last-context purge regression keeps a real durable reservation pending with
an empty registry. Runtime observation and indicator planning must still accept
the empty contexts array without changing the authoritative registry. After
purge completion, reconciliation and flush must persist the empty layout.

An isolated native check on 2026-10-02 used installed Herdr 0.9.2, private XDG
directories and `/bin/sh` without login configuration or agent resume:

| Scenario | Observed result |
| --- | --- |
| Purge an existing running named session | Native stop and delete completed; session directory, registry entry and pending operation were absent |
| Make Herdr unavailable after registration, then retry with another `XDG_CONFIG_HOME` | Initial purge retained a pending operation; retry deleted the original recorded target and left the new root untouched |
| Replace the stopped session directory after recording pending intent | Retry reported a conflict and preserved the replacement directory |

The first native run exposed Herdr's default `0755` child directory modes inside
its private root. The corrected guard accepts those native modes and refuses
group/other write permissions; a regression covers both cases. All disposable
server processes were stopped and reaped. No live user sessions or compositor
workspaces were used. These checks exercise the native integration; they do not
prove atomic exclusion of concurrent Herdr startup or handoff. That residual
name-based deletion boundary is documented in [lifecycle recovery](lifecycle-recovery.md).

## Application forget compensation (LAB-209)

`TestAppForgetCancellationSerializesCompensationWithRebindFocused` exercises both
CLI entrypoints with real state locks and a fake compositor. It cancels forget
after the original unmark, starts rebind during compensation, and checks the
replacement's sole mark ownership, registry identity and retired reservation.
The previous implementation fails with `mark_ownership_changed`.

Core tests additionally cover cancellation with an unchanged, replaced or closed
window, foreign or moved marks, compositor replacement, and both committed and
uncommitted uncertain database outcomes. The recovery-gate test proves state
recovery remains excluded through compensation and the locks are then released.
These are deterministic concurrency and fault-injection checks; they do not
claim a real machine reboot or application-session recovery.
