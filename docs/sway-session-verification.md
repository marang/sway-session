# Persistent Sway Session Verification

> **Interactive-desktop safety:** never run these procedures on a single-digit
> workspace. Create a disposable named workspace numbered 98 or higher, and
> verify every target workspace before issuing a move, restore, or close.

## Current process-boundary procedure (LAB-101)

Current builds run session observation and restore in `sway-session daemon`.
The title animator is tested separately and is not a prerequisite for any step
below. Before release, start the packaged daemon in one terminal:

```sh
/usr/bin/sway-session daemon
```

Then request the one-shot context launch from a second terminal:

```sh
/usr/bin/sway-session restore
```

With at least one registered context, verify that the daemon repairs its
`persist:<uuid>` mark, restores its saved workspace and layout after remapping,
and updates `layout.json` after the debounce interval. Then stop only the title
animator and repeat placement/layout restore; it must still converge. Finally,
restart the animator with the session daemon stopped and verify title animation
continues while neither `contexts.json`, `layout.json`, nor either session
socket is opened or created by the animator. Re-run the narrow broker boundary
check separately because moving its server did not change either protocol:

```sh
/usr/share/doc/sway-title-animator/scripts/verify-codex-boundary.sh \
  CONTEXT_UUID PANE_ID CODEX_SESSION_UUID HERDR_HISTORY STATE_FILE HERDR_SOCKET
```

The automated `make verify` gate includes a dependency/source boundary check,
an animator fake-Sway integration test with all session roots absent, daemon
capture/placement/layout tests, and the existing broker security tests.

## Desktop application group procedure (LAB-98)

Use an isolated XDG state root and disposable workspace 98 or higher. Register
one ordinary application, save its placement, close its last top-level window,
and confirm follow mode changes `desired_open` only after the two-second grace.
Queue it with `sway-session restore <context>` and verify the daemon launches it
after the five-second adoption interval, moves and marks only one unique anchor,
and writes `application-session.json` before the window maps.

Repeat with two indistinguishable top-level windows: the group must count as
present, no duplicate process may launch, and neither window may be guessed as
the layout anchor. Restart only the daemon and reload Sway; neither action may
repeat a recorded launch. A real compositor restart must create a new attempt
identity. During restore, manually focus or move a test window and confirm the
pending focus/layout work yields to that live action. Keep Chrome/Slack
application-internal restore prompts and additional windows app-owned; this
project restores one optional outer anchor, not private tabs, profiles, URLs,
or per-window application state.

## Visible desktop integration procedure (LAB-99)

Use only disposable named workspaces `98: LAB-99 E2E` and, when a landing
workspace is needed, `99: LAB-99 Landing`. Verify the focused workspace before
every move or close. Never create, move, close, or restore a test window on a
single-digit workspace.

1. Start the animator with an isolated config and the session daemon with
   isolated state/runtime roots. Before any successful desktop registration,
   confirm no application indicator is shown.
2. Register one ordinary app, then exercise a later `swaynag` approval so all
   four title states are visible: `○` unregistered, `◔` pending, `●`
   registered/follow, and `▲` pinned/autostart. Dismiss one approval and verify
   pending disappears after expiry. Check the glyphs with Sway configured for
   Noto Sans Mono on normal, tabbed, stacked, narrow, focused, and unfocused
   titlebars.
3. Cover a native Wayland app, an XWayland app, Chrome's single top-level
   application-owned restore, Slack Flatpak when installed, and an
   Alacritty/Herdr context on a mixed workspace. Herdr windows and classifiable
   XWayland dialogs must not receive desktop-app indicators. Record the known
   Sway 1.12 limitation that native Wayland parent/type metadata is unavailable;
   those surfaces remain part of the application-level group until LAB-93.
   Additional application-owned windows must not be rearranged.
4. Exercise register, rebind, reapprove after changing a disposable user-local
   desktop entry, pin, unpin, archive, activate, and forget. Compare
   `sway-session --json app list` with the private registry; do not publish its
   paths or checksums.
5. Reload the Sway config and confirm neither daemon nor restore is launched a
   second time. Replace the compositor/socket in an isolated environment and
   confirm the one-shot restore runs once. Stop the animator while the daemon
   restores, then stop the daemon while the animator continues animating.
6. Confirm one reconciliation pass emits one consolidated degraded diagnostic
   when multiple indicator/catalog or workspace details fail. Verify supported
   placement and capture continue despite the presentation failure.

Before handoff, run `make verify`, `make packaging-check`, the AppArmor policy
checks, the `code-review` workflow, and the repository-level architecture
checkpoint. Remove only the isolated roots, test entries, windows, and
workspaces created by this procedure.

### LAB-98 live evidence (2026-08-31)

The source-built daemon was exercised against the real Sway compositor with
isolated XDG state/runtime roots and a disposable user desktop entry launching
Alacritty under the unique `lab98-e2e` Wayland app ID. Every test window stayed
on `98: LAB-98 E2E`; no test window was created, moved, or closed on a
single-digit workspace.

- `app register-focused --yes` stored the approved desktop snapshot and added
  exactly one `persist:<uuid>` mark.
- The daemon captured the marked anchor and workspace 98 in `layout.json`.
- Closing the last top-level changed follow-mode `desired_open` to false only
  after the close grace.
- An explicit `restore <uuid>` changed it back to true; the per-compositor
  attempt was visible in `application-session.json` when the launched window
  was first observed, and the resulting anchor was marked on workspace 98.
- Restarting only the source daemon with the same real Sway socket left one
  window and one attempt: no launch was replayed.

The source daemon, both disposable Alacritty windows, its workspace, and all
isolated test state were removed after the run.

### LAB-99 live evidence (2026-08-31)

The visible desktop integration was exercised against the interactive Sway
1.12 compositor using only `98: LAB-99 E2E` and `99: LAB-99 Pending`. No test
window was created, moved, restored, or closed on a single-digit workspace.
State, runtime, desktop-catalog additions, binaries, and screenshots remained
under one disposable `/tmp` root.

- Before the first successful registration, the test Alacritty had neither a
  persistence mark nor an application indicator. A deliberately mutable test
  desktop entry was rejected by the launcher-trust boundary; registration was
  repeated with root-owned `/usr/share/applications/Alacritty.desktop`.
- `app register-focused --yes` enabled the opt-in latch, wrote one typed desktop
  context, and converged on the container-scoped registered mark. `app pin` then
  replaced it with the pinned mark. Real screenshots showed `●` and `▲`
  immediately before the Alacritty icon, and `app list` returned only the one
  desktop context as JSON.
- A second root-owned Deepin Calculator application received `○`, then `◔`
  while a one-time registration approval was active. This live case exposed
  that Sway reports the floating application leaf as `floating_con`; the
  animator had accepted only `con`. The window classifier and regression test
  were corrected, after which both states rendered in the real floating
  titlebar.
- With the animator stopped, a fresh daemon launched the pinned Alacritty,
  placed it on workspace 98, added its stable context mark, and added the
  container-scoped pinned indicator. The daemon was started through Sway for
  this step so its GUI child did not inherit the Codex AppArmor profile.
- After stopping that daemon, the source-built animator continued independently
  and rendered the restored window's `▲` title with no session daemon process
  running. The automated process-boundary test separately proves it opens no
  session state or session socket.

The two disposable application windows, both high-numbered workspaces, all
test processes, and the isolated roots were removed after the run. The wider
manual matrix above remains the procedure for Chrome/Slack, XWayland dialogs,
tabbed/stacked titlebars, and user-local reapproval; automated tests cover those
planner and approval contracts where no matching live application was used in
this run.

### LAB-101 live evidence (2026-08-31)

The process split was exercised against the interactive Sway compositor with
an isolated XDG state root, runtime root, Herdr configuration, registry, and
one disposable context on workspace 98. The installed animator was suspended
while the source-built `sway-session daemon` recognized the new window, added
its stable mark, captured a floating 520-by-360 outer rectangle, and restored
the window to workspace 98 with that rectangle after it was closed.

The restore process was launched by Sway itself and deliberately inherited
foreign `HERDR_*` and `CODEX_THREAD_ID` values. The typed launcher removed
those pane-local variables before starting the new Alacritty/Herdr context;
the restored window remained mapped and correctly placed throughout a
12-second stability observation. This also verifies that the session path does
not require a running animator. After stopping the session daemon, the
source-built animator ran against real Sway with separate empty state/runtime
roots: it created only its own instance-lock file and no session directory,
state file, or broker socket. The disposable context and Herdr state were
purged and all temporary test files were removed afterward.

This document records the manual LAB-80 evidence gathered on 2026-08-29. It
separates the outer Sway restore from components which were not available in
the verification environment.

## Environment

- Sway 1.12 with a fresh headless wlroots backend and a private IPC socket.
- Alacritty windows launched through the real `sway-session` executable.
- Five registered contexts with fixed UUIDs and a private XDG state root.
- A typed fake `herdr --session <name>` executable which kept each Alacritty
  window alive. It did not emulate Herdr panes, history, or agent resume.
- The normal repository Go 1.26.5 toolchain and automated verification gate.

The headless compositor was isolated from the interactive desktop session. Its
state, runtime directory, processes, and sockets were removed after the test.

## Observed results

The following outer-window behavior was exercised against real Sway IPC:

- Horizontal split, tabbed, and stacked child order restored and converged.
- Parent-relative split proportions restored, including a workspace-fullscreen
  child whose reported Sway `percent` temporarily changed to `1`.
- A floating Alacritty outer rectangle restored to `(100, 120, 420, 260)`.
  Sway reported a `(100, 147, 420, 233)` content rectangle plus a 27-pixel
  decoration; capture and planning converged on the command-visible outer
  rectangle.
- Workspace fullscreen and the saved focused context restored.
- No temporary `_sway_session_restore_...` marks remained after convergence.
- `swaymsg reload` preserved a normalized hash of all managed tree nodes and
  did not launch duplicate windows.
- An old compositor was terminated, a new Sway process with a new IPC socket
  was started, and the persisted stacked/floating/focus snapshot restored in
  the new tree. This was a real compositor restart, not a config reload.
- A context archived before `restore` was not launched. After activation, a
  later launch in the same compositor run restored its saved floating state.
- Removing the fixed Sway IPC endpoint ended the animator instead of leaving a
  reconnect loop against a dead compositor.

Failure exercises used separate state roots:

- An empty executable search path produced a structured
  `missing_executable` diagnostic and launched no context.
- Malformed registry JSON produced one structured state diagnostic, and its
  file hash was unchanged after the failed load.
- With two contexts, one valid context mapped while the other reported its
  vanished project directory. The valid context was not rolled back or
  suppressed by the independent failure.

The live checks exposed and led to regression tests for fullscreen proportion
capture, decorated floating geometry, empty-workspace focus, delayed window
mapping after the startup settle deadline, same-run archive/activate restore,
and disappearance of a fixed Sway endpoint.

## Commands and automated evidence

The final branch gate is:

```sh
GOTOOLCHAIN=go1.26.5 make verify
```

Focused session, planner, IPC, and daemon packages were also run repeatedly,
shuffled, and under the race detector during the manual fix/review loop.

## Explicit verification gaps

- No machine reboot was performed because that would disrupt the active user
  environment. The new-process compositor restart above verifies the same
  outer persisted-state boundary without claiming reboot coverage.
- Only one headless output was available. No physical second monitor was
  unplugged or reconnected, so output-hotplug behavior remains unverified.
- Herdr was not installed. Inner pane reconstruction, genuine pane-history
  display, resumed shells, and resumed Codex processes remain unverified. The
  fake executable verifies only the typed outer launcher contract.
- The updated AppArmor profile passed parser syntax validation, but the loaded
  user profile was not reloaded because the environment did not provide
  non-interactive privilege escalation. No live deny/allow claim is made.

These gaps do not weaken the automated negative tests around state paths,
Herdr sockets, the narrow Codex broker, or typed launcher input, but they must
remain distinct from end-to-end operational evidence.
