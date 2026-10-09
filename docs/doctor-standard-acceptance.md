# Standard Doctor integration acceptance (LAB-322, LAB-326)

This record covers the owned standard file, explicit adoption/profile choices,
independent runtime findings, safe file repair and the actual CLI/TUI surface.
It does not authorize or establish a production installation or release.

## Candidate and environment

The acceptance runs on 2026-10-09 used Arch Linux x86_64, kernel
`7.2.8-arch1-2`, Go `1.26.5`, Python `3.14.7` and Sway `1.12`.
The comparison baseline is `c4a3e9fac5fea912e9bea6f77c9fc5d26bcd0551`.
The initial acceptance product source was `40578e57796837d7c0f3321769a35ecba89ccd0e`;
the executing binary reports `dev`, that exact commit and `modified=true`.
The flag records pending acceptance evidence in the build checkout. Its SHA-256
is `46afe37f9c553b48532333212e5c3b802318ef2b905cde4bb734d2f00ce527ca`.
The executable was built with `GOTOOLCHAIN=go1.26.5 make build` (CGO disabled,
repository build stamp), then copied outside the checkout for execution.
The acceptance-observer and documentation changes immediately following that
run did not alter its product executable. Later product corrections are separate
candidates; this initial artifact identity and its results are historical
evidence, not an assertion that every subsequent product revision was tested.

Every compositor uses the headless backend and Pixman renderer, workspace 98,
private configuration and XDG roots, and an explicitly selected private socket.
The subprocess environment uses an allowlist: production `SWAYSOCK`,
`WAYLAND_DISPLAY`, `DISPLAY`, D-Bus addresses and configuration overrides are
never inherited. Session/Herdr configuration belongs to the fixture; Herdr
inspection uses its bounded read-only version/help probes. No Herdr session or
production sway-session process is started.

The sandbox remaps system ownership and cannot create the private Wayland
socket. The same scoped acceptance commands therefore ran outside that
sandbox, with actual host ownership and socket support. Production safety
checks were retained without modification.

## Behavioral evidence

The reusable [executable acceptance](../scripts/doctor-standard-acceptance.py)
uses the built CLI for text/JSON and real NO_COLOR PTYs. Its bounded VT observer
records displayed cells and navigates with the same keys available to users.
The Go private-Sway tests separately load all source fixtures and inspect the
public service/CLI report plus rendered TUI. Runtime errors remain independent
of a successful source repair: the deliberately absent explicit session config
in the Go adoption case keeps operational exit code 3 while `sway.integration`
becomes `ok` and the applied repair result remains visible.

That initial artifact passed all 24 executable scenarios: nine CLI scenarios, two
startup profiles, twelve PTY scenarios and the no-runtime-effects sentinel.
Both private-Sway Go tests passed their nine subcases under Go 1.26.5; the eight
focused repair-safety race tests also passed at the final product source.

| Scenario | Expected and observed behavior | Proof level | Status |
| --- | --- | --- | --- |
| New startup-only setup | Implicit adoption is refused; explicit preview changes neither file; confirmed adoption creates daemon/restore directives and appends one direct include while preserving previous text. | Actual CLI text/JSON and real PTYs at 80×24/48×16 | Passed |
| Explicit default shortcuts | Explicit selection adds exactly the two supported terminal bindings; adoption/profile decisions precede the preview. | Actual CLI text/JSON and real PTYs at both sizes | Passed |
| Preserve and switch | Unspecified selection preserves either existing profile and reports that no repair is needed. Explicit profile switches change only the owned file; the main file stays byte-identical. | Actual CLI and real PTYs at both sizes | Passed |
| Manual migration/protected files | Historical daemon-only files, foreign files and manually edited owned files retain their bytes and have no automatic repair action. Explicit adoption cannot override that protection. Manual guidance remains reachable. | Actual CLI text/JSON and real PTYs at both sizes | Passed |
| Direct and repeated includes | Supported owned files with a literal direct include report `ok`; repeating the direct include does not change Doctor's source finding. No effective-order or key-execution claim is made. | Actual CLI; loaded private-Sway sources in Go tests | Passed |
| Only indirect include | Native Sway accepts and loads the fixture, while Doctor reports `warning` and honestly states that a literal direct include was not observed in the selected main file. | Actual CLI and private Sway | Passed |
| Foreign dynamic expressions | Wallpaper-/idle-shaped shell substitutions and command sequences leave the standard finding's status, detail and limitation unchanged. | Actual CLI comparison; private-Sway source fixtures | Passed |
| Startup declarations, both profiles | An inert owned executable named sway-session records exactly one `daemon` and one `restore` invocation on private startup; two reloads produce no further invocations. | Real private Sway plus owned trace subprocesses | Passed |
| Preview and cancel | Adoption and preview cancellation leave the main file byte-identical and the missing standard file absent. | Actual CLI preview and real PTYs at both sizes | Passed |
| Original private backups | Adoption backs up the exact original main text; a profile switch backs up the exact original owned file. Backup modes are `0600`. | Actual CLI and PTYs, filesystem byte/mode checks | Passed |
| Stale preview/editor save | Editing the main file between actual TUI preview and apply rejects the stale plan, preserves the editor's bytes and leaves the standard file absent. | Real PTYs at both sizes and filesystem checks | Passed |
| Concurrent-writer compensation | In-place/atomic root saves, concurrent snippet creation, equal-content writers, replacement during rollback and failed compensation preserve the other writer's files. | Focused race tests with real files and injected write boundaries | Passed |
| Cancellation and changed directory | Canceled repair plans, unsafe executable paths, changed files and replaced directories are refused. | Focused race tests and private-Sway service tests | Passed |
| Complete narrow guidance | Adoption limits, profile limits, first-start commands, next-session/reload distinction, result/backup and recheck feedback stay reachable by paging; error recovery and cancel/quit controls remain visible. | Actual PTYs at 80×24/48×16 plus bounded rendering tests | Passed |
| No runtime effects from Doctor | An inert private `exec_always` observer records only initial compositor startup throughout the CLI/TUI matrix. No additional load occurs, and no sway-session runtime or session-state directory appears. | Real private Sway trace and filesystem checks | Passed |

## Current acceptance after review corrections

The corrected product source is
`3c1dc2611a4118e324fa71e531447be79b9e5eca`. Its executing candidate reports
`dev`, that commit and `modified=false`; its SHA-256 is
`38d886e3b7a564fbc8364ea41e318dc6f61660c8b1cbfecb84833192daf8e9b4`.
It was built with Go 1.26.5, CGO disabled, the repository's build flags and an
explicit matching build stamp, directly into a disposable directory outside
this checkout. This documentation update changes no product source.

At that revision, the complete canonical `make verify` passed, including
Bash/Zsh/Fish completions and the private packaging fixtures. All 24 actual
CLI/PTY scenarios and all nine private-Sway Go subcases passed again. A
10-second, two-worker `FuzzLiteralDirectInclude` run passed 92,873 executions.
Additional actual-CLI/native-Sway probes verified the five corrected families:
indented-comment continuations, lost block framing after oversized lines,
include block heads with a following physical brace, escaped whitespace before
braces, and Unicode whitespace belonging to a distinct filename. These probes
used only disposable sources and the same isolated compositor boundary.

Committed regression tests check both literal evidence and its repair
consequences: required adoption, refused unsafe append, preserved source bytes,
and profile changes that touch only the intended files. Non-ASCII filename
bytes remain literal; quoted Unicode directory names remain supported.
Review conclusions and PR/merge approval are separate gates from this
behavioral evidence. The initial artifact above remains historical evidence.

## Reproduce

Build a candidate whose basename is `sway-session`; the repair engine deliberately
refuses any other basename. Use the declared Go toolchain and a disposable build
directory outside the checkout, then run:

```sh
mkdir -p /tmp/lab322-candidate
GOTOOLCHAIN=go1.26.5 CGO_ENABLED=0 go build -o /tmp/lab322-candidate/sway-session ./cmd/sway-session
python3 -B scripts/doctor-standard-acceptance.py --binary /tmp/lab322-candidate/sway-session
SWAY_SESSION_HEADLESS_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 \
  go test ./cmd/sway-session -run '^TestDoctor(Configuration|StandardStartup)Headless$' -count=1 -v -timeout=3m
GOTOOLCHAIN=go1.26.5 go test -race ./internal/doctor \
  -run '^Test(RepairRejectsStaleFileAndDirectoryPlans|RepairPreservesConcurrentRootSave|RepairDoesNotReplaceConcurrentSnippetCreation|RollbackDoesNotRemoveEqualContentFromAnotherWriter|FailedCompensationPreservesBothConcurrentFiles|RollbackPreservesReplacementDuringRemoval|RepairRefusesUnsafeExecutableAndCancellation|RepairPlanPreviewApplyBackupAndIdempotence)$' \
  -count=1 -v -timeout=2m
```

These commands reproduce the behavioral checks. The direct Go build creates
its own artifact identity; it does not reproduce the stamped digest above.
The runner records the actual executing build metadata and digest for each run.

The Python runner creates and removes its own `/tmp` root. `--keep` retains only
that private fixture's logs, PTY captures and JSON artifact identity/scenario
record; these raw diagnostics stay outside the checkout. `--skip-tui` explicitly
records the real-PTY boundary as not run and is not sufficient for LAB-326.

## Limits and remaining gates

The startup traces prove Sway's handling of the declarations. They do not prove
the behavior of a production daemon or restore operation. GET_CONFIG proves
loaded main-source text; it does not prove effective include order or executed
shortcuts. Optional broker checks retain their owner-checked connect-only
liveness meaning. No physical keyboard shortcut, workstation login/reboot,
production reload, hidden-startup search, conflicting-binding exclusion, native
Sway validation in the production Doctor path, package-manager installation or
release publication was performed.

The canonical `GOTOOLCHAIN=go1.26.5 make verify` at the initial acceptance
source passed. Its completion
check ran Bash, Zsh and portable Fish 4.9.3 without workstation installation.
Uncached unit/race tests, vet/staticcheck, AppArmor, packaging (three private
installation fixtures), standalone checks, release/VM helper tests, CGO-disabled
build and whitespace checks passed. These private installation fixtures do not
establish a package-manager installation or production transition.

The local code/adversarial/security reviews and scoped architecture checkpoint
at the initial acceptance source reported no remaining findings. They were
preparatory reviews and do not substitute for reviewing later product changes.
PR and merge gates remain open.
No unexecuted boundary or pending review/merge is recorded as passed.
