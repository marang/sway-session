# Persistent Sway Work Sessions

Status: Implemented on the LAB-80 feature branch; pending merge

Tracking issue: [LAB-80](https://linear.app/riotbox/issue/LAB-80/add-persistent-sway-work-session-restoration)

## Purpose

Add opt-in persistence for explicitly registered Sway work contexts. A work
context owns one outer Alacritty window backed by one named Herdr session.
Sway restores the outer window's workspace and layout, while Herdr restores
the terminal panes, pane history, and resumable agents inside that window.

The feature is split between the existing long-running title animator and a
new `sway-session` command. Both programs live in this repository and share
small internal packages for bounded Sway IPC, state, identity, and restore
planning.

## Goals

- Restore active work contexts automatically after a real Sway start.
- Persist only windows that explicitly opt in with a `persist:` identity.
- Restore workspace placement without duplicating Sway's output assignment
  behavior.
- Reconstruct split, tabbed, and stacked layout state as reliably as current
  Sway commands allow.
- Keep the restore operation idempotent and isolate failures by context and
  workspace.
- Preserve terminal screen contents through Herdr's experimental pane-history
  support.
- Preserve the existing AppArmor boundary around Codex and avoid exposing the
  general Herdr control socket.
- Keep runtime state private, versioned, atomic, and outside Git.

## Non-goals

- Capturing or launching arbitrary unregistered desktop applications.
- Persisting output assignments. Sway remains responsible for mapping
  workspaces to currently available outputs.
- Executing shell command strings stored in runtime state.
- Restoring arbitrary process memory or shell state independently of Herdr.
- Supporting multiple outer Sway windows for one context in the first version.
- Depending on an unreleased or locally patched Sway build.
- Replacing Herdr's internal pane and agent-session persistence.

## Decisions

1. Only explicitly registered contexts are managed. Temporary browser windows,
   dialogs, and unrelated terminals remain untouched.
2. Every context has an immutable UUID. Human labels and provider references,
   such as `LAB-123`, are mutable metadata rather than technical identity.
3. A managed window uses the mark `persist:<uuid>` and a stable generic
   application ID derived from the UUID. Provider names never appear in the
   application ID contract.
4. Closing a window does not deactivate its context. The context opens again
   on the next restore unless explicitly archived.
5. `archive` is reversible. `purge` is the only operation that permanently
   removes context state and requests removal of the corresponding Herdr
   session and pane history.
6. Automatic restore runs once during an actual Sway start, never during
   `swaymsg reload`.
7. Restore is best effort per workspace until Sway ships a stable declarative
   layout-restore interface.
8. The first version supports exactly one outer Alacritty/Herdr window per
   context. Repeated restore operations reuse that window instead of creating
   duplicates.
9. Launch metadata is typed. The first launcher kind is `herdr`; no state file
   contains a command interpreted by a shell.
10. Codex does not receive access to the general Herdr control socket. Native
    resume metadata crosses a narrow, validated reporting boundary.
11. A workspace containing both managed and unregistered tiled windows
    degrades to workspace placement only. Version 1 never removes an
    unregistered leaf from a saved tree and then claims the remaining layout
    can be restored exactly.
12. A typed launcher identity is registry-wide unique. For Herdr, the identity
    is the launcher kind plus validated session name, including for archived
    contexts.

## Architecture

```text
                         Sway IPC
                            |
            +---------------+----------------+
            |                                |
  sway-title-animator                  sway-session
  long-running daemon                  one-shot CLI
            |                                |
  observe marked windows              manage contexts
  persist layout tree                 launch missing windows
  apply restore plans                 archive / activate / purge
            |                                |
            +---------------+----------------+
                            |
                  XDG state directory
                            |
                  Alacritty + Herdr
                            |
                panes, history, Codex resume
```

### Component responsibilities

`sway-title-animator` remains the single long-running Sway listener. Its new
session-state component:

- observes marked windows and their relevant ancestors in `GET_TREE`;
- persists meaningful layout changes after a debounce period;
- recognizes managed windows when they appear;
- moves those windows to saved workspaces; and
- applies bounded Sway command plans for layout, size, floating state,
  fullscreen state, and focus.

`sway-session` is a separate binary with a one-shot CLI lifecycle. It:

- owns the context registry;
- lists, registers, archives, activates, restores, and purges contexts;
- detects an already mapped context before launching anything;
- invokes only configured, typed launcher adapters; and
- reports failures without preventing other contexts from restoring.

Herdr owns everything inside the terminal window:

- named sessions;
- tabs and panes;
- pane processes;
- saved pane screen contents; and
- supported agent-session resume metadata.

## Runtime state

The default state root is:

```text
${XDG_STATE_HOME:-$HOME/.local/state}/sway-session/
```

It contains two files with separate writers:

```text
contexts.json   # written by sway-session
layout.json     # written by sway-title-animator
```

The directory uses mode `0700`; regular state files use mode `0600`. The state
directory is opened without following symlinks, then all reads, temporary-file
creation, renames, cleanup, and directory syncs are performed relative to that
held directory descriptor. Non-regular files are opened non-blocking and
rejected after descriptor-based type, owner, mode, and link-count checks.

Writes use a temporary file in the held directory followed by an atomic rename.
Every read-modify-write operation holds the state-directory lock from load
through validation, mutation, rename, and directory sync so concurrent CLI
processes cannot lose each other's changes. The documents include explicit
schema versions.

If the atomic rename succeeds but the following directory sync fails, the new
document is already visible while its crash durability is unknown. The state
layer reports that condition with a typed error and returns the visible
candidate; callers must reload and reconcile before retrying the mutation or
performing dependent external side effects.

"Preserve the last valid version" has a deliberately fail-closed meaning in
version 1: an invalid new candidate never replaces the valid on-disk file, and
a failed load never replaces a caller's already loaded in-memory value. If the
primary file is already malformed when a process starts, the process reports an
actionable error and does not automatically fall back to a `.bak` copy. A future
disk-recovery design would require generations or tombstones so an old backup
cannot resurrect an archived or purged context.

### Context registry

The exact schema will be finalized with tests, but its semantic shape is:

```json
{
  "version": 1,
  "contexts": [
    {
      "id": "immutable-uuid",
      "label": "LAB-123",
      "provider": "linear",
      "state": "active",
      "launcher": {
        "kind": "herdr",
        "session": "lab-123",
        "cwd": "/home/markus/Dev/example"
      }
    }
  ]
}
```

`provider` and `label` are optional presentation metadata. Launcher fields are
validated values, not executable command fragments. Executable paths and
fixed argument templates come from trusted program configuration or compiled
adapter policy. Version 1 bounds the registry at 128 contexts, matching the
worst-case two placement operations per context under the 256-action planner
limit.

### Layout snapshot

The layout snapshot stores only workspaces containing registered marked
windows. It includes:

- workspace name or number;
- a required restore mode (`layout` or `placement_only`);
- stable context identity for each managed leaf;
- parent layout kinds (`splith`, `splitv`, `tabbed`, and `stacked`);
- child order and parent-relative tiled proportions in the range `[0, 1]`;
- focused descendant;
- required geometry for every top-level floating entry, with normal nested
  layout and proportion state for descendants; and
- leaf or nested-container fullscreen state, limited to one fullscreen node per
  workspace and one global fullscreen node per snapshot.

`layout` mode contains only managed leaves and the restorable tree state.
`placement_only` contains only the managed context identities: it moves those
windows to the workspace but stores and applies no tiling, ordering, proportion,
floating, fullscreen, geometry, or focus state. Capture must select
`placement_only` whenever a saved tiling or grouped-floating subtree mixes
managed and unregistered leaves. Independent unregistered floating windows do
not affect managed layout subtrees. Unregistered windows therefore remain
outside the saved layout contract instead of being silently removed from the
tree.

It deliberately does not persist output names. When an output disappears,
Sway moves its workspaces. When an output returns, existing Sway workspace
assignment rules remain authoritative. Restored floating geometry is clamped
to the currently visible workspace rectangle.

## Capture behavior

The existing daemon already subscribes to Sway window, workspace, and shutdown
events. Session persistence will retain typed event information instead of
reducing every event to an empty notification.

On a relevant structural or focus event, the daemon obtains a fresh bounded
`GET_TREE`, extracts only registered marked windows and their ancestors,
computes a semantic hash, and schedules an atomic write after approximately one
quiet second. Animation frames and presentation-only title or urgency events
never trigger or postpone state writes by themselves.

Because Sway does not publish an IPC event for every resize or geometry
change, an existing registry also enables a low-frequency semantic `GET_TREE`
observation. Unchanged trees do not schedule writes, and a missing registry
keeps this observation disabled.

Before extracting a restorable tree, capture inspects the complete workspace.
If it finds an unregistered tiled leaf alongside a managed leaf, it records the
workspace as `placement_only`. It does not collapse `managed A | unregistered B
| managed C` into the misleading tree `A | C`.

Snapshot guards prevent destructive transient writes:

- an empty tree during startup does not replace the previous snapshot;
- disappearing windows during Sway shutdown do not replace the previous
  snapshot;
- an IPC disconnect keeps the previous snapshot intact; and
- restore-in-progress mutations are not considered the new user preference
  until the restore reaches a stable state.

Startup waits up to ten seconds for active contexts already present in the
previous snapshot. After that bounded settling period, a fresh observation may
update complete workspaces while incomplete workspaces retain their last exact
tree. Archived contexts do not block startup and keep their saved placement.

Scratchpad contents are outside the version 1 restore contract. Moving a
managed window temporarily to Sway's synthetic scratchpad workspace therefore
does not replace its last saved real-workspace placement.

## Restore flow

1. Sway starts the title animator and runs `sway-session restore` once.
2. `sway-session` loads and validates the active context registry.
3. It reads `GET_TREE` and identifies existing managed windows by stable
   application ID or `persist:<uuid>` mark.
4. Existing windows are reused. Each missing context is launched exactly once
   through the Herdr adapter.
5. The title animator observes each mapped window, applies its stable mark, and
   moves it to the saved workspace.
6. After the workspace's expected managed windows have appeared or a bounded
   settling timeout expires, a pure restore planner translates the saved tree
   into Sway commands.
7. For each exact-layout workspace, managed leaves are first moved to a
   reserved staging workspace. The planner then rebuilds groups top-down using
   deterministic temporary marks, followed by proportions, floating state and
   clamped geometry, fullscreen state, and focus. Existing matching groups are
   reused rather than rebuilt.
8. Each mutation is acknowledged and followed by a fresh tree observation.
   Temporary marks and the staging workspace make an interrupted restore
   discoverable after a daemon restart. Neither is captured as user state.
9. An explicit structural failure rolls the managed leaves back to their saved
   workspace in placement-only form and excludes that workspace from the
   remainder of the startup restore. A presentation-detail failure is reported
   and skipped, so other details and workspaces can continue. The last good
   snapshot remains the persistence base throughout reconstruction and is not
   replaced by the immediate degraded tree while that workspace still contains
   the same managed-context set. A later context move, addition, or removal
   releases that quarantine as a new user or lifecycle decision.

Read-only IPC requests may reconnect and retry automatically. A connection
failure after sending `RUN_COMMAND` has an unknown outcome and is never retried
blindly. The restore coordinator obtains a fresh `GET_TREE`, compares observed
state with the desired state, and replans only commands that remain necessary.

The title animator sends `RUN_COMMAND` messages through the existing bounded
Sway IPC implementation. It does not spawn `swaymsg` or build shell commands.
Automatic reconstruction applies only to contexts first observed without their
stable mark during the current startup. A pre-existing marked window is treated
as the user's current preference. A connection loss while applying the mark
does not lose this new-window classification.

### Best-effort compatibility

Sway 1.12 exposes enough state through `GET_TREE` to capture the desired
structure but does not provide released `append_layout` support. The first
implementation therefore reconstructs layouts with normal runtime commands.
It must report any detail it cannot reproduce rather than claiming an exact
restore.

Runtime `split` commands cannot reliably recreate a distinct layout parent
which contains only one child. A saved workspace requiring such a singleton
group is therefore reported and kept at workspace placement only; an already
matching live singleton group is left untouched.

If a future stable Sway release adds declarative layout restore, that support
can become a new planner backend without changing context identity or the
persisted semantic model.

## CLI contract

The user-facing commands are:

```text
sway-session register --session <name> [--cwd <path>] [--label <label>] [--provider <name>] [--id <uuid>]
sway-session restore [--socket <path>] [context]
sway-session list
sway-session archive <context>
sway-session activate <context>
sway-session purge [--yes] <context>
sway-session report-codex-session # Codex SessionStart hook only
```

Commands accept an exact canonical UUID or an unambiguous exact human label.
Human-readable output uses labels first and retains the full UUID so duplicate
labels remain operable. Machine consumers receive an explicit structured-output
option rather than parsing presentation text.

`restore` is idempotent. `archive` removes a context from automatic restore but
keeps its registry record and Herdr state. `activate` reverses archive. `purge`
requires deliberate confirmation in an interactive terminal, with an explicit
non-interactive confirmation flag for automation.

Herdr session names follow Herdr's 64-byte ASCII name contract. The reserved
name `default` is rejected because Herdr maps it to non-deletable default state.
Concurrent restores hold the registry lock across observation and launch.
They also recognize an exact pending Alacritty argument vector in `/proc`, so
a process which started before its Wayland window mapped is not launched again
after a mapping timeout.

## Herdr integration

Herdr must be configured with pane history enabled:

```toml
[experimental]
pane_history = true
```

Pane history may contain terminal output, tokens, paths, or other sensitive
material. Herdr's state must remain owner-only. Full-disk encryption protects
it only while the machine is powered off; archive keeps it, while purge is the
explicit deletion boundary.

Owner-only file modes do not isolate another process running under the same UID.
The Codex AppArmor profile must therefore deny direct access to Herdr history
and session state in addition to protecting the control socket.

The Herdr launcher uses a fixed executable and argument structure to attach to
a validated named session. The stable Alacritty application ID is generic and
derived from the context UUID, not from Linear or another provider.
Before launch, `sway-session` validates owner-only, symlink-free Herdr state
and config paths and the pane-history opt-in. Purge independently validates the
state root, discovers the exact named session through Herdr's structured list,
stops it if running, re-observes it, deletes it, and verifies absence before
removing the registry entry.

## Codex and AppArmor boundary

Codex must not connect to Herdr's general same-user control socket. That socket
can control panes and could turn an allowed integration into command injection
against an unconfined terminal process. Codex must also be denied direct read,
write, link, rename, and deletion access to Herdr session/history files and the
`sway-session` state root. The broad `allow all` profile requires explicit deny
rules for every finalized state and runtime path.

Resume support instead uses a narrow reporter with one operation:

```text
known context UUID + valid Codex session ID -> record association
```

The reporter cannot inject pane input, run Herdr commands, select arbitrary
executables, change launch metadata, or expose the underlying state files. It
runs as a narrow broker outside the Codex confinement boundary; Codex receives
access only to that broker's validated reporting endpoint. AppArmor policy and
negative tests must prove the general Herdr socket and both state roots remain
inaccessible from Codex while the reporting path works.

The hook also carries Herdr's bounded current-pane identity as routing metadata.
The broker never accepts a socket path or method from the hook: it resolves the
context's unique typed launcher from one validated atomic registry snapshot and
confines the pane identity to that named session's fixed `herdr.sock`. A fixed
read-only `pane.process_info` request plus the reporter's kernel `SO_PEERCRED`
PID proves that the reporting process descends from the selected pane before
the sole mutation is sent. The hook ignores Codex transcript paths and launch
commands. Herdr's native adapter derives the only resume command, the typed
`codex resume <validated-uuid>` form, from the saved association.

## Sway startup

The intended Sway configuration is:

```text
exec_always --no-startup-id sway-title-animator --replace
exec --no-startup-id sway-session restore
```

The restore command intentionally uses `exec`, not `exec_always`, so a config
reload cannot duplicate windows. Startup ordering is race-safe: the animator
performs an initial tree refresh, and the continuing event subscription catches
windows mapped after that refresh.

Session persistence is opt-in and disabled by default for existing users until
configured. Removing the one-shot restore line stops automatic launches without
deleting state.

## Failure behavior

- A malformed registry or layout file produces one actionable diagnostic and
  never triggers partial interpretation as executable input.
- A missing Herdr or Alacritty executable fails the affected context only.
- A missing project directory is reported and not silently replaced with the
  home directory.
- A duplicate stable application ID is treated as an ambiguity and does not
  launch another window.
- A missing workspace is created by moving the window to its saved workspace;
  Sway then applies its normal workspace/output configuration.
- An unsupported layout detail is logged as degraded restore while supported
  placement continues.
- A mixed managed/unregistered tiled workspace is explicitly restored as
  workspace placement only.
- An interrupted `RUN_COMMAND` response triggers fresh observation and
  replanning, never automatic command replay.
- Repeated `restore` calls converge on one window per active context.

## Delivery plan

### Phase 1: Shared foundations

- Extract bounded IPC framing and requests into an internal package without
  changing current title-animation behavior.
- Add validated identity and versioned state types.
- Add owner-only atomic state persistence, descriptor-relative path handling,
  complete read-modify-write locking, and corruption/concurrency tests.
- Introduce the `sway-session` command skeleton and structured diagnostics.

### Phase 2: Capture and placement

- Preserve typed Sway events in the daemon.
- Extract marked workspace trees from `GET_TREE`.
- Detect mixed managed/unregistered tiling and persist placement-only
  degradation.
- Add debounced semantic snapshots and startup/shutdown guards.
- Sample `GET_TREE` at a low frequency while a registry exists because Sway
  does not emit IPC events for every resize or geometry change; animation
  frames never drive session persistence.
- Recognize stable application IDs, apply marks, and restore workspaces.

### Phase 3: Layout reconstruction

- Implement a pure saved-tree-to-command planner.
- Restore nested split, tabbed, and stacked layouts.
- Restore ordering, proportions, floating state, fullscreen state, and focus.
- Add failure isolation and explicit degraded-restore reporting.
- Re-observe and replan after commands with an unknown outcome.

### Phase 4: Context lifecycle and Herdr

- Implement register, list, restore, archive, activate, and purge.
- Add the typed Alacritty/Herdr launcher and duplicate detection.
- Enforce registry-wide uniqueness of typed launcher identities.
- Enable and validate Herdr pane history.
- Add safe startup configuration and packaging for both binaries.

### Phase 5: Secure Codex resume

- Implement the narrow session-ID reporting boundary.
- Extend the Codex AppArmor policy to deny the general Herdr socket, direct
  Herdr history/session access, and direct `sway-session` state access.
- Test allowed reporting and denied pane-control/file-access paths separately.
- Verify resumed Codex sessions do not require replaying arbitrary original
  command lines from state.

### Phase 6: Verification and rollout

- Run automated state, planner, IPC, lifecycle, and race tests.
- Manually exercise split, tabbed, stacked, floating, fullscreen, and focus
  restoration in a real Sway session.
- Test a compositor reload, a complete reboot, archived contexts, missing
  executables, malformed state, and partial restore failure.
- Run `make verify` and the repository `code-review` workflow.
- Document observed limitations and create only bounded follow-up issues.

## Test matrix

Automated tests must cover:

- UUID and mark validation;
- schema-version rejection and migration;
- safe path, ownership, mode, symlink, and atomic-write behavior;
- non-blocking rejection of FIFOs and descriptor-relative operation after a
  directory-path replacement;
- coordinated two-process read-modify-write serialization;
- post-rename directory-sync failure reporting and visible-state
  reconciliation;
- preservation of the on-disk file after an invalid write candidate and the
  in-memory value after an invalid load;
- semantic snapshot hashing and debounce behavior;
- startup and shutdown empty-tree guards;
- restore planning for nested split, tabbed, and stacked trees;
- placement-only capture and planning for mixed managed/unregistered tiling;
- floating geometry clamping;
- one-window-per-context duplicate prevention;
- registry-wide typed launcher-identity uniqueness;
- archive, activate, and purge transitions;
- typed launcher validation and absence of shell evaluation;
- per-context and per-workspace failure isolation;
- IPC reconnects, bounded payload handling, and no replay of ambiguous
  mutating commands; and
- positive narrow-agent reporting plus negative Herdr-socket and direct-state
  access.

Manual evidence must distinguish:

- Sway config reload from a real Sway restart;
- one-monitor behavior from unplugging and reconnecting a second monitor;
- outer Sway layout restoration from inner Herdr pane restoration;
- pane-history display from a genuinely resumed shell or Codex process; and
- exact restore from explicitly reported best-effort degradation.

## Deferred extensions

- Multiple outer windows per context via explicit child identities.
- Additional typed launchers for other terminal multiplexers or applications.
- A stable declarative-layout backend after upstream Sway support is released.
- Optional retention policies for archived Herdr history.
- A richer interactive session browser after the core CLI is proven reliable.

These extensions must not weaken stable identity, typed launch policy, or the
Codex/AppArmor boundary established by the first version.
