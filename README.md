# sway-session

<p align="center">
  <a href="docs/branding.md"><img src="docs/assets/sway-session-wordmark-dark-banner.jpeg" width="640" alt="sway-session — persistent workspaces for the Sway compositor"></a>
</p>

sway-session brings your registered terminals and desktop applications back when
Sway starts, with their saved workspaces and window layout. Terminals use
[Herdr](https://github.com/herdrdev/herdr) to keep sessions and pane history.
Desktop applications handle their own tabs, documents, and other internal state.

It runs on Linux with Sway. Persistent terminals require Herdr and either
Alacritty or Foot. Desktop-entry launches use `gio`; Flatpak applications also
require `flatpak`.

## Install

Download a package or archive from
[GitHub releases](https://github.com/marang/sway-session/releases), or install
the Arch Linux package from the AUR:

```sh
yay -S sway-session
```

To build from source, use Go 1.26.5:

```sh
git clone https://github.com/marang/sway-session.git
cd sway-session
make verify
make install
```

Source installs put the executable in `~/.local/bin`, completion files in
`~/.local/share`, and integration templates in
`~/.local/share/doc/sway-session`. Packages install the templates under
`/usr/share/doc/sway-session`. Make sure the executable is on your `PATH`.

## Set up Sway and Herdr

For persistent terminals, enable pane history in your Herdr configuration
(`~/.config/herdr/config.toml`, or under `$XDG_CONFIG_HOME`):

```toml
[experimental]
pane_history = true
```

You can also use the supplied [Herdr template](contrib/herdr/config.toml).
Keep Herdr configuration and history private: terminal output can contain tokens
and other sensitive information.

Create the configuration directory, then run Doctor inside Sway:

```sh
mkdir -p ~/.config/sway/config.d
sway-session doctor
```

If your Sway configuration is elsewhere, create `config.d` next to its main
configuration file. In Doctor, select the Sway integration check and press `f`.
Choose whether to add terminal shortcuts, review the file changes, then press
`y` to apply them. Escape cancels. Doctor backs up files it changes. Keep only
one copy of the sway-session startup commands and includes.

For noninteractive setup, preview the changes first, then apply them:

```sh
sway-session doctor --fix sway.integration --adopt-standard
sway-session doctor --fix sway.integration --adopt-standard --yes
```

Doctor writes `config.d/50-sway-session-doctor.conf` and adds its include to your
Sway configuration. For a different main file, add
`--sway-config /absolute/path/to/sway/config`. To include the standard terminal
shortcuts, add `--shortcuts default`; define `$mod` before the include and free
`$mod+Return` and `$mod+Shift+Return` first.

For manual setup, add these lines to your Sway configuration:

```conf
exec --no-startup-id /usr/bin/sway-session daemon
exec --no-startup-id /usr/bin/sway-session restore
```

For a source install, use `$HOME/.local/bin/sway-session` in place of
`/usr/bin/sway-session`. The supplied
[Sway template](contrib/sway/50-sway-session.conf) also includes optional
terminal bindings. Packages install it as
`/usr/share/doc/sway-session/50-sway-session.conf`; source installs use
`~/.local/share/doc/sway-session/50-sway-session.conf`. Use one setup method.

Reloading Sway activates new bindings. The startup commands run at your next
Sway login. To use them in the current session, start `sway-session daemon` in
a terminal and run `sway-session restore` when you want saved windows restored.
Keep startup commands as `exec`: `exec_always` would run them on every reload.

## Terminals

```sh
sway-session terminal --new
sway-session terminal --project my-project --cwd "$PWD"
sway-session terminal --new --role codex --role shell
sway-session terminal --ephemeral --cwd "$PWD"
sway-session terminal manage
```

- `--new` creates a separate persistent session each time.
- `--project NAME` opens or focuses the same named session on later calls.
- With no identity option, `terminal` reuses one default session.
- `--role codex --role shell` creates a Codex-and-shell workspace in a fresh
  Herdr session. Existing occupied panes are left as they are.
- `--ephemeral` opens a terminal without saving it for restore.

The terminal manager shows saved sessions, open windows, Herdr activity, and
what will return at the next login. Its main controls are:

| Key | Action |
| --- | --- |
| ↑/↓ or j/k | Select a session |
| Enter | Open or focus it |
| `/` | Filter |
| `e` | Rename |
| `a` | Archive or activate |
| `t` | Retry a failed restore |
| `d` | Preview deletion |
| `r` | Refresh |

Closing a terminal during normal use archives it after a short grace period,
so it stops returning at login. Its Herdr session and background agents remain.
To reopen it, activate it with `a`, then press Enter. A terminal crash can also
cause archival; the saved session is still available.

Shutdown, Sway logout, and uncertain observations preserve restore eligibility.
Automatic close detection needs working logind shutdown protection. Without it,
archive sessions explicitly with `a`.

For command-line management:

```sh
sway-session terminal list
sway-session terminal rename --label "Release work" CONTEXT_UUID
sway-session archive CONTEXT_UUID
sway-session activate CONTEXT_UUID
sway-session terminal cleanup --archived-before 2026-09-01
```

Cleanup previews archived sessions. To delete one, review
`sway-session purge CONTEXT_UUID`, then repeat with `--yes` and the exact UUID.
Purging stops and deletes the Herdr session, including its history and running
work. Archiving keeps it.

## Restore

```sh
sway-session restore --preview
sway-session restore
sway-session restore CONTEXT_UUID
sway-session restore-report
sway-session restore-report --retry CONTEXT_UUID
```

The preview shows which terminals and applications are eligible for the next
login without launching anything. `restore` opens eligible saved contexts;
giving a context selects it explicitly. The daemon restores workspace placement
and layout, so it must be running.

`restore-report` shows recorded attempts and explains failures or incomplete
placement. After resolving the cause, retry the affected context. Archived
contexts must be activated before using `--retry`. If an agent-pane setup fails
after a terminal is registered, retry that same context or named request rather
than creating another session.

## Desktop applications

Focus the application window you want to save, then run:

```sh
sway-session app register-focused
sway-session app list
sway-session app status
```

Review the launcher approval dialog. If the dialog is unavailable, use
`app register-focused --yes` from a trusted terminal after checking the launcher.
Registration requires an unambiguous application identity.

Applications start in **follow** mode: they return at login if left open, and
stop returning after their last window closes during normal use. **Pinned**
applications keep returning even after you close them:

```sh
sway-session app pin CONTEXT
sway-session app unpin CONTEXT
sway-session app archive CONTEXT
sway-session app activate CONTEXT
```

Use `app rebind-focused CONTEXT` to associate a replacement window, or
`app reapprove CONTEXT` after an approved launcher or executable changes.
`app forget --yes CONTEXT_UUID` removes the registration.

sway-session saves the application's outer window placement. For applications
with several indistinguishable windows, it avoids choosing a window to move.
The application decides which tabs, files, profiles, or URLs reopen.

To keep a registered application in the scratchpad, register it on a normal
workspace first, then move it to the scratchpad as usual. Its saved anchor
returns hidden or shown on its saved workspace. Scratchpad cycling order and
additional application windows may differ after restore.

## Configuration and command help

The default terminal is Alacritty. To use Foot, create
`~/.config/sway-session/config.toml` (or under `$XDG_CONFIG_HOME`):

```toml
version = 2

[terminal]
adapter = "foot"
session_manager = "herdr"
```

The supported adapters are `alacritty` and `foot`; the session manager is `herdr`.
Bash, Zsh, and Fish completion files are included with the installation.

Use `sway-session --help` for the command list and `sway-session help COMMAND`
for options, for example `sway-session help app`. Global options precede the
command: `sway-session --json terminal list` or
`sway-session --config /path/to/config.toml terminal --new`.
`sway-session --version` identifies the installed build.

## State backup and recovery

State is stored under `~/.local/state/sway-session`, or
`$XDG_STATE_HOME/sway-session`. Backups contain sway-session registration and
layout metadata. Back up Herdr history, application data, approved desktop
launchers, and configuration separately.

```sh
install -d -m 0700 "$HOME/sway-session-backups"
sway-session state backup --output "$HOME/sway-session-backups/before-change.sqlite3"
sway-session state recover --from "$HOME/sway-session-backups/before-change.sqlite3"
```

Use a new backup filename. Recovery previews by default; inspect it before
applying. Use clean absolute paths, with an owner-only `0600` backup file in a
`0700` directory owned by your user. Use `state backup` instead of copying a live
database file.

Before applying, stop the daemon and all other sway-session processes, including
standalone brokers. Recovery requires a valid `XDG_RUNTIME_DIR` and refuses
busy state. Then run:

```sh
sway-session state recover --from "$HOME/sway-session-backups/before-change.sqlite3" --yes
```

Recovery reports where it retained previous state. If interrupted, preserve the
source and repeat the same command to resume. An error can follow a partial
replacement: inspect the reported result and artifact paths before restarting
the daemon. See [recovery details](docs/sway-session-plan.md#durable-state).

## Troubleshooting

Run `sway-session doctor` for setup checks and repair previews, or
`sway-session doctor --check` for a noninteractive report. Doctor does not start
the daemon or reload Sway for you.

If windows do not return, check `restore --preview` for archive policy and
`restore-report` for launch and placement results. If Doctor reports a daemon
build mismatch after an upgrade, follow its stop/start instructions.

For an interrupted registration, rebind, or purge, run
`sway-session state operations` and follow the
[operation recovery guide](docs/lifecycle-recovery.md#operator-recovery).

## Further documentation

- [Agent hooks and session reporting](docs/agent-reporting.md)
- [Optional AppArmor hardening](contrib/apparmor/agent-home-guard) and
  [security limitations](docs/agent-reporting.md#security-limitations)
- [Architecture](docs/sway-session-plan.md)
- [Verification and live testing](docs/sway-session-verification.md)
- [Contributor workflow](docs/workflow_conventions.md) and
  [releasing](docs/releasing.md)

For development, run `make fmt` and `make verify`. Live compositor tests use
disposable state and workspace 98 or higher; see the verification guide.
