# Desktop application acceptance closeout

This is the LAB-95 evidence review, dated 2026-10-03. Its delivered children
LAB-96–99 and LAB-101 belong to the pre-extraction history; they must not be
implemented again. The parent closes the delivered application-level contract
with the limits below, rather than claiming every originally proposed browser
and interaction scenario has been executed.

## Evidence classes

| Contract | Evidence | Limit |
| --- | --- | --- |
| Explicit registration, one-time approval and workspace deduplication | `TestAppRegisterFocusedFlatpakIsExplicitMarkedAndIdempotent`, `TestAppRegisterFocusedAmbiguityUsesOneTimeTypedSwaynagChoices`, `TestAppRegisterWorkspaceDeduplicatesApplicationInternalWindows` | Injected compositor and approval presentation; not a graphical chooser acceptance run |
| Exact Wayland, XWayland and Flatpak identity; no title matching or ambiguous anchor selection | `TestResolveFocusedApplicationWaylandXWaylandAndFlatpak`, `TestResolveFocusedApplicationNeverGuessesAmbiguousEntry`, `TestFocusedApplicationRejectsTransientAndDialogSurfaces` | Deterministic identity fixtures |
| Protected user desktop approval, revalidation and fixed typed launchers | `TestUserDesktopApprovalCreatesProtectedSnapshotAndTypedGioLaunch`, `TestDesktopApplicationLauncherUsesOnlyTypedFlatpakArguments`, `TestRebindRejectsLauncherChangedAfterApprovalWasReviewed` | Real disposable files, injected launcher in the unit tests |
| Follow last-window close and picker transition | `TestFollowApplicationHealthyCloseRequiresFreshAbsence`, `TestFollowApplicationUncertainLifecycleRequiresNewHealthyPresence`, `TestApplicationRestoreCoordinatorPreservesProfilePickerTransition` | Deterministic lifecycle/clock boundaries; not a real browser profile-picker run |
| Shutdown must preserve desired-open | `TestFollowApplicationShutdownHeadless`; [PR #50](https://github.com/marang/sway-session/pull/50) | Actual private Sway; injected shutdown guard and launcher |
| Fresh pre-launch presence prevents a duplicate | `TestSessionRuntimeApplicationLaunchHeadless`; [PR #50](https://github.com/marang/sway-session/pull/50) | Real mapping during preparation, injected process start; no exactly-once guarantee across independent external launches |
| Adopted pinned apps remain closed across daemon restart | `TestApplicationAdoptionDaemonRestartHeadless`; [PR #56](https://github.com/marang/sway-session/pull/56) | Real window and store/runtime reconstruction; new compositor has a separate startup opportunity |
| Bounded startup concurrency and conservative durable attempts | `TestApplicationRestoreCoordinatorPersistsAttemptsAndBoundsParallelLaunches`, `TestApplicationRestoreCoordinatorUsesFreshAttemptBudgetForNewCompositor` | Coordinator/storage evidence, not simultaneous browser startup timing |
| Late placement and user cancellation | `TestSessionRuntimeLateApplicationHeadless`, `TestSessionRuntimeRestoreCleanupHeadless` | Actual private Sway windows; application startup is injected |
| Application-level indicator marks on all matching windows | `TestPlanApplicationIndicatorsConvergesTwoWindowsInTheSameState`, version-1 wire fixture | Does not prove a font, app icon, or optional animator's rendering |
| Scratchpad capture/restore and saved visibility | `TestSessionRuntimeScratchpadHeadless`; [PR #66](https://github.com/marang/sway-session/pull/66) | Actual Sway/Wayland/XWayland and compositor restart; ordinary test windows, injected application launcher |
| Regular guest reboot and logind inhibitor ordering | [KVM CI 37135653022](https://github.com/marang/sway-session/actions/runs/37135653022), [PR #66](https://github.com/marang/sway-session/pull/66) | Two terminal contexts and two normal reboots; CI merge-ref `6c780b36a2b865b1c4fac1e5c6c086ad14fdfc45`. Not Chrome, Slack, notebook, agent-resume or AppArmor acceptance |

The full current-source gate remains `make verify`. Test names identify
executable coverage; they do not upgrade synthetic evidence into an actual app
or reboot result. A KVM result applies to its recorded build and scenario.

## Connected desktop fixture

The opt-in desktop acceptance fixtures add actual application startup to the
existing component evidence. Run explicitly:

```sh
GOTOOLCHAIN=go1.26.5 sh scripts/verify-desktop-apps.sh --chrome
GOTOOLCHAIN=go1.26.5 sh scripts/verify-desktop-apps.sh --slack
GOTOOLCHAIN=go1.26.5 sh scripts/verify-desktop-apps.sh --all
```

Chrome's `TestDesktopAcceptanceChromePrivate` passed under the race detector
with Chrome 154.0.8037.57 and Sway 1.12 on native Wayland and XWayland. It uses
the real default catalog, focused registration with explicit `--yes` approval,
protected user desktop snapshot, production launcher preparation and actual
`/usr/bin/gio`. After the first compositor and its children exit, a new private
compositor reloads the same registration/layout and starts the browser once.
Fresh window events drive reconciliation; workspace 98, unchanged focused and
hidden controls, and four steady passes without extra launches/effects are
asserted. A dedicated fixture GIO subreaper verifies no owned descendants remain;
this substitutes for production `ExecProcessStarter` detachment semantics.

All application profiles, desktop approvals, databases, D-Bus connections and
compositor sockets are disposable. Neither normal verification nor an upgrade
runs desktop applications on the user's session. This is compositor-restart
acceptance, not an operating-system reboot or an authenticated browser session.

The workstation's Chrome 154.0.8037.57 system desktop entry contains a duplicate
`StartupWMClass` key. The strict catalog rejects malformed entries. A valid
disposable user entry can test Chrome and GIO without changing that system file
or weakening validation, but cannot establish acceptance of the installed stock
entry. No application-specific launcher wrapper is added to sway-session.

Slack 4.52.155 is installed as user Flatpak `com.slack.Slack`. Executable lookup
alone does not discover it. Its fixture uses the installed launcher on native
Wayland, a read-only installation, private PID/mount/network/IPC namespaces,
disposable Flatpak data and an owned Flatpak portal on a private session bus.
The existing signed-in profile is outside the test boundary. Network isolation
also means this is window/identity/placement evidence, not authenticated Slack
or online-content acceptance.

`TestDesktopAcceptanceSlackFlatpakPrivate` passed under the race detector with
Flatpak 1.18.4 and Sway 1.12. Both compositor phases observed Slack's native
app ID and sandbox application ID `com.slack.Slack`, engine `org.flatpak` and
distinct instance IDs. Explicit registration and its repeated invocation store
one unchanged context. A fresh compositor reloads the captured registry/layout
and starts `/usr/bin/flatpak run --user com.slack.Slack` once, restoring workspace
99. Four subsequent passes add no starts or placement effects.

The host verifies the root-owned Flatpak executable before entering the user
namespace, where kernel UID translation makes the ordinary root-owner check
inapplicable. The fixture verifies the same binary hash on the read-only mount
and injects that fixed resolution for registration. It uses the real Flatpak
installation probe and production `DesktopApplicationLauncher.SpecContext`,
with a tracking starter. It does not execute the complete default daemon
preparation adapter inside that namespace. Production executable validation
and launcher policies are unchanged.

An actual XWayland probe mapped Slack with class `com.slack.Slack`, but Sway
reported no sandbox application, engine or instance identity. The focused
resolver requires observed `sandbox_app_id` for a Flatpak catalog entry;
matching the advertised class is insufficient. This probe did not reach
registration or restore acceptance. Sway's sandbox fields come from the
Wayland client's security context, as implemented in
[Sway 1.12](https://github.com/swaywm/sway/blob/1.12/sway/tree/view.c#L199).
`TestResolveFocusedFlatpakRequiresObservedSandboxIdentity` covers missing and
wrong sandbox IDs with a positive control; removing the missing-ID guard makes
the Wayland and XWayland negative cases fail. No class-based Flatpak fallback is
introduced.

## Contract reconciliation and explicit deferrals

[LAB-250](https://linear.app/riotbox/issue/LAB-250/explain-desktop-application-lifecycle-and-restore-states-in-app)
delivers the remaining desktop inventory explanation contract through additive
commands. Bare `app list` preserves its offline saved-registration output and
JSON contexts envelope. Bare `app status [--socket PATH]` preserves focused
registration behavior, including the unregistered-window message.

- `app list --inventory [--socket PATH]` explains all saved application contexts.
- `app status --context UUID_OR_EXACT_LABEL [--socket PATH]` explains one saved
  application, regardless of active, archived or desired-open state.
- `app status --inventory [--socket PATH]` explains the focused registered
  application and requires Sway for selection.

List inventory and explicit context selection remain usable offline. Optional
presence and launcher observations share a three-second deadline after the
saved database snapshot closes. Unknown evidence remains unknown, including
nullable window counts and visibility; observed absence is distinct from saved
closed state. Ambiguous presence never supplies a guessed placement anchor.
Global `status` continues to provide its established live placement surface.

The inventory separates durable restore policy, live presence, launcher
validation, recorded launch guards and restore outcomes. It reuses existing
policy, group-identity and launcher validators. Flatpak validity additionally
requires an observed installation; validity is not a launch guarantee. A
recorded conservative duplicate-launch guard is not proof of an in-flight
process. Automatic and explicit pending outcomes retain their separate sources,
proof stages and current/changed/unknown identity applicability. Window mapping
is not proof of completed placement or layout. Saved placement-only fidelity
and recorded layout failure or timeout are explained without reconstructing a
new live eligibility decision.

The opt-in JSON result exposes a versioned `applications` projection with
`contexts:[]`. It omits approved launcher paths, process arguments, environment
and socket identity; labels and workspaces remain operational data. Reading
inventory does not launch, approve, repair or mutate session metadata. This is
a logical state-read contract, not a promise that SQLite never touches an
existing WAL or other database files. Fixture tests cover offline/unknown,
observed/ambiguous, pending/proof, launcher trust, privacy, compatibility and
read-only completion behavior. They do not add real-application acceptance.

The [LAB-253 real guest matrix](research/lab-253-desktop-reboot-acceptance.md),
completed 2026-10-15, adds actual graphical confirmation/cancellation, material
reapproval, Chrome multiwindow ambiguity with safe layout degradation, Follow
close under real logind protection, Pinned no-watchdog checks and Chrome/Slack
through two regular acceptance reboots. It records Chrome's actual chooser
with its main window retained; no main-window gap is claimed. Slack's empty
profile exposed neither optional multiwindow nor picker UI, and its XWayland
registration remains unsupported without observed sandbox identity.

The pinned vendor Debian Chrome entry was well formed and accepted; a separate
malformed duplicate-key fixture was rejected. The historical malformed Arch
entry remains unsupported. These version/distribution-specific results do not
loosen trust validation. The new opt-in KVM scenario uses real applications;
its ordinary synthetic checker tests alone remain insufficient acceptance.

The separate roadmap gates remain LAB-89 (first-process confinement and cold
resume), LAB-93 (durable unique client window tags), and LAB-94 (privileged
desktop launch authorization). Their research does not enable those features.
Application tabs, documents, authentication and conversation content remain
application-owned. No private app session files or terminal contents are
acceptance evidence.
