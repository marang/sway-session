# Sway Session architecture and product plan

Version: 1.0
Status: Active
Project: [Sway Session](https://linear.app/riotbox/project/sway-session-74dd95a8e064)

## Purpose

sway-session gives explicitly registered work contexts durable outer-window
identity across Sway starts. It combines a private registry, typed terminal
launch adapters, desktop-application presence groups, bounded compositor
reconciliation, and narrow owner-only brokers.

The standalone repository was extracted under LAB-119 from the complete
sway-title-animator history. The split changes source ownership, packaging, and
release identity only. It does not change CLI behavior, XDG paths, stored state,
marks, sockets, environment variables, application IDs, or wire protocols.

## Goals

- Restore registered terminal and desktop-application contexts without
  restoring application-private state.
- Preserve one stable context UUID across capture, archive, activation,
  terminal recovery, and exact deletion.
- Treat user bindings, workspace switches and saved-window placement/layout
  changes as higher priority than automation; window focus alone does not
  cancel saved placement.
- Bound every compositor reconciliation pass without limiting registry size.
- Keep state private, typed, versioned, and recoverable after ambiguous effects.
- Remain fully independent of any title-animation process.

## Non-goals

- Session restore for unsupported compositors.
- Shell command templates or executable paths supplied by configuration.
- Browser tabs, editor buffers, URLs, documents, or other application-private
  restore.
- Guessing between ambiguous windows or desktop entries.
- A general privileged command broker.
- An animation or audio subsystem.
- A third shared repository for the intentionally small IPC or mark contracts.

## Architecture

~~~mermaid
flowchart TB
    subgraph Entry[Process entry points]
        CLI[CLI commands]
        Daemon[long-running daemon]
        TUI[terminal manage TUI]
    end
    subgraph Core[Domain and durable state]
        Session[internal/session]
        DB[(state.sqlite3)]
        Statefile[internal/statefile]
    end
    subgraph Boundaries[Typed external boundaries]
        IPC[internal/swayipc]
        Init[internal/herdrinit]
        Start[internal/sessionrequest]
        Report[internal/agentreport]
        Indicator[internal/titleindicator]
        Diagnostic[internal/diagnostic]
    end
    HerdrProcess[typed Herdr process/control boundary]
    CLI --> Session
    TUI --> Session
    Daemon --> Session
    Session --> DB
    Session --> Statefile
    Session --> IPC
    Session --> HerdrProcess
    CLI --> Init
    Init --> Session
    Init --> HerdrProcess
    Daemon --> Start
    Daemon --> Report
    Start --> Session
    Report --> Session
    Session --> Indicator
    CLI --> Diagnostic
    Daemon --> Diagnostic
~~~

### Package ownership

- cmd/sway-session parses the public CLI, starts the explicit daemon, adapts
  Sway operations, owns the terminal management TUI, and wires narrow services.
- internal/session owns validated registry entities, typed terminal identity,
  desktop identity and launch approval, SQLite storage, capture, placement,
  layout restore, activity, and lifecycle coordination.
- internal/statefile owns owner-only directory, regular-file, and lock checks
  used by runtime artifacts.
- internal/swayipc owns bounded framing, request/reply validation, tree types,
  event decoding, and reconnect behavior.
- internal/shutdownwatch owns logind lifecycle observation and its delay
  inhibitor. It publishes a generation guard, never writes session state.
- internal/herdrinit owns fixed, idempotent role initialization behind the
  closed Herdr session-manager adapter. It is not an executable.
- internal/sessionrequest accepts one protocol-v1 ensure-and-start operation.
- internal/agentreport accepts protocol-v3 agent-session associations with
  optional native event origin and source-less protocol-v2 associations and
  owns the shared bounded transport and service.
- internal/titleindicator owns only the versioned presentation mark wire
  contract.
- internal/diagnostic owns stable human and JSON diagnostics.
- internal/doctor owns read-only setup inspection and previewed, bounded
configuration repairs. The CLI and doctor TUI share one service; neither
interface has a second repair implementation. Broker checks pin their
owner-only Unix-socket entry, use a connect-only liveness probe, and compare
the peer credentials with the verified daemon PID; they never send a broker
request while diagnosing.

### Setup inspection and repair

`doctor` defaults to the interactive setup view only when both stdin and stdout
are terminals. `--check`, `--json`, and non-TTY invocation stay read-only.
Stable check IDs accompany `ok`, `warning`, `error`, and `unavailable` statuses.
Unavailability is explicit evidence uncertainty, not success or a new package
dependency. An error report exits 3, usage errors exit 2; warnings alone do not
fail the command.

Repairs are separate from inspection: `--fix sway.integration` prepares a
preview; adding `--yes` applies it. The TUI uses the same Plan/Apply boundary,
requires confirmation, and never applies simply by selecting a check. Plans
hold private input snapshots; Apply revalidates them, creates exclusive private
backups, uses descriptor-relative atomic file replacements, and rolls back
provable partial changes on failure. No state database, service, session,
package, hook, or security policy is changed by this repair.

Sway integration inspection has one narrow source boundary: the selected main
file and its sibling `50-sway-session-doctor.conf`. The name, location and v1
ownership header remain stable. A byte-exact parser recognizes two profiles:
one-time `exec --no-startup-id` daemon and restore declarations, optionally
followed by both `$mod+Return` / `$mod+Shift+Return` standard terminal bindings.
Historical subsets are recognized as legacy partial profiles and require manual
migration; reordered, foreign or manually changed files are protected.

The main-file recognizer observes only a literal top-level direct include of the
standard sibling. It accepts normalized literal absolute or sibling-relative
paths, including
double-quoted and `./` sibling paths, without variable/glob expansion or include
traversal. Parent traversal (`..`) remains outside the recognized boundary.
Command bodies remain opaque. It does not classify foreign shell expressions,
resolve variables, read startup scripts, infer hidden calls or compare foreign
bindings. The old general Sway/shell classifier is intentionally retired, with
no fallback. Inspection is bounded to 1 MiB per file, 64 KiB per logical line
and block depth 64. These are inspection bounds, not limits on saved contexts.

`ok` establishes a supported standard profile and observed direct include on
disk. `warning` covers an unobserved include, a dangling direct include or a
legacy partial profile. Missing and unrecognized standard files are
`unavailable`. Check ID `sway.integration`, public JSON fields, statuses and
exit codes stay stable. In-process adoption selection is excluded from JSON.
Sway IPC, daemon identity and broker liveness checks retain independent runtime
results; source inspection neither proves effective bindings or load order nor
guarantees a successful next login.

RepairOptions makes creation/recovery and direct-include insertion conditional
on explicit adoption. New profiles default to startup-only; an unspecified
shortcut selection preserves an existing supported profile. Explicit
`--shortcuts none|default` changes only the owned snippet. `--adopt-standard` and
`--shortcuts` are additive CLI options restricted to `--fix sway.integration`;
the TUI gathers equivalent choices before preview. Adoption authorizes limited
writes, not a conclusion that previous configuration was cleaned up. Users
migrate old starts/includes themselves, free the chosen bindings, define `$mod`
before the first include and review load position before opting in. An absent
main file is created manually. Doctor never removes foreign entries or edits unsupported snippets.

Plans snapshot only the main file and standard sibling. Apply revalidates both
sources, ownership and directory identity, preserves exclusive private original
backups, and writes atomically using file descriptors. Concurrent writer
compensation requires proof of the installed inode, not merely matching bytes;
uncertain compensation preserves the displaced file and reports its path.
Appending a new direct include requires a standalone top-level EOF position;
unfinished continuations, unclosed blocks and exhausted recognition bounds
require manual correction first. This guard is not full Sway validation.
No SQLite mutation, process start/stop, restore request or Sway reload occurs.
Startup declarations remain `exec`; reload activates bindings but does not run
new daemon/restore startup declarations. Those run at the next Sway session.

GET_VERSION supplies the loaded root path. GET_CONFIG exposes source text and
GET_BINDING_STATE the current mode; neither supplies effective binding inventory
or repair authority. Disposable private Sway validation belongs in the test
workflow, outside the production inspection path.

## Durable state

Runtime state is
$XDG_STATE_HOME/sway-session/state.sqlite3, or
~/.local/state/sway-session/state.sqlite3 when XDG_STATE_HOME is unset.
Configuration is
$XDG_CONFIG_HOME/sway-session/config.toml, or
~/.config/sway-session/config.toml when XDG_CONFIG_HOME is unset. Runtime
sockets and locks remain below $XDG_RUNTIME_DIR/sway-session.

`state backup --output PATH` exports sway-session metadata to a private backup
file. `state recover --from PATH` validates that input and previews the target
without preparing runtime locks or modifying state. Only `--yes` applies the
replacement. Neither operation backs up or restores Herdr history, application
state, or configuration, and neither controls processes or the compositor.
Paths must be clean and absolute; portable recovery input is a current-owner,
regular `0600` file inside a current-owner `0700` parent directory.
Approved desktop launcher files and external Herdr session files are not
included in the backup. Recovery on another machine requires providing them
separately and checking launcher approvals against its installed applications.

The permanent `.state-access.lock` gate is held shared for the full lifetime
of ordinary database handles and operations, including their external effects;
recovery requires exclusive access. Read-only access to an older root without
the gate uses a shared root-directory lock during bootstrap and creates no
metadata. `state-recovery.json` and `state-recovery-completed.json` are versioned,
owner-only recovery coordination metadata outside the replaceable SQLite file.
They must survive database replacement to record pending and completed recovery;
they do not introduce another storage format for session documents. Session
runtime records remain in SQLite. A `recovery-UUID` directory retains the
previous database archive or raw bundle.

Recovery apply holds the CLI's existing daemon lock through the operation and
requires exclusive state access in the session core. It refuses a running
daemon, invalid runtime setup, or busy state. Stop standalone brokers and all
older CLI and broker processes beforehand; mixed-version writers are not
supported. The previous healthy database becomes a private standalone rollback
file. Corrupt previous state is preserved as a raw bundle, explicitly marked as
unvalidated. An interrupted replacement blocks ordinary state access and is
resumed by repeating the same source path with explicit `--yes`. A durable
completion receipt permits idempotent retry only while the installed database
remains unchanged and has no live WAL/SHM files. After normal writers resume,
applying the same source with `--yes` deliberately performs a new recovery.
A partial result on error retains installation status and rollback locations.
A preview does not guarantee exclusive access at apply time or live session
resumability.
The [backup and recovery instructions](../README.md#state-backup-and-recovery)
describe the operating procedure and JSON summaries.

SQLite schema 1 stores schema-5 contexts, layout schema 2, terminal
creation/focus activity, compositor identity, and application launch attempts.
Layout v2 stores application scratchpad placements separately from normal
workspace trees, with visibility and the normal workspace of shown anchors.
Valid v1 SQLite, legacy JSON, and backup layouts upgrade only in memory on read;
the next layout save writes v2. Row and payload versions must agree, and v1
documents cannot contain scratchpad fields. Database/context schemas and broker
protocols are unchanged. Older binaries reject v2 layouts rather than treating
scratchpad membership as a workspace or silently discarding it.
Restore reports use optional typed scratchpad intent rather than a synthetic
workspace name. Completion requires fresh membership and visibility proof,
including the saved normal workspace for shown anchors. Live application
placement is available through the read-only `status` command; ambiguous groups
have unknown placement instead of a guessed anchor.
Context rows may include optional `lifecycle: {reason, at}` metadata for the
latest explicit archive, explicit activation, or confirmed terminal-close
transition. This is a bounded explanation attached to authoritative state, not
an event history or a separate restore policy. Missing metadata stays unknown;
no legacy backfill is inferred from an archive timestamp. The additive field
keeps document/schema versions and paths stable. Older binaries with strict
context decoding do not understand newly written lifecycle metadata and fail
closed; do not mix old and new writers or assume downgrade compatibility.

A shared pure next-login policy evaluator supplies terminal automatic selection,
desktop desired-open eligibility, layout membership, manager explanations, and
the read-only `restore --preview` interface. Runtime observation uncertainty is
reported separately; the preview never runs the stateful application planner
or triggers lifecycle effects.

Optional restore-report tables retain structured diagnostic evidence: the
latest automatic run and the latest explicit attempt for each context, plus a
single run ID/timestamp for the most recent automatic interruption. Attempt
tokens prevent late updates from overwriting a newer request; window,
placement, and layout proofs are independent of launch acceptance. Reports are
never read as lifecycle authority or desired layout. A fresh explicit attempt
may rearm the existing daemon layout algorithm only after current policy and
identity checks, an already marked live window or application anchor, and an
exact match with the currently saved layout. Newly mapped unmarked windows first
pass through normal adoption before structural restoration;
report observation must not preempt that adoption. A historical
record cannot supply a layout to replay. Application launch retries clear a
coordinator guard only after a newer explicit request, fresh policy/identity
and absence checks, and this daemon's definitive rejection of the matching
launch attempt. Accepted or ambiguous launches retain their guards across
restart. A candidate guard removal is persisted before coordinator adoption.
Window focus alone does not cancel saved placement, including automatic map
and return focus or a manual click. No mapping-focus allowance or foreign-window
history is needed. Layout observation positively selects eligible registered
terminals and unambiguous application anchors from the current tree and registry;
it neither authorizes commands nor changes eligibility. Move/close intent is
validated from the event payload against saved eligible contexts. Actual adoption
and structural effects retain their lifecycle guards.
History writes are short
transactions around observations and effects, not transactions containing those
effects. Missing optional tables mean no history, and read-only access does not
create them. Existing database and context schema versions remain unchanged.

The database uses WAL and full synchronous commits. Database and sidecar files
must be regular, single-link, current-owner files with mode 0600.

Transactions validate and encode outside writer acquisition, update only
changed rows, and never span Sway IPC, Herdr, process inspection, or launcher
calls. External effects use observe/record/act/re-observe sagas so a timeout or
lost acknowledgement can be resolved from fresh state.

The existing pre-release migration remains the only legacy bridge. When no
database exists but recognized JSON runtime documents do, normal state opening
fails closed. terminal manage action m imports valid documents atomically and
leaves every source document unchanged. LAB-119 adds no migration because all
runtime paths and formats stay stable.

## Terminal lifecycle

Terminal configuration is a strict version-2 typed choice: adapter alacritty or
foot, with session manager herdr. Existing contexts persist the chosen adapter.
A later config change applies only to new identities unless an archived,
unmapped context passes the explicit reconfigure checks.

terminal --new creates a UUID-keyed instance and derives a bounded Herdr session
name from the full UUID. terminal --project NAME resolves one stable project
identity. terminal with neither option resolves a default identity.
--ephemeral launches an ordinary typed terminal and never touches the registry.

Terminal and desktop application processes start in independent Unix sessions.
The shared process starter returns after successful process creation and reaps
each owned child asynchronously with its own `Cmd.Wait`. An application's later
exit is not a launch failure. A short-lived CLI can exit while the application
continues; the long-lived daemon does not retain exited children as zombies.

The management TUI distinguishes durable restore eligibility from observed
window presence. An active context is enabled for automatic restore; it need
not have a mapped terminal. An archived context may still have an open window.
Presence is transient presentation state derived from a bounded, read-only
Sway tree observation, never persisted into the context state or substituted
for it. Failed or ambiguous observations are unknown. Refreshing inventory
must preserve selection and must not retain an old presence claim as current
after an observation failure. Background Herdr/agent liveness is a separate
concern and is not inferred from the presence of a terminal window.

Terminal inventory observations are ephemeral and do not change SQLite state.
The typed session-manager observer shares bulk discovery across all listed
contexts, then queries running sessions through bounded owner-validated sockets.
The manager loads saved inventory first and receives activity asynchronously;
generation checks discard results superseded by refresh or mutation, and closing
the manager cancels its probes. Live agent detection is independent of saved
agent associations. Unknown evidence is explicit and cannot authorize purge;
the existing fresh exact-target deletion checks remain authoritative.

A live, unambiguous close event for a previously observed active terminal stages
an in-memory close candidate. After a short grace period, the daemon confirms
absence from a fresh tree under the terminal lifecycle lock, then archives the
unchanged context in a short database update. It retains history, identity,
and placement; it never purges the session or kills background agents. Existing
archived-context activation and explicit restore remain the reopening paths.
An absent context at daemon startup is not evidence of a manual close.

Close candidates are valid only within one healthy Sway subscription and one
healthy shutdown-observer generation. The logind observer holds a delay
inhibitor and suppresses close handling before releasing it on shutdown/sleep
preparation. During initialization, it retains up to 64 distinct session-removal
paths with their signal senders until its own PID's session is resolved. An
unrelated logout does not disable the validated guard; own-session removal,
sender inconsistency or excessive startup churn fails closed. The pending set
is discarded after attribution or termination. Sway shutdown invalidates the synchronous stream guard before
queueing the event. Disconnect, cancellation, ambiguous identities, or loss of
shutdown protection discard candidates conservatively. No monitor support means
explicit archive remains available, not that every disappearance becomes a
manual close. Root-forced shutdowns and third-party logout paths which kill
clients before signaling Sway/logind are not an observable intent contract.
Sway's close event does not carry a reason: normal closes, shell exit, and
terminal-adapter failure are indistinguishable. This feature protects
machine/compositor shutdown, not automatic crash recovery of the terminal
adapter. A retained archived session can still be explicitly reopened.
This adds no context schema, state path, or package dependency metadata change.

The manager retains the visible selection row after archive/delete so repeated
cleanup advances through remaining entries. Activation, rename, open and refresh
preserve context identity; filters survive reloads.

~~~mermaid
sequenceDiagram
    actor User
    participant CLI
    participant Lock as lifecycle lock
    participant DB as SQLite
    participant Sway
    participant Herdr as terminal manager
    User->>CLI: terminal identity and optional roles
    CLI->>Lock: acquire owner-only lock
    CLI->>DB: resolve or durably create context
    CLI->>Sway: observe exact typed identity
    alt already mapped
        CLI->>Sway: focus exact container
    else absent
        CLI->>Herdr: start or attach named session
        Herdr-->>Sway: map adapter window
        CLI->>Sway: verify stable mapping
    end
    CLI->>Herdr: initialize roles idempotently
    CLI->>Sway: verify mapping again
    CLI->>Lock: release
    CLI-->>User: typed result with context UUID
~~~

A process spawn is not terminal success. The same Sway container must remain
mapped across bounded stability checks. A rejected adapter start can roll back a
fresh unused identity. Once an adapter launch is accepted, its named manager
state may exist despite window loss, so the context remains the recovery
identity. Role initialization is idempotent and never restarts an agent in an
occupied pane.

After exactly one adapter start has been accepted by the current open request,
multiple matching pending processes are resampled under the existing mapping
deadline. No process is selected and no window focus or role initialization is
authorized while that ambiguity remains, even if one window is already visible.
A fresh unambiguous process observation must precede the normal stable-window
checks. Persistent ambiguity fails explicitly and preserves the recovery context;
ambiguity observed before this request starts an adapter remains an immediate
error. Disappearing pending processes never authorize a second start after the
accepted one.

## Capture and restore

The daemon owns session observation, stable hidden marks, desktop presence,
layout capture, terminal and application restore, placement, and layout
reconstruction. The top-level restore command queues or launches selected
contexts, but daemon reconciliation applies saved placement and layout.

~~~mermaid
sequenceDiagram
    actor User
    participant Restore as one-shot restore
    participant DB as SQLite
    participant Manager as terminal adapter
    participant Sway
    participant Daemon
    User->>Restore: restore optional-context
    Restore->>DB: read active or selected contexts
    Restore->>Sway: observe current windows
    Restore->>Manager: start missing terminal sessions
    Manager-->>Sway: map windows
    Restore-->>User: launch or mapping result
    Sway-->>Daemon: relevant event
    Daemon->>DB: load saved workspace and layout
    Daemon->>Sway: fresh observation
    Daemon->>Sway: bounded placement/layout actions
    Daemon->>DB: short captured-state commit
~~~

Automatic terminal restore launches at most two missing adapters in each
lifecycle-locked wave, then reloads database and compositor state. Application
preflight rotates across at most two candidates per pass. Placement and
indicator actions use rotating bounded batches and advance after planning even
when a complete batch is rejected. Layout restore re-observes after each
mutation and has a complexity-scaled convergence budget. These are latency and
request-size limits, never a total-context cap.

Live binding, focus, and layout events invalidate stale reconciliation work.
An automatic action never assumes its previous observation still holds.

## Desktop applications

A desktop context is created only after exact Wayland, XWayland, or Flatpak
identity resolution and explicit approval. Ambiguous desktop-entry or window
matches are rejected. System entries are root-owned launch material. Approved
user-local entries are copied into owner-only immutable snapshots; later source
or executable changes require reapproval.

Every matching eligible top-level is one application presence group. Multiple
indistinguishable windows prove presence but do not provide an anchor. Only an
existing stable mark or one unique match permits placement. Follow mode enables
desired-open from live presence. Disabling it after the last window disappears
requires prior healthy presence, the close grace, fresh absence under the
registry lock, and the same healthy logind and Sway subscription generations.
The shared shutdown observer also protects automatic terminal archival; no
second monitor is created. Its memory guard is rechecked immediately before
the registry mutation, outside any external calls or SQLite write transaction.
Tree acquisition and every planning pass share one lifecycle generation, so a
tree captured before shutdown or rearm cannot seed new healthy close evidence.

Shutdown, sleep, monitor failure, missing logind protection, or compositor
disconnect preserve Follow desired-open. Changes to either generation discard
close observations and timers even if an unsafe interval fell between passes.
Rearming requires new healthy presence before a subsequent absence can count
as a close; time spent unsafe cannot consume the close grace. App adoption and
launch attempts remain intact so uncertainty cannot cause a watchdog relaunch.
Explicit archive, policy changes, and lifecycle reservations retain authority.
Startup absence is never interpreted as a historical close. Forced shutdowns
or logout tools which kill clients before notifying Sway/logind remain outside
the observable intent contract. A crash and an ordinary last-window close
cannot be distinguished while lifecycle observation is otherwise healthy.

Pinned mode keeps desired-open across starts. Launch intent is recorded before
process start so daemon restart or ambiguous outcome cannot duplicate an
attempt in one compositor session.

Observed application presence also satisfies its startup opportunity. Adoption
evidence is durably stored separately from launch attempts before placement,
policy mutation or a subsequent launch. Restarting the daemon in the same
compositor therefore cannot relaunch an adopted application after it closes.
A failed observation write retains the pending evidence in memory and defers
effects until a later fresh pass can save it. A new compositor clears adoption
evidence; explicit Follow desired-close/rearm and authorized rejected-launch
retry can rearm the corresponding opportunity. An adopted, completed launch
does not occupy a concurrency slot after its window closes.

The optional schema-1 `application_adoptions` table stores context IDs,
observation timestamps and compositor identity. Its revision triggers and
context foreign key participate in the application-session conflict checks.
Existing databases without the extension remain readable without writes, and
metadata backups validate either complete form. Each row carries its own
compositor identity so an older binary resetting session metadata cannot revive
observations from a previous compositor. Older binaries cannot enforce the new
adoption guarantee themselves.

Each prepared desktop launch is confirmed against a new bounded Sway tree
under the lifecycle/registry lock, outside SQLite transactions. Preparation can
block on launcher validation; the caller's earlier absence is insufficient.
Fresh presence is adopted without starting another process or consuming an
attempt, even when placement is ambiguous. Current policy, reservations,
adoption, attempts and concurrency use the same coordinator rules. Attempt
timestamps are sampled after preparation and observation, rather than inherited
from the beginning of reconciliation.

The event subscription must retain the same connected generation before
preparation, after observation and immediately before process start. Missing,
invalid or ambiguous evidence defers the candidate without a launch attempt.
A disconnect after intent has already committed prevents process start but
retains that intent conservatively. Candidate rotation and the two-preflight
pass bound also bound the additional tree requests. An independent application
can still map between the final observation and process start: SQLite and Sway
cannot atomically exclude this race, so this is not a global exactly-once launch
guarantee. User-opened windows are never closed to enforce the earlier plan.

An active, desired-open application in the saved exact layout may finish its
startup after the settling deadline. The daemon retains that startup intent
until the first uniquely identified unmarked anchor is adopted, then resumes
the saved structural restore. Existing marked windows, ambiguous groups,
changed identities or restore eligibility, and later replacement windows do
not receive another startup restore opportunity. User interaction and lost
event-stream continuity cancel the pending opportunity, including while the
application is still missing. Fresh observations also retire it when the
surviving workspace structure changes without an input event, including before
the startup deadline. Successful daemon-owned initial placement refreshes the
affected workspace observations before comparison resumes. Geometry is compared
after startup settles and while the window set is unchanged. Initial client and
decoration geometry can settle without user input, and mapping a new view
legitimately resizes its siblings, so a simultaneous resize cannot be
distinguished from that mapping by this check. Placement-only snapshots retain placement-only
behavior, and a fresh degradation retires the pending structural intent before
the debounced snapshot is written. A live application in the daemon's temporary
restore workspace still counts as present for application lifecycle tracking;
staging must not change `DesiredOpen` or trigger a duplicate launch. This does
not restore application-internal state or pane directories.

Sway 1.12 exposes sufficient XWayland transient metadata to exclude recognized
dialogs but not equivalent native Wayland parent/type data. Per-window native
Wayland identity remains tracked in LAB-93.

## Presentation mark wire contract

The daemon is the sole producer of version-1 application indicator marks.
Marks contain state and Sway container ID only; they contain no context UUID,
launcher, registry value, or restore state. An optional independent renderer
may consume them.

internal/titleindicator/testdata/v1.json is authoritative for valid and invalid
wire examples. sway-session and sway-title-animator intentionally keep
byte-identical copies and run the same golden test. Any v1 change requires an
explicit compatibility decision and coordinated updates in both repositories.
From sibling checkouts, compare with:

~~~sh
cmp ../sway-session/internal/titleindicator/testdata/v1.json \
  ../sway-title-animator/internal/titleindicator/testdata/v1.json
~~~

Unknown future marker versions are ignored so one implementation never removes
marks owned by a version it does not understand. There is no third repository
or runtime dependency for this small contract.

## Security boundaries

IPC payload sizes are fixed-bounded before allocation. File-backed state
requires private directories and safe regular files. Daemon and terminal
lifecycle locks are held for their defined process/effect windows.

session-start.sock accepts one versioned ensure-and-start request without pane
roles or command strings. agent-report.sock accepts a protocol-v3 association
with optional native event origin, or a source-less protocol-v2 association
after peer credentials and pane-process ancestry checks. Neither returns raw
registry contents. LAB-125 removes the legacy Codex report endpoint and CLI;
provider events are translated at the CLI hook-input boundary into the generic
input shape. LAB-138 preserves the native event origin and confirms Herdr's
live association through a bounded snapshot. Source-less report v2 clients
retain their original wire shape and receive v2 replies; new v3 clients never
downgrade to an older broker. Session-start v1 and stored state are unchanged.
See [agent reporting](agent-reporting.md) for the input and compatibility contract.

### Shared-workspace session starts (LAB-270)

The session-start broker permits multiple independently identified terminal
contexts on one explicitly requested numbered workspace, including unrelated
tiled and floating occupants. Workspace occupancy is not a context identity
or authentication boundary. LAB-88 originally limited creation to empty
workspaces; LAB-90 extended exclusivity to reuse and final acceptance. The
history records conservative placement and race checks, but no threat model
in which neighboring windows require whole-workspace exclusion. The supported
contract now verifies the requested context instead of counting all occupants.

Each request retains its exact metadata, immutable context UUID, and unique
managed leaf window. Numbered destination workspaces must be unambiguous.
Live placement conflicts are rejected rather than silently moving an existing
context. Saved placement is additionally checked before restoring an unmapped
context; a mapped context's current placement takes precedence over a saved
snapshot that may await capture. Reuse explicitly focuses the observed container ID and
checks a fresh tree after the command; a close, replacement, duplicate,
placement change, or lost focus prevents a successful response. A new context
is launched through the existing trusted restore path after selecting the
destination, and its actual placement is verified before initialization.
Workspace selection cannot reserve compositor focus against user actions;
incorrect placement is reported without moving neighboring windows.

Requests remain serialized, with the existing lifecycle and registry locks;
external calls still occur outside SQLite write transactions. Partial launch
or initialization failures retain the registration for an exact retry. Herdr
initialization remains scoped to the named session and changes only a proven
empty idle shell. Existing sessions and their pane layouts are left intact.

Owner-only socket/state permissions, peer credentials, cwd validation, trusted
system executable resolution, message bounds, and the fixed command/role
vocabulary are unchanged. Safe, allowlisted rejection reasons use the existing
protocol-v1 error string and expose no private paths, session names, titles,
registry payloads, or arbitrary child-process errors. Stored schemas and the
request/response fields do not change; older clients retain generic rejection
handling. This change does not strengthen the separate AppArmor confinement
boundary described below.

The included `agent-home-guard` AppArmor template is an optional agent
hardening measure, not a sway-session runtime requirement. Its ready-to-use
example attaches to Codex; a different agent needs its own profile name and
executable attachment. Its file rules protect the default Herdr history and
sway-session state trees, but pathname-socket connect mediation is not reliable
on every supported kernel and launched terminal panes remain unconfined. LAB-89
tracks a stronger agent sandbox boundary. The
[AGS/Herdr contract audit](https://github.com/marang/sway-session/blob/main/docs/research/ags-herdr-broker-confinement.md) records
the upstream evidence and the conditions for replacing the current start path.
Root-owned launchers and typed requests do not by themselves confine the
host-shell startup or the agent and shell panes created by Herdr.

## Standalone and release decisions

- Go module: github.com/marang/sway-session.
- Command and package: sway-session only.
- First standalone release: v0.1.0.
- Release artifacts: Linux amd64 and arm64 tar.gz, DEB, and RPM.
- Arch package: source build with Sway as its sole runtime dependency and Go as
  its sole build dependency.
- Integration documentation: /usr/share/doc/sway-session.
- Source history is preserved, while old sway-title-animator tags are not
  published from the new remote.
- The bootstrap PKGBUILD uses SKIP only because v0.1.0 has no archive yet. The
  release workflow replaces it with the downloaded immutable archive checksum
  and refuses publication if SKIP remains.

The old combined sway-title-animator package may own /usr/bin/sway-session.
Release rollout must avoid an overwrite: upgrade sway-title-animator to a
version that no longer owns that path before installing sway-session, or
install verified packages for both repositories in one package-manager
transaction. Never recommend --overwrite.

## Active follow-ups

The following existing issues belong to the Sway Session Linear project and
Codebase → Sway Session label:

- LAB-89: stronger sandboxing for broker-created agent sessions.
- LAB-93: stable native Wayland per-window identity.
- LAB-94: explicit privileged desktop restore design.
- LAB-250: complete desktop inventory lifecycle explanations.
- LAB-252: investigate bounded restore-report CI contention.
- LAB-253: remaining real Chrome/Slack lifecycle and guest reboot acceptance.

The [native window-tag audit](https://github.com/marang/sway-session/blob/main/docs/research/xdg-toplevel-tag-audit.md) records that
Sway 1.12 already exposes client tags. Duplicate/mutable tag semantics and
unproven durable application adoption keep generic per-window restore gated;
application-level restore remains the fallback.

The [privileged desktop restore evaluation](https://github.com/marang/sway-session/blob/main/docs/research/privileged-desktop-restore-gate.md)
documents the current administrative-launcher rejection boundary and the
consent, identity and upstream mechanism requirements for a future design.
It does not enable privileged restore or automatic authentication at login.

Active sequencing and acceptance criteria live in Linear; this document owns
the durable architecture and compatibility decisions.
