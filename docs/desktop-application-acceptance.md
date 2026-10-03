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

`app list` reports registered application contexts and durable policy. `app
status [--socket PATH]` reports the focused application's registration; it does
not accept a context argument. Global `status` adds observed application
presence and placement, with unknown values for ambiguous groups. These are
different observations, not the complete original list of pending, missing,
ambiguous and degraded lifecycle explanations. That remaining product work is
tracked in [LAB-250](https://linear.app/riotbox/issue/LAB-250/explain-desktop-application-lifecycle-and-restore-states-in-app).

The broader real-app matrix is explicitly deferred to
[LAB-253](https://linear.app/riotbox/issue/LAB-253/complete-real-chrome-and-slack-lifecycle-and-reboot-acceptance): graphical
confirmation/cancellation, actual profile-picker gaps and indistinguishable
multi-window restoration, Follow close under real logind shutdown protection,
and Chrome/Slack across two regular guest reboots. Deterministic and private
compositor tests cover the relevant decisions but do not substitute for those
application-specific runs. Stock malformed desktop-entry compatibility also
needs its own decision; silently accepting duplicate trust metadata is not an
acceptance fix.

The separate roadmap gates remain LAB-89 (first-process confinement and cold
resume), LAB-93 (durable unique client window tags), and LAB-94 (privileged
desktop launch authorization). Their research does not enable those features.
Application tabs, documents, authentication and conversation content remain
application-owned. No private app session files or terminal contents are
acceptance evidence.
