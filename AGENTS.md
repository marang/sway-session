# Repository guidance

## Project

sway-session is a Linux-only Go program for explicit, persistent work sessions
in Sway. It owns session registration, terminal and desktop-app lifecycle,
capture, placement, layout restore, and narrow owner-only brokers. It
communicates directly with the Sway/i3 IPC socket.

This repository is independent of sway-title-animator. Do not add an animator,
audio capture, animation presets, or a source dependency on the animator
repository. The two repositories intentionally retain small copies of the
bounded Sway IPC and presentation-mark packages; no third shared module is
planned.

## Required checks

Run make verify before handing off code changes. It checks formatting, unit and
race tests, go vet, staticcheck, AppArmor, completion, packaging, standalone
boundaries, a CGO_ENABLED=0 build, and whitespace. Use make fmt for Go
formatting. The module requires Go 1.26; the preferred security-patched
toolchain is Go 1.26.5 as declared in go.mod.

Run the code-review skill before opening or finalizing a PR. Run review-codebase
after every fifth substantive branch or at a meaningful project checkpoint,
whichever comes first.

## Workflow

docs/workflow_conventions.md is canonical for planning, branches, PRs, review,
releases, and cleanup.

Work is coordinated in the shared Linear Lab team. Every issue for this
repository must belong to the
[Sway Session project](https://linear.app/riotbox/project/sway-session-74dd95a8e064)
and carry the mutually exclusive Codebase → Sway Session label. Normal
implementation starts from one issue in In Progress, uses a branch containing
its LAB-* key, and reaches Done only after its PR is merged.

## Architecture

- cmd/sway-session: CLI, explicit daemon, Sway integration, typed terminal
  management TUI, and broker adapters.
- internal/session: validated contexts and identities, terminal adapters,
  owner-only SQLite state, activity, application lifecycle, capture, and
  restore coordination.
- internal/sessionrequest: owner-only typed session-start protocol and service.
- internal/codexreport: owner-only Codex SessionStart reporting protocol and
  service.
- internal/herdrinit: fixed initialization behind the typed Herdr adapter; it
  has no standalone executable.
- internal/statefile: private-directory, file, and lock primitives.
- internal/swayipc: bounded i3/Sway IPC framing, tree types, events, and
  reconnect behavior.
- internal/titleindicator: versioned, presentation-only Sway mark protocol; it
  contains no registry or restore state.
- internal/diagnostic: stable structured and human-readable CLI diagnostics.

Keep new responsibilities in the matching package. Prefer small pure helpers
and injected process, time, terminal, and compositor boundaries.

Session runtime state belongs in the owner-only state.sqlite3 database;
configuration stays in strict text files. Do not impose a total context-count
limit. Keep Sway IPC work bounded per reconciliation pass and resumable from a
fresh observation. SQLite transactions must be short and must never contain
external Sway, Herdr, process, or desktop-launcher calls; coordinate effects as
explicit retryable sagas around durable transitions.

## Compatibility

Keep the established CLI, XDG config/state paths, database and document schema
versions, runtime socket names, environment variables, application IDs, hidden
marks, and broker protocol v1 wire shape stable unless a dedicated issue
explicitly changes that contract.

internal/titleindicator/testdata/v1.json is the authoritative v1 mark-wire
fixture. Both this repository and sway-title-animator must keep an identical
copy and test against it. Compare the files across sibling checkouts when the
wire package changes; never silently update only one side.

## Safety

- Never trust IPC payload lengths without a fixed upper bound.
- Never mutate session state from a read-only completion path.
- Never guess between ambiguous windows, contexts, launchers, or application
  identities.
- Preserve owner-only permissions and exclusive daemon/lifecycle locks.
- Do not run Sway, Herdr, process, or launcher calls inside SQLite transactions.
- Real compositor tests must use disposable state roots and workspace 98 or
  higher. Never create, move, close, restore, or purge test windows on
  single-digit workspaces.
- Never commit credentials, private registry contents, pane history, captured
  terminal output, sockets, generated binaries, or transient logs.
