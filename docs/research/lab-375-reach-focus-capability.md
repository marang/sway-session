# LAB-375 Reach focus capability

Investigated 2026-10-16 for the
[named-environment design](../named-work-environments.md). This records a
capability boundary, not release acceptance or a change to user focus policy.

## Result

The supported Sway 1.12 interfaces examined do not provide a generic,
launch-scoped veto covering initial map and later activation. A late window can
take focus after a newer user selection even if sway-session sends no focus
command. The first version therefore blocks every missing-member launch before
starting a process. Existing uniquely anchored windows can still be focused in
place. On 2026-10-16 the user explicitly selected this safe existing-window
first version for LAB-375; missing-member opening is outside that delivery.

Sway's `should_focus` evaluates mapping on the active workspace independently
of activation tokens. Its first-window exception precedes the `no_focus` check.
`view_map` later applies that decision. Consequently `Focus=false` in our adapter
cannot prevent this compositor effect. Separately, mapped-view activation
follows `focus_on_window_activation`; a map-only rule does not cover every user
policy. [Sway 1.12 view.c](https://github.com/swaywm/sway/blob/1.12/sway/tree/view.c#L650)

`exec --no-startup-id` still creates launcher context and exports
`XDG_ACTIVATION_TOKEN`; it suppresses `DESKTOP_STARTUP_ID`, not compositor focus.
Workspace association is useful but does not establish revocable focus
authority. [Sway 1.12 exec_always.c](https://github.com/swaywm/sway/blob/1.12/sway/commands/exec_always.c#L25)

Unmapped-surface activation can associate launcher context, while an already
mapped surface can request activation. This protocol path supplies no
per-request no-focus cancellation contract.
[Sway 1.12 xdg_activation_v1.c](https://github.com/swaywm/sway/blob/1.12/sway/xdg_activation_v1.c)

`no_focus` adds persistent compositor criteria, rather than a scoped guard with
begin/end/revoke operations. Changing global configuration or leaving temporary
criteria behind would add authority and failure modes outside this feature.
[Sway 1.12 no_focus.c](https://github.com/swaywm/sway/blob/1.12/sway/commands/no_focus.c)

## Actual private-compositor proof

Two disposable runs used Sway 1.12, Alacritty 0.17.0, isolated HOME/XDG/config
roots, empty terminal profiles running `sleep`, and workspaces 98, 99 and 100.
A 1.2-second delay before mapping allowed a newer explicit IPC focus/workspace
selection after process start. Owned children, process groups and roots were
cleaned up. No host Sway configuration, private app state or user windows were
used. Earlier sandbox attempts that could not create a Wayland socket were
cleaned up and do not count as successful proof runs.

| Delayed launch and newer selection | Actual result |
| --- | --- |
| Direct launch on 98; select populated 99 | Late window maps on 99 and takes focus |
| Sway `exec --no-startup-id` on 98; select populated 99 | Late window stays on 98; selected 99 window keeps focus |
| Same Sway launch; explicitly select an existing 98 window | Late window takes focus on 98 |
| Exact configured `no_focus`; existing window selected on populated 98 | Selected window keeps focus |
| Same `no_focus`; select empty 100 before first window maps | Late first window takes focus |

These are terminal map-focus counterexamples. Selection was an explicit IPC
action, not physical input. Positive final samples alone do not prove the
absence of transient focus changes. The runs do not establish behavior for real
Chrome/Slack, Flatpak, XWayland activation or every application token path. They
are sufficient to disprove a generic guarantee from workspace association or
ordinary `no_focus`; they do not prove no future compositor integration can work.

## Supported existing-window evidence

`TestReachExistingWindowsHeadless` separately exercises the candidate's supported
execution path with a real subscribed event stream: a managed terminal on 98,
an ordinary custom-app-ID Alacritty app surrogate on 99 with desired-open false,
shared definitions and a registered missing worktree member. All five cases ran
without skips, normally and with the race detector. Exact window IDs, counts,
workspaces and stored definitions/lifecycle intent stayed unchanged; commands
were focus-only and no application launch was admitted. A real focus event
between observations canceled stale Reach.

This is executor/IPC evidence, not a full TUI-to-Reach-RPC run, real browser
session-content restore or proof of concurrent broker-client behavior. Broker
framing/authentication/cancellation and dispatcher concurrency have separate
deterministic tests. The interactive 48×16 PTY smoke covered fresh-state
discovery, Unicode editing/cancel, empty-group creation/rename, picker, blocked
no-primary preview, filtering, resizing and clean quit, without host IPC.

## Gate for any future missing launch

A safe capability must associate suppression with the exact intended launch,
cover map and later activation, remain effective after process start and newer
selection, and work through the approved terminal/GIO/Flatpak routes. It must
also handle empty-workspace races, existing user rules, reloads and cancellation.
Restoring focus after a late map cannot undo the initial interruption. No
compositor extension, global policy rewrite or replacement launcher was added.
