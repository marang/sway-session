# Sway configuration parsing and Doctor validation

Date: 2026-10-09.

**Recommendation:** keep Doctor focused on sway-session startup and its required
bindings. Use a bounded, read-only integration analysis with uncertainty attached
to the relevant requirement. Sway's native validator can be an optional,
separate diagnostic or an isolated check of a proposed repair; it cannot supply
the integration evidence itself. Copying the full Sway interpreter is excessive
for that scope.

Scope: upstream Sway **1.12**, annotated tag
[`45961113734a4d2cd3a652723c7499aa5d2e19b6`](https://api.github.com/repos/swaywm/sway/git/tags/45961113734a4d2cd3a652723c7499aa5d2e19b6),
which resolves to commit
[`88869399f421d9180dd8b6ed0b5a1f4a3585d252`](https://github.com/swaywm/sway/commit/88869399f421d9180dd8b6ed0b5a1f4a3585d252).
All Sway links below use that release. No newer Sway behavior is assumed.
The official glibc manual is used only to explain the flags passed to
`wordexp`. The upstream research used public source and documentation only.
The separate local observations below used the user's authorized configuration
review and isolated validation. No private configuration text is saved here.

## The parsing path

Sway treats configuration as commands interpreted against configuration state.
The documented format includes continuations, prefix blocks, quoted arguments,
and commands restricted to configuration or runtime.
[Release configuration manual](https://github.com/swaywm/sway/blob/1.12/sway/sway.5.scd#L7-L68).

| Stage | Verified behavior |
| --- | --- |
| File reader | `read_config` folds continuations, skips blank/comment lines, tracks prefix blocks, and calls the command interpreter. [Reader](https://github.com/swaywm/sway/blob/1.12/sway/config.c#L666-L868). |
| Tokenizer | `split_args` tracks quotes, escapes and criteria brackets. It is not a shell parser. [Tokenizer](https://github.com/swaywm/sway/blob/1.12/common/stringop.c#L89-L139). |
| Command dispatch | `config_command` recognizes blocks, can expand a variable-held command name, resolves the handler, replaces variables, tokenizes again, applies handler-specific quote stripping and unescaping, then calls the handler. Config/runtime handler availability depends on state. [Config interpreter](https://github.com/swaywm/sway/blob/1.12/sway/commands.c#L341-L430), [handler lookup](https://github.com/swaywm/sway/blob/1.12/sway/commands.c#L165-L181). |
| Variable storage | `set` updates a name or creates it; the symbol list is sorted longest-name-first. Values are joined text, with substitution already performed by dispatch. [Set handler](https://github.com/swaywm/sway/blob/1.12/sway/commands/set.c#L10-L58). |
| Variable replacement | Replacement matches name prefixes, honors its escape rules and `$$`, retains unmatched dollars, and skips newly inserted text within that pass. It is independent of shell quote semantics. [Replacement](https://github.com/swaywm/sway/blob/1.12/sway/config.c#L890-L939). |
| Includes | Expansion runs relative to the parent directory. Canonical paths prevent repeated includes. Included files share configuration state. [Include loading](https://github.com/swaywm/sway/blob/1.12/sway/config.c#L556-L625). |
| Runtime command list | Runtime execution additionally splits comma/semicolon command lists and handles criteria. This differs from config dispatch. [Runtime interpreter](https://github.com/swaywm/sway/blob/1.12/sway/commands.c#L208-L332). |

Consequently, a Go shell lexer plus a final variable map cannot claim Sway
equivalence. An exact copy would also need the command handlers, their state
transitions, keymap/criteria behavior, and include expansion. This is an
architectural inference from the separate reader, interpreter and handler
boundaries above.

## What native validation proves

The documented invocation is `sway -C -c <config>`. The binary initializes its
server, invokes `load_main_config(path, false, true)`, and returns 0 or 1. It
returns before normal IPC initialization, backend startup, swaybar startup, and
deferred-command execution.
[CLI manual](https://github.com/swaywm/sway/blob/1.12/sway/sway.1.scd#L16-L20),
[main control flow](https://github.com/swaywm/sway/blob/1.12/sway/main.c#L340-L375).

Validation uses temporary configuration state and frees it afterward.
[Validation load](https://github.com/swaywm/sway/blob/1.12/sway/config.c#L446-L513).
Ordinary output configuration parsing postpones modesetting and swaybg startup
while reading.
[Output handler](https://github.com/swaywm/sway/blob/1.12/sway/commands/output.c#L107-L121).

The limits matter directly to Doctor:

- `exec` and `exec_always` defer when inactive or validating. Their shell command
  is therefore not executed or checked as shell syntax by `-C`; normal execution
  eventually uses `sh -c`. An accepted startup line does not establish that a
  daemon started successfully.
  [Exec validation and execution](https://github.com/swaywm/sway/blob/1.12/sway/commands/exec_always.c#L17-L64),
  [exec reload behavior](https://github.com/swaywm/sway/blob/1.12/sway/commands/exec.c#L7-L19).
- Binding handlers validate options and key combinations, then store the command
  string. They do not validate its eventual command-list behavior. Matching
  bindings can overwrite earlier ones without becoming a validation failure.
  [Binding construction](https://github.com/swaywm/sway/blob/1.12/sway/commands/bind.c#L390-L490),
  [binding replacement](https://github.com/swaywm/sway/blob/1.12/sway/commands/bind.c#L266-L301).
- `for_window` validates criteria but stores the associated command list for
  later execution.
  [Rule handler](https://github.com/swaywm/sway/blob/1.12/sway/commands/for_window.c#L8-L36).
- `include` explicitly disregards included-file load failures and returns
  success. Thus errors in an included file need not produce a nonzero
  `-C` exit. Preserve the validator's diagnostics as well as exit status.
  [Include handler](https://github.com/swaywm/sway/blob/1.12/sway/commands/include.c#L4-L14).

Native validation checks what Sway checks during loading in that environment.
It does not prove the daemon's lifecycle, the presence of particular
sway-session commands, or effective behavior after a keypress. Those are
separate integration questions.

## Validation can run shell code

**`-C` is not a side-effect-free parse operation.** Two inspected expansion
functions pass **0** to `wordexp`, rather than `WRDE_NOCMD`:

| Reachable path | Source |
| --- | --- |
| Include paths | [`wordexp(path, &p, 0)`](https://github.com/swaywm/sway/blob/1.12/sway/config.c#L595-L625) |
| General path expansion | [`expand_path`](https://github.com/swaywm/sway/blob/1.12/common/stringop.c#L314-L329) |
| Wallpaper filename | [Background handler calls `expand_path`](https://github.com/swaywm/sway/blob/1.12/sway/commands/output/background.c#L78-L87) |
| XKB filename | [XKB file handler calls `expand_path`](https://github.com/swaywm/sway/blob/1.12/sway/commands/input/xkb_file.c#L18-L35) |
| ICC profile filename | [Color profile handler calls `expand_path`](https://github.com/swaywm/sway/blob/1.12/sway/commands/output/color_profile.c#L85-L108) |

The glibc contract makes `WRDE_NOCMD` the flag that rejects command substitution.
Without it, shell substitutions in those arguments can execute. Command
substitution stderr is discarded by default.
[Official glibc flags](https://sourceware.org/glibc/manual/latest/html_node/Flags-for-Wordexp.html).
The danger does not require a Sway `exec` directive.

For a sanitized example, a stored wallpaper value containing
`$(printf '%s' /example/background.png)` remains text during `set`, but becomes a
shell substitution when passed through background path expansion. A shell
expression in an unrelated wallpaper or idle command is not, by itself,
evidence that sway-session startup is missing or malformed. The first sentence
follows the inspected storage/expansion paths; the second is the scope rule
recommended for Doctor.

There is also initialization outside parsing: `server_init` creates a backend,
renderer and allocator and opens a Wayland socket before the validation branch.
[Backend/renderer initialization](https://github.com/swaywm/sway/blob/1.12/sway/server.c#L247-L308),
[Wayland socket creation](https://github.com/swaywm/sway/blob/1.12/sway/server.c#L665-L679).

For any optional native validation, the recommended runner boundary therefore
needs:

1. A headless backend and software renderer, a private runtime directory and no
   connection to the user's compositor.
2. Filesystem/process/network isolation that also confines command-substitution
   children; a private runtime directory alone does not constrain arbitrary
   shell commands. Expose required configuration, wallpaper, XKB and ICC inputs
   read-only, without exposing host service sockets.
3. A bounded runtime and captured output, with cleanup of the entire child
   process tree.
4. An explicit environment contract. Changing `HOME`, environment variables,
   paths or visible files can change includes and substitutions; missing
   sandbox inputs must be reported as environmental limits.

These are deductions about an optional runner, not claims that a particular
sandbox was tested. Auditing the complete include tree helps identify expected
substitutions but is not a substitute for that boundary.

## IPC does not expose the evaluated configuration

`GET_CONFIG` returns `config->current_config`.
[IPC reply](https://github.com/swaywm/sway/blob/1.12/sway/ipc-server.c#L907-L915).
That buffer is the main file's text as read, with continuations folded; included
file contents and evaluated values are not appended.
[Buffer construction](https://github.com/swaywm/sway/blob/1.12/sway/config.c#L732-L774).

`GET_VERSION` reports the loaded config filename.
[Version reply](https://github.com/swaywm/sway/blob/1.12/sway/ipc-json.c#L226-L237).
`GET_BINDING_MODES` gives names and `GET_BINDING_STATE` gives the active mode.
They do not enumerate complete effective bindings.
[Binding queries](https://github.com/swaywm/sway/blob/1.12/sway/ipc-server.c#L883-L904),
[official protocol manual](https://github.com/swaywm/sway/blob/1.12/sway/sway-ipc.7.scd#L1040-L1124).
There is no query for an evaluated config, variable table, startup execution
results, complete binding table or retained configuration-error list in the
release's IPC command/event surface.
[Complete IPC enum](https://github.com/swaywm/sway/blob/1.12/include/ipc.h#L6-L40).
This absence is inferred from that surface, not a promise about future Sway
versions.

Sending `reload` is not a read-only validation API: after its initial load it
schedules the real reload.
[Reload handler](https://github.com/swaywm/sway/blob/1.12/sway/commands/reload.c#L54-L73).

## Smallest useful Doctor boundary

The following is a proposal, rather than an existing product contract:

| Choice | Fit for Doctor's integration checks |
| --- | --- |
| Copy the full interpreter | High maintenance and version coupling; lexical similarity still does not provide handler or runtime equivalence. |
| Use only native validation | Correct authority for its load-time checks, but gives no required integration evidence and needs isolation. |
| Keep bounded integration analysis | Fits the four declared requirements and existing repair scope; support and uncertainty must be explicit. |
| Add optional native validation beside it | Useful for an explicit diagnostic or candidate-repair check, if independently justified and safely isolated. |

Keep source observation, integration evidence, and optional native validation as
separate results. The core analyzer should return evidence for each existing
requirement: source location, recognized command/binding, and a bounded reason
when that requirement cannot be determined. Preserve include order, variable
definition order and mode/binding precedence within the supported subset.
Do not turn every dynamic variable or unrelated shell command into uncertainty
for every startup check.

For example, a supported unrelated `exec swayidle ...` should be irrelevant to
the sway-session startup requirement. An opaque wrapper that might launch
sway-session, or an include whose contents cannot be determined, may make the
relevant requirement uncertain. Doctor can then decline a repair that risks
duplicating or overriding an integration, while still reporting known evidence
for the other requirements.

The simple seam is a read-only **integration-evidence analyzer** over bounded
source inputs, with an independently injectable **native-validation runner**
only if a separate validation use case is selected. A native validation result
must never silently upgrade unknown integration evidence to present or absent.
No general Sway health checker is required to correct an overbroad uncertainty
warning.

## Current sway-session implementation and local observations

The reviewed product source is main commit
`c4a3e9fac5fea912e9bea6f77c9fc5d26bcd0551`; its Doctor implementation is unchanged
from released 0.6.4 source `af3cc7bd006ab35991ab4918adaf9bc07be8f5ed`.

Production `Service.Check` combines runtime checks with `inspectSwayConfig`.
It obtains the main configuration filename from `GET_VERSION` unless explicitly
overridden, then reads files on disk. Logical-line lexing, ordered variable
storage, bounded include traversal and command relevance feed four findings:
one-time daemon startup, one-time restore startup, and the two default terminal
shortcuts. Neither `Check`, `Plan` nor `Apply` invokes `sway --validate`.
Plan's proposed-config validation reruns the same integration recognizer;
Apply replans and verifies file fingerprints before guarded writes.
[Service](../../internal/doctor/doctor.go),
[config traversal/report](../../internal/doctor/config.go),
[classification](../../internal/doctor/config_classifier.go),
[repair](../../internal/doctor/repair.go).

An unresolved shell payload and a recognized sway-session reference currently
share a boolean result. A defined wallpaper variable containing command
substitution fails the bounded expansion, so an unrelated idle command can
become an alleged possible daemon/restore startup. The report preserves known
declaration locations but marks both startup findings uncertain and suppresses
repair. Distinguishing known references from unresolved analysis would improve
the diagnosis without removing repair protections.
[Relevance result and expansion](../../internal/doctor/config_relevance.go),
[caller](../../internal/doctor/config_classifier.go).

Local checks on 2026-10-09 inspected the full main file and its glob include
graph: **13 files, 496 physical lines, 17,344 bytes**, with no further includes.
The required daemon/restore/default terminal declarations appeared once each;
the dynamic idle/wallpaper expression produced the Doctor warning. IPC's main
file text matched the on-disk main file exactly after folding its one
backslash-newline continuation. This does not prove that included files remain
identical to their last loaded versions, because GET_CONFIG omits them.

Native Sway 1.12 checks ran with a headless backend and software renderer in
private process/network/IPC namespaces, read-only host files, hidden host
runtime/device paths, private runtime/cache and an allowlisted environment.
All 13 original file digests were unchanged afterward. No live reload or
autostart was performed.

| Local native case | Exit status | Captured Sway diagnostics |
| --- | --- | --- |
| Entire actual include graph | 0 | No stderr or stdout; no configuration errors observed |
| Invalid command in a private root file | 1 | Unknown command and config-load error |
| Invalid command in a private included file | 0 | The same configuration errors despite successful process status |

The first isolation attempt failed before Sway started because its mount
destination was beneath the read-only root. Its failure record is retained;
the corrected private mount produced the measurements above. These are local
validation observations, not a claim that autostarts, shortcuts or hardware
behavior were exercised. Raw private configuration and diagnostics remain
outside the repository.

## Integration alternatives discussed

These are design alternatives, not implemented changes. The central choice is
what Doctor promises: current runtime health, recognition of supported
declarations, or an exhaustive claim about arbitrary scripts and overrides.
The last promise cannot be delivered by a bounded configuration recognizer.

| Alternative | Benefit | Limit or additional responsibility |
| --- | --- | --- |
| Narrow the existing recognizer | Smallest migration; unrelated configuration no longer controls the startup verdict | Some include and relevant variable handling remains. Arbitrary wrappers cannot be proved harmless; repair must be restricted to established evidence. |
| Make one owned integration snippet the supported setup contract | Doctor checks a small known file and repairs only that file | An owned snippet already exists for repair. The change is its role, not another file. Its presence does not prove inclusion or prevent later binding overrides; custom setups need an explicit reduced-check path. |
| Check runtime health only | Removes configuration interpretation from Doctor | A running daemon and healthy brokers do not prove next-login startup or working shortcuts. Setup instructions and optional behavioral checks would provide separate evidence. |
| Register configured shortcuts through IPC | Sway interprets our commands; no scan for shortcut declarations is necessary | Requires explicit authority to own the chosen keys, reapplication after reload, and defined mode/conflict behavior. Successful registration does not guarantee that a later command will not replace it. |
| Move startup to a user service | Explicit process supervision and service diagnostics | Requires compositor readiness, the correct environment and session lifetime; startup still needs a reliable session trigger. This alone does not remove the shortcut question. |

Runtime shortcut registration is a supported Sway 1.12 command path:
`bindsym` belongs to the common handler table used by runtime command execution.
Equal bindings may replace existing commands, which explains the ownership
tradeoff above.
[Runtime command dispatch](https://github.com/swaywm/sway/blob/1.12/sway/commands.c#L41-L97),
[Binding replacement](https://github.com/swaywm/sway/blob/1.12/sway/commands/bind.c#L266-L301).
User services can be associated with graphical session lifetime; a compositor
integration must still establish that lifecycle and its environment.
[systemd graphical-session targets](https://github.com/systemd/systemd/blob/main/man/systemd.special.xml).

Shortcut defaults are also a product choice: users can invoke the CLI or select
different keys. Absence of our two default chords need not mean that the
application is broken. Making shortcut setup optional is independent of which
startup alternative is selected.

The proportionate direction is to combine the owned snippet with runtime
health checks and optional shortcuts, while limiting automatic repair to owned
content. Report configuration evidence and runtime observations separately;
do not label a setup fully active merely because its file exists. Native Sway
validation remains a separate optional diagnostic. No new startup signal,
service, CLI command or configuration change is introduced by this discussion.
