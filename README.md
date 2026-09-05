# sway-session

<p align="center">
  <a href="docs/branding.md"><img src="docs/assets/sway-session-icon.jpeg" width="160" height="160" alt="sway-session logo: a tiled window layout with a green pane"></a>
</p>

sway-session keeps explicitly registered work contexts available across Sway
starts. It restores terminal adapters backed by named Herdr sessions, tracks
desktop applications as application-level groups, and reconciles their outer
Sway workspace and layout without owning application-private state.

The project is Linux-only and talks directly to the bounded Sway/i3 IPC socket.
It is independent of sway-title-animator: neither program requires the other.

## What it owns

~~~mermaid
flowchart LR
    User[CLI or key binding] --> CLI[cmd/sway-session]
    CLI --> Store[(owner-only state.sqlite3)]
    CLI --> Herdr[typed Herdr adapter]
    CLI --> Sway[bounded Sway IPC]
    Daemon[sway-session daemon] --> Store
    Daemon --> Sway
    Daemon --> Launch[desktop and terminal launchers]
    Daemon --> Broker[owner-only typed brokers]
    Broker --> Herdr
    Store -. short transactions only .-> Daemon
    Sway -. v1 presentation marks .-> Render[optional title renderer]
    Render -->|title format commands| Sway
~~~

The daemon observes live compositor state, maintains marks, captures layout,
restores missing desired applications, and applies placement. One-shot restore
can launch or map a terminal, but daemon reconciliation performs its saved
workspace placement and layout reconstruction.

## Install

Build from source with Go 1.26.5 or a compatible Go 1.26 toolchain:

~~~sh
git clone https://github.com/marang/sway-session.git
cd sway-session
make verify
make install
~~~

The default source prefix is ~/.local. It installs:

~~~text
~/.local/bin/sway-session
~/.local/share/bash-completion/completions/sway-session
~/.local/share/zsh/site-functions/_sway-session
~/.local/share/fish/vendor_completions.d/sway-session.fish
~/.local/share/doc/sway-session/50-sway-session.conf
~/.local/share/doc/sway-session/contrib/...
~~~

Release archives contain the same integration assets under repository-relative
paths. DEB, RPM, and Arch packages install integration documentation under
/usr/share/doc/sway-session. The only package runtime dependency is Sway.
Selected integrations are discovered at runtime: persistent terminals require
Herdr plus Alacritty or Foot, desktop-entry launch uses gio, and Flatpak restore
uses flatpak.

Published archives and packages are available from the
[GitHub releases](https://github.com/marang/sway-session/releases) page. After
the v0.1.0 package ownership transition described in docs/releasing.md is
complete, Arch users can install the standalone AUR package with:

~~~sh
yay -S sway-session
~~~

Do not use a package-manager overwrite flag to install it over an older
combined sway-title-animator package that still owns /usr/bin/sway-session.

The first standalone release is planned as v0.1.0. Until that immutable tag
exists, the checked-in Arch metadata deliberately uses SKIP rather than an
invented archive checksum. The release workflow replaces it with the checksum
of the actual tag archive, refuses to publish SKIP, builds the source package,
and records the exact verified metadata.

## Sway setup

Include contrib/sway/50-sway-session.conf, or add:

~~~conf
exec --no-startup-id /usr/bin/sway-session daemon
exec --no-startup-id /usr/bin/sway-session restore
bindsym $mod+Return exec --no-startup-id /usr/bin/sway-session terminal --new
bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral
~~~

Source installs can replace /usr/bin with $HOME/.local/bin. Both startup
commands intentionally use exec, not exec_always: reloading the Sway config
must not start another daemon or request another startup restore. The daemon
also holds an owner-only exclusive runtime lock.

## Persistent terminals

Herdr pane history is required for persistent work contexts. Copy
contrib/herdr/config.toml to the herdr directory under $XDG_CONFIG_HOME (or
~/.config/herdr/config.toml when unset) and keep it private because pane history
may contain command output, paths, and tokens.

The default typed adapter is Alacritty with Herdr. The strict optional config is
$XDG_CONFIG_HOME/sway-session/config.toml, falling back to
~/.config/sway-session/config.toml:

~~~toml
version = 2

[terminal]
adapter = "alacritty"
session_manager = "herdr"
~~~

The only adapter values are alacritty and foot. The only session-manager value
is herdr. These values select compiled argument shapes; configuration cannot
supply executable paths, command templates, or shell snippets.

Common commands:

~~~sh
sway-session terminal --new
sway-session terminal --new --role codex --role shell
sway-session terminal --project LAB-119 --cwd "$PWD"
sway-session terminal --ephemeral --cwd "$PWD"
sway-session terminal manage
sway-session --json terminal list
sway-session --json terminal status --project LAB-119
sway-session terminal rename --label "Release work" CONTEXT_UUID
sway-session terminal cleanup --archived-before 2026-09-01
~~~

The CLI is its own authoritative syntax reference. Use sway-session --help for
the command index and sway-session help COMMAND for every current option, for
example:

~~~sh
sway-session help terminal
sway-session help register
sway-session help restore
sway-session help app
sway-session help daemon
sway-session help request-start
~~~

Global --json and --config options precede the command. Command help includes
the terminal list, status, cleanup, reconfigure, rename, and manage forms; all
desktop-app subcommands; optional Sway socket overrides; registration identity
and presentation metadata; and the read-only completion interface.

terminal --new creates one fresh persistent identity and Herdr session.
terminal without an identity reuses one default terminal; --project reuses one
stable named identity. --ephemeral creates no registry state. cleanup only
previews archived candidates; deletion always uses an exact reviewed UUID.

In `terminal manage`, saved contexts and open windows are separate counts.
Each entry shows its observed window presence (`open`, `closed`, or `unknown`)
separately from whether automatic restore is enabled or the context is
archived. Closing a window keeps its saved context; archiving excludes it from
automatic restore and does not mean its window is closed. Codex and shell
panes inside one terminal belong to the same context.

Window presence is a Sway observation refreshed when the TUI opens, after
successful actions, or with `r`. It does not indicate whether a background
Herdr server or agent is running. An unavailable compositor or ambiguous
window identity produces `unknown`, not a claim that the terminal is closed.

~~~mermaid
sequenceDiagram
    actor User
    participant CLI as sway-session terminal
    participant DB as state.sqlite3
    participant Sway
    participant Manager as typed Herdr adapter
    User->>CLI: terminal --new or --project
    CLI->>DB: create or resolve identity
    CLI->>Sway: observe matching window
    alt window already mapped
        CLI->>Sway: focus exact container
    else window absent
        CLI->>Manager: start or attach named session
        Manager-->>Sway: map typed terminal window
        CLI->>Sway: verify same container is stable
    end
    CLI->>Manager: initialize requested roles idempotently
    CLI->>Sway: verify window again
    CLI-->>User: context UUID and actions
~~~

Lifecycle commands accept an exact UUID or unambiguous exact label:

~~~sh
sway-session register --session lab-119 --label LAB-119 --provider linear
sway-session list
sway-session archive LAB-119
sway-session activate LAB-119
sway-session --json purge LAB-119
sway-session purge --yes CONTEXT_UUID
~~~

Archive retains the named Herdr session but removes the context from automatic
restore. purge first previews the canonical UUID; the destructive --yes form
accepts only that UUID, stops and deletes the exact Herdr session, and removes
its registry entry.

## Restore behavior

~~~mermaid
sequenceDiagram
    actor User
    participant CLI as sway-session restore
    participant DB as state.sqlite3
    participant Manager as typed terminal adapter
    participant Sway
    participant Daemon as sway-session daemon
    User->>CLI: restore optional-context
    CLI->>DB: read active desired contexts
    CLI->>Sway: observe current containers
    CLI->>Manager: start missing terminal sessions
    Manager-->>Sway: map windows
    CLI-->>User: launch or mapping result
    Sway-->>Daemon: window event
    Daemon->>DB: read saved placement and layout
    Daemon->>Sway: re-observe then apply bounded actions
    Daemon->>DB: commit captured state after effects
~~~

Automatic restore is deliberately bounded and resumable. It launches at most
two missing terminal adapters in a lifecycle-locked wave and then re-observes
state. Placement and indicator planners rotate bounded batches. Layout restore
re-observes after each mutation. Those limits bound latency and IPC request
size; they are not registry capacity limits.

## Desktop applications

Focus a normal top-level window and register it explicitly:

~~~sh
sway-session app register-focused
sway-session --json app list
sway-session app status CONTEXT
sway-session app pin CONTEXT
sway-session app unpin CONTEXT
sway-session app archive CONTEXT
sway-session app activate CONTEXT
sway-session app rebind-focused CONTEXT
sway-session app reapprove CONTEXT
sway-session app forget --yes CONTEXT_UUID
~~~

Package installs use a native swaynag approval flow. A source-only install can
make the same decision from a trusted terminal with --yes. Ambiguous desktop
identity is never guessed. System entries are revalidated as root-owned launch
material; approved user-local entries are stored as owner-only immutable
snapshots and must be reapproved after source or executable changes.

Follow mode remembers whether at least one matching top-level remains open
after a short close grace. Pinned mode keeps the application desired-open
across Sway starts. Multiple indistinguishable windows prove presence but are
not guessed between for anchor placement. sway-session restores the optional
outer anchor only; tabs, documents, profiles, URLs, and application-internal
state remain application-owned.

The daemon emits versioned hidden marks for optional presentation clients:

~~~text
unregistered  _sway_session_app_indicator_v1_unregistered_CONTAINER
pending       _sway_session_app_indicator_v1_pending_CONTAINER
registered    _sway_session_app_indicator_v1_registered_CONTAINER
pinned        _sway_session_app_indicator_v1_pinned_CONTAINER
~~~

internal/titleindicator/testdata/v1.json is the authoritative v1 wire fixture
shared byte-for-byte with sway-title-animator. Unknown future versions are
ignored so each owner cleans up only marks it understands.

## State and compatibility

Runtime state remains at $XDG_STATE_HOME/sway-session/state.sqlite3, falling
back to ~/.local/state/sway-session/state.sqlite3.

SQLite schema 1 contains schema-5 context payloads, captured layout, terminal
activity, compositor identity, and application launch attempts. It uses WAL,
full synchronous commits, owner-only mode 0600, and short transactions. Sway,
Herdr, process, and launcher calls always occur outside transactions.

Configuration remains under the XDG config root. Runtime sockets and the daemon
lock remain under $XDG_RUNTIME_DIR/sway-session. The standalone extraction does
not rename CLI commands, config/state paths, sockets, environment variables,
hidden marks, application IDs, schemas, or the two version-1 broker protocols.

Existing pre-release JSON users retain the explicit source-preserving migration
already provided by terminal manage: press m when the database is absent and
legacy runtime documents are present. The import is idempotent, transactional,
and does not delete its JSON source. No additional extraction migration is
required.

## Narrow Codex integration

The daemon hosts two owner-only typed endpoints: session-start.sock for a fixed
ensure-and-start request and codex-report.sock for a validated SessionStart
association. Neither endpoint returns raw registry contents or accepts
arbitrary commands.

~~~sh
/usr/bin/sway-session --json request-start \
  --session lab-119 --cwd "$PWD" --label LAB-119 --workspace 98
~~~

The supplied Codex hook and AppArmor policy remain an experimental boundary.
They do not reliably mediate pathname-socket connect on every supported kernel,
and broker-created terminals and panes are currently unconfined. Review that
risk before enabling the integration.

Source installs can merge contrib/codex/hooks.json. Package installs can merge
/usr/share/doc/sway-session/contrib/codex/hooks.json. The AppArmor template is
/usr/share/doc/sway-session/contrib/apparmor/codex-home-guard and the live
positive/negative verification helper is
/usr/share/doc/sway-session/scripts/verify-codex-boundary.sh. The live verifier
requires /usr/bin/sway-session to be a root-owned regular executable owned by
the sway-session package.

## Completion

Bash, Zsh, and Fish adapters are shipped. They use the read-only completion
contexts interface for archive, activate, restore, purge, terminal-status, and
app-forget candidates. Completion never launches a terminal, contacts Sway, or
mutates state. It inserts only canonical UUIDs and does not parse the private
database directly.

## Development

~~~sh
make fmt
make verify
~~~

The verification gate includes unit and race tests, vet, staticcheck,
CGO-disabled build, AppArmor and completion checks, packaging consistency,
standalone source boundaries, and whitespace. Real compositor checks must use
disposable XDG roots and workspace 98 or higher; see
docs/sway-session-verification.md.

This repository preserves the complete project history but publishes no old
sway-title-animator tags. Standalone releases begin at v0.1.0.
