# Persistent Sway Work Sessions

Status: Core Herdr work-session restore, the dedicated session-daemon process,
explicit desktop application group restore and visible integration, shell
completion, and the typed terminal adapter contract are implemented
through LAB-106.

Tracking issue: [LAB-80](https://linear.app/riotbox/issue/LAB-80/add-persistent-sway-work-session-restoration)

## Purpose

Add opt-in persistence for explicitly registered Sway work contexts. A work
context owns one outer typed terminal-adapter window backed by one named Herdr
session; Alacritty is the compiled default adapter.
Sway restores the outer window's workspace and layout, while Herdr restores
the terminal panes, pane history, and resumable agents inside that window.

The feature is owned entirely by the `sway-session` command and its explicit
long-running daemon. The title animator is a separate animation/audio process
with no session state, registry, restore, Herdr, or broker responsibility. Both
programs remain in this repository and may share the small bounded Sway IPC
package. Explicitly registered normal desktop applications reuse the session
identity and layout model; this does not turn restore into an automatic process
or window recorder.

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
- Let explicitly registered normal desktop applications share the existing
  UUID, mark, workspace, and layout model without persisting their private
  in-application state.

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

1. Only explicitly registered contexts are managed. Unrelated terminals and
   Sway-classifiable XWayland transient/dialog windows remain untouched. Native
   Wayland surfaces sharing one `app_id` cannot be classified by parent/type in
   Sway 1.12 and therefore remain application-group presence until LAB-93 adds
   stable per-window tags.
2. Every context has an immutable UUID. Human labels and provider references,
   such as `LAB-123`, are mutable metadata rather than technical identity.
3. A managed window uses the mark `persist:<uuid>` and a stable generic
   application ID derived from the UUID. Provider names never appear in the
   application ID contract.
4. Closing a Herdr context window does not deactivate its context. The context
   opens again on the next restore unless explicitly archived. Registered
   desktop applications instead persist their explicit desired-open state.
5. `archive` is reversible. `purge` is the only operation that permanently
   removes context state and requests removal of the corresponding Herdr
   session and pane history.
6. Automatic restore runs once during an actual Sway start, never during
   `swaymsg reload`.
7. Restore is best effort per workspace until Sway ships a stable declarative
   layout-restore interface.
8. The first version supports exactly one outer terminal-adapter/Herdr window
   per context. Repeated restore operations reuse that window instead of
   creating duplicates. LAB-105 additionally gives `sway-session terminal`
   its own stable default identity and hashed named project identities; it does
   not solve arbitrary per-window application identity, which remains LAB-93.
9. Launch metadata is a validated tagged union. Registry schema version 5 is
   the only supported on-disk form. It models Herdr, system desktop entries,
   approved user-local desktop entries, Flatpak application IDs, a closed
   terminal adapter (`alacritty` or `foot`), optional stable terminal identity,
   archive time, and an explicit fresh-terminal-instance discriminator. Fresh
   instance session names use the UUID without separators. No state file
   contains a generic command, argument vector, environment, or value
   interpreted by a shell.
10. Codex does not receive access to the general Herdr control socket. Native
    resume metadata crosses a narrow, validated reporting boundary.
11. A workspace containing both managed and unregistered tiled windows
    degrades to workspace placement only. Version 1 never removes an
    unregistered leaf from a saved tree and then claims the remaining layout
    can be restored exactly.
12. A typed launcher identity is registry-wide unique. For Herdr, the identity
    is the launcher kind plus validated session name, including for archived
    contexts.
13. Desktop persistence state crosses the process seam only as one versioned,
    underscore-prefixed, container-scoped Sway mark per eligible window. The
    daemon derives it from registry and approval state; the animator renders it
    without reading session state.

## Architecture

```text
                         Sway IPC
                            |
            +---------------+----------------+
            |                                |
  sway-title-animator                  sway-session daemon
  animation + audio                    session runtime
            |                                |
  title_format only                    observe / mark / place
            ^                                |
            +---- hidden indicator mark -----+
                                             |
                                      capture / layout restore
                                             |
                                      narrow broker endpoints
                                             |
                                      XDG session state
                                             |
                                 terminal adapter + Herdr
                                             |
                                  panes, history, Codex resume
```

### Component responsibilities

`sway-title-animator` is an independent animation/audio process. It:

- reads only the tree data needed to render and update title formats;
- renders a configured glyph for a versioned presentation-only indicator mark;
- optionally analyzes live audio for sound-reactive presets; and
- does not open session state, session sockets, the registry, or Herdr.

`sway-session` is a separate binary with one-shot lifecycle commands and an
explicit long-running `daemon`. It:

- owns the context registry;
- lists, registers, archives, activates, restores, and purges contexts;
- detects an already mapped context before launching anything;
- invokes only configured, typed launcher adapters;
- observes registered windows, repairs stable marks, and restores placement;
- derives unregistered, pending, registered/follow, and pinned presentation
  marks for eligible normal top-levels after the durable indicator preference
  is activated;
- captures debounced semantic layout snapshots and applies bounded restore
  plans for layout, size, floating state, fullscreen state, and focus;
- hosts the existing narrow session-start and Codex-report endpoints without
  expanding either protocol; and
- reports failures without preventing other contexts from restoring.

The desktop-app registration surface resolves only the currently focused
eligible top-level window, or a previewed current-workspace batch. A healthy
already-registered focus is a status/mark-repair no-op rather than a toggle.
Ambiguity is presented through owner-only, single-use, two-minute operation
tokens under `${XDG_RUNTIME_DIR}/sway-session/app-operations`; the token schema
cannot carry executable commands, arguments, environments, titles, or URIs.
`swaynag` receives only a fixed root-owned `sway-session app confirm <token>`
action. The Codex profile denies those token files, and neither application
registration nor confirmation is exposed by `sessionrequest`.

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

It contains the active session documents with separate purposes:

```text
contexts.json   # written by sway-session lifecycle commands and brokers
layout.json     # written by sway-session daemon
application-runtime/
  application-session.json # per-compositor conservative launch attempts
desktop-approvals/ # immutable approved user-local .desktop snapshots
```

The owner-only `${XDG_RUNTIME_DIR}/sway-session/` directory contains the
exclusive `daemon.lock` and short-lived confirmation tokens rather than
persistent state. At most 256 token files are retained; expired tokens are
pruned and every confirmation consumes its token before applying work.

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

`application-session.json` identifies one compositor lifetime from the
current-user-owned Sway socket inode and records each desktop launch intent before the
typed process starter runs. A config reload retains that identity; replacing
the compositor socket creates a fresh session and attempt budget. This is not a
process watchdog: a failed, ambiguous, crashed, or deliberately closed app is
not automatically retried in the same compositor lifetime.

If the atomic rename succeeds but the following directory sync fails, the new
document is already visible while its crash durability is unknown. The state
layer reports that condition with a typed error and returns the visible
candidate; callers must reload and reconcile before retrying the mutation or
performing dependent external side effects.

"Preserve the last valid version" has a deliberately fail-closed meaning: an
invalid new candidate never replaces the valid on-disk file, and a failed load
never replaces a caller's already loaded in-memory value. Schema version 5 is
the sole accepted registry form. Version 1 through version 4, malformed, and
unknown-version input is left byte-for-byte untouched and is never partially
interpreted. A general disk-recovery design would require generations or
tombstones so stale state cannot resurrect an archived or purged context.

### Context registry

The registry schema is version 5. Its terminal-instance discriminator and
short UUID-derived session spelling are validated as current-schema invariants;
provider or session text cannot infer an instance identity. Earlier pre-release
schemas are unsupported and fail closed without changing the file.

Before first use of this release, stop every `sway-session daemon` process and
remove `${XDG_STATE_HOME:-$HOME/.local/state}/sway-session/`. This one-time
reset deliberately forgets contexts, layouts, desktop approvals, and
application runtime state; it does not automatically remove Herdr sessions or
pane history. Normal registry mutations serialize on the state-directory lock;
the independent owner-only runtime `daemon.lock` still excludes a second daemon.

The registry's Herdr-compatible shape is:

```json
{
  "version": 5,
  "preferences": {
    "desktop_indicators": false
  },
  "contexts": [
    {
      "id": "immutable-uuid",
      "label": "LAB-123",
      "provider": "linear",
      "state": "active",
      "launcher": {
        "kind": "herdr",
        "session": "lab-123",
        "cwd": "/home/markus/Dev/example",
        "terminal": {
          "adapter": "alacritty",
          "identity": {
            "kind": "project",
            "project": "LAB-123"
          }
        }
      }
    }
  ]
}
```

`provider` and `label` are optional presentation metadata. Launcher fields are
validated values, not executable command fragments. Executable paths and fixed
argument templates come from trusted program configuration or compiled adapter
policy. The registry remains bounded at 128 contexts, matching the worst-case
two placement operations per context under the 256-action planner limit.
`desktop_indicators` is a durable activation latch: it starts false, becomes
true in the first successful desktop-registration transaction, and is not reset
by later archive or forget operations.

### Typed terminal identities

`sway-session terminal --new` creates a fresh persistent terminal context on
every invocation. The context UUID is also its unique Sway application
identity and deterministically names its unique Herdr session. Fresh contexts
have no reusable lookup key: agents address each one by the UUID returned in
the versioned JSON result. Concurrent calls serialize registry creation but
produce separate UUIDs, sessions, processes, and independently restorable
windows.

`sway-session terminal` is a narrow terminal launch surface, separate from
manual `register` contexts. With no options it creates or reuses one default
stable identity. `--project NAME` creates or reuses a project identity and
derives a bounded Herdr session name from a hash of the validated name; the
project text is never used as a Herdr argument or filesystem name. A new
project identity defaults to the caller's current directory, while a new
default identity defaults to the caller's home directory. An explicit cwd must
exist and, on reuse, match the persisted cwd exactly; a mismatch is an identity
conflict. The adapter is persisted with a new identity. A different configured
adapter produces a typed conflict rather than a silent launcher change. The
non-destructive switch sequence is archive, close the mapped terminal,
`terminal reconfigure` (optionally with `--project NAME`), then activate;
context ID, Herdr session, cwd, and pane history remain unchanged. Reconfigure
holds the registry transaction while it proves through Sway and exact process
observation that no window or pending old-adapter launch remains. Reusing an
archived identity fails with its UUID and requires an explicit `activate`; it
is never launched outside daemon management while still archived.

`--ephemeral` opens an ordinary terminal through the same closed adapter
contract but has no identity and does not read or write terminal registry
state. It accepts only optional cwd selection. The strict version-2 terminal
configuration lives at
`${XDG_CONFIG_HOME:-$HOME/.config}/sway-session/config.toml`; an absent default
file selects compiled `alacritty` and `herdr`. `terminal.adapter` has the closed
values `alacritty` and `foot`; `terminal.session_manager` initially has only the
closed value `herdr`. It has no executable, argument, template, environment, or
shell configuration fields. Version-1 pre-release config files are rejected
rather than migrated and must be replaced with the current template.

The terminal lifecycle depends on the small `TerminalSessionManager` interface
rather than Herdr argv. The selected compiled adapter validates its context,
builds the manager process spec, validates requested roles, and performs
idempotent initialization. `--role` is either absent or repeated exactly twice
with one shell and one supported logical agent. A partial initialization result
retains the context UUID; `terminal --context UUID` retries that exact context
without allocating another window or session.

`terminal list`, `terminal status`, and `terminal cleanup
--archived-before YYYY-MM-DD` are registry-only, read-only inventory and
preview operations. They use a current-schema snapshot and return an
unsupported-version or state diagnostic without modifying unsupported input.
The cleanup command returns only archived typed-terminal
contexts before the UTC date and never purges. All global `--json` results have
a stable version; terminal-open results expose terminal actions. A JSON purge
without `--yes` returns a preview and diagnostic rather than prompting. Actual deletion still
requires the exact selected UUID and `--yes` for noninteractive automation.

The intended Sway bindings are `$mod+Return` for an explicit fresh persistent
instance (`terminal --new`) and `$mod+Shift+Return` for an ephemeral terminal.
Their packaging template is maintained separately. LAB-105/LAB-106 guarantee
stable identity only for terminals launched by this command; per-window
persistence for arbitrary identical application windows remains deferred to
LAB-93.

### Explicit desktop application identities

The version-2 registry adds `desktop` and `flatpak` launcher variants alongside
the unchanged `herdr` variant. A desktop context stores application-level
desired-open state and either `follow` or `pinned` restore policy. Its window
identity stores only exact compositor-visible values: Wayland app ID, or the
XWayland class and instance pair, plus optional `StartupWMClass` resolver
metadata and Flatpak sandbox application ID. Titles, URLs, profile names,
arguments, environments, and application-private session data are forbidden.

System desktop launchers store a resolved desktop-file ID and path. User-local
desktop launchers additionally store an owner-only immutable desktop-file
snapshot and the source desktop-file checksum and, when the approved executable
is user-owned, its absolute path and checksum.
Flatpak launchers store a validated application ID and the system or user
installation. Launcher identity is registry-wide unique. Application identities
must also be non-overlapping; `StartupWMClass` is a resolution hint and never a
way to distinguish two otherwise identical live windows.

The bounded XDG catalog follows desktop-file precedence, including hidden
tombstones and nested desktop-file IDs. A malformed higher-precedence entry
claims its ID and fails closed instead of exposing a lower-precedence launcher.
The catalog is only discovery metadata: origin, ownership, mutability, hashes,
and installation state must be revalidated at registration and again before a
launch. This schema/catalog slice is tracked by
[LAB-96](https://linear.app/riotbox/issue/LAB-96/add-versioned-desktop-application-identities-and-registry-migration).

### Desktop application group lifecycle

The daemon treats every eligible top-level matching one registered identity as
one application presence group. One pre-marked window is the anchor. Without a
mark, exactly one normal-workspace window may be adopted; multiple
indistinguishable windows prove presence but are never guessed between. The
anchor alone participates in workspace/layout restore. Additional windows,
tabs, profiles, URLs, authentication state, and application-private restore
prompts remain application-owned.

On a real compositor start, the daemon waits five seconds for independently
autostarting applications. It then launches only active desired-open groups
which remain absent, with at most two pending first-window mappings. Launch
intent is atomically persisted before the typed desktop/Flatpak adapter starts
the process. The same application is not retried after a daemon restart,
explicit launcher failure, mapping ambiguity, or Sway config reload. A replaced
Sway socket begins a new compositor session and a fresh attempt budget.

Follow-mode state becomes open as soon as any matching top-level appears and
becomes closed only when the last one has remained absent for two seconds. The
grace preserves profile-picker/authentication-to-main-window transitions.
Pinned state always remains desired-open but is not a same-session process
watchdog. Scratchpad windows count as presence, while their placement remains
out of scope until LAB-92.

When the activation latch is true, the daemon derives exactly one
presentation-only state per eligible normal top-level: unregistered, pending
approval, registered/follow, or pinned. Active typed approval files are
owner-only, bounded, expiring inputs. Consumed, expired, stale, or superseded
operations cannot keep a window pending. Herdr contexts and Sway-classifiable
XWayland dialogs are suppressed. The daemon publishes one globally unique,
container-scoped versioned hidden Sway mark per eligible window and repairs it
during normal observation; mark failures are consolidated as a degraded
diagnostic and never block restore. The animator maps that mark to the
configurable `○`, `◔`, `●`, or `▲` default immediately before the app icon.

New unique anchors are moved to their saved workspace and marked without
focus. Anchors mapped during startup may participate in the saved outer layout;
late anchors receive placement only and never trigger a disruptive full-layout
rebuild. Quiet later moves update the normal debounced snapshot. Live binding,
focus, close, or non-daemon move activity supersedes conflicting restore work,
and saved focus is applied at most once.

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
observation. Unchanged trees do not schedule writes. A missing registry keeps
capture disabled but retains one slow discovery observation so the first
registration by a separate CLI process cannot be missed if its mark event
races the registry commit.

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

1. Sway starts the independent title animator, starts `sway-session daemon`,
   and runs `sway-session restore` once.
2. `sway-session` loads and validates the active context registry.
3. It reads `GET_TREE` and identifies existing managed windows by stable
   application ID or `persist:<uuid>` mark.
4. Existing windows are reused. Missing Herdr contexts are launched through the
   one-shot restore adapter. After its adoption grace, the daemon launches each
   missing desired-open desktop application at most once for that compositor
   session, with no more than two first-window mappings pending.
5. The session daemon observes each mapped window, applies its stable mark, and
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

The title animator uses the shared bounded Sway IPC implementation only for
`title_format` updates and their reset. The session daemon independently uses
that implementation for placement and layout commands; neither process spawns
`swaymsg` or builds shell commands. Automatic reconstruction applies only to
contexts first observed without their stable mark during the current startup.
A pre-existing marked window is treated as the user's current preference. A
connection loss while applying the mark does not lose this new-window
classification.

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
sway-session request-start --session <name> --workspace <number> [--cwd <path>] [--label <label>] [--provider <name>]
sway-session report-codex-session # Codex SessionStart hook only
sway-session app register-focused [--desktop-id <id>] [--yes]
sway-session app register-workspace [--yes]
sway-session app confirm <one-time-token>
sway-session app status
sway-session app list
sway-session app rebind-focused [--desktop-id <id>] [--yes] <context>
sway-session app reapprove [--yes] <context>
sway-session app pin|unpin|archive|activate <context>
sway-session app forget --yes <context>
sway-session terminal [--new | --project <name> | --ephemeral] [--cwd <path>]
sway-session terminal list
sway-session terminal status [context] [--project <name>]
sway-session terminal cleanup [--archived-before YYYY-MM-DD]
sway-session terminal reconfigure [--project <name>] [--socket <path>]
sway-session completion contexts <archive|activate|restore|restore-active|purge|terminal-status|app-forget>
```

`sway-session --json app list` returns only desktop-application contexts in a
stable UUID order. `sway-session --json terminal list` returns typed terminal
inventory in stable context-ID order; `terminal status` and `terminal cleanup`
are similarly machine-friendly read-only projections. These records can
include machine-local paths and session metadata, so treat output as private
operational state.

Non-destructive context selectors and purge previews accept an exact canonical
UUID or an unambiguous exact human label. Confirmed `purge --yes` accepts only
the canonical UUID returned by preview.
Human-readable output uses labels first and retains the full UUID so duplicate
labels remain operable. Machine consumers receive an explicit structured-output
option rather than parsing presentation text.

The completion endpoint is a public, bounded, read-only projection. It reads
one lock-free current-schema snapshot, never creates or changes state, and
does not contact Sway, Herdr, or the network. Each successful text record is a
canonical UUID plus one presentation-only description separated by a tab.
Bash, Zsh, and Fish adapters own only the static command grammar and native
display integration; they never read the private registry or evaluate endpoint
output. A missing, malformed, or unsupported registry silently removes dynamic
candidates while leaving static completion available. Top-level `purge`
candidates remain Herdr-only until launcher-specific deletion semantics exist;
application registrations use `app forget`. `restore-active` is the adapter
scope for `restore --require-active` and excludes every archived context.

`restore` is idempotent. `archive` removes a context from automatic restore but
keeps its registry record and Herdr state. `activate` reverses archive. `purge`
requires deliberate confirmation in an interactive terminal, with an explicit
`--yes` non-interactive confirmation flag and exact canonical UUID for automation. In `--json` mode it
never prompts: without `--yes`, it emits a preview plus confirmation
diagnostic.

Desktop registration is explicit and applies an optimistic stable mark only as
part of the serialized registry transaction. A rejected mark or failed state
commit compensates the other side; an ambiguous IPC outcome is resolved with a
fresh tree instead of replaying the command. User-local launcher source or
executable changes block until `reapprove`. `rebind-focused` shows and then
atomically replaces the old launcher/identity while transferring the stable
mark. Administrative and indirect entries (`pkexec`, `sudo`, shells, `env`, and
similar wrappers) are rejected; privileged launch support remains LAB-94.

Herdr session names follow Herdr's 64-byte ASCII name contract. The reserved
name `default` is rejected because Herdr maps it to non-deletable default state.
Concurrent restores hold the registry lock across observation and launch.
They also recognize an exact pending typed terminal-adapter argument vector in
`/proc`, so a process which started before its Wayland window mapped is not
launched again after a mapping timeout.

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
a validated named session. The stable terminal application ID is generic and
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
runs as a narrow broker outside the Codex confinement boundary; the supported
integration path exposes only that broker's validated reporting endpoint. A
complete boundary would require negative tests to prove the general Herdr
socket and both state roots remain inaccessible from Codex while the reporting
path works. The current live check fails closed instead of claiming success
when pathname socket-connect mediation is absent.

The hook also carries Herdr's bounded current-pane identity as routing metadata.
The broker never accepts a socket path or method from the hook: it resolves the
context's unique typed launcher from one validated atomic registry snapshot and
confines the pane identity to that named session's fixed `herdr.sock`. A fixed
read-only `pane.process_info` request plus the reporter's kernel `SO_PEERCRED`
PID proves that the reporting process descends from the selected pane before
the sole mutation is sent. The hook ignores Codex transcript paths and launch
commands. Herdr's native adapter derives the only resume command, the typed
`codex resume <validated-uuid>` form, from the saved association.

Codex-triggered creation uses a second owner-only runtime endpoint. Its request schema
contains only typed context metadata and a numbered Sway workspace; it cannot
contain Herdr roles, pane identifiers, or command text. The session daemon's
unchanged typed endpoint ensures exact context identity, requires the target
workspace to be empty, and restores the outer managed window through the normal
`sway-session` path. The standalone `broker` command remains a compatibility
entry point for that same protocol, but automatic startup uses the daemon.

Security status: this creation path is experimental and does not yet confine
the terminal, Herdr pane shell, or agent which it creates. User-writable shell
startup files and executable lookup can escape the caller's AppArmor profile.
The current broad AppArmor deny-list also cannot reliably mediate `connect(2)`
to pathname-based Sway, Herdr, or container API sockets on every supported
kernel; file-path deny rules alone must not be described as proof of that
connection boundary. It may be enabled only with those risks explicitly
accepted; documentation must not describe it as a complete confinement
boundary. The agreed Agent Sandbox follow-up, threat model, and exit criteria
are tracked in
[LAB-89](https://linear.app/riotbox/issue/LAB-89/harden-broker-created-herdr-sessions-with-agent-sandbox-integration).

Pane initialization remains outside the animator and behind `sway-session`'s
typed terminal-session-manager seam. The Herdr adapter loads one exact active
context under the registry lock, derives the named Herdr session and cwd, and
calls the ordinary Herdr CLI with fixed
`snapshot`, `pane split`, and typed `agent start` argument shapes. It mutates
only a session proven to use the supported snapshot protocol and to have one
workspace, one tab, one pane, and no agent; every other existing or ambiguous
session is a no-op. The existing request-start protocol remains byte-for-byte
unchanged and still accepts no roles or command text. Its trusted daemon side
uses the fixed `codex` + `shell` layout after placement, so confined Codex does
not need a privileged general-purpose `sway-session` transition.

The live boundary verifier invokes only `/usr/bin/sway-session` after checking
that it is a regular root-owned mode-0755 file belonging to the installed
`sway-title-animator` package. A
user-writable client found earlier in `PATH` must never stand in for the
production integration.

Session-manager roles are logical Herdr agent kinds rather than executable paths
or runtime selections. Alternate trusted launchers, including containerized
agent execution, remain an integration concern outside the persisted Sway
context and do not expand the request broker protocol.

## Sway startup

The intended Sway configuration is:

```text
exec_always --no-startup-id /usr/bin/sway-title-animator --replace
exec --no-startup-id /usr/bin/sway-session daemon
exec --no-startup-id /usr/bin/sway-session restore
```

The daemon and restore commands intentionally use `exec`, not `exec_always`, so
a config reload cannot duplicate processes or windows. The daemon additionally
holds an exclusive owner-only runtime lock. Startup ordering is race-safe: the
daemon waits for a confirmed event subscription before its initial tree
refresh, so windows mapped around that observation cannot escape subsequent
reconciliation. The animator may start, stop, or be absent without changing
session capture or restore behavior.

Session persistence is opt-in and disabled by default for existing users until
configured. Removing the one-shot restore line stops automatic Herdr context
launches without deleting state. Desired-open desktop applications are restored
by the daemon itself; unpin, archive, or forget them before disabling that
behavior, or stop configuring the daemon.

## Failure behavior

- A malformed registry or layout file produces one actionable diagnostic and
  never triggers partial interpretation as executable input.
- A missing Herdr or selected terminal-adapter executable fails the affected
  context only.
- A missing project directory is reported and not silently replaced with the
  home directory.
- A newly launched outer context removes inherited `HERDR_*` pane metadata and
  `CODEX_THREAD_ID` so manual restore from inside Herdr cannot become an
  accidental nested-Herdr launch. It then injects only the trusted
  `HERDR_CONFIG_PATH` resolved by `sway-session` plus the new context ID.
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
- Every successful daemon move is followed by a losslessly delivered Sway tick
  barrier. The barrier expires any missing no-op move event before a later user
  move can be mistaken for daemon activity; reconnects invalidate all pending
  attribution.
- Loss of the subscribed event stream is itself delivered losslessly and
  immediately cancels pending startup or layout restoration before reconnect,
  so unobserved user intent is never overwritten during an outage.
- Repeated Herdr `restore` calls converge on one outer window per active Herdr
  context. Desktop restore is application-level: all matching top-levels count
  as one presence group, and repeated requests only queue desired-open state
  for the daemon.

## Delivery plan

### Phase 1: Shared foundations

- Extract bounded IPC framing and requests into an internal package without
  changing current title-animation behavior.
- Add validated identity and versioned state types.
- Add owner-only atomic state persistence, descriptor-relative path handling,
  complete read-modify-write locking, and corruption/concurrency tests.
- Introduce the `sway-session` command skeleton and structured diagnostics.

### Phase 2: Capture and placement

- Preserve typed Sway events in the session daemon.
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
- Add the typed terminal-adapter/Herdr launcher and duplicate detection.
- Enforce registry-wide uniqueness of typed launcher identities.
- Enable and validate Herdr pane history.
- Add safe startup configuration and packaging for the session and animator
  binaries.

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

### Phase 7: Explicit desktop application groups

- Add trusted desktop/Flatpak launcher identities and explicit registration.
- Observe all matching top-levels as one presence group with one optional
  unambiguous layout anchor.
- Persist follow/pinned desired-open lifecycle with a bounded close grace.
- Adopt autostarts before launching missing groups, cap pending mappings at
  two, and persist conservative attempt evidence per compositor lifetime.
- Yield placement, structural restore, and focus to live user activity.
- Keep scratchpad and standards-based per-window identity in LAB-92/LAB-93.

### Phase 8: Visible integration and release

- Publish four application-level states through one versioned hidden Sway mark
  owned by `sway-session daemon`.
- Render configurable equal-width Noto Sans Mono defaults before the app icon
  without giving the animator registry or restore responsibility.
- Ship the one-shot startup stanza, optional registration binding, structured
  app inventory, optional Flatpak/GIO dependencies, and consolidated degraded
  diagnostics.
- Validate the process split, packaging, AppArmor documentation, independent
  reviews, and real Sway behavior on disposable workspace 98 or higher.

### Phase 9: Shell completion

- Expose one bounded read-only completion projection from `sway-session`.
- Install static Bash, Zsh, and Fish adapters in their standard package vendor
  directories and include them in release archives and source installs.
- Insert canonical context UUIDs while showing safe label, state, launcher,
  provider, and Herdr-directory metadata through each shell's native UI.
- Preserve static command and option completion when dynamic state is absent,
  invalid, or unreadable; add syntax, injection, fallback, and packaging tests.

### Phase 10: Reusable typed terminals (LAB-105)

Implemented: add the closed Alacritty/Foot adapter contract, the initial strict
terminal configuration schema (superseded by version 2 in LAB-110), stable
default and hashed project identities, the
non-persistent ephemeral terminal path, and read-only terminal inventory and
cleanup preview. The release registry contract is schema 5 only; older
pre-release state is reset rather than interpreted.

Automated verification covers adapter/config validation, identity reuse and cwd
conflicts, ephemeral non-persistence, stable JSON actions, read-only inventory,
cleanup date filtering, and archive timestamps. A real Sway
end-to-end run completed on 2026-09-02 with isolated state and configuration on
workspaces 98 and 99. It verified persistent create/reuse/focus/status,
archive/cleanup/purge, and an ephemeral launch that left the registry unchanged.

### Phase 11: Fresh persistent terminal instances (LAB-106)

Implemented: add the explicit `terminal --new` mode. Every call allocates a
fresh context UUID, derives a unique bounded Herdr session from that UUID, and
launches a separately identifiable terminal window. Keep default and project
identity reuse available, make the shipped `$mod+Return` binding use `--new`,
and retain `$mod+Shift+Return` as the state-free ephemeral path.

The JSON result reports the exact context UUID, Herdr session, `instance`
identity kind, and `created`/launch actions. Invalid `--new --project` and
`--new --ephemeral` combinations, plus persistent options combined with
`--ephemeral`, fail before configuration, filesystem, registry, Sway, or
process access. Concurrent creation is serialized without collapsing distinct
invocations into one context. A real isolated headless-Sway end-to-end run on
2026-09-02 verified two such instances on workspaces 98 and 99, capture,
marking, restore without the animator, independent archive/activate/purge, and
complete cleanup using Herdr 0.8.2.

Schema v5 fresh Herdr names retain the complete UUID while removing its
separators. The shorter spelling leaves room for Herdr's longer
`herdr-client.sock` name under a standard home directory. Both pathname-socket
lengths are validated against Linux's `sockaddr_un` limit before a new context
is committed; an unusually long custom `XDG_CONFIG_HOME` fails with an
actionable diagnostic.

Only the schema-5 short spelling is supported by the release. Pre-release
registries use the one-time state reset described above rather than retaining
an alternate terminal-instance identity.

### Phase 12: Configurable terminal session manager (LAB-110)

Implemented: add the closed `terminal.session_manager` configuration choice
and the `TerminalSessionManager` seam. `herdr` is the initial manager; raw
executables, argv templates, environment fragments, and shell evaluation remain
forbidden. `terminal --new` and exact `terminal --context UUID` invocations can
request one agent plus one shell with repeated typed `--role` options.

The Herdr adapter owns launch preflight and rollback-safe empty-session
initialization. Initialization runs while the registry lock proves the context
active, so archive and purge cannot race the dependent Herdr mutations. A
failed agent start retains the context UUID and rolls back to a retryable single
shell. The separately installed `sway-herdr-init` binary and its AppArmor child
profile are removed. The unchanged request-start broker performs the fixed
Codex+shell layout internally and therefore does not gain role or command
fields.

## Test matrix

Automated tests must cover:

- UUID and mark validation;
- schema-version rejection with byte-for-byte preservation of unsupported input;
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
- version-1 through version-4 and unknown registry rejection, with no
  mutation, no inferred instance discriminator, and no partial interpretation;
- mixed Herdr, system-desktop, approved user-local, and Flatpak identities;
- Wayland, XWayland, sandbox, and ambiguous application-identity fixtures;
- application-group adoption, profile-picker transitions, last-window close
  grace, pinned state, two-launch concurrency, daemon restart, Sway reload,
  compositor replacement, late mapping, ambiguity, and user interruption;
- inactive/active indicator mode, pending approval expiry, four-state marker
  convergence, hidden-mark cleanup, glyph validation, and exact title ordering;
- bounded XDG desktop-entry precedence, hidden tombstones, malformed-entry
  fail-closed behavior, and explicit catalog invalidation;
- archive, activate, and purge transitions;
- typed launcher validation and absence of shell evaluation;
- closed terminal-adapter configuration, fresh per-call instance creation,
  stable default/project identity reuse, cwd conflict rejection, ephemeral
  non-persistence, JSON terminal actions and session identity, inventory
  ordering, and read-only archive cleanup previews;
- closed terminal-session-manager selection, role rejection before state/Sway
  access, bounded fresh-shell readiness, exact-context initialization retry,
  broker-fixed Codex/shell roles, and registry-serialized Herdr mutation;
- per-context and per-workspace failure isolation;
- IPC reconnects, bounded payload handling, and no replay of ambiguous
  mutating commands; and
- positive narrow-agent reporting plus negative Herdr-socket and direct-state
  access.

Manual evidence must distinguish:

- Sway config reload from a real Sway restart;
- one-monitor behavior from unplugging and reconnecting a second monitor;
- outer Sway layout restoration from inner Herdr pane restoration;
- pane-history display from a genuinely resumed shell or Codex process;
- exact restore from explicitly reported best-effort degradation; and
- persistent/default and ephemeral terminal bindings in a real Sway session,
  including adapter selection, project reuse, and focus behavior.

## Deferred extensions

- Multiple outer windows per context via explicit child identities.
- Additional typed launchers beyond the implemented Herdr identity and the
  explicitly registered desktop/Flatpak application design.
- Scratchpad restoration for registered applications, tracked in LAB-92.
- Generic per-window application identity after stable `xdg-toplevel-tag`
  support is available, tracked in LAB-93.
- Privileged or `pkexec` desktop application restore, tracked in LAB-94.
- A stable declarative-layout backend after upstream Sway support is released.
- Optional retention policies for archived Herdr history.
- A richer interactive session browser after the core CLI is proven reliable.

These extensions must not weaken stable identity, typed launch policy, or the
Codex/AppArmor boundary established by the first version.
