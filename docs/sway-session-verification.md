# Persistent Sway Session Verification

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
