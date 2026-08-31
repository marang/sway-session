# sway-title-animator

<p align="center">
  <img src="docs/assets/logo.svg" alt="sway-title-animator logo" width="900">
</p>

Animated Unicode titlebars for Sway.

It adds app labels, small status badges, and a generated animation to the
focused window title. It works with normal titlebars and looks especially good
in Sway's `tabbed` and `stacked` layouts.

This is a Linux/Wayland tool. It talks directly to Sway's IPC socket, so it is
not useful on macOS or Windows.

## Demo

Built-in animation presets:

```text
aurora         ▁▂▃▄▅▆▇█▇▆▅▄▃▂▁
aurora_sound   ▇▆▅▄▃▂▁▁▂▃▅▇█
loom           ░▒≈⌁≋░▒▓✦▓▒░≋⌁≈▒░
loom_sound     ▒⌁≋≈▒░▒⌁≋≈▒░
bloom          ──⌁──❧─⌁──✦
bloom_sound    ──⌁──✦──⌁──
spectrum       ·─━▆⟨▇█▇▆━┃━▆▇█▇⟩▆━─·
spectrum_sound ⟨·─━▆▇┃▇▆━─·┃·─━▆▇┃▇▆━─·⟩
square         ⎺⎺⎤⎽⎽⎽⎽⎽⎡⎺⎺⎺⎺⎤⎽⎽⎡
square_sound   ⎺⎺⎺⎤⎽⎽⎽⎽⎽⎡⎺⎺⎺⎤⎽⎽⎽⎡
ripples        ·  ╴─═●═─╶
ripples_sound      ╴─═◉═─╶
radar          ╶◜╴──┄──═●═──┄──╋──┄──
radar_sound    ╋──┄──═◜═──┄◆──╋──┄
constellation       ·   ✦      •    ✧
constellation_sound · ●•  ✦   ●   • ·
circuit        ─╍──╪──═●═──╍──╼╾───
circuit_sound  ───╾╪╼───═●═────
glitch         ───╍╪▒▓╳──┄──
glitch_sound   ─────┄──░▒▓╳────
braid          ╱╱╱╳╲╲╲╳╱╱╱╳╲╲╲
braid_sound    ╱╱╳╲╲╲╱✦╱╳╲╲
ribbon         ·░▒▓◐▓▒░··◑▒▓█◒▒░·
ribbon_sound   ░▒▓█◐▒░·░◑▓✦▓◒░
domino         ━·━·━·╲·▮·▮·▮·▮
domino_sound   ━·━✦╱·━·▣·━·╲✦▮·▮
comet          ░░▒▒▓▓✶☄▓▒░░··░▒✦
comet_sound    ·●•░▒▓☄  •   ●∙
smileys        ｡･ʕ•ᴥ•ʔっ･ﾟ
smileys_sound  · • ● ʕ◉ω◉ʔ
wave           ▁▃▅▇█◜╲▅▃▁
wave_sound     ▁▂▃╲◜╱▇≋•▃▂▁
spline         ⢀⣠⠤⠒⠉⠢⣀  ✦
spline_sound   ⠒⠒⠤⠤⣀⣀⠤◇⠒
```

Each launch gets a fresh motion seed. Presets keep their visual identity, but
their timing, density, drift, and occasional events evolve organically instead
of repeating a short fixed performance.

`aurora_sound` lays its frequency field out from bass on the left to highs on
the right and expands energy across Aurora's full eight-step bar-height range.
Beats lift a localized cluster; even extreme peaks remain normal tallest
Aurora bars (`█`). It reads
`@DEFAULT_MONITOR@` through `parec`, reacts at the full configured FPS, and
renders the normal Aurora while capture is available but silent. If capture is
unavailable, every sound companion also renders its normal base preset
after the single actionable warning. Sound companions are opt-in and are not
part of the default rotation.

`spectrum_sound` keeps Spectrum's mirrored instrument display, arranging bass
toward the outside and treble near the center. `wave_sound` retains Wave's
complete swell, backwash, crest, and curl choreography while bass and mids lift
the existing wave, treble adds bounded spray, and an onset briefly raises and
broadens a local part of the existing swell. During silence they render their
complete base animations. Like all
sound companions, both are opt-in and stay out of the default rotation.

`square_sound` preserves Square's connected scan line while bass changes its
plateau lengths and level changes duty cycle. Each beat appends exactly the next
connected high or low plateau—starting with one new character—while the
direction stays stable until the whole waveform is complete. `ripples_sound`
keeps the distributed organic base rings and adds broad bass rings or narrow
high-frequency rings from bounded onset history. Active onset rings use
`◎`/`◉` target cores to remain distinct from other sparse organic presets.
During silence they render their complete base animations.

`domino` stages a complete chain reaction: upright stones tip, fall, rest, and
stand back up with organically varying spacing, direction, and speed.
`domino_sound` keeps that choreography moving continuously. Beats start local
outward cascades, bass controls their reach, mids control propagation, and
treble adds restrained collision sparks. Silence keeps the calm base chain.

Sound-reactive presets require the `parec` command. Install the PulseAudio
command-line utilities for your distribution (`libpulse` on Arch Linux,
`pulseaudio-utils` on Debian/Ubuntu). PipeWire users can use the same command
through `pipewire-pulse`. Verify the dependency with:

```sh
command -v parec
```

The completed design and rollout record for all sound companions is documented
in [docs/sound-presets-plan.md](docs/sound-presets-plan.md).

The `square` preset uses Unicode terminal-graphics scan lines rather than
Braille pixels. Its trace holds still while it is drawn from left to right or
right to left, and every plateau gets its own length. Occasionally, a short
pulse travels right across the completed trace and temporarily overwrites it.
Matched scan-line and bracket glyphs keep every edge connected.

## Terminal Preview

Preview every registered animation at the same time without connecting to
Sway:

```sh
sway-title-animator --preview
```

The Bubble Tea preview uses one labeled line per preset with a blank spacer
between animations. Every base preset is immediately followed by its
`<base>_sound` companion, so both forms can be compared while scrolling. If the
terminal is not tall enough, scroll manually with the arrow keys,
`Page Up`/`Page Down`, or `Home`/`End`; it never auto-scrolls. Press `q` or
`Ctrl-C` to exit and restore the previous terminal contents.

## Install

Build from source:

```sh
git clone https://github.com/marang/sway-title-animator.git
cd sway-title-animator
make install
```

This installs:

```text
~/.local/bin/sway-title-animator
~/.local/bin/sway-session
~/.local/bin/sway-herdr-init
```

Make sure `~/.local/bin` is in your `PATH`.

The native `swaynag` approval path deliberately invokes the root-owned
`/usr/bin/sway-session`, so it is available from a distribution-package
install. A source-only install must make the same explicit decision from a
trusted terminal with `sway-session app ... --yes`; it must not silently route
an approval token into a user-writable executable.

Release archives and distribution packages also contain all three programs and the
Sway, Herdr, Codex-hook, and AppArmor integration templates. Archives retain
the repository-relative template paths; distribution packages install them
under `/usr/share/doc/sway-title-animator`. The differing paths are called out
below.

## Sway Setup

For a distribution-package install, add the complete startup stanza below or
include the packaged `45-title-animator.conf` template:

```conf
exec_always --no-startup-id /usr/bin/sway-title-animator --replace --fps 25
exec --no-startup-id /usr/bin/sway-session daemon
exec --no-startup-id /usr/bin/sway-session restore
```

Source-install users can replace `/usr/bin` with `$HOME/.local/bin`. Only the
animator uses `exec_always`; the daemon and one-shot restore deliberately use
`exec`, so a config reload cannot launch or restore the session again. Restart
the Sway session once after first adding this stanza; a plain config reload
starts only the animator and intentionally does not perform the initial daemon
start or one-shot restore. Later animator/config updates can be applied with:

```sh
swaymsg reload
```

## Persistent Work Sessions

The optional `sway-session` CLI gives one explicitly registered work context
one Alacritty window backed by one named [Herdr](https://herdr.dev/) session.
Sway restores the outer workspace and layout; Herdr restores the terminal tabs,
panes, supported agent sessions, and pane screen history.

On first access, valid version-1 context registries are atomically upgraded to
version 2. The exact old bytes remain owner-only in `contexts.v1.json` beside
the active registry as manual rollback evidence. Malformed or unknown-version
state is never migrated, and the rollback file is never loaded automatically.
Version 2 also supports explicit normal desktop-application registrations.
The independent session daemon groups every matching top-level window into one
application presence, adopts one unambiguous window as the optional layout
anchor, and restores missing desired-open applications after a five-second
autostart adoption grace period. It launches at most two applications while
their first window is still pending and records launch intent before starting
the process, so a daemon restart or ambiguous launcher outcome cannot duplicate
the attempt in the same real Sway compositor session.

Install Alacritty and Herdr, then enable Herdr pane history in
`${XDG_CONFIG_HOME:-$HOME/.config}/herdr/config.toml`:

```toml
[experimental]
pane_history = true
```

Keep the directory and config private because pane history can contain command
output, paths, and tokens:

```sh
chmod 700 "${XDG_CONFIG_HOME:-$HOME/.config}/herdr"
chmod 600 "${XDG_CONFIG_HOME:-$HOME/.config}/herdr/config.toml"
```

Register the current project and start or attach its named Herdr session:

```sh
sway-session register --session lab-80 --label LAB-80 --provider linear
sway-session restore LAB-80
```

To register an ordinary application, focus one of its top-level windows and
run:

```sh
sway-session app register-focused
```

The command resolves exact Wayland, XWayland, or Flatpak identity evidence and
opens a native `swaynag` approval for package installs. Source-only installs use
the documented explicit `--yes` path. An ambiguous match presents explicit
desktop entry choices and is never guessed. A repeat on an already registered
window reports its status and repairs a missing stable mark; it is deliberately
not a toggle. To preview one confirmation for all unregistered eligible apps on
the focused workspace, use `sway-session app register-workspace`. `--yes` is
available for deliberate noninteractive use.

After the first successful desktop registration, eligible normal top-levels
show one application-persistence indicator immediately before the app icon:

```text
○  unregistered and eligible
◔  awaiting an explicit approval
●  registered in follow mode
▲  pinned as a sway-session autostart
```

Follow mode remembers whether the app remains desired-open; deliberately
closing its last window disables the next startup restore after the two-second
grace. Pinned mode keeps desired-open set across Sway starts. It is a bounded
autostart, not an unbounded crash supervisor. Indicator mode remains inactive
until the first registration succeeds. The daemon owns state derivation and
hidden Sway marks; the animator only renders observed marks and remains usable
when the daemon is absent.

For an optional Sway keybinding, choose an otherwise unused chord, for example:

```conf
bindsym $mod+Ctrl+p exec --no-startup-id /usr/bin/sway-session app register-focused
```

System desktop entries are revalidated as root-owned launch material. Flatpak
registration records only the validated app ID and installation. A user-local
desktop entry is copied to an owner-only approved snapshot; changes to the
source entry or its user-owned executable block later launch until explicit
`app reapprove`. The confirmation preview shows the launcher origin and first
executable token, never file/URI arguments.

Desktop-entry restore uses the system `gio` command (GLib). Flatpak restore
additionally needs `flatpak`; neither dependency is required for title
animation or Herdr-only sessions.

Application lifecycle and repair commands accept an exact UUID or unambiguous
label:

```sh
sway-session app status
sway-session app list
sway-session app rebind-focused <context>
sway-session app reapprove <context>
sway-session app pin <context>
sway-session app unpin <context>
sway-session app archive <context>
sway-session app activate <context>
sway-session app forget --yes <context>
```

Machine consumers can list only desktop-application contexts with:

```sh
sway-session --json app list
```

The result is an object with `command: "app list"` and a `contexts` array
sorted by context UUID. Context records may contain local launcher paths and
approval checksums, so treat the output as private machine state rather than
publishing it in logs or bug reports.

`pin` keeps desired-open state independent of whether the app was open at the
last clean shutdown; `unpin` returns to follow mode. Rebind previews the old and
new exact identities. Forget removes only the outer registration and live Sway
mark; it never attempts to delete application-private state.

In follow mode, the app becomes desired-open when any matching eligible
top-level appears and desired-closed only after its last window has remained
absent for two seconds. Profile pickers and authentication-to-main-window
transitions therefore do not create a close/relaunch cycle. Multiple
indistinguishable windows prove that the application is already present but are
never guessed between for layout. Only a unique or already marked anchor is
moved to the saved workspace; later application-owned windows are left alone.
Scratchpad windows count as presence but scratchpad placement is intentionally
deferred to LAB-92. Use `sway-session restore <context>` to atomically queue a
desired-closed active desktop app for the daemon without bypassing its launch
journal.

Sway exposes XWayland transient/type metadata, so classifiable XWayland dialogs
are excluded. Sway 1.12 does not expose equivalent parent/type evidence for
native Wayland surfaces; a matching Wayland `app_id` therefore belongs to the
application-level presence group. Stable per-window disambiguation remains the
LAB-93 `xdg-toplevel-tag` follow-up.

For an AppArmor-confined Codex workflow, the long-running `sway-session daemon`
also exposes the existing separate typed start endpoint. It combines exact
context registration or reuse with placement on one empty numbered workspace,
without accepting pane roles or commands:

> **Experimental security boundary:** the narrow interfaces protect registry
> files and constrain the intended request shapes, but the current AppArmor
> deny-list does not reliably mediate `connect(2)` to pathname-based Sway,
> Herdr, or container API sockets on every supported kernel. The launch path
> also creates an unconfined terminal, Herdr pane shell, and agent. Direct
> socket use, user-writable shell startup files, or executable lookup can
> therefore escape the intended boundary. Enable this workflow only when that
> risk is explicitly accepted. Future Agent Sandbox hardening is tracked in
> [LAB-89](https://linear.app/riotbox/issue/LAB-89/harden-broker-created-herdr-sessions-with-agent-sandbox-integration).

```sh
/usr/bin/sway-session --json request-start \
  --session lab-88 \
  --cwd "$PWD" \
  --label LAB-88 \
  --workspace 7
```

Pass the returned `.contexts[0].id` to the packaged initializer:

```sh
/usr/bin/sway-herdr-init --json \
  --context 8f33d6d0-7c54-4da1-9e38-2bd290ef85ca \
  --role codex \
  --role shell
```

The initializer derives the named Herdr session and working directory from the
protected registry and holds its mutation lock through the dependent Herdr
operation, so `archive` and `purge` cannot race initialization. It only splits
a session proven to contain exactly one empty pane with the supported snapshot
protocol. The current integration targets Herdr 0.8.2 protocol 20 and rejects
other protocol versions before mutation. It invokes Herdr's normal typed
`agent start` operation and otherwise
returns a safe no-op. Existing Herdr layouts are never reshaped. The AppArmor
transition intentionally recognizes only the root-owned
`/usr/bin/sway-herdr-init` from a distribution package. The broker likewise
launches only a root-owned system `sway-session` with a system-only executable
search path. The outer Herdr launcher removes inherited `HERDR_*` pane metadata
and `CODEX_THREAD_ID` before starting a distinct context, then injects only its
new registered context ID. Do not allow-list a user-writable source-install
copy.

Pane roles are logical Herdr agent kinds such as `codex`, not executable paths
or runtime definitions. A future trusted wrapper or container launcher belongs
to the Herdr/agent integration layer; `sway-session` deliberately does not
persist direct-versus-sandbox execution details.

Lifecycle commands accept an exact UUID or an unambiguous exact label:

```sh
sway-session list
sway-session archive LAB-80
sway-session activate LAB-80
sway-session purge --yes LAB-80
```

Archive excludes a context from automatic restore while retaining its Herdr
state. Purge stops and deletes the exact named Herdr session before removing
the registry entry; without `--yes`, it requires a terminal and the full UUID.
Use the global option before a command, for example `sway-session --json list`,
for stable machine-readable results and diagnostics. The complete automatic
startup stanza is documented in [Sway Setup](#sway-setup) and shipped as
`contrib/sway/45-title-animator.conf`.

The daemon owns session observation, marking, placement, layout snapshots,
layout restore, and both narrow broker endpoints. The animator remains an
independent title-animation/audio process and never opens the registry, layout
state, Herdr state, or session sockets. The daemon and restore lines
intentionally use `exec`, not `exec_always`, so reloading the Sway config does
not launch duplicates; the daemon also holds an exclusive runtime lock. The
one-shot restore checks Sway and already-started typed Alacritty processes
before launching Herdr contexts. Desktop applications are restored only by the
daemon, which distinguishes a Sway config reload from a replaced compositor
socket and never treats reload as a new launch session.

### Secure Codex resume

Codex resume metadata uses the session daemon's owner-only runtime broker. The
Codex hook never opens Herdr state, the Herdr control socket, the outer-session
registry, or the Sway IPC socket. It sends only the registered context UUID,
Herdr's current pane identity, and a canonical Codex session UUID; the broker
maps the context to its fixed named Herdr session and emits exactly
one mutating method, `pane.report_agent_session`. Before the mutation, the
broker uses the fixed read-only `pane.process_info` method and `SO_PEERCRED` to
prove that the reporter descends from the selected pane's shell. Transcript
paths and original command lines are ignored. Herdr later constructs its typed
`codex resume <uuid>` operation from that association.

Enable Codex hooks in `~/.codex/config.toml`:

```toml
[features]
hooks = true
```

After a source `make install`, merge the `SessionStart` entry from
`contrib/codex/hooks.json` into `~/.codex/hooks.json`; it uses
`~/.local/bin/sway-session`. Distribution-package users should instead merge
`/usr/share/doc/sway-title-animator/contrib/codex/hooks.json`, which uses
`/usr/bin/sway-session`. When installing from a release archive into another
location, change the hook command to that exact absolute binary path. Do not
retain Herdr's stock Codex SessionStart hook: it connects directly to the
general Herdr socket and defeats this boundary. The managed Alacritty launch
injects `SWAY_SESSION_CONTEXT_ID`; Herdr supplies `HERDR_PANE_ID`, so the hook
is a silent no-op in other terminals.
On the next Codex start, review this exact command in the Hooks prompt and trust
it; untrusted hooks do not run.

Install and load the matching AppArmor profile:

```sh
sudo install -m 0644 contrib/apparmor/codex-home-guard /etc/apparmor.d/codex-home-guard
sudo apparmor_parser -r /etc/apparmor.d/codex-home-guard
```

For a distribution-package install, use
`/usr/share/doc/sway-title-animator/contrib/apparmor/codex-home-guard` as the
source path in the first command.

The template assumes the default XDG paths under `~/.config` and
`~/.local/state`. The narrow initializer deliberately derives those defaults
from the account database instead of trusting caller-provided XDG variables;
custom XDG roots are not supported by this narrow helper yet. The policy denies
direct Herdr history and `sway-session` state access and protects the relevant
socket pathnames from ordinary file operations. On kernels where AppArmor does
not mediate pathname socket connections through those file rules, it does not
enforce the intended direct Sway, Herdr, or container API connection deny; the
typed `codex-report.sock` and `session-start.sock` workflow remains the only
supported integration path. The root-owned
`sway-herdr-init` receives a separate constrained profile for the fixed Herdr
initialization described above. Its `Px` transition scrubs unsafe dynamic
loader variables such as `LD_PRELOAD` before granting the child profile's
permissions. The parent Codex profile intentionally leaves GitHub CLI
configuration accessible so `gh` can use credentials stored in the desktop
keyring; do not use file-backed GitHub tokens with this policy. The initializer
child profile does not need GitHub access and continues to deny it. From the
matching Herdr pane, run the packaged
`/usr/share/doc/sway-title-animator/scripts/verify-codex-boundary.sh` for a live
positive/negative enforcement check after the profile, Herdr, and one
registered context are active. A checkout copy can invoke the same script, but
a successful live pass still requires the root-owned distribution-package
binaries under `/usr/bin`. The check never resolves a user-writable
`sway-session` through `PATH`. It fails closed when
the kernel exposes a known pathname-connect or runtime-path mutation gap
instead of reporting a complete boundary.

## Choose a Preset

List presets:

```sh
sway-title-animator --list-presets
```

Run a single preset:

```sh
sway-title-animator --replace --preset aurora --fps 25
sway-title-animator --replace --preset aurora_sound --fps 25
sway-title-animator --replace --preset spectrum_sound --fps 25
sway-title-animator --replace --preset radar --fps 25
sway-title-animator --replace --preset comet --fps 25
sway-title-animator --replace --preset wave --fps 25
sway-title-animator --replace --preset wave_sound --fps 25
sway-title-animator --replace --preset spline --fps 25
sway-title-animator --replace --preset smileys --fps 25
sway-title-animator --replace --preset square --fps 25
sway-title-animator --replace --preset square_sound --fps 25
sway-title-animator --replace --preset ripples --fps 25
sway-title-animator --replace --preset ripples_sound --fps 25
sway-title-animator --replace --preset bloom --fps 25
sway-title-animator --replace --preset glitch --fps 25
sway-title-animator --replace --preset ribbon --fps 25
sway-title-animator --replace --preset domino --fps 25
sway-title-animator --replace --preset domino_sound --fps 25
```

Omit `--preset` to rotate through the configured presets:

```sh
sway-title-animator --replace --fps 25
```

## Configuration

By default, the tool reads:

```text
~/.config/sway-title-animator/config.toml
```

Start from the example config:

```sh
sway-title-animator --init-config
```

This creates `~/.config/sway-title-animator/config.toml` if it does not exist.
It will not overwrite an existing config.

The config can change timing, glyphs, app icons, rotation order, and simple
frame-based animations.

```toml
[settings]
fps = 25
motion = 0.22
rotation_hold_frames = 260
rotation_blend_frames = 75
detect_child_process = true

[audio]
# device = "@DEFAULT_MONITOR@"
sensitivity = 1.0
motion = 0.75

[rotation]
presets = [
  "loom", "aurora", "bloom", "spectrum", "square", "ripples",
  "radar", "constellation", "circuit", "glitch", "braid", "comet",
  "smileys", "wave", "spline",
]

[indicators]
unregistered = "○"
pending = "◔"
registered = "●"
pinned = "▲"

[icons]
alacritty = "▣"
firefox = "🌐"
riotbox = "♪"

[animation.marquee]
fill = true
frames = [
  "··░░▒▒▓▓▒▒░░··  ",
  "·░░▒▒▓▓▒▒░░··  ·",
  "░░▒▒▓▓▒▒░░··  ··",
]
```

All four indicator values must be distinct printable single-rune glyphs with
the same terminal width. The defaults are covered by Noto Sans Mono; configure
Sway with a matching Pango font for predictable titlebar metrics, for example
`font pango:Noto Sans Mono 10`.

The old `[showcase]` section and `showcase_*` timing options are intentionally
not aliases. Rename them to `[rotation]`, `rotation_hold_frames`, and
`rotation_blend_frames` before starting the animator.

`audio.device` can select a specific playback-monitor source. `sensitivity`
adjusts captured signal gain and `motion` adjusts the global visual response;
both accept values greater than `0` through `10`. There is no backend setting:
`parec` is currently the single production capture backend. Capture uses
48 kHz stereo PCM; the shared analyzer retains left/right balance while
providing 32 frequency bands and aggregate bass-to-treble features. Gentle
automatic gain normalization adapts across sources, while a short reconnect
warm-up prevents startup spikes. The visual response combines slower
attack/release envelopes, broad neighboring frequency regions, and at most one
current transient per event class so real music produces deliberate motion
instead of frame-by-frame jitter. `wave_sound` uses a continuous horizontal
swell with local beat-driven lifts; Aurora keeps the vertical bar vocabulary.

Run with a specific config:

```sh
sway-title-animator --config ~/.config/sway-title-animator/config.toml
```

With `detect_child_process = true`, terminal windows can include the active
child process in the label, for example `Alacritty › nvim`.

## Notes

Sway titlebars are text-only. This tool cannot draw bitmap icons or create
separate left/right layout regions inside a titlebar. It uses Unicode glyphs and
Sway's `title_format`, so the result depends on your font.

## Development

Development requires Go 1.26. The module selects Go 1.26.5 as its preferred
security-patched toolchain.

Run the same verification gate used by CI:

```sh
make verify
```

Planning, Linear routing, branches, pull requests, review, and cleanup follow
[the repository workflow](docs/workflow_conventions.md).
