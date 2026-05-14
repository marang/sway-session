# sway-title-animator

Animated Unicode titlebars for Sway.

It adds app labels, small status badges, and a generated animation to the
focused window title. It works with normal titlebars and looks especially good
in Sway's `tabbed` and `stacked` layouts.

This is a Linux/Wayland tool. It talks directly to Sway's IPC socket, so it is
not useful on macOS or Windows.

```text
▣ Alacritty wl: shell ▁▂▃▄▅▆▇█▇▆▅▄▃▂▁
🌐 firefox wl: GitHub ╶◜╴──┄──═●═──┄──
♪ riotbox wl: session ╱╱╱╳╲╲╲╳╱╱╱╳╲╲
```

## Demo

Built-in animation presets:

```text
aurora         ▁▂▃▄▅▆▇█▇▆▅▄▃▂▁
spectrum       ·─━▆⟨▇█▇▆━┃━▆▇█▇⟩▆━─·
radar          ╶◜╴──┄──═●═──┄──╋──┄──
constellation       ·   ✦      •    ✧
circuit        ─╍──╪──═●═──╍──╼╾───
braid          ╱╱╱╳╲╲╲╳╱╱╱╳╲╲╲
comet          ░░▒▒▓▓✶☄▓▒░░··░▒✦
showcase       rotates through all configured presets
```

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
title_align left
titlebar_border_thickness 1
titlebar_padding 8 3
show_marks yes
exec_always --no-startup-id sway-title-animator --replace --preset showcase --fps 25
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
sway-title-animator --replace --preset radar --fps 25
sway-title-animator --replace --preset comet --fps 25
```

Use `showcase` to rotate through all configured presets:

```sh
sway-title-animator --replace --preset showcase --fps 25
```

## Configuration

By default, the tool reads:

```text
~/.config/sway-title-animator/config.toml
```

Start from the example config:

```sh
mkdir -p ~/.config/sway-title-animator
cp config.example.toml ~/.config/sway-title-animator/config.toml
```

The config can change timing, glyphs, app icons, showcase order, and simple
frame-based animations.

```toml
[settings]
fps = 25
motion = 0.22

[showcase]
presets = ["aurora", "spectrum", "radar", "comet"]

[icons]
alacritty = "▣"
firefox = "🌐"
riotbox = "♪"

[animation.lift]
fill = true
frames = [
  "▁▁▂▃▄▅▆▇█▇▆▅▄▃▂▁",
  "▁▂▃▄▅▆▇█▇▆▅▄▃▂▁▁",
  "▂▃▄▅▆▇█▇▆▅▄▃▂▁▁▁",
]
```

Run with a specific config:

```sh
sway-title-animator --config ~/.config/sway-title-animator/config.toml
```

## Notes

Sway titlebars are text-only. This tool cannot draw bitmap icons or create
separate left/right layout regions inside a titlebar. It uses Unicode glyphs and
Sway's `title_format`, so the result depends on your font.
