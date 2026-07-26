# Sound-reactive preset plan

Status: Accepted design; the unnamed default rotation, startup audio
configuration, backend-neutral capture seam, 48 kHz stereo continuous feature
analysis, normalization/reconnect warm-up, bounded onset events,
`aurora_sound`, `spectrum_sound`, `wave_sound`, `square_sound`,
`ripples_sound`, and the scrollable Bubble Tea preview foundation are
implemented. The remaining companions, diagnostics, and expanded preview
modes are planned.

Linear project:
[Sway Title Animator | P001 | Sound-Reactive Presets](https://linear.app/riotbox/project/sway-title-animator-or-p001-or-sound-reactive-presets-e8a4308a9902)

## Goal

Add an opt-in `<preset>_sound` companion for every visual preset. Each companion
must retain the base preset's visual language. Audio changes its motion and
events instead of replacing it with a generic equalizer.

The target balance during active audio is approximately 75% audio influence and
25% slow organic motion. Audio-reactive events replace competing random special
events while sound is active. Rare preset-specific idle events may remain
during silence.

Sound companions are never added to the default rotation automatically. Users
opt in by naming them in `[rotation].presets`.

## Runtime and configuration contract

When no `--preset` is supplied, the animator rotates through the configured
base presets. Rotation is behavior, not a visible preset. This migration was
implemented in
[LAB-29](https://linear.app/riotbox/issue/LAB-29/replace-showcase-preset-with-default-rotation-semantics):

- remove the `showcase` preset name;
- rename `[showcase]` to `[rotation]`;
- do not retain a `showcase` legacy alias; and
- reject an old `[showcase]` block with an actionable message that says to
  rename it to `[rotation]`.

The initial audio configuration remains deliberately small:

```toml
[audio]
# device = "@DEFAULT_MONITOR@"
sensitivity = 1.0
motion = 1.0

[rotation]
presets = ["aurora", "loom", "square"]
```

- `device` is an optional playback-monitor override.
- `sensitivity` controls audio gain before preset mapping.
- `motion` controls the audio-wide amount of reactive movement.
- Per-preset overrides remain future work until real usage demonstrates the
  need.
- Do not expose an `audio.backend` option while only one production backend
  exists.
- Configuration is validated once at startup. Changes take effect after
  restarting with `--replace`; live reload is out of scope.

The startup fields and backend-neutral capture seam were implemented in
[LAB-31](https://linear.app/riotbox/issue/LAB-31/add-audio-configuration-and-a-backend-neutral-capture-contract).
The 48 kHz stereo stream and continuous shared features were implemented in
[LAB-33](https://linear.app/riotbox/issue/LAB-33/upgrade-the-shared-analyzer-to-48-khz-stereo-features).
Normalization and reconnect warm-up were implemented in
[LAB-43](https://linear.app/riotbox/issue/LAB-43/add-audio-normalization-and-reconnect-warm-up).
Onset classes/history, spectral flux, and peak hold were implemented in
[LAB-44](https://linear.app/riotbox/issue/LAB-44/add-onset-detection-peak-hold-and-bounded-audio-events).

Only playback monitor/output sources are supported. Microphone capture is not
part of this feature. `--doctor` should reject a custom source that it can
reliably identify as a microphone. A future microphone mode, if ever wanted,
requires a separately named explicit opt-in and privacy design.

## Capture architecture

The first production backend uses `parec` through the PulseAudio compatibility
service:

- capture `@DEFAULT_MONITOR@` unless `audio.device` overrides it;
- request stereo, 48 kHz, signed 16-bit little-endian PCM;
- aggregate stereo to mono for shared energy features while retaining left,
  right, and balance features for suitable presets;
- start capture only while an active preset or preview requires audio;
- keep one capture and analyzer alive during rotation when any configured
  rotation entry is sound-reactive;
- stop promptly on cancellation and reconnect after backend or device loss;
- use one `parec` process and one FFT pipeline, regardless of the number of
  visible sound presets; and
- preserve the production `CGO_ENABLED=0` build.

The capture interface must remain backend-neutral. A future native PipeWire
backend is tracked separately in
[LAB-25](https://linear.app/riotbox/issue/LAB-25/investigate-native-pipewire-capture-without-cgo).
`parec` remains the production backend until another implementation satisfies
the same lifecycle, diagnostics, performance, and no-CGO contract.

Capture backpressure is strictly bounded: latest sample wins. If analysis or
rendering falls behind, discard old audio blocks rather than increasing
latency or memory usage.

## Shared audio model

The analyzer exposes stable musical features instead of making every renderer
interpret raw PCM or FFT bins:

- 32 ordered, non-overlapping logarithmic frequency bands;
- overall level;
- bass, low-mid, high-mid, and treble energy;
- spectral centroid for perceived bright/dark balance;
- spectral flux for sudden timbre changes;
- general onset, bass onset, and high-frequency onset with cooldown;
- peak hold with decay;
- left level, right level, and stereo balance; and
- a monotonically increasing visual revision only when visible audio state
  changes.

Do not add BPM or beat-grid tracking in the first version. Presets react to the
appropriate onset class instead.

Use gentle automatic gain normalization that preserves musical dynamics rather
than forcing every passage to the same loudness. Initial timing targets are:

- attack: 80–120 ms;
- release: 250–400 ms; and
- normalization: substantially slower than envelope release.

On startup or reconnect, collect roughly 300–500 ms of warm-up history while
showing the calm idle form, then blend into the audio response. A strong first
onset may pass through once the detector has enough data to classify it
reliably.

Audio-driven event motion uses elapsed real time and is independent of the
configured FPS. Slow organic base motion may remain phase-based.

The shared state keeps a bounded recent-onset history of about eight entries.
Each entry contains event identity, age, strength, frequency region, and stereo
position. Renderers consume this immutable snapshot and remain deterministic
and stateless.

The implemented detector measures positive changes in the normalized 32-band
spectrum and exposes general, bass, and high-frequency events with independent
cooldowns. Detection starts only after the complete reconnect warm-up and is
suppressed during silence. The immutable snapshot retains the newest eight
events for up to 1.5 seconds with monotonic process-lifetime IDs; reconnects
clear detector history without reusing IDs. Peak hold decays by elapsed time,
not frame count.

## Runtime states and diagnostics

Every sound preset has three distinct states:

1. **No capture:** render the normal base preset and emit one friendly,
   actionable warning per process lifetime.
2. **Capture with silence:** render a calm, recognizable preset-specific idle
   form without random twitching.
3. **Active audio:** attack quickly, decay smoothly, and retain slow organic
   motion without competing random special events.

Silence is not a capture failure and must never produce repeated warnings.

For previews with working capture but no signal:

- after about one second of reliable initial silence, show a quiet status hint
  such as `Play audio to preview sound-reactive animations`;
- after audio has been active, wait about three seconds of silence before
  showing the hint again;
- remove the hint automatically when audio becomes active; and
- keep the hint inside the preview UI rather than writing repeated log lines.

There is no synthetic demo-audio mode. A sound preview shows real captured
output or the truthful silence/unavailable state.

## Doctor

Add a read-only `--doctor` command that checks:

- `parec` availability;
- PulseAudio/PipeWire-Pulse reachability;
- default or configured playback-monitor source;
- a bounded sample capture;
- the Sway IPC socket;
- configuration syntax and obsolete sections;
- runtime directory and instance-lock viability; and
- terminal Unicode capability relevant to the registered presets.

Severity is contextual:

- missing audio support is informational when no sound preset is selected or
  configured; and
- it is a failed check with a non-zero exit code when a selected preset or
  rotation entry requires audio.

`--doctor --fix` may create or migrate project-owned configuration directories
and files. It must not install packages, invoke `sudo`, or modify system audio,
Sway, or terminal configuration.

## Preview and preset discovery

Use Bubble Tea and the Bubbles viewport as the terminal-preview foundation.
`--preview-all` runs in the alternate screen, reacts to terminal resize, and
restores the previous terminal contents on normal exit, `q`, `Ctrl-C`, or an
error.

Planned preview modes:

- `--preview`: base presets only;
- `--preview-sound`: sound companions only;
- `--preview-pair <preset>`: one base/sound comparison; and
- `--preview-all`: every base and sound preset in one manually scrollable
  viewport.

The initial viewport supports:

- arrow keys for line scrolling;
- `Page Up` and `Page Down`;
- `Home` and `End`;
- `q` and `Ctrl-C` to quit; and
- no automatic scrolling.

All animation models continue updating outside the visible viewport. Pair
previews use the same seed, width, and timebase for both variants so audio is
the only intended source of divergence.

`--list-presets` groups each sound companion beneath its base preset, marks it
with `[audio]`, and indicates which entries are in the active rotation.

Interactive selection, pause, pair switching, and preview-local live controls
are intentionally a follow-up:
[LAB-28](https://linear.app/riotbox/issue/LAB-28/expand-the-bubble-tea-preview-into-an-interactive-animation-browser).

## Similarity contract

Every newly registered base or sound preset participates in the universal
all-pairs visual-similarity guard.

A base/sound pair uses a documented kinship rule, not a blanket allowlist:

- during silence, the sound companion must remain recognizably in the base
  preset's visual family;
- under deterministic active test signals, the sound companion must show a
  meaningful measurable response; and
- against every unrelated preset, both variants must pass the normal distance
  threshold across representative widths, seeds, phases, and audio snapshots.

This protects both sides of the design: companions cannot become generic
equalizers, and intentional family resemblance cannot hide two unrelated
presets that look effectively identical.

## Preset-specific designs

### `aurora_sound` — implemented reference

- Frequency bands select the height of adjacent Aurora bars.
- Overall level scales the available height.
- Strong band peaks become `╿`; extreme peaks become `┃`.
- Silence becomes a continuous `▁` baseline.

### `spectrum_sound` — implemented

- Preserve the mirrored spectrum and its enclosing brackets.
- Map bass to the outer bars, mids toward the inner bars, and treble to the
  center so the form still reads as a symmetric instrument display.
- Peak hold creates brief `┃` accents; centroid gently shifts the bright center.
- Silence retains a dim, narrow symmetric pulse.

### `radar_sound` — implemented

- Bass controls sweep speed and the weight of the central target.
- Onsets create temporary echoes at positions derived from the strongest
  frequency band.
- Treble adds short fine-grained blips; mids widen detected targets.
- Stereo balance places new echoes left or right of center.
- Silence keeps one slow sweep rather than freezing the preset.

### `constellation_sound`

- Map frequency bands to fixed horizontal star regions.
- Energy controls star brightness (`·`, `•`, `✧`, `✦`) without randomly moving
  the entire sky.
- Strong onsets trigger one short supernova; spectral flux produces small
  shooting stars.
- Stereo balance selects the side on which transient stars appear.
- Silence leaves a sparse, slowly drifting constellation.

### `circuit_sound`

- Bass launches current pulses into the circuit.
- Frequency bands choose which branches light up; mids lengthen active routes.
- Treble creates short contact sparks at junction glyphs.
- Stereo balance selects the initial current direction.
- Silence shows the intact circuit with a single low-power diagnostic pulse.

### `braid_sound`

- Bass increases the braid's lateral amplitude.
- Midrange controls crossing density and strand separation.
- Strong onsets emphasize one crossing with `╳`; treble adds brief highlights
  that travel along a strand.
- Stereo balance determines which strand receives the highlight.
- Silence keeps a relaxed, slowly woven braid.

### `loom_sound`

- Low frequencies control the spacing of the heavy warp threads.
- Mids modulate the interlaced weft density.
- Treble adds small thread glints; onsets tighten one section like a shuttle
  impact.
- Stereo balance selects shuttle direction.
- Silence returns to a loose, readable textile pattern.

### `comet_sound`

- A bass onset launches a comet instead of using arbitrary launch timing.
- Overall level controls tail length and density.
- Spectral centroid controls velocity: darker audio moves more heavily, brighter
  audio more quickly.
- Stereo balance selects launch side and direction.
- Treble creates short tail sparkles without spawning additional full comets.
- Silence leaves occasional very slow ambient particles.

### `smileys_sound`

- Bass makes faces bounce by changing their local vertical/weight vocabulary,
  not their terminal row.
- Overall level controls travel speed and spacing.
- Midrange selects open/closed mouth expressions; treble adds ear or sparkle
  accents.
- Strong onsets trigger a brief synchronized reaction, with a cooldown to avoid
  visual spam.
- Stereo balance biases where new faces enter.
- Silence keeps one relaxed face drifting slowly.

### `wave_sound` — implemented

- Bass controls swell height and wavelength.
- Low mids shape the body of the wave; high mids generate foam.
- Treble produces sparse spray accents.
- Strong onsets create a breaker that travels across the existing wave.
- Stereo balance selects breaker direction.
- Silence retains a small continuous tide.

### `spline_sound`

- Divide the spectrum into the spline's control-point regions.
- Band energy displaces individual control points, producing a continuous
  audio-shaped curve rather than independent bars.
- Centroid moves the tracer; strong onsets briefly increase its brightness.
- Stereo balance biases the tracer direction.
- Silence settles toward a shallow, slowly breathing curve.

### `square_sound` — implemented

- Bass controls plateau length and therefore apparent frequency.
- Overall level changes the high/low duty cycle while preserving connected scan
  lines.
- Onsets launch the existing overwrite runner; onset strength controls its
  length and speed.
- Stereo balance selects the initial build or runner direction.
- Silence draws a low-frequency trace with rare, slow runners.

### `ripples_sound` — implemented

- Each detected onset creates a new ripple at a position selected by frequency
  region and stereo balance.
- Onset strength controls initial radius and glyph weight.
- Bass makes broad slow ripples; treble makes narrow fast ones.
- Overall level controls how many simultaneous ripples may survive.
- Silence allows existing ripples to decay completely before a subtle idle
  pulse.

### `bloom_sound`

- Strong onsets open a bloom from bud to flower.
- Bass controls the stem/anchor weight; mids control petal span.
- Treble creates short pollen sparkles after the bloom opens.
- Sustained audio keeps petals open without repeatedly retriggering them.
- Stereo balance gently bends the bloom rather than moving it wholesale.
- Silence closes the bloom gradually to a small bud.

### `glitch_sound`

- Spectral flux controls glitch density; stable sustained tones remain mostly
  stable.
- Bass transients create short horizontal displacement blocks.
- Treble controls fine noise and broken-line accents.
- A very strong onset may produce one bounded full-width tear with a cooldown.
- Stereo balance biases displacement direction.
- Silence becomes an almost clean line, preserving contrast with `ribbon`.

### `ribbon_sound`

- Frequency bands modulate brightness along the existing woven ribbon.
- Bass changes ribbon width, mids change curvature, and treble adds traveling
  highlights.
- Centroid controls drift speed; stereo balance controls drift direction.
- Strong onsets create one twist that propagates through the ribbon rather than
  a screen-wide glitch.
- Silence retains a dim, slowly floating ribbon.

### `shutter_sound` — implemented

- Bass onsets close the aperture briefly and then release it.
- Sustained low-mid energy controls how far the doors remain closed.
- Treble adds thin edge highlights; it must not introduce glitch fragments.
- Peak strength controls the weight of the inward arrows and center seam.
- Stereo balance permits slightly asymmetric closing without breaking the
  mechanical form.
- Silence leaves the aperture mostly open with a slow mechanical breath.

## Delivery sequence

1. Establish the backend-neutral capture contract, startup configuration,
   bounded event model, and `--doctor` checks. The `[rotation]` migration and
   unnamed default behavior are implemented in LAB-29; startup audio
   configuration and the capture seam are implemented in LAB-31.
2. Implement the fixed-format `parec` lifecycle, smoothing/normalization,
   spectral features, and deterministic audio test harness. The 48 kHz stereo
   format, continuous features, time-based smoothing, and stereo fixtures are
   implemented in LAB-33; normalization and reconnect warm-up are implemented
   in LAB-43; event features are implemented in LAB-44.
3. Deliver sound companions in reviewable PRs of no more than two related
   presets:
   - `spectrum_sound` and `wave_sound` — implemented in LAB-46;
   - `square_sound` and `ripples_sound` — implemented in LAB-36;
   - `radar_sound` and `shutter_sound` — implemented in LAB-37;
   - `comet_sound` and `bloom_sound`;
   - `constellation_sound` and `circuit_sound`;
   - `braid_sound` and `loom_sound`;
   - `spline_sound` and `ribbon_sound`; and
   - `smileys_sound` and `glitch_sound`.
4. After all sound companions are implemented, expand the Bubble Tea preview
   with the four planned preview modes, manual viewport, status hint, and
   grouped preset listing. Interactive browser work remains tracked in LAB-28.
5. Profile and tune the complete set without changing the accepted behavioral
   contract. Sound companions remain explicit `[rotation]` opt-ins.

## Verification strategy

Automated tests never require Sway, a real audio server, or an audio device.

Use deterministic synthetic PCM fixtures for:

- silence and near-silence;
- fixed-frequency sine tones for every aggregate frequency region;
- impulses and repeated onsets;
- frequency sweeps;
- left/right panning and centered stereo;
- loud/quiet transitions for attack, release, and normalization; and
- bursts faster than rendering to prove bounded latest-sample behavior.

Use a fake `parec` process for startup, partial reads, cancellation, reconnect,
device loss, bounded retry, and one-time diagnostics. A local real-audio and
real-Sway pass remains complementary manual evidence.

## Acceptance criteria

For the shared engine and every sound companion:

- exactly `width` terminal columns at every tested width;
- no panic for widths from zero through the configured maximum;
- deterministic output for a fixed animation seed, time, and audio snapshot;
- clearly different frames for silence, bass-heavy, mid-heavy, treble-heavy,
  stereo-biased, and onset inputs where the preset uses those features;
- attack, release, warm-up, normalization, and event expiry remain
  time-based and FPS-independent;
- base and sound variants satisfy the kinship and active-response rules;
- all unrelated preset pairs pass the universal similarity guard;
- no audio process starts unless the selected preset, configured rotation, or
  preview requires it;
- only one capture/analyzer exists per process;
- missing `parec`, silence, capture loss, cancellation, and restart have
  distinct tested behavior;
- queues, goroutines, subprocesses, and event histories remain bounded; and
- terminal state is restored after every preview exit path.

Performance budgets on a representative Linux desktop:

- one selected preset or normal rotation: less than 2% of one CPU core on
  average at the default FPS;
- all-presets preview: less than 10% of one CPU core on average; and
- no sustained memory growth, goroutine leak, subprocess leak, or accumulating
  audio latency.

Repository delivery follows `docs/workflow_conventions.md` and the shared
`make verify` gate.
