# Sway Session architecture and product plan

Version: 1.0
Status: Active
Project: [Sway Session](https://linear.app/riotbox/project/sway-session-74dd95a8e064)

## Purpose

sway-session gives explicitly registered work contexts durable outer-window
identity across Sway starts. It combines a private registry, typed terminal
launch adapters, desktop-application presence groups, bounded compositor
reconciliation, and two narrow owner-only brokers.

The standalone repository was extracted under LAB-119 from the complete
sway-title-animator history. The split changes source ownership, packaging, and
release identity only. It does not change CLI behavior, XDG paths, stored state,
marks, sockets, environment variables, application IDs, or wire protocols.

## Goals

- Restore registered terminal and desktop-application contexts without
  restoring application-private state.
- Preserve one stable context UUID across capture, archive, activation,
  terminal recovery, and exact deletion.
- Treat live user focus and layout changes as higher priority than automation.
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
        Codex[internal/codexreport]
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
    Daemon --> Codex
    Start --> Session
    Codex --> Session
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
- internal/herdrinit owns fixed, idempotent role initialization behind the
  closed Herdr session-manager adapter. It is not an executable.
- internal/sessionrequest accepts one protocol-v1 ensure-and-start operation.
- internal/codexreport accepts one protocol-v1 Codex SessionStart association.
- internal/titleindicator owns only the versioned presentation mark wire
  contract.
- internal/diagnostic owns stable human and JSON diagnostics.

## Durable state

Runtime state is
$XDG_STATE_HOME/sway-session/state.sqlite3, or
~/.local/state/sway-session/state.sqlite3 when XDG_STATE_HOME is unset.
Configuration is
$XDG_CONFIG_HOME/sway-session/config.toml, or
~/.config/sway-session/config.toml when XDG_CONFIG_HOME is unset. Runtime
sockets and locks remain below $XDG_RUNTIME_DIR/sway-session.

SQLite schema 1 stores schema-5 contexts, layout schema 1, terminal
creation/focus activity, compositor identity, and application launch attempts.
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
existing stable mark or one unique match permits placement. Follow mode derives
desired-open from presence after a short close grace. Pinned mode keeps
desired-open across starts. Launch intent is recorded before process start so
daemon restart or ambiguous outcome cannot duplicate an attempt in one
compositor session.

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
roles or command strings. codex-report.sock accepts one versioned association
after peer credentials and pane-process ancestry checks. Neither returns raw
registry contents.

The included AppArmor profile is experimental. Its file rules protect the
default Herdr history and sway-session state trees, but pathname-socket connect
mediation is not reliable on every supported kernel and launched terminal panes
remain unconfined. LAB-89 tracks a stronger agent sandbox boundary.

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
- LAB-92: scratchpad persistence.
- LAB-93: stable native Wayland per-window identity.
- LAB-94 and LAB-95: remaining bounded session roadmap slices.
- LAB-116: current post-SQLite session follow-up.

Active sequencing and acceptance criteria live in Linear; this document owns
the durable architecture and compatibility decisions.
