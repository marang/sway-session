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
aurora_sound   ▁▁▂▃▇╿▅▂▁▆┃▃▁
loom           ░▒≈⌁≋░▒▓✦▓▒░≋⌁≈▒░
bloom          ──⌁──❧─⌁──✦
spectrum       ·─━▆⟨▇█▇▆━┃━▆▇█▇⟩▆━─·
square         ⎺⎺⎤⎽⎽⎽⎽⎽⎡⎺⎺⎺⎺⎤⎽⎽⎡
ripples        ·  ╴─═●═─╶
radar          ╶◜╴──┄──═●═──┄──╋──┄──
constellation       ·   ✦      •    ✧
circuit        ─╍──╪──═●═──╍──╼╾───
glitch         ───╍╪▒▓╳──┄──
braid          ╱╱╱╳╲╲╲╳╱╱╱╳╲╲╲
ribbon         ·░▒▓██▓▒░··░▒▓█▓▒░·
shutter        ███▶░│     │░◀███
comet          ░░▒▒▓▓✶☄▓▒░░··░▒✦
smileys        ｡･ʕ•ᴥ•ʔっ･ﾟ
wave           ▁▃▅▇█◜╲▅▃▁
spline         ⢀⣠⠤⠒⠉⠢⣀  ✦
```

Each launch gets a fresh motion seed. Presets keep their visual identity, but
their timing, density, drift, and occasional events evolve organically instead
of repeating a short fixed performance.

`aurora_sound` keeps Aurora's bar language but maps the bars to
frequency bands from the current default audio output. It reads
`@DEFAULT_MONITOR@` through `parec`, reacts at the full configured FPS, and
falls back to a straight `▁` bottom line when no sound or monitor is available.
Strong peaks use `╿`; extreme peaks use `┃`. It is opt-in and is not part of
the default rotation.

Sound-reactive presets require the `parec` command. Install the PulseAudio
command-line utilities for your distribution (`libpulse` on Arch Linux,
`pulseaudio-utils` on Debian/Ubuntu). PipeWire users can use the same command
through `pipewire-pulse`. Verify the dependency with:

```sh
command -v parec
```

The design and rollout plan for sound companions of every remaining animation
is documented in [docs/sound-presets-plan.md](docs/sound-presets-plan.md).

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
between animations. If the terminal is not tall enough, scroll manually with
the arrow keys, `Page Up`/`Page Down`, or `Home`/`End`; it never auto-scrolls.
Press `q` or `Ctrl-C` to exit and restore the previous terminal contents.

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
```

Make sure `~/.local/bin` is in your `PATH`.

## Sway Setup

Add this to your Sway config:

```conf
exec_always --no-startup-id sway-title-animator --replace --fps 25
```

Then reload Sway:

```sh
swaymsg reload
```

## Choose a Preset

List presets:

```sh
sway-title-animator --list-presets
```

Run a single preset:

```sh
sway-title-animator --replace --preset aurora --fps 25
sway-title-animator --replace --preset aurora_sound --fps 25
sway-title-animator --replace --preset radar --fps 25
sway-title-animator --replace --preset comet --fps 25
sway-title-animator --replace --preset wave --fps 25
sway-title-animator --replace --preset spline --fps 25
sway-title-animator --replace --preset smileys --fps 25
sway-title-animator --replace --preset square --fps 25
sway-title-animator --replace --preset ripples --fps 25
sway-title-animator --replace --preset bloom --fps 25
sway-title-animator --replace --preset glitch --fps 25
sway-title-animator --replace --preset ribbon --fps 25
sway-title-animator --replace --preset shutter --fps 25
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
motion = 1.0

[rotation]
presets = [
  "loom", "aurora", "bloom", "spectrum", "square", "ripples",
  "radar", "constellation", "circuit", "glitch", "braid", "comet",
  "smileys", "wave", "spline",
]

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

The old `[showcase]` section and `showcase_*` timing options are intentionally
not aliases. Rename them to `[rotation]`, `rotation_hold_frames`, and
`rotation_blend_frames` before starting the animator.

`audio.device` can select a specific playback-monitor source. `sensitivity`
adjusts captured signal gain and `motion` adjusts the global visual response;
both accept values greater than `0` through `10`. There is no backend setting:
`parec` is currently the single production capture backend. Capture uses
48 kHz stereo PCM; the shared analyzer retains left/right balance while
providing 32 frequency bands and aggregate bass-to-treble features.

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
