# LAB-94: future privileged desktop restore — research and decision gate

Research date: 2026-10-03. Scope: the LAB-94 privileged-desktop restore design reminder.
LAB-94 is a design reminder. It does **not** authorize privileged restore in v1, an implementation, a workaround, or a ticket completion claim.
Recommendation: preserve the current rejection boundary until a separately authorized, application-specific design satisfies the gates below.

## Evidence scope and source pins

- Repository: clean checkout at `e039d7db8d7e0d37ff9ba8d139b55bc638d50220`; repository source links below pin that inspected snapshot.
- Method: read the launcher, its tests, application commands, restore policy, and adjacent lifecycle/report/broker code; inspect existing assertions without running tests.
- External evidence: official specifications, project documentation, and upstream source only, browsed on the research date.
- Polkit: the [upstream releases page](https://github.com/polkit-org/polkit/releases) lists 127 as latest; [release commit](https://github.com/polkit-org/polkit/commit/9e4894c969eecf26a3ba762f4f7a268aa0fb3e51) is `9e4894c969eecf26a3ba762f4f7a268aa0fb3e51`.
- Polkit source citations pin that commit. The [hosted HTML manual](https://polkit.pages.freedesktop.org/polkit/) still labels itself version 124; relevant API/security facts were checked against release-127 source.
- Desktop Entry Specification: [version 1.5](https://specifications.freedesktop.org/desktop-entry/latest/index.html), with versioned section links below.
- GIO: [launch documentation](https://docs.gtk.org/gio/method.AppInfo.launch.html) labels its library 2.90.0; this live page is date-pinned, not an immutable source revision.
- `run0`: [current upstream source](https://github.com/systemd/systemd/blob/main/man/run0.xml), date-pinned, corroborates the authentication model in [systemd v256 source][run0]. No installed-version claim.
- No privileged application was executed and no user configuration or private identity was inspected. This source audit does not claim live authentication or application acceptance.

## Current behavior verified by source and existing assertions

| Boundary | What the inspected code establishes |
| --- | --- |
| Known administrative/indirect launchers | Lowercased executable basenames reject `sudo`, `su`, `pkexec`, `run0`, `doas`, shells, and named indirect runners; Python/Ruby/Perl prefixes are also rejected. This is an explicit shape filter. [map and predicate][reject] |
| Registration | Focused registration requires an exact candidate or explicit selection; `--yes` does not bypass launcher validation. Application confirmation consumes a short-lived typed operation, then reloads catalog/window evidence and prepares trust approval. [commands][app-command], [application operations][app-operation], [apply registration][app-apply] |
| System desktop trust | Registration and launch reread bounded, root-owned desktop material through checked path components; launch reparses and reapplies the rejection filter. System executable contents are not pinned by a stored digest. [approval][approval], [launch revalidation][revalidate] |
| User desktop trust | Approval creates a private desktop snapshot and hashes source bytes; a non-root executable also gets a stored path/digest. Launch checks source, snapshot, and any recorded executable digest. Changes require explicit reapproval. Root-owned executable digests are not retained. [approval][approval], [revalidation][revalidate] |
| Launch boundary | Desktop launch builds `/usr/bin/gio` plus typed `launch`/path arguments and a fixed PATH override. The process starter inherits other environment values and reports process acceptance without waiting for application exit. This is neither a privilege adapter nor a fully sanitized privileged environment. [spec][launch-spec], [starter][starter] |
| Restore eligibility | Active and desired-open makes either Follow or Pinned eligible; archived/desired-closed does not. Eligibility is not authorization, mapping, or application-internal recovery. [policy][restore-policy] |
| Pinned and launch attempts | Pinned preserves desired-open; adopted presence and durable attempts suppress another automatic launch in the same compositor lifetime. It is not a process watchdog. [coordinator][coordinator], [Pinned assertion][pinned-test] |
| Groups and shutdown | Matching top levels establish group presence; only a stable matching mark or unique candidate supplies an anchor. Fresh launch confirmation rejects lost/changed event streams and shutdown. [groups][groups], [confirmation][launch-confirm] |
| Existing report/brokers | Restore progress already distinguishes acceptance, mapping, placement, and layout. Its status/reason allowlists have no dedicated authentication states. Start/report brokers have narrow typed requests and owner checks, not arbitrary privileged execution. [report][report], [start request][start-request], [peer check][peer-check], [agent report][agent-report] |

`TestDesktopEntryRejectsAdministrativeAndIndirectExecutables` explicitly covers `pkexec`, absolute `sudo`, `sh`, `env`, `python3`, `node`, and `wine64`; `su`/`run0` are in the map but are not separate cases in that test. [assertions][launcher-tests]
The same suite asserts protected snapshots, changed-source/executable rejection, typed GIO/Flatpak arguments, and cancellable snapshot-lock waiting. [launcher tests][launcher-tests]
CLI tests assert one-time approval replay rejection and ambiguous registration handling; policy tests exercise active/archived, desired-open, and both policies. These are inspected assertions, not newly obtained passing results. [CLI tests][cli-tests], [policy tests][policy-tests]

The denylist is **not a complete privileged-identity detector**: renamed helpers, direct applications using privileged services, scripts, and D-Bus activation are outside what a basename proves.
System D-Bus-only entries can pass the filter; user-local D-Bus-only entries are rejected. An ordinary-looking entry or executable can still request authentication internally. Thus no universal absence-of-login-prompts guarantee follows from this filter. [filter][reject], [validation][approval], [activation semantics][desktop-keys]

## Primary-source findings

- Desktop launch metadata describes executable/arguments and availability, not a standard privilege authorization contract. `TryExec` checks installation; no standard key binds a polkit action or authenticated principal. Treat any universal trust inference as unsupported. [standard keys][desktop-keys]
- `Exec` has its own quoting/field-code rules and permits PATH resolution. `DBusActivatable=true` instead directs implementations to activate through D-Bus, ignoring `Exec`; the service name comes from the desktop filename. Executable-only approval cannot establish that activation identity. [Exec][desktop-exec], [keys][desktop-keys], [D-Bus activation][desktop-dbus]
- GIO says successful launch can still be followed by startup failure; launch acceptance is not window evidence. [GIO launch][gio-launch]
- Polkit separates the privileged mechanism, requesting subject, authority, and session authentication agent. The mechanism must authorize the requested operation; a client-side precheck cannot replace it. [polkit 127][polkit-policy], [developer guidance][polkit-apps]
- Interactive authorization is intended to follow a user action. Checks may wait for authentication, and synchronous waiting must not block the GUI/service thread. Login reconciliation is insufficient consent for a privileged prompt. [authority implementation/API documentation][polkit-authority]
- `auth_self` and `auth_admin` differ; retained variants can authorize later checks for the same action/subject even with changed detail variables. Local policy can also authorize without a prompt. Product consent must therefore be separate from a password challenge. [polkit policy][polkit-policy]
- `pkexec` defaults to root, selects its action using executable-path/optional first-argument annotations, and does not validate remaining arguments. Its `command_line` variable is unsuitable for security checks. A desktop ID or generic executable match is insufficient authorization evidence. [pkexec 127][pkexec]
- `pkexec` can create a text authentication agent when a session agent is absent. Its restricted environment normally excludes X11 display credentials; GUI exceptions are documented as discouraged. These docs do not establish a supported general elevated Wayland desktop launch. [pkexec][pkexec]
- `run0` obtains elevation through polkit and starts a service in a different execution context. It does not solve desktop identity, consent, or restore tracking merely by replacing another rejected command. [run0][run0]
- Authority results distinguish authorized/challenge and support cancellation IDs. A challenge can also mean no suitable agent was available, so it is not proof that a dialog exists. Agent authentication carries separate action/identity/cookie data. [authority wire contract][authority-wire], [agent contract][agent-wire]
- `pkexec` documents 126 for dismissed authentication, 127 for authorization/error, and otherwise the program exit status. Inference: the same program statuses can collide with those values; these integers alone cannot prove cancellation or definitive non-dispatch. [pkexec][pkexec]
- Polkit 127 warns that PID-based subject tracking has known races; safe process identity needs supported PID-fd plumbing and trusted caller credentials. Do not present PID/start-time checks as universally race-free. [process identity documentation][polkit-process]

## Bounded threat model

Proposed scope: prevent unintended or stale elevated restore, substituted launch material, approval replay, wrong-principal authorization, ambiguous placement, and duplicate effects after uncertainty.
Trust the selected upstream mechanism, polkit authority/session agent, kernel/compositor, and administrative package installation. Malicious root or a fully compromised graphical session is outside this design.

| Threat or trust boundary | Required future constraint |
| --- | --- |
| Same-user broker client or automation | Owner-only access authenticates a Unix owner, not a human decision. Existing sockets must never accept arbitrary executable/argv/environment/target-user/action requests. A privileged mechanism must enforce its own narrow authorization. [peer check][peer-check], [polkit guidance][polkit-apps] |
| Desktop/executable/activation replacement | Bind approval to exact launch material, executable origin/content, activation route/service owner, and supported operation. Revalidate at registration, launch decision, and dispatch; address the check-to-execution race in the upstream adapter contract. [current revalidation][revalidate], [activation][desktop-dbus] |
| Policy/principal drift or cached grant | Revalidate action ID, helper binding, target principal, actual caller/session/seat, authority generation, and effective mechanism decision each launch. An action-file digest is not the effective authorization decision. [policy][polkit-policy], [subject API][authority-wire] |
| Dialog mistaken for restored application | Authentication dialogs belong to the session agent, never the registered application group. App IDs/classes/marks support matching, not proof of elevated identity; native Wayland parent/type attribution remains a recorded upstream limitation. [agent contract][agent-wire], [groups][groups], [project limitation][identity-limit] |
| Secret disclosure or forged response | Keep passwords, OTPs, authentication responses, and polkit cookies entirely in the agent/authority path; never persist or forward them through session state, approval tokens, brokers, logs, diagnostics, or environment. [agent contract][agent-wire] |
| Cancel/timeout/shutdown race | Cancellation of a check is not rollback of an already dispatched operation. Persist conservative intent, stop further effects, and require observed/typed evidence before deciding whether launch occurred. [cancellation API][authority-wire], [existing attempt guard][coordinator] |

## Safe future proposal — design requirements only

1. Limit support to named applications and typed operations using an upstream privileged mechanism. Elevated GUIs require a documented integration; arbitrary Exec interpretation stays unsupported. [mechanism guidance][polkit-apps]
2. Obtain **per-registration consent** to track that exact privileged application, privilege scope, target principal, and restore policy. Registration grants tracking capability, not permanent authorization to execute.
3. Obtain a separate **per-launch decision** after showing the exact application, requested operation/privilege, identity changes, and partial/unknown-outcome consequences. Bind a single-use, expiring decision to context revision, attempt, compositor/session generation, policy, and resolved identities.
4. On login, expose a bounded pending item only: no privileged launch or interactive authentication. Pinned preserves intent until a later explicit decision; it must not repeatedly prompt or relaunch after a close/crash. [interactive-check guidance][polkit-authority], [Pinned behavior][pinned-test]
5. Immediately before dispatch, repeat desktop/executable/activation/polkit identity validation and fresh group/lifecycle observation. Require the mechanism to check actual authorization for the real caller and exact operation. Changed or unprovable identity invalidates consent; do not guess. [identity API][polkit-process], [fresh confirmation][launch-confirm]
6. Let the existing trusted session agent handle any challenge. Missing agent, inactive/wrong session, denied policy, or unavailable authority yields a typed deferred/failed outcome without a text-agent or credential-capture fallback. [agent contract][agent-wire], [missing-service guidance][polkit-apps]
7. Retain only non-secret intent, identity revisions, timestamps/generations, and typed outcomes in owner-only state. Keep authentication/external calls outside short SQLite transactions and long-lived lifecycle locks; bound queue size, concurrency, observation work, and deadlines. [repo transaction rule][repo-rules], [async guidance][polkit-authority]
8. Keep privileged capability outside existing generic start/report brokers. Any new boundary must resolve an already approved context/operation, reject unknown fields, enforce caller identity and replay/expiry checks, and offer no command-language escape. [current request shape][start-request], [agent request][agent-report]

Fresh product consent is required even if polkit returns authorization without another password prompt. Whether a fresh credential challenge is also mandatory is a user product decision; cached/implicit grants mean generic polkit use cannot promise that challenge.
Registration `--yes`, pinning, restore eligibility, or a previously successful password challenge must never silently become forever consent. Do not install passwordless authorization rules or alter/revoke global session grants to manufacture this behavior. [policy][polkit-policy], [application-rule guidance][polkit-apps]

## Proposed typed states and partial mapping

These names describe future semantics, not current wire/schema additions. Authentication phase, dispatch evidence, and window/placement/layout proofs must be independent fields. Existing report allowlists require an explicit compatibility decision. [current report][report]

| Proposed state | Evidence and behavior |
| --- | --- |
| `awaiting_launch_consent` | Login/policy identifies intent only; no authentication request or privileged effect. |
| `authentication_required` / `agent_unavailable` | Mechanism says challenge is needed, or session agent cannot serve it; no claim that a prompt opened. |
| `pending_authentication` | An explicitly consented attempt is awaiting the trusted agent; bounded deadline and cancellation handle, no stored credentials. |
| `denied` / `cancelled` / `consent_expired` / `authentication_timeout` | Distinct pre-dispatch outcomes only with authoritative evidence; late replies cannot revive expired consent. A dispatch race becomes `outcome_unknown`. |
| `launch_rejected` | Adapter conclusively proves no dispatch; a new explicit decision may retry after revalidation. |
| `launch_accepted` / `awaiting_window` | Dispatch accepted; authorization/acknowledgement does not prove a window or recovery. |
| `mapped_partial` / `ambiguous_group` | Presence exists, but requested mapping/placement/layout evidence is incomplete or anchor selection is ambiguous; preserve proven work and do not pick a window. |
| `completed` | Every requested window/placement/layout proof exists; never imply application-private session recovery. |
| `outcome_unknown` / `interrupted` | Shutdown, lost reply, transport failure, or mapping timeout may follow dispatch; retain intent/effect uncertainty and never automatically retry. |

Use per-context outcomes for a mixed restore batch; one cancellation must not erase another application's proven mapping. Bound interactive work so multiple pending contexts cannot create a prompt storm.
During shutdown/logout/sleep, cancel pending checks, invalidate launch consent, preserve Pinned desired-open, and stop dispatch. A restart/new compositor must not turn an uncertain privileged attempt into an automatic retry; require a new explicit decision and fresh observation.
Cancellation after acceptance cannot promise to stop the privileged operation or close its windows safely. Preserve partial mapping; never close an independently opened window to enforce an earlier restore plan. [existing race limit][launch-race], [GIO limitation][gio-launch]

## Product decisions, upstream prerequisites, and honest gate

| Gate owner | Evidence/decision required before implementation can be authorized |
| --- | --- |
| User/product | Choose supported applications and privilege scope; ordinary GUI/helper versus elevated GUI; permitted principals; registration/launch consent UI and CLI semantics; Pinned/Follow policy; challenge freshness; deadlines and explicit retry/partial-result UX. Current `--yes` supplies none of these decisions. |
| Application/mechanism upstream | Document exact action/operation/principal mapping, noninteractive capability checks, supported session-agent interaction, cancellation semantics, reliable dispatch acknowledgement, and outcomes. No universal desktop-to-polkit mapping is established. [developer warning about action mapping][polkit-apps] |
| Polkit/session stack | Demonstrate trusted caller/PID-fd support, correct seat/session attribution, agent/authority loss behavior, policy-change handling, and the promised cache/challenge semantics on selected supported versions. [process warning][polkit-process], [policy][polkit-policy] |
| Compositor/application identity | Demonstrate privileged-instance versus ordinary-instance and agent-dialog separation, including Wayland/XWayland, multiple windows, and service activation. LAB-93's documented native metadata gap is a dependency to evaluate, not proof all identity problems will be solved. [project limitation][identity-limit] |
| sway-session compatibility/security | Separately authorize typed launcher/state/report changes, schema/protocol handling, AppArmor/packaging implications, and review of the narrow boundary. Preserve established brokers and lifecycle/transaction invariants. [repository rules][repo-rules] |

**Gate remains closed:** no selected upstream integration, product decisions, or end-to-end privileged restore evidence was obtained here. Official docs support boundaries and research questions; they do not establish readiness to enable privileged restore.

## Future verification requirements

- Pure/fixture tests: retain known helper/indirect rejection, explicitly cover `su`/`run0`, and distinguish wrapper/D-Bus/unknown classification from a complete privilege detector; verify desktop/executable/service/action changes revoke consent.
- Consent/authentication tests: registration cannot launch; every privileged attempt consumes fresh consent; replay/expiry, changed context/policy/session, retained grants, wrong principal, no agent/authority, and denied/cancelled/timed-out challenges remain distinct; never collect/store credential responses.
- Race/failure tests: cancellation versus authorization/dispatch, late responses, policy/agent/authority changes, caller PID reuse, restart/shutdown around durable intent, accepted-effect/lost-reply, and unknown outcomes retain the no-auto-retry guarantee.
- Mapping/lifecycle tests: ordinary and elevated instances sharing IDs, authentication dialogs, ambiguous groups, partial batches, delayed/no window, placement/layout failures, Follow transitions, and Pinned close/crash/restart never produce a watchdog or duplicate prompt.
- Boundary tests: untrusted same-UID requests, unknown fields, attacker-supplied action/argv/environment/principal, and stale context tokens cannot convert owner-only sockets into a privileged gateway.
- Separately authorized integration evidence: disposable state/identities and compositor, workspaces 98+, selected packaged upstream helper/action/agent versions, Wayland/XWayland outcomes, cache behavior, cancel/timeouts, and shutdown recovery. No live user identity/configuration or real administrative work.
- For eventual code changes, run `make verify`, required code review, and applicable behavioral/security validation; report unrun evidence honestly. This report performs none of those implementation gates. [required checks][repo-rules]

[reject]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/desktop_launcher.go#L29
[approval]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/desktop_launcher.go#L99
[revalidate]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/desktop_launcher.go#L338
[launch-spec]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/desktop_launcher.go#L304
[launcher-tests]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/desktop_launcher_test.go#L16
[app-command]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/cmd/sway-session/app_commands.go#L53
[app-apply]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/cmd/sway-session/app_commands.go#L650
[app-operation]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/application_operation.go#L24
[cli-tests]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/cmd/sway-session/app_commands_test.go#L501
[restore-policy]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/restore_policy.go#L11
[policy-tests]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/restore_policy_test.go#L5
[starter]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/launcher.go#L24
[coordinator]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/application_restore.go#L299
[pinned-test]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/application_restore_test.go#L573
[groups]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/application_restore.go#L460
[launch-confirm]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/cmd/sway-session/application_launch.go#L13
[report]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/session/restore_report.go#L76
[start-request]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/sessionrequest/protocol.go#L25
[peer-check]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/sessionrequest/server.go#L264
[agent-report]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/internal/agentreport/protocol.go#L30
[identity-limit]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/docs/sway-session-plan.md#L550
[launch-race]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/docs/sway-session-plan.md#L524
[repo-rules]: https://github.com/marang/sway-session/blob/e039d7db8d7e0d37ff9ba8d139b55bc638d50220/AGENTS.md#L19
[desktop-keys]: https://specifications.freedesktop.org/desktop-entry/1.5/recognized-keys.html
[desktop-exec]: https://specifications.freedesktop.org/desktop-entry/1.5/exec-variables.html
[desktop-dbus]: https://specifications.freedesktop.org/desktop-entry/1.5/dbus.html
[gio-launch]: https://docs.gtk.org/gio/method.AppInfo.launch.html
[polkit-policy]: https://github.com/polkit-org/polkit/blob/9e4894c969eecf26a3ba762f4f7a268aa0fb3e51/docs/man/polkit.xml
[pkexec]: https://github.com/polkit-org/polkit/blob/9e4894c969eecf26a3ba762f4f7a268aa0fb3e51/docs/man/pkexec.xml
[polkit-authority]: https://github.com/polkit-org/polkit/blob/9e4894c969eecf26a3ba762f4f7a268aa0fb3e51/src/polkit/polkitauthority.c
[authority-wire]: https://github.com/polkit-org/polkit/blob/9e4894c969eecf26a3ba762f4f7a268aa0fb3e51/data/org.freedesktop.PolicyKit1.Authority.xml
[agent-wire]: https://github.com/polkit-org/polkit/blob/9e4894c969eecf26a3ba762f4f7a268aa0fb3e51/data/org.freedesktop.PolicyKit1.AuthenticationAgent.xml
[polkit-process]: https://github.com/polkit-org/polkit/blob/9e4894c969eecf26a3ba762f4f7a268aa0fb3e51/src/polkit/polkitunixprocess.c
[polkit-apps]: https://polkit.pages.freedesktop.org/polkit/polkit-apps.html
[run0]: https://github.com/systemd/systemd/blob/v256/man/run0.xml
