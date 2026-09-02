# Repository guidance

## Project

`sway-title-animator` is a Linux-only Go program that renders animated Unicode
art in Sway title formats. It communicates directly with the Sway/i3 IPC socket.

## Required checks

Run the shared local/CI gate before handing off code changes:

```sh
make verify
```

This checks formatting, unit tests, race tests, `go vet`, `staticcheck`, the
`CGO_ENABLED=0` build, and whitespace errors. Use `make fmt` to format Go files.
Use `goimports` or `gofmt` on changed Go files. The module requires Go 1.26;
the preferred security-patched toolchain is Go 1.26.5 as declared in `go.mod`.

## Workflow

`docs/workflow_conventions.md` is the canonical planning, branch, PR, review,
and cleanup workflow.

Work is coordinated in the shared Linear `Lab` team. Every issue for this
repository must belong to the
[Sway Title Animator P001 project](https://linear.app/riotbox/project/sway-title-animator-or-p001-or-sound-reactive-presets-e8a4308a9902)
and carry the mutually exclusive `Codebase` → `Sway Title Animator` label.
Normal implementation starts from one issue in `In Progress`, uses a branch
containing its `LAB-*` key, and reaches `Done` only after its PR is merged.

Run the `code-review` skill before opening or finalizing a PR. Run
`review-codebase` after every fifth substantive branch or at a meaningful
project checkpoint, whichever comes first.

## Architecture

- `main.go`: CLI parsing and process startup only.
- `daemon.go`: animation-only event subscription and frame loop; it must not
  read session state or host session brokers.
- `animator.go`: title calculation, caching, and Sway title updates.
- `instance_lock.go`: single-instance lock and safe replacement.
- `config.go` / `model.go`: configuration and shared data types.
- `animations.go`, `animations_extra.go`, `animation_random.go`: pure animation
  rendering and deterministic seeded motion.
- `audio_meter.go`: optional `parec` capture and spectral analysis.
- `preview.go`: terminal preview and terminal-width handling.
- `cmd/sway-session`: persistent work-session CLI and its explicit long-running
  `daemon`, including desktop-app presence/lifecycle, bounded launch adoption,
  capture, marking, placement, layout restore, reusable typed terminal launch
  and inventory, and the existing narrow broker endpoints.
- `cmd/sway-herdr-init` / `internal/herdrinit`: fixed, registry-locked
  initialization of one empty Herdr session without general pane control.
- `internal/sessionrequest`: owner-only typed session-start protocol and broker
  service.
- `internal/swayipc`: bounded i3/Sway IPC framing and reconnect behavior shared
  by both commands.
- `internal/titleindicator`: versioned presentation-only Sway mark protocol
  shared by the session daemon and animator; it contains no registry or restore
  state.
- `internal/session`: validated context/application and terminal identity,
  strict typed terminal-adapter configuration (`alacritty` or `foot` only),
  versioned session state, and the pure desktop-app restore coordinator.
- `internal/statefile`: owner-only, bounded, transactional JSON state
  persistence.
- `internal/diagnostic`: structured and human-readable CLI diagnostics.

Keep new responsibilities in the matching module instead of growing `main.go`.
Prefer small pure helpers and injected process/time/terminal boundaries for
long-running behavior.

`sway-title-animator` must remain usable without a registry or running
`sway-session daemon`. It must not depend on `internal/session`,
`internal/sessionrequest`, `internal/codexreport`, `internal/herdrinit`, or
`internal/statefile`, and must never open session state files or sockets.
Conversely, session capture and restore must not depend on the animator.

## Animation invariants

- Every animation returns exactly the requested terminal width after truncation.
- Widths at and near zero must not panic.
- Organic movement may vary between launches, but a fixed `animationSeed` must
  remain deterministic for tests.
- `square` uses connected scan-line glyphs, never Braille, and builds in place
  from left or right rather than shifting the completed waveform.
- New presets must participate in the all-pairs visual-similarity guard.
  Intentional relationships require a documented allowlist entry.
- Sound variants use the `<base>_sound` name, keep the base preset's visual
  language, use the base preset when capture is unavailable, and provide a calm
  complete base animation when capture is available but silent.
- Active sound variants must preserve the base preset's temporal choreography
  between beats. Audio may reshape, thicken, brighten, or add bounded events,
  but must not replace a moving base cycle with a mostly static instrument.
- Shared visual smoothing must not discard analyzer-approved onset events.
  Validate beat cadence and band range against numeric live-monitor metadata in
  addition to synthetic snapshots; never store captured audio.
- Do not add sound variants to the default rotation; users opt in explicitly.

## Audio

Audio capture is optional and must start only when an active preset requires it.
`parec` failures must degrade safely, report one actionable diagnostic, retry
without a busy loop, and stop promptly on cancellation. Keep FFT band ranges
ordered and non-overlapping.

The design backlog for additional sound presets is
`docs/sound-presets-plan.md`.

## Safety

- Never trust IPC payload lengths without a fixed upper bound.
- Never cache a title-format update that Sway rejected or failed to acknowledge.
- Never signal a PID from the instance file without validating executable and
  process start time.
- Preserve the exclusive instance lock for the full daemon lifetime.
