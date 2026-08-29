# Persistent Sway Session Verification

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
