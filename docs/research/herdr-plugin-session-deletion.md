# Herdr plugins and conditional named-session deletion

Date: 2026-10-02.

**Conclusion:** the supported plugin surface cannot provide crash-safe,
conditional named-session deletion against concurrent native start/attach and
live handoff. A plugin can package the workflow, record intent, and coordinate
cooperating callers. Enforcing the condition against Herdr itself requires a
core lifecycle/conditional-operation contract that these versions do not expose.
This is an inference from the boundaries below, not an implementation proposal.

Scope: official [marketplace](https://herdr.dev/plugins/) and
[plugin documentation](https://herdr.dev/docs/plugins/), Herdr **v0.9.3**
(tag resolves to commit
[`7b116c05bfda646af39d2524c54e70c751f57ee8`](https://github.com/herdrdev/herdr/commit/7b116c05bfda646af39d2524c54e70c751f57ee8)),
and current `master` observed at
[`d6b40d4edd550ccea081f089605a64314f8c8b27`](https://github.com/herdrdev/herdr/commit/d6b40d4edd550ccea081f089605a64314f8c8b27).
Plugin sources and startup call sites were independently read and compared;
the files cited below for those paths are byte-identical between these refs.
Deletion/API paths were checked separately in the same investigation.
The initial plugin review did not run Herdr/Sway operations or inspect user
runtime state. The identity follow-up below additionally exercised a disposable
headless Herdr session; no user session or compositor was accessed.

## Verified plugin boundaries

1. **Executable extensions, not an embedded lifecycle authority.** Plugins launch
   ordinary commands, call the existing CLI/socket API, and own their durable
   files or database. They receive runtime context and a plugin state directory;
   there is no Herdr-managed plugin storage API. The manifest exposes `build`,
   `startup`, `actions`, `events`, `panes`, and `link_handlers`, with no pre-start,
   pre-delete, or pre-handoff veto field.
   [Documentation, lines 18–30](https://github.com/herdrdev/herdr/blob/v0.9.3/docs/next/website/src/content/docs/plugins.mdx#L18-L30),
   [environment/storage, lines 253–270](https://github.com/herdrdev/herdr/blob/v0.9.3/docs/next/website/src/content/docs/plugins.mdx#L253-L270),
   [storage contract, lines 357–360](https://github.com/herdrdev/herdr/blob/v0.9.3/docs/next/website/src/content/docs/plugins.mdx#L357-L360),
   [manifest parser, lines 11–34](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/api/plugins/manifest.rs#L11-L34),
   [manifest schema, lines 36–67](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/api/schema/plugins.rs#L36-L67).

2. **Normal startup hooks occur after restoration and readiness announcement.**
   `run_server` binds the API, constructs `App::new`, constructs the headless
   server, prints readiness, schedules startup hooks, then enters `server.run`.
   `App::new` loads and restores the saved session before returning. Hook
   completion is not a readiness gate.
   [Bootstrap, lines 29–90](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/server/headless/bootstrap.rs#L29-L90),
   [restore, lines 357–397](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/mod.rs#L357-L397).

3. **Handoff hooks occur after the replacement owns the session.** The importer
   restores runtimes, starts its API/server, reports ready, waits for commit,
   assumes ownership, unpauses readers, reports ownership, prints readiness,
   and only then schedules startup hooks. A startup hook cannot veto that
   ownership transfer. The documented hook also does not run on client attach,
   configuration reload, or plugin link/enable; it is a one-shot command, not a
   supervised daemon.
   [Import ordering, lines 156–198](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/server/headless/bootstrap.rs#L156-L198),
   [documented lifecycle, lines 233–249](https://github.com/herdrdev/herdr/blob/v0.9.3/docs/next/website/src/content/docs/plugins.mdx#L233-L249).

4. **Dispatch is asynchronous and has no veto result.** `start_plugin_command`
   starts a thread, spawns a child, and returns `Ok(log)` without waiting for
   completion. Both `run_plugin_startup_hooks` and `run_plugin_event_hooks`
   discard its return value. Child completion/failure updates the command log;
   it does not stop the server. Event emission schedules hooks and pushes the
   event onward without awaiting a plugin decision.
   [Spawn/completion, lines 118–180](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/api/plugins/runtime.rs#L118-L180),
   [startup/event dispatch, lines 183–266](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/api/plugins/runtime.rs#L183-L266),
   [completion handling, lines 136–164](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/api.rs#L136-L164),
   [event emission, lines 761–764](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/api.rs#L761-L764).

5. **The manifest hook allowlist contains only the following events.**
   [Authoritative allowlist, lines 286–329](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/api/schema/events.rs#L286-L329),
   [wire names, lines 223–253](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/api/schema/events.rs#L223-L253).

   | Prefix | Allowed suffixes |
   | --- | --- |
   | `workspace.` | `created`, `updated`, `closed`, `renamed`, `moved`, `reordered`, `focused` |
   | `worktree.` | `created`, `opened`, `removed` |
   | `tab.` | `created`, `closed`, `renamed`, `moved`, `focused` |
   | `pane.` | `created`, `closed`, `focused`, `moved`, `exited`, `agent_detected`, `agent_status_changed` |

   There is no session/server start, delete, shutdown, or handoff event, let
   alone a cancellable pre-operation event. Unknown manifest names generate
   warnings rather than creating extension points; runtime dispatch enforces
   the allowlist. `startup` is its own manifest entrypoint, not an allowlisted
   `[[events]]` name. The broader socket subscription surface also declares
   no session/server lifecycle event.
   [Name validation, lines 326–332](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/api/plugins/manifest.rs#L326-L332),
   [runtime filter, lines 218–235](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/app/api/plugins/runtime.rs#L218-L235),
   [subscription enum, lines 16–85](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/api/schema/events.rs#L16-L85).

## Relevant native boundaries

- The delete CLI accepts a session name and `--json`, then directly calls
  `crate::session::delete_session(&name)`, independently of plugin dispatch.
  Deletion checks the socket's running status and then calls `remove_dir_all`;
  there is no expected-generation or lease argument. The public `SessionInfo`
  has name, default/running flags, socket path, and session directory, with no
  durable generation identifier.
  [CLI, v0.9.3 lines 509–531](https://github.com/herdrdev/herdr/blob/v0.9.3/src/cli.rs#L509-L531),
  [delete, v0.9.3 lines 299–319](https://github.com/herdrdev/herdr/blob/v0.9.3/src/session.rs#L299-L319),
  [same delete logic on master, lines 308–328](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/session.rs#L308-L328),
  [SessionInfo, lines 20–27](https://github.com/herdrdev/herdr/blob/v0.9.3/src/session.rs#L20-L27).
- The API declares server stop/live-handoff/reload and `session.snapshot`,
  without a conditional session-delete or session lease/lock operation. During
  handoff the old server transfers runtimes, removes public sockets, and waits
  for replacement readiness. Consequently, socket unavailability can occur
  during an active ownership transition. The lifecycle and API schema blobs
  are identical across both inspected refs.
  [API methods, lines 48–77](https://github.com/herdrdev/herdr/blob/v0.9.3/src/api/schema.rs#L48-L77),
  [handoff socket interval, lines 165–181](https://github.com/herdrdev/herdr/blob/v0.9.3/src/server/headless/lifecycle.rs#L165-L181).

## Capability versus required guarantee

The following are deductions from the cited lifecycle, dispatch, and native
operation boundaries, rather than additional documented promises:

| Required property | Plugin capability and limit |
| --- | --- |
| Persist deletion intent across crashes | Own files/database can record intent and recovery progress. Herdr does not atomically couple that state to native lifecycle operations. |
| Delete only the intended session incarnation | A plugin may track its own token, but native deletion takes only a name; there is no exposed native generation comparison bound to deletion. |
| Exclude concurrent start/attach/handoff | A plugin lock serializes cooperating helpers only. Native lifecycle operations have no supported obligation to acquire it, and hooks cannot veto them. |
| Recover before a session becomes usable | Startup reconciliation begins after restore/readiness, and after ownership transfer on handoff. A crash, launch error, or failed hook does not establish a blocking recovery barrier. |
| Distinguish inactivity from handoff | A socket probe alone is insufficient during the verified socket-replacement interval. A plugin has no native lease/conditional operation to close the check-to-delete race. |

A wrapper controlling **every** start, attach, deletion, and handoff could impose
an additional cooperation protocol, but that is a different operating contract
and is outside the plugin-only guarantee assessed here. A plugin remains useful
as the workflow/UI layer over a future native conditional lifecycle primitive.
Neither asynchronous notifications nor plugin-owned durable state supplies that
missing native serialization boundary.

## Identity follow-up: internal identifiers and isolated lifecycle check

The user asked whether creation times or other internal identifiers could avoid
requiring a new persistent session UUID. This is a distinct question from whether
the native delete operation excludes concurrent lifecycle changes. A new UUID is
one possible contract, not the only conceivable solution.

### Additional identifiers found in current source

| Candidate | What it identifies | Limitation for whole-session deletion recovery |
| --- | --- | --- |
| `client_shell_boot_id` | One running headless server, generated from PID and current nanoseconds. Client-shell requests reject a different boot ID. | It is regenerated by `HeadlessServer::new`, including on restart/handoff, and is not persisted in the whole-session snapshot. The boot-checked command lane does not support session deletion. |
| Socket device/inode | The currently bound Unix socket file. Herdr compares these before removing its owned socket. | Socket replacement during normal lifecycle transitions changes this identity; it does not identify the saved session across restarts. |
| `terminal_id` | A server-owned terminal, allocated using time and a process-local counter. | Ordinary snapshot restoration allocates fresh terminal IDs; `PaneSnapshot` does not serialize them. |
| Workspace/tab/pane IDs | Objects within a session. Workspace IDs are allocated from a process-local counter and reserved after restoration. | A fresh session can reuse the same IDs, including `w1`. They are not whole-session incarnation IDs. |
| `layout_fingerprint` | A SHA-256 hash of serialized snapshot content, used to validate optional screen history. | Equivalent content has the same hash; ordinary edits can change the hash within one session. |
| Directory device/inode and filesystem birth time | The filesystem object containing the named session. | Useful replacement evidence, but not accepted by the native delete API. Availability and filesystem semantics matter; observing metadata does not prevent later replacement or concurrent writes. |

Sources: [server boot ID construction](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/server/headless.rs#L350-L358),
[boot check](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/server/headless/endpoint_requests.rs#L15-L39),
[allowed client-shell commands](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/server/client_commands.rs#L15-L58),
[socket identity and removal](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/ipc.rs#L282-L318),
[terminal ID allocation](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/terminal/id.rs#L5-L24),
[workspace ID allocation](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/workspace.rs#L104-L110),
[snapshot schema](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/persist/snapshot.rs#L14-L40),
[snapshot fingerprint](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/persist/snapshot.rs#L404-L413).

There is no top-level session creation timestamp or incarnation UUID in the
persisted `SessionSnapshot`. File modification/change timestamps describe
filesystem updates; they are not a stable session creation field. Saving uses
a temporary file and rename, so the snapshot file's own inode is also unsuitable
as a stable session identity.
[Snapshot writes](https://github.com/herdrdev/herdr/blob/d6b40d4edd550ccea081f089605a64314f8c8b27/src/persist/io.rs#L44-L61).

### Executed check

The installed binary reported **Herdr 0.9.2**. This runtime observation is not a
claim to have executed 0.9.3 or master. Source inspection above used the pinned
master commit. The check used a unique `/tmp/hi-*` root, isolated XDG config,
state, data, cache and runtime directories, explicit `--session identity-probe`,
cleared inherited Herdr overrides, and a non-login `/bin/sh`. Agent resume and
update checks were disabled. No Sway window was created.

Sequence: start the headless server; create one workspace with a fixed label and
cwd; attempt a second server using the same name; stop and observe; restart,
stop and observe; delete; recreate the same named session and workspace; stop
and observe; delete the disposable session. Observations were taken after each
owned server exited, avoiding a snapshot write during comparison.

| Observation | Ordinary restart | Delete and recreate |
| --- | --- | --- |
| `name`, `default`, `socket_path`, `session_dir` | Unchanged | Unchanged |
| Full parsed saved snapshot | Identical | Identical |
| Directory device/inode pair | Unchanged | Changed |
| Directory filesystem birth time | Unchanged | Changed |
| Runtime terminal IDs | Changed | Changed |

The second simultaneous server was rejected as already running. The native
namespace addresses one session directory per name; it does not keep two
historical incarnations under that same name. The stopped snapshot's top-level
keys were `active`, `collapsed_space_keys`, `selected`, `sidebar_section_split`,
`sidebar_width`, `version`, and `workspaces`.

All test servers were stopped and waited for, and the disposable named session
was deleted. Only isolated test evidence remains under `/tmp/hi-g5xoipo9`; its
`result.json` records boolean comparisons, with no user session data. This was a
sequential identity check, not a destructive concurrent-start/handoff race test.

### Implication for the pending deletion design

A durable, cancellable deletion intent can schedule retries after `busy` without
becoming a generic replay queue for arbitrary Herdr commands. Additional
filesystem identity checks can reject a visibly replaced directory and improve
our diagnostics. They do not close the interval between an external check and
Herdr's name-only `remove_dir_all`, or exclude startup/handoff writers. Any
eventual native solution must bind its identity precondition to the deletion
under shared lifecycle coordination. That identity need not necessarily be a new
UUID; the current API simply has no way to express and enforce the condition.

## Accepted integration boundary

After reviewing these findings, the user explicitly accepted deletion through
Herdr's existing name-based command and dropped the plugin approach. A local
recovery design may therefore use that command with a durable operation journal
and repeated filesystem identity observations. This supersedes treating a new
native conditional-delete API as an unconditional prerequisite for all local
improvements.

The guarantee must remain precise: observed replacement directories block the
recorded operation; retries do not silently select a new target. External
identity observations do not make Herdr's name-based deletion atomic with
startup, handoff, or replacement. That remaining concurrency limitation is an
accepted integration boundary, not evidence that upstream exclusion exists.
