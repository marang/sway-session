# LAB-374 Herdr remote-session restoration

Assessed 2026-10-16 against sway-session `b4b240a8cd1b8442f89ff54c30127f31f646bf33`
and Herdr 0.9.5. This records existing behavior and a bounded integration gap;
it does not add a remote launcher or change session ownership. The user wants
preparation for future use of both saved Machines and direct remote attach;
neither is currently in use. No production remote setup is part of this work.

## Result

**Herdr saved Machines already reconnect through an ordinary sway-session
terminal, including after two real local-machine reboots.** No sway-session
transport implementation is needed for this shared Herdr workflow. The remote
server and its processes must remain alive, and SSH access must work again.

**A particular sway-session context is not bound to a particular remote
machine/session.** Herdr stores the desired selected machine globally under its
client XDG state root. Reopening context A after that selection changes can show
machine B. A still-open A can retain its own selection. This was reproduced with
two real frontends, not inferred only from the absence of a remote CLI flag.

For exact context-to-remote restoration, a separate design must choose an
explicit supported binding and observation contract. Do not copy Herdr's
credential store/catalog or change its global selection before every launch.
Remote health, remote process survival and the existence of a local managed
window are separate facts.

## Version and authority

The inspected source is the fresh official
[Herdr v0.9.5 release](https://github.com/herdrdev/herdr/releases/tag/v0.9.5),
commit [`2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01`](https://github.com/herdrdev/herdr/tree/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01).
The tested Linux x86_64 executable reported 0.9.5 and had SHA-256
`699e66a6134689bec0fd15f85c135479969fe4872332a8e08c5b7b5bdc9b059d` on both
sides. Its reported version and artifact hash are not a source-build attestation.
The private wire protocol is 22; endpoint generation is 1. Compatibility also
requires negotiated capabilities, not merely matching version text.
[Wire definition](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/protocol/wire.rs#L20),
[endpoint definition](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/protocol/endpoint.rs#L15),
[capability guard](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/client/endpoint/supervisor.rs#L295).

The links below pin implementation claims to that release. Current upstream
[connecting-machines guidance](https://herdr.dev/docs/connecting-machines/) is
useful operational documentation, but can change independently.

## Supported routes and identities

| Route | Identity and existing behavior | sway-session boundary |
| --- | --- | --- |
| Local frontend with saved Machines | Normal `herdr --session LOCAL_NAME` loads enabled saved SSH profiles and global desired selection. A profile has opaque ID, SSH target and remote session. Remote server owns PTYs/processes/state. | Existing adapter supports this ordinary frontend. Shared selection reconnects; no per-context remote identity is recorded. |
| Direct remote attach | `herdr --remote TARGET --session REMOTE_NAME` explicitly identifies target/session and creates a local UI bridge. Per-attach socket/PID is transient. | Existing adapter does not emit `--remote` or persist its target. An unrelated manually opened remote window is not automatically a managed context. |
| Saved-machine API | `herdr --machine PROFILE_ID api snapshot` routes to that enabled profile. Unique case-sensitive labels are also accepted. | Useful remote availability/structure check; not an interactive launcher or proof of a particular frontend's active selection. |
| Ordinary `ssh` inside a local pane | An arbitrary process owned by the local server, optionally attaching another remote Herdr. | Detaching from a surviving local server may preserve SSH. Restarting the local server does not generically recreate SSH/authentication/process state. |

[Remote arguments](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/remote/args.rs),
[profile resolution](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/cli/target.rs),
[saved bridges](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/remote/saved.rs).

Labels are presentation, not identity. Profiles can be renamed; distinct IDs can
refer to the same target/session. SSH aliases are not canonical physical-host
IDs. Do not guess between duplicate labels, aliases, profiles or remote panes.

### Why ordinary startup works

Normal local-client startup loads `EndpointCatalog`, starts supervisors for all
enabled SSH profiles and activates the desired profile after a coherent remote
snapshot arrives. Profile and selection files are separate:
`$XDG_STATE_HOME/herdr/client/endpoints.json` and
`client/endpoint-selection.json`. This selection is not keyed by local
`--session`. Catalog refresh preserves a running frontend's live selection;
a newly opened frontend loads the shared persisted selection.
[Client startup/activation](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/client/mod.rs#L235),
[catalog and selection](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/client/endpoint/catalog.rs#L101),
[reconnect supervisors](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/client/endpoint/supervisor.rs#L80).

Saved **UI** bridging starts a missing remote server through
`remote-client-bridge`. Saved **API** forwarding only connects to an existing
API socket. Neither background route installs a missing package or replaces an
incompatible installation; interactive setup has a separate approval flow.
Do not generalize API no-start behavior to UI reconnect.
[UI server startup](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/remote/host.rs#L6),
[saved bridge distinction](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/remote/saved.rs#L19),
[installed-binary discovery](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/remote/attach.rs#L1393).

## Actual isolated evidence

All live tests used private HOME/XDG/config/runtime roots, generated fixture
keys and pinned known-hosts, a private account database/home in an unprivileged
Bubblewrap SSH server, and genuine OpenSSH. No production endpoint, SSH agent,
user SSH configuration, credentials, registry or terminal history was read.
A private PATH wrapper selected an explicit `ssh -F` configuration; Herdr
`remote.manage_ssh_config=false` prevented generated managed config from
including host settings. This exercises unmanaged SSH, not production
ControlMaster/MFA behavior. Synthetic processes and markers were the only work.

The local adapter's required `pane_history=true` was enabled only in disposable
local roots; it was never read/exported. Remote agent auto-resume and update
checks were disabled. Owned processes, keys, state, logs and VM disks were
removed after each fixture. Rendering witnesses remained transient under
`/tmp`; this document contains derived results, not captured terminal output.

### Native Sway / SSH

Sway 1.12 and Alacritty 0.17.0 ran on a private compositor and workspace 98.
The actual candidate `register` and `terminal --context UUID` paths opened the
ordinary local named frontend. Both Herdr sides used 0.9.5.

| Probe | Observed result |
| --- | --- |
| Close/reopen the same exact managed context | New local window; same remote pane/terminal IDs and same remote synthetic process PID/start ticks. Remote marker visible before and after. |
| Two local named contexts with different saved selections | Existing A retained remote Alpha; newly opened B selected Beta. After changing the shared desired selection in fixture setup, reopening A selected Beta too. Both real frontends showed the corresponding markers. This was controlled selection-file seeding, not simulated user clicking. |
| Two concurrent Open calls for an already-open context | Managed window ID set remained unchanged; no duplicate local window. This does not establish concurrent first-launch or remote-server bootstrap behavior. |

The global-selection counterexample is a missing per-context contract, not a
remote connection failure. Each remote session had a distinct synthetic marker
and retained process identity. A label or successful API query alone would not
establish which remote work was actually displayed.

### Transport, authentication and compatibility failures

The two-frontend fixture then terminated only its recorded, identity-checked
SSH bridge processes. A fixture wrapper refused new transports; the remote API
failed and both UIs displayed Endpoint unavailable/reconnecting. Restoring only
the transport produced a newly changed synthetic remote marker in both UIs.
The original remote process PID/start ticks remained unchanged.

Separate negative probes replaced only the fixture client's pinned host key
with a wrong generated key, then its private identity with an unauthorized
generated key. In each case the explicit saved-machine API failed, while the
controller's separate trusted connection confirmed both remote processes were
unchanged. Restoring the original known-good files allowed normal supervisor
retry; both UIs eventually displayed a new post-repair marker. No changed host
key was accepted and no production credentials were involved. Failure-time
host-key/auth UI notices were not separately captured; the refusal evidence is
the actual CLI result, not a claim about every visible diagnostic.

An authentic incompatible peer was tested separately: the official
[Herdr 0.8.0 Linux x86_64 asset](https://github.com/herdrdev/herdr/releases/download/v0.8.0/herdr-linux-x86_64),
commit `346411fa21afd297f5ed3b3fa56f9e3fbf7654b7`, downloaded and verified before
execution against the [published asset digest](https://github.com/herdrdev/herdr/releases/expanded_assets/v0.8.0)
`b872ea7e40fa2cb17e857ac9b62b1bf26db7b403c622f5d2f3f5b35f6e9acd28`.
An actual private SSH remote client and running server reported 0.8.0, private
protocol 19, without endpoint generation. With local 0.9.5:

| Probe | Actual rejection |
| --- | --- |
| Saved-machine status | Exit 1, error at installed endpoint-capability discovery: matching Herdr is not ready; interactive install/update guidance |
| Saved-machine API snapshot | Exit 1 at remote API bridge discovery: machine API forwarding unsupported; no snapshot returned |

These were early capability rejections, **not an exercised private-wire
handshake mismatch**. The old executable's hash stayed unchanged after both
probes; the host 0.9.5 executable was unchanged too. No compositor or UI
selection was tested in that negative fixture.
[Old wire definition](https://github.com/herdrdev/herdr/blob/346411fa21afd297f5ed3b3fa56f9e3fbf7654b7/src/protocol/wire.rs#L15),
[discovery requirements](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/remote/attach.rs#L2176).

Recovery stays with Herdr/OpenSSH. `herdr machine reconnect PROFILE_ID` requires
an interactive terminal and, on this Linux release, managed SSH configuration.
The unmanaged fixture verified those refusals; it restored its own known-good
files and waited for background retry instead. It did not prove an MFA or
managed-authentication recovery flow. Source distinguishes network backoff
(initial 500 ms, capped at 120 seconds) from attention-required failures
(rechecked after 30 seconds). Do not promise every network recovery within
30 seconds or start another sway-session retry loop.
[Supervisor retry policy](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/client/endpoint/supervisor.rs#L214),
[interactive CLI guard](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/cli/machine.rs#L179),
[managed SSH guard](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/remote/attach.rs#L731).

### Two actual local-machine reboots

A fresh SHA-verified public Arch image used the repository's existing
[`verify-vm-reboot.py`](https://github.com/marang/sway-session/blob/b4b240a8cd1b8442f89ff54c30127f31f646bf33/scripts/verify-vm-reboot.py) KVM guest and unchanged
install/baseline/observe/reboot helpers. QMP reported KVM enabled; QEMU was
11.1.2. The clean stamped candidate was the sway-session commit above, binary
SHA-256 `a5e3c354d668496ed6a9b42511929a89ba75ac66c0f253154ad9f6aa66fedbb0`.
The base-image SHA-256 was
`360f0fa49db6813bdc8e35bed230a2dc2ae3567b7b5ab74719c0a706e4e34e87`, as pinned in
the repository VM manifest. Guest packages included Sway 1.12-4, Alacritty
0.17.0-1, OpenSSH 10.6p1-2 and systemd 262. Cloud-init updated the kernel from
7.2.7 at baseline to 7.2.9 after the first reboot; this was not a fixed OS image
throughout the run.

Two acknowledged guest systemd timers requested regular `systemctl reboot`.
Three distinct boot IDs and the original baseline were compared. Normal
getty/PAM/logind login started Sway, the candidate daemon and one-shot restore.
Two local contexts returned with the original ordered tabbed layout on 98,
no staging windows, the current daemon and its real delay inhibitor.

| Boundary | Remote server/process | Visible selected frontend |
| --- | --- | --- |
| Original baseline | Original PID/start ticks, pane and terminal identity set recorded | Fixture remote connected; original remote marker visible |
| Reboot 1, transport deliberately absent at login | Original server and process still alive | Local shell; remote row says reconnecting |
| Restore only SSH transport after reboot 1 | All original identities unchanged | Automatically returns to the original remote marker |
| Reboot 2, transport absent / then restored | All original identities unchanged in both phases | Same Local/reconnecting → remote transition |

Only the private SSH reverse-forward was re-established after each reboot;
no frontend relaunch, reselect, placement repair or extra restore command was
sent. Process start ticks prevented PID reuse from satisfying survival checks.
The executor and an independent root review inspected all five real synthetic
screenshots. A benign first-run welcome overlay did not obscure the marker or
connection state. This visually proves the selected visible frontend, not every
background tab. Local layout evidence excludes focus, geometry and proportions.

These were **local guest reboots with a surviving remote server**, not remote
OS reboots or remote process recreation. The one-off controller imported the
existing helpers; it is not a committed automated regression gate or a release
acceptance run.

## Observation and lifecycle boundaries

The compiled [terminal launcher](https://github.com/marang/sway-session/blob/b4b240a8cd1b8442f89ff54c30127f31f646bf33/internal/session/terminal_launcher.go)
uses the local working directory, context app-ID, `HERDR_CONFIG_PATH` and
`herdr --session LOCAL_NAME`. It removes inherited `HERDR_*` variables but
preserves the XDG root, allowing Herdr's saved-machine path. The
[context model](https://github.com/marang/sway-session/blob/b4b240a8cd1b8442f89ff54c30127f31f646bf33/internal/session/model.go) has no remote target/profile
binding. A remote working directory cannot be substituted into local-directory
validation.

[Herdr observation](https://github.com/marang/sway-session/blob/b4b240a8cd1b8442f89ff54c30127f31f646bf33/internal/session/herdr_observation.go) and
[session paths/deletion](https://github.com/marang/sway-session/blob/b4b240a8cd1b8442f89ff54c30127f31f646bf33/internal/session/herdr.go) operate on local named
sessions. A healthy local session/window is not remote readiness. Archiving and
normal close preserve Herdr work; local purge retains its existing local
session scope. There is no new authority to delete a remote server/profile or
initialize remote panes. No remote API calls may be added inside SQLite
transactions. See also [local lifecycle recovery](../lifecycle-recovery.md).

For diagnostics, `herdr machine status PROFILE_ID --json` checks the route now;
`herdr --machine PROFILE_ID api snapshot` checks remote structural availability.
Neither command proves an already-open frontend has activated that route.
`machine list --json` reports persisted desired selection, not a live UI
selection query. Raw snapshots include metadata such as CWD/title/agent fields;
evidence must whitelist structural IDs/counts rather than publish the JSON.
[Snapshot schema](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/api/schema/session.rs),
[machine CLI](https://github.com/herdrdev/herdr/blob/2ecf19a92fc3a7fff75b6ce3655ef04f4625ec01/src/cli/machine.rs).

At login while offline, a Local shell can be displayed even though the desired
remote profile is still pending. During an established connection loss the
fixture instead displayed Endpoint unavailable/reconnecting. Neither is a
remote-health success. Do not apply remote commands to Local as a fallback.

## Implementation decision and remaining scope

For Herdr-owned shared saved Machines: document and use existing reconnect.
There is no demonstrated need for another SSH launcher, credential schema or
sway-session reconnect supervisor. Herdr remains the authority for transport,
remote authentication, capabilities and server lifecycle.

For an exact remote machine/session per sway-session context:
[LAB-376](https://linear.app/riotbox/issue/LAB-376/design-explicit-herdr-remote-bindings-for-persistent-contexts)
is the bounded design follow-up covering explicit identity, supported attach/selection
mechanism, remote-aware observation, missing/disabled/changed profiles and
non-destructive local lifecycle. It must cover two hosts with identical remote
session names and concurrent frontends. Local CWD, remote CWD and pane-role
initialization need explicit contracts before any schema/launcher extension.
Changing shared global selection as an implicit launch side effect is not an
acceptable binding. A display label or current local socket must not become the
remote identity.

Direct remote attach was source-traced, not exercised by these saved-machine
live fixtures. No direct-remote managed adapter, remote OS reboot, remote server crash recovery,
agent conversation resume, arbitrary shell/CWD recreation, production SSH/MFA,
managed ControlMaster recovery, or per-context remote pane selection is claimed.
A remote-server restart may recover Herdr layout while starting different
processes; persistence of a surviving process is a separate property. Cancellation
here consists of owned fixture process teardown, not a tested interactive setup
cancel flow. These are explicit limits, not reasons to fabricate broad remote
infrastructure under this assessment.
