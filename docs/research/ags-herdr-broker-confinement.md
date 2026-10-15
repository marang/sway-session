# LAB-89: AGS / Herdr broker-created pane confinement contract

Original audit: **2026-10-03**. Latest recheck:
**[2026-10-15](#upstream-recheck--2026-10-15)**; the gate remains unmet. The
sections before that recheck preserve the original audit's source pins and
evidence scope.

Original scope: upstream launch, configuration overlays,
mounts, agent identification, shell panes, restart/resume, and uncertain launch
outcomes. This is a source-and-documentation investigation, not a live security
validation.

**Decision: the upstream integration gate is unmet.** The inspected upstreams
provide useful sandbox and terminal primitives, but no documented joint contract
that keeps broker-created agent and shell panes confined throughout launch,
restart, resume, and failure recovery. Broker-created process confinement must
remain an explicit experimental limitation; do not implement a security
workaround. In particular, an AGS
wrapper launched through Herdr is not sufficient evidence of confinement.
This decision is an inference from the supported surfaces and implementation
facts below, not a claim that every possible composition is impossible.

## Source pins and evidence limits

The GitHub repository, commit, release, tag, and recursive tree APIs were queried
on the research date. Source files were downloaded as inert text and read at
these exact commits:

| Upstream | Version / ref inspected | Commit timestamp (UTC) | Pin |
| --- | --- | --- | --- |
| [Agent Sandbox][AGS-repo] (AGS) | Latest stable **v0.24.1**, also current `main` | 2026-10-01 10:25:56 | [`d2d66e209be102126064c40640552ff3a92016d9`][AGS-commit] |
| [Herdr][H-repo] | Latest stable **v0.9.3** | 2026-09-29 19:06:04 | [`7b116c05bfda646af39d2524c54e70c751f57ee8`][H-stable] |
| Herdr | Current `master`, newer than v0.9.3 | 2026-10-02 23:21:24 | [`5da0a01e1eedda054db0c81dd3a780000c40d9f0`][H-head] |

Release publication dates differ from commit timestamps: AGS v0.24.1 was
published **2026-10-01 10:29:54 UTC**; Herdr v0.9.3 was published
**2026-09-29 19:29:23 UTC**. [AGS release][AGS-release],
[Herdr release][H-release].

Herdr's pane/agent request schemas, pane snapshot schema, `app/agent_resume.rs`,
vendored PTY environment builder, and the socket API/session-state documentation
are byte-identical between the two Herdr pins. Relevant launch/restore sections
of changed files were compared separately; the findings below apply to both
pins. Implementation links use the current commit so line numbers are exact.
The links to `docs/next` identify repository documentation at a pinned commit,
not a promise that every unreleased feature is in the installed stable binary.
[Stable schema][H-stable-schema], [current schema][H-split-schema],
[stable resume][H-stable-resume], [current resume][H-resume],
[stable session documentation][H-stable-session-doc],
[current session documentation][H-session-doc].

No AGS-to-Herdr integration was found in AGS's README, architecture/configuration/
command documentation, or launch source, or in Herdr's configuration,
integration, agent-support, session-state, and socket API documentation and
corresponding launch/resume source. This is a bounded absence finding at these
pins, not a claim about private integrations, all upstream issues, or future
releases. [AGS architecture][AGS-architecture], [AGS commands][AGS-commands],
[Herdr integrations][H-integrations], [Herdr agent support][H-agent-support].

## Launch and configuration contract

| Requirement | Verified upstream fact | Implication for LAB-89 (inference) |
| --- | --- | --- |
| Confine the pane's initial process without first starting a host shell | `pane.split` accepts cwd/env but no command argv and constructs the configured shell. `agent.start` requires an available shell, chooses a built-in agent executable, and writes a shell command to its PTY. [Split schema][H-split-schema], [split handler][H-split], [agent start][H-start]. | These operations do not offer the required initial-process boundary. Starting AGS after shell creation leaves host shell startup outside AGS. |
| Direct argv launch | `layout.apply` accepts a per-leaf `command: Vec<String>` and env, then uses `spawn_argv_command`, which directly constructs the requested program and literal arguments. Omitted commands create shells. [Layout documentation][H-layout-doc], [layout handler][H-layout], [argv spawn][H-argv]. | A real direct launch primitive exists, including support for an absolute executable. It is a layout/tab operation, not a documented AGS security integration; it does not establish the other rows' guarantees. |
| Explicit executable and complete environment | Direct argv launch inherits the server's environment through the vendored `CommandBuilder`; Herdr removes selected variables, applies supplied env entries, and injects its own pane identity/socket variables. The API env object is an overlay, with no reset/allowlist field. Native `agent.start` selects a bare built-in command rather than accepting an arbitrary absolute executable. [Base environment][H-base-env], [builder initialization][H-builder], [pane environment][H-pane-env], [agent schema][H-start-schema], [start argv][H-start]. | Explicit argv and explicit env additions are supported; a complete approved launch environment is not expressed by these requests. Path pinning in a caller does not remove inherited HOME, configuration, or shell behavior. |
| Disable all agent-writable repository overlays | AGS `--config` selects only the base config. Every agent run still resolves `.ags/config.toml` from the current repository/worktree root and merges a trusted overlay. Trust is recorded by canonical repository root, without a content hash; an already trusted root bypasses the interactive prompt. [Precedence][AGS-config], [loading][AGS-load], [trust resolver][AGS-trust]. | No documented run option disables repository overlays independently of trust. An agent-writable trusted overlay can change subsequent launches; `--config` is not a closed policy boundary. |
| Lockdown must preserve the approved policy | AGS loads the merged config before preparing lockdown. Lockdown suppresses generic configured mounts and host bridges, but still uses configuration for image/runtime/home staging and boot directories. Its run options expose lockdown, config, extra directories, and env, without an overlay-disable policy. [Load order][AGS-load-order], [run options][AGS-options], [plan construction][AGS-plan], [home staging][AGS-home]. | `--lockdown` is useful host-exposure reduction, but does not close the overlay-selection gap or define an immutable broker policy. |

AGS itself invokes Podman directly with argv; its launch entrypoint is
`bash -lc` **inside the container**, followed by the agent command. That source
does not imply an AGS host login shell. The host-shell concern above is Herdr's
pane/start/resume path. Trusted AGS base configuration can also explicitly
authorize host command secret helpers before container startup; repository
overlays are forbidden from defining those helpers. These are distinct trust
boundaries. [Podman invocation][AGS-execute], [container entrypoint][AGS-podman],
[secret helper contract][AGS-secrets].

**Missing contract:** an upstream-supported pane launch whose first process is
the approved confinement launcher, with an absolute executable/literal argv,
an authoritative environment policy, and an unconditional exclusion of
repository-controlled sandbox policy. No shell alias, RC edit, wrapper command
typed into a host shell, or inferred environment convention is proposed here.

## Exact mounts, homes, sockets, and agent identification

**Facts about AGS scope:**

- The work directory is canonicalized and mounted writable at its mapped path.
  External Git worktree/submodule metadata is discovered and mounted writable
  separately, including in lockdown. Runtime extra directories are also added
  in lockdown. Therefore the workspace directory alone is not the full writable
  host scope. [Working directory][AGS-workdir], [mount assembly][AGS-mounts].
- Ordinary runs add infrastructure caches, configured agent/generic/tool mounts,
  a dedicated SSH-agent socket when available, and active bridge resources.
  Agent resources for enabled agents have shared mounting behavior; selection
  of one agent is not equivalent to exposing only that agent's home. The managed
  Node store is mounted read-only. [Ordinary mounts][AGS-mounts],
  [infrastructure mounts][AGS-infrastructure], [documented behavior][AGS-run-doc].
- AGS defaults container HOME to `/home/dev` and sets its own PATH. Explicit
  `--env` values have final precedence over these defaults. Auth proxy,
  clipboard, host UI, webview relay, optional PSP, and optional Wayland resources
  have distinct mounts; some expose a service directory containing sockets
  rather than an individual socket. Lockdown suppresses these bridges and
  direct selected-agent-home mounting, stages the selected runtime read-only
  and filtered home writable in a per-run temporary directory, and removes the
  staged history when the run ends. [Environment][AGS-env],
  [bridge mounts][AGS-mounts], [lockdown staging][AGS-lockdown],
  [home paths][AGS-home], [run documentation][AGS-run-doc].
- Lockdown retains networking and exact Git metadata/extra-directory mounts.
  Its default security configuration includes keep-id, no-new-privileges,
  dropped capabilities, and bounded process counts; it also sets `label=disable`.
  This is not evidence of an AppArmor profile or a complete host-resource denial
  policy for sway-session. [Run documentation][AGS-run-doc],
  [security flags][AGS-security].

**Facts about Herdr identity and control:** managed panes receive
`HERDR_SOCKET_PATH`, pane/workspace/tab identity, and `HERDR_ENV=1`. The Herdr
socket provides broad session/pane/input/process-launch control as well as agent
reporting. Merely exporting the socket path into AGS neither mounts the socket
nor restricts what a mounted socket authorizes. [Environment injection][H-pane-env],
[socket path injection][H-socket-env], [control methods][H-methods].

Herdr detection examines processes in the pane's foreground process group and
recognizes known agents from their process names/argv. It supports several
wrapper patterns, so it would be inaccurate to say that all wrapped agents are
undetectable. No AGS/container-specific identity or readiness contract was found
in the inspected detection/launch sources. Custom socket reporting is supported,
including session identity and resume argv, but it is not a confinement
attestation. [Foreground group][H-foreground], [agent recognition][H-detect],
[custom reports][H-report-doc].

**Inference / missing capability:** a future integration must define the entire
canonical host-to-container mount set and every home/config/runtime resource,
including external Git metadata, with permissions and approved socket scope.
It must exclude the host home, private sway-session state/configuration,
unrelated sessions, compositor control, and broad Herdr control unless a
specific authorized capability is part of the contract. It must define how the
wrapped agent's identity, readiness, and session references are associated with
the broker's pane without expanding that scope. Existing identity environment
variables and a mounted host control socket do not supply these guarantees.

## Restart, resume, and shell panes

**Facts:** Herdr explicitly distinguishes detach from snapshot restore: detach
keeps existing processes alive; server/machine restart does not. Ordinary
snapshot panes return as new shells in saved directories. Unsupported, missing,
invalid, duplicate, or stale native session references also fall back to normal
shells. [Session-state contract][H-session-doc].

The persisted pane schema records cwd, labels, agent identity/session/resume
information, and optional launch argv. It has no launch env, mount set, sandbox
mode, or policy identity. On snapshot restart, generic saved launch argv is not
replayed as a confined process; it is retained in the inspected restore path
only for imported live handoff runtimes. Fresh ordinary runtimes use the
configured shell and newly constructed identity environment. [Snapshot schema][H-snapshot],
[restore selection][H-restore].

Eligible native/custom agent resume first starts a configured shell and submits
the planned command to its PTY. The documented custom `resume_argv` must begin
with a **bare command name**; an absolute launcher path is rejected. It carries
no environment or confinement policy. Native-session reporting and custom
resume-command support therefore do not restore the initial-process boundary.
[Deferred resume][H-resume], [argv validation][H-resume-validation],
[resume reporting contract][H-report-doc].

Live handoff can import an existing runtime rather than launching a replacement;
this is a separate mechanism and is not evidence that a later cold restart
preserves confinement. The inspected handoff restore marks saved command panes
for shell respawn on exit; that respawn uses the configured shell with a fresh
identity environment. This does not mean every initially created argv pane
automatically respawns a shell. [Imported runtime and command handling][H-restore],
[shell respawn][H-respawn], [initial argv pane state][H-tab].

AGS has a genuine `--agent shell` mode and can retain a shell inside its container
after a tmux-managed agent exits. `--stop-when-done` changes that tmux behavior.
Those container-shell capabilities do not define Herdr's initial, split,
fallback, restarted, or exit-respawn shell behavior. Lockdown discards per-run
agent history, so it cannot simply be assumed to support durable agent resume.
[Shell profile][AGS-shell], [tmux exit behavior][AGS-tmux],
[lockdown lifecycle][AGS-run-doc].

**Missing contract:** persist an owner-controlled confinement policy identity
and apply it on every cold restore/resume and replacement shell. Unavailable
policy, launcher, image, credentials/session state, or required mounts must
produce an explicit unsupported/error state, with no host-shell fallback.
Ordinary user shell panes and shells after agent exit need an explicit policy
decision within that same contract. A layout export or custom `resume_argv`
string does not establish this lifecycle guarantee.

## Unknown launch outcomes, retries, and rollback

**Facts:** `agent.start` returns after it queues input and records managed-agent
state; interactive readiness is a later observation. It rejects duplicate names
and busy targets. Pending launch detection mismatch/timeout clears the managed
name; the timeout branch does not terminate the possibly running command.
[Start/acknowledgement][H-start], [managed launch reconciliation][H-pending].

`layout.apply` does provide local rollback when building its new tab fails,
closing that tab and shutting down detached terminal runtimes; a successful
replacement creates the new tab before closing the old one. This is real local
failure handling, not a transaction covering AGS containers or external effects.
[Apply failure/replacement][H-layout], [rollback][H-layout-rollback].

The inspected Herdr request envelope has a correlation `id`; ordinary requests
are dispatched to the app without a documented launch-deduplication receipt.
AGS uses a generated container name and foreground `podman run --rm`, returning
an exit status; it also has a specific network-mode probe/retry path. These
surfaces do not document a durable shared operation identity linking one broker
request to one pane/container/confinement policy. [Request envelope][H-request],
[request dispatch][H-dispatch], [container naming][AGS-workdir],
[execution/retry][AGS-execute], [run flags][AGS-podman].

**Inference / missing capability:** losing a response, timing out, or failing to
recognize the agent is not proof that no command/container started. Duplicate
name rejection is not an idempotent successful result, and terminal shutdown is
not proof that all container/sidecar effects were undone. A future contract must
expose authoritative launch/recovery identity, distinguish failed-before-launch
from accepted/running/unknown, and define observation, retry, cancellation, and
cleanup across pane and container ownership. Unknown outcomes must never cause
an automatic unconfined retry or guessed destructive rollback.

## Minimum gate before implementation

These are proposed acceptance requirements derived from the gaps above, **not
current upstream capabilities or a local workaround design**. Reopen confinement
implementation only after a documented, versioned upstream-supported contract
and source evidence cover all of the following:

1. **Initial process:** broker-created panes directly launch the approved
   absolute confinement executable with literal argv and controlled environment;
   no preliminary host shell or host shell startup configuration runs.
2. **Policy authority:** repository overlays can be unconditionally excluded,
   and image, launcher, runtime, homes, env, and mount decisions come from an
   owner-controlled policy that the agent cannot replace or broaden.
3. **Resource scope:** the complete canonical mount/home/socket set, including
   Git metadata and required reporting resources, is explicit and enforceable.
   Agent detection/reporting works without exposing general host control.
4. **Durable lifecycle:** restore, native/custom resume, splits, handoff exit,
   agent exit, and shell panes reuse the approved confinement policy. Missing
   or invalid policy fails closed with a visible error, never a host shell.
5. **Recovery:** stable operation/pane/container identity and authoritative
   status make lost responses and retries reconcilable; cleanup/cancellation
   has documented ownership and effects, including partial launch failures.
6. **Evidence:** after the contract exists, separately validate the supported
   version in disposable state and compositor workspaces 98+, covering denied
   host access, writable-overlay changes, cold restart/resume, shell fallback,
   and uncertain/partial launches. No such validation has been performed here.

Until then, retain the experimental limitation and the distinction between
broker hardening and confinement of processes it causes to be launched.

## Current local integration: supplied parent audit evidence

This section records the coordinating audit's supplied findings; it is not a
second local audit. The broker pins root-owned Herdr and its initializer behind
the manager and revalidates lifecycle/context
([broker.go](../../cmd/sway-session/broker.go), lines 20–53).
The restore runner filters `LD_*` and fixes PATH while inheriting HOME/config
([service.go](../../internal/sessionrequest/service.go), lines 41–75).
The initializer pins executable/cwd/timeouts and removes `HERDR_*`/`LD_*`,
without a general host HOME/RC boundary
([runner.go](../../internal/herdrinit/runner.go), lines 25–80).
It observes a pre-existing idle project host shell, creates the second shell
with `pane split --cwd --no-focus`, and uses `agent start --kind`
([init.go](../../internal/herdrinit/init.go), lines 65–106).
The parent reports no AGS-bound context state presently. These controls do not
satisfy the missing upstream launch/restore confinement contract identified
above.

No sandbox code, Podman, production Herdr, or host shell/configuration snippets
were executed for this research. No tests, build, live integration, or access
denial checks were run. The parent's separate verification is not claimed as
evidence for this report.

## Upstream recheck — 2026-10-15

**Decision: the implementation gate remains unmet.** This recheck updates the
2026-10-03 evidence above; it does not authorize a security wrapper or change the
accepted experimental limitation. A documentation update is warranted because
AGS now offers approved host-side startup hooks, including under lockdown.
That capability adds a relevant pre-container trust boundary; it is not the
joint Herdr confinement contract required by LAB-89. [AGS v0.27.0][R27-release].

### Current pins and comparison scope

Public GitHub release and commit metadata and pinned source archives were read
on **2026-10-15**. Only inert source/documentation files were downloaded under
`/tmp`; no upstream build, installation, hook, container, or live session ran.

| Upstream | Version / ref | Commit timestamp (UTC) | Exact commit |
| --- | --- | --- | --- |
| AGS | Latest stable **v0.27.0**, also current `main` | 2026-10-13 09:16:59 | [`87859cb70b1deca3ae7ca8d07bfb8c115bb640c8`][R27-ags-pin] |
| Herdr | Latest stable **v0.9.5** | 2026-10-14 15:56:41 | [`2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01`][R27-herdr-stable] |
| Herdr | Current `master`, newer than v0.9.5 | 2026-10-14 17:18:21 | [`916fcf8de029c6cac965b50f3cdb9687cd7d1537`][R27-herdr-head] |

Release publication times are **2026-10-13 09:19:09 UTC** for AGS and
**2026-10-14 16:07:28 UTC** for Herdr. Herdr v0.9.5 describes itself as a hotfix
for v0.9.4. [AGS release][R27-release], [Herdr release][R27-h-release].

AGS stable and head are the same commit. Herdr's split/start/layout handlers,
PTY launch source and vendored command builder, snapshot/restore implementation,
deferred resume and argv validation, managed-agent state, and session/agent
documentation were compared between the new stable and head pins: these files
are byte-identical. Their pane/agent request schema sources differ because head
adds shared CLI projection declarations; the inspected split/start fields and
resume restriction remain equivalent. Implementation links below use head;
they do not imply unreleased CLI refactoring is in v0.9.5.
[Stable schemas][R27-stable-schemas], [head schemas][R27-schemas],
[stable agent schema][R27-stable-agent-schema], [head agent schema][R27-agent-schema].

The previous audit's selected Herdr source files were compared directly against
the new head, using the files at each exact commit. The layout handler, pane
snapshot schema, deferred resume, and session-state documentation remain
byte-identical to the prior head.
The source comparison found no new confinement policy field or lifecycle path
in the inspected changed launch/restore code. [Prior snapshot][R27-prior-snapshot],
[current snapshot][R27-snapshot], [prior resume][R27-prior-resume],
[current resume][R27-resume], [current restore][R27-restore].

### Material changes and their limits

**AGS host prepare hooks are new.** Global and project `[[prepare_hook]]` lists
accumulate. Before container creation, approved executables receive a versioned
JSON context and can contribute environment values, generated read-only files,
bind mounts, and 1Password references. Approval binds declaration, argv,
entrypoint contents, and project scope; execution uses a private copy of approved
entrypoint bytes. Separate repository-overlay trust does not grant hook
execution approval. Unknown/new/changed hooks fail noninteractively, and an
untrusted discovered overlay declaring hooks fails rather than silently
skipping them. [Hook protocol and trust][R27-hooks],
[launch order][R27-lifecycle], [overlay loading][R27-load].

These are deliberate host-user executable approvals. Interpreters, dynamic
libraries, imported/sourced code, interpreter-script arguments, and resources
loaded by approved code are **not transitively content tracked**. Approved hooks
still run with host-user authority under `--lockdown`; lockdown rejects surviving
hook bind mounts and secret references, while allowing protected literal env
and generated files. It does not constrain what approved host code can itself
read or change. Therefore a broker policy would also need an authoritative
host-hook policy and transitive trust boundary before launch. This is a gate
requirement, not a claim that hooks bypass their documented approval mechanism.
[Documented trust limits and lockdown][R27-hooks],
[configuration contract][R27-hook-config].

The hook `launch_id` identifies the prepare phase, **not the container name**.
Only `prepare` is implemented; `started` and `finished` are reserved. Hooks have
bounded execution/cancellation but no AGS-managed external-service lifecycle or
automatic retry. Deliberately daemonized or regrouped processes and external
side effects remain the approved hook's responsibility. This is not a durable
pane/container receipt or proof of complete rollback. [Hook lifecycle and
resource ownership][R27-hooks].

**Herdr has improved input validation and clarified readiness limits.** Its
agent-start path now validates the input submission before setting managed
state and queueing the shell command. It still starts the selected built-in
agent by sending input to an existing shell. Current documentation explains
that screen detection can report unfamiliar approval prompts as idle, idle
status does not authorize approval, and agent state does not prove background
work has finished. These improvements do not attest sandbox entry or container
ownership. [Agent start][R27-start], [status documentation][R27-agent-doc].

The documented `HERDR_AGENT=<agent>` hint for a host-visible wrapper was already
present at the prior pin. It selects an existing screen manifest for that
foreground process and cannot be observed if set only inside a VM/container.
It is useful recognition support, **not a newly introduced AGS integration** or
confinement/readiness attestation. The optional `child-groups` process-detection
mode is explicitly best effort. [Wrapper recognition][R27-agent-doc],
[Linux environment observation][R27-hint].

### Gate checked against current source

| Required boundary | Current evidence | Assessment |
| --- | --- | --- |
| First process, absolute argv, authoritative environment | `pane.split` still has cwd/env and constructs the configured shell; `agent.start` still sends a built-in executable command to a shell. `layout.apply` retains real direct argv launch. Its command builder inherits the server environment, applies selected removals/overrides, and injects Herdr identity/control variables. No environment reset/allowlist is expressed in these request fields. [Split][R27-split], [start][R27-start], [argv][R27-argv], [inherited environment][R27-env], [pane environment][R27-pane-env]. | Direct argv remains useful, but there is no supported broker-wide first-process/environment policy. |
| Trusted configuration only | AGS run options still have `--config` and `--lockdown` without an overlay-disable policy. The loader resolves `.ags/config.toml` from the repository/worktree root and merges an approved overlay. Ordinary overlay trust still records the canonical root, not current overlay contents; hook approval adds a separate check for hooks. [Options][R27-options], [loader][R27-load], [root trust][R27-trust]. | An explicit base file does not unconditionally exclude repository-controlled sandbox policy. |
| Exact minimal mounts and homes | AGS keeps writable workdir and discovered external Git metadata mounts; explicit extra directories remain possible in lockdown. Ordinary runs also add configured homes/mounts, runtime/cache and bridge resources. Runtime generations can mount read-only; development pnpm caches are worktree-scoped, and protected cache exposure is rejected. These safeguards do not define the broker's complete approved mount/socket set. [Plan][R27-plan], [cache/mount policy][R27-mounts], [lockdown staging][R27-lockdown]. | No joint contract excluding all private homes/state and broad host-control capabilities was found. |
| Confined shell and durable cold restore/resume | AGS retains `--agent shell`, but Herdr snapshot panes still restore as shells; the pane schema stores no launch environment, mount set or confinement identity. Deferred resume spawns the configured shell before sending argv; custom resume still requires a bare command name and rejects absolute paths. Saved launch argv is retained for imported handoff runtime, not replayed as a confinement policy on cold restore. [Session behavior][R27-session], [snapshot][R27-snapshot], [resume][R27-resume], [validation][R27-validation], [restore][R27-restore]. | Agent and shell roles, replacement shells, cold restore and resume still lack the required shared fail-closed policy. |
| Uncertain launches and recovery | Managed-agent timeout still clears its name without terminating the possibly running command. Layout construction retains local tab/runtime rollback. Herdr correlation IDs and AGS prepare IDs are not a shared durable launch identity; no pane/container/policy recovery receipt was found in the inspected request, launch and hook contracts. [Pending state][R27-pending], [layout rollback][R27-layout], [request][R27-request], [hook protocol][R27-hooks]. | Lost replies and timeout cannot be treated as failed-before-launch or safely retried blindly. |

The negative integration findings are bounded: AGS README, architecture,
configuration, command/startup-hook documentation and launch/trust/plan sources;
Herdr current integration, agent-support/agent, session-state and socket API
documentation plus the cited schemas, launch, detection and restore sources.
No AGS-specific joint launch/restore/recovery contract was found there. This is
an inference from those inspected public surfaces at the pins above, not a
claim about every issue, private integration, possible composition, or future
version. [AGS architecture][R27-architecture], [Herdr integrations][R27-integrations],
[Herdr socket API][R27-socket-doc].

Retain the [six minimum gate requirements](#minimum-gate-before-implementation),
with host-hook policy authority now explicitly part of requirement 2. Requirement
6 still needs separate adversarial and live evidence after a supported contract
exists. LAB-89 remains open pending upstream support; continue independent
roadmap work. No adversarial access-denial, cold restart, real compositor,
AGS/Herdr end-to-end, or hook-execution validation was performed for this
source-only recheck. Repository verification, if run for the research update, is
separate evidence and cannot establish process confinement.

[AGS-repo]: https://github.com/thomaspeklak/agent-sandbox
[AGS-commit]: https://github.com/thomaspeklak/agent-sandbox/commit/d2d66e209be102126064c40640552ff3a92016d9
[AGS-release]: https://github.com/thomaspeklak/agent-sandbox/releases/tag/v0.24.1
[AGS-architecture]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/docs/ARCHITECTURE.md
[AGS-commands]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/docs/COMMANDS.md#L1-L16
[AGS-config]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/docs/CONFIG.md#L15-L45
[AGS-load]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/lifecycle.rs#L401-L439
[AGS-load-order]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/lifecycle.rs#L36-L101
[AGS-trust]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/trust.rs#L68-L118
[AGS-options]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/cli.rs#L31-L48
[AGS-plan]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/plan/build.rs#L173-L465
[AGS-mounts]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/plan/build.rs#L217-L363
[AGS-workdir]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/plan/build_workdir.rs#L1-L79
[AGS-infrastructure]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/plan/build_workdir.rs#L110-L151
[AGS-env]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/plan/build_env.rs#L34-L195
[AGS-security]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/plan/types.rs#L139-L174
[AGS-lockdown]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/lockdown.rs#L97-L214
[AGS-home]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/lockdown.rs#L217-L264
[AGS-run-doc]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/docs/COMMANDS.md#L85-L135
[AGS-secrets]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/docs/CONFIG.md#L345-L367
[AGS-execute]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/podman/exec.rs#L259-L313
[AGS-podman]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/podman/args.rs#L41-L128
[AGS-shell]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/agent.rs#L156-L161
[AGS-tmux]: https://github.com/thomaspeklak/agent-sandbox/blob/d2d66e209be102126064c40640552ff3a92016d9/crates/ags/src/plan/build_env.rs#L343-L379
[H-repo]: https://github.com/herdrdev/herdr
[H-stable]: https://github.com/herdrdev/herdr/commit/7b116c05bfda646af39d2524c54e70c751f57ee8
[H-head]: https://github.com/herdrdev/herdr/commit/5da0a01e1eedda054db0c81dd3a780000c40d9f0
[H-release]: https://github.com/herdrdev/herdr/releases/tag/v0.9.3
[H-stable-schema]: https://github.com/herdrdev/herdr/blob/7b116c05bfda646af39d2524c54e70c751f57ee8/src/api/schema/panes.rs#L18-L35
[H-stable-resume]: https://github.com/herdrdev/herdr/blob/7b116c05bfda646af39d2524c54e70c751f57ee8/src/app/agent_resume.rs#L226-L317
[H-stable-session-doc]: https://github.com/herdrdev/herdr/blob/7b116c05bfda646af39d2524c54e70c751f57ee8/docs/next/website/src/content/docs/session-state.mdx
[H-integrations]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/docs/next/website/src/content/docs/integrations.mdx
[H-agent-support]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/docs/next/website/src/content/docs/add-herdr-support.mdx
[H-split-schema]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/api/schema/panes.rs#L18-L35
[H-split]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/app/api/panes.rs#L34-L141
[H-start-schema]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/api/schema/agents.rs#L166-L176
[H-start]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/app/agents.rs#L145-L231
[H-layout-doc]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/docs/next/website/src/content/docs/socket-api.mdx#L197-L255
[H-layout]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/app/api/layouts.rs#L90-L180
[H-argv]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/pane.rs#L2315-L2358
[H-base-env]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/vendor/portable-pty/src/cmdbuilder.rs#L138-L150
[H-builder]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/vendor/portable-pty/src/cmdbuilder.rs#L250-L263
[H-pane-env]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/pane.rs#L95-L205
[H-socket-env]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/integration/env.rs#L28-L33
[H-methods]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/docs/next/website/src/content/docs/socket-api.mdx#L36-L112
[H-foreground]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/platform/linux.rs#L349-L393
[H-detect]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/detect/mod.rs#L243-L281
[H-report-doc]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/docs/next/website/src/content/docs/socket-api.mdx#L651-L706
[H-session-doc]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/docs/next/website/src/content/docs/session-state.mdx#L7-L115
[H-snapshot]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/persist/snapshot.rs#L99-L121
[H-restore]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/persist/restore.rs#L537-L705
[H-resume]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/app/agent_resume.rs#L226-L317
[H-resume-validation]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/agent_resume.rs#L56-L88
[H-respawn]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/app/api.rs#L510-L607
[H-tab]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/workspace/tab.rs#L137-L177
[H-pending]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/terminal/state.rs#L2225-L2277
[H-layout-rollback]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/app/api/layouts.rs#L505-L525
[H-request]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/api/schema.rs#L35-L40
[H-dispatch]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/api/server.rs#L515-L563

[R27-release]: https://github.com/thomaspeklak/agent-sandbox/releases/tag/v0.27.0
[R27-ags-pin]: https://github.com/thomaspeklak/agent-sandbox/commit/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8
[R27-herdr-stable]: https://github.com/herdrdev/herdr/commit/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01
[R27-herdr-head]: https://github.com/herdrdev/herdr/commit/916fcf8de029c6cac965b50f3cdb9687cd7d1537
[R27-h-release]: https://github.com/herdrdev/herdr/releases/tag/v0.9.5
[R27-hooks]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/docs/STARTUP_HOOKS.md
[R27-hook-config]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/docs/CONFIG.md#L560-L573
[R27-lifecycle]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/crates/ags/src/lifecycle.rs#L38-L115
[R27-load]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/crates/ags/src/lifecycle_config.rs#L6-L67
[R27-trust]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/crates/ags/src/trust.rs#L74-L118
[R27-options]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/crates/ags/src/cli_types.rs#L7-L23
[R27-plan]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/crates/ags/src/plan/build.rs#L173-L363
[R27-mounts]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/crates/ags/src/plan/build_workdir.rs#L110-L190
[R27-lockdown]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/crates/ags/src/lockdown.rs#L97-L264
[R27-architecture]: https://github.com/thomaspeklak/agent-sandbox/blob/87859cb70b1deca3ae7ca8d07bfb8c115bb640c8/docs/ARCHITECTURE.md
[R27-stable-schemas]: https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/api/schema/panes.rs#L18-L42
[R27-schemas]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/api/schema/panes.rs#L21-L40
[R27-stable-agent-schema]: https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/api/schema/agents.rs#L166-L177
[R27-agent-schema]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/api/schema/agents.rs#L185-L199
[R27-start]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/app/agents.rs#L145-L235
[R27-split]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/app/api/panes.rs#L34-L107
[R27-argv]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/pane.rs#L2456-L2498
[R27-env]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/vendor/portable-pty/src/cmdbuilder.rs#L138-L150
[R27-pane-env]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/pane.rs#L100-L224
[R27-snapshot]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/persist/snapshot.rs#L99-L129
[R27-resume]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/app/agent_resume.rs#L226-L317
[R27-validation]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/agent_resume.rs#L56-L88
[R27-restore]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/persist/restore.rs#L537-L705
[R27-pending]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/terminal/state.rs#L2206-L2257
[R27-layout]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/app/api/layouts.rs#L90-L180
[R27-request]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/api/schema.rs#L36-L45
[R27-session]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/docs/next/website/src/content/docs/session-state.mdx#L7-L115
[R27-agent-doc]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/docs/next/website/src/content/docs/agents.mdx#L60-L88
[R27-hint]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/src/platform/linux.rs#L787-L797
[R27-integrations]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/docs/next/website/src/content/docs/integrations.mdx
[R27-socket-doc]: https://github.com/herdrdev/herdr/blob/916fcf8de029c6cac965b50f3cdb9687cd7d1537/docs/next/website/src/content/docs/socket-api.mdx

[R27-prior-snapshot]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/persist/snapshot.rs#L99-L121
[R27-prior-resume]: https://github.com/herdrdev/herdr/blob/5da0a01e1eedda054db0c81dd3a780000c40d9f0/src/app/agent_resume.rs#L226-L317
