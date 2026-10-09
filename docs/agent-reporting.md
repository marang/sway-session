# Agent-session reporting

The reporting broker associates an agent's own session ID with a registered
terminal context and a Herdr pane. Herdr owns agent startup and resume state;
sway-session validates and forwards one association, without implementing a
second agent manager.

## Hook interface

Run `sway-session report-agent-session` in the reporting agent's managed pane.
Supply one JSON object on stdin with `agent`, `agent_session_id`, and optional
`event_origin`. No other fields are accepted.
The agent kind must be supported by the typed Herdr adapter. The session ID is
an opaque token of 1–512 ASCII bytes, not necessarily a UUID: it starts with a
letter or digit and contains only letters, digits, `.`, `_`, `:`, and `-`.
It must be the actual session
identity from the agent, never a guessed value or command line.

The command derives context and pane identities from `SWAY_SESSION_CONTEXT_ID`
and `HERDR_PANE_ID`, and requires `HERDR_ENV=1`. A syntactically valid payload
outside Herdr is a silent no-op. In Herdr, missing context or pane identities
are errors. Invalid managed reports fail with the existing structured
CLI diagnostic envelope and exit code 3. No registry or Herdr state is read by
the hook client. The generic mode accepts no socket override or arguments.

`event_origin` is the actual native event-origin token, such as `resume` or
`clear`. It is optional and bounded to 64 ASCII characters: a lowercase letter
followed by lowercase letters, digits, underscores or hyphens. Empty or absent
means no origin; it never grants replacement authority. Unknown well-formed
origins pass through unchanged so Herdr can apply its own provider rules. Do
not substitute `startup`, normalize an unknown event, or infer an origin.

Integrations must translate their native hook event to these fields; this
command does not install or infer provider hooks. For Codex, use
`sway-session report-agent-session --codex-hook` as the SessionStart hook
command. This CLI mode accepts Codex's native JSON on stdin, checks its UUID
and CODEX_THREAD_ID agreement, and translates native `source` to generic
`event_origin` without changing it, then uses the generic report path. It ignores
unrelated events, calls outside Herdr, and calls with a visibly stale
HERDR_SOCKET_PATH. If AppArmor hides the socket path, the broker still verifies
the reporting process and pane association.
Provider parsing is not part of the daemon or wire protocol.

## Wire and backend boundary

The canonical endpoint is `$XDG_RUNTIME_DIR/sway-session/agent-report.sock`.
Its version-3 newline-delimited JSON request contains `version`, `context_id`,
`pane_id`, `agent`, `agent_session_id`, and optional `event_origin`. Peer PID comes from Unix credentials,
never from a request field. The daemon validates a registered Herdr context,
queries the selected pane's shell process and checks the reporter's process
ancestry before calling Herdr's fixed `pane.report_agent_session` operation.
The Herdr ownership `source` field is constructed by the adapter, not supplied
by callers. The native event origin becomes Herdr's optional
`session_start_source`; these two source fields have different meanings.

After Herdr replies `ok`, the adapter reads one bounded `session.snapshot`
and requires the exact selected pane, agent, ownership source, ID kind and
requested value. An ignored or conflicting report is a failure, even if Herdr
acknowledges it. A missing snapshot association schema also fails with an
upgrade diagnostic; Herdr 0.9.1 exposes this schema. This confirms the live
association at observation time, not a durable disk flush or every later TUI
selection. Snapshot responses remain capped at 64 KiB and share the report
deadline; an oversized snapshot fails verification. Herdr owns persistence
and resume planning.

The socket is owner-only, messages and concurrent work are bounded, and calls
have deadlines. Replies contain protocol status only, never registry contents,
transcripts, or general Herdr responses. No Sway or Herdr operation runs inside
a SQLite transaction.

## Upgrade the Codex hook

LAB-125 intentionally removes `report-codex-session`, `codex-report.sock`, and
the protocol-v1 report adapter. LAB-138 adds reporting protocol v3; the new
broker still accepts the original source-less v2 request shape and replies in
v2 to those clients. V2 reports cannot carry a nonempty `event_origin`. A new
CLI always sends v3, even when origin is absent. It never retries using v2 or
drops metadata to reach an old broker: a v2 response produces an explicit
matching-package/daemon-restart diagnostic. Upgrade the CLI and daemon together
and restart the running daemon before using the new hook. Broker rejections
remain redacted; the daemon diagnostics contain the reason. Session-start
protocol v1, socket names, stored state and configuration require no migration.

1. Install the updated sway-session package. The hook needs no `jq` or
   separate shell adapter.
2. Replace only the old sway-session SessionStart command in your Codex hook
   configuration with the command from
   `/usr/share/doc/sway-session/contrib/codex/hooks.json`. Preserve unrelated
   hooks. It invokes `/usr/bin/sway-session report-agent-session --codex-hook`
   directly.
3. For a source installation, `contrib/codex/hooks.json` targets the default
   user-local binary path. Adjust that path for another prefix.
4. If using AppArmor, review and load the updated policy template with
   `agent-report.sock` access. The hook does not install or reload policy.
5. Restart the sway-session daemon so it serves the new report protocol. Restart or
   resume the relevant Codex process so its new SessionStart hook is used;
   already running agents do not retroactively emit that event.

The CLI mode reads at most 16 KiB of provider input, rejects malformed or
mismatched identities, and reports the actual session ID and supplied event origin. It never reads
the registry, starts an agent, or accepts a destination socket. Keep Codex's
normal hook timeout; no hook should wait indefinitely for a broker.

## Named agent workspaces

From a terminal inside Sway, ask the running daemon to open a named
Codex-and-shell workspace:

```sh
sway-session request-start --session my-project --cwd "$PWD" --label "My project" --workspace 5
```

Choose a stable session name for each work context. Repeating the request opens
or focuses that context without restarting its agent or replacing occupied
panes. Different contexts can share a workspace. An existing context must
already belong to the requested workspace; use Sway controls to move it first.
If creation or initialization fails, resolve the cause and retry the same
request. Changing the name can create a duplicate context.

## Security limitations

The optional `agent-home-guard` AppArmor template includes session-start and
agent-report paths. Its ready-to-use attachment is Codex; another agent needs a
separate copied profile with its executable attachment changed. It remains
experimental: pathname socket-connect mediation depends on kernel support, and
broker-created terminals and agent panes are unconfined. Supporting more agent kinds
is not a claim that those agents are sandboxed. This change does not install or
reload the host's AppArmor policy.

The template restricts direct access to private Herdr history, sway-session
state, credential stores, shell histories and browser profiles. It permits
the two sway-session broker paths. Review the template before enabling it.
For the packaged Codex example:

```sh
sudo install -m 0644 /usr/share/doc/sway-session/contrib/apparmor/agent-home-guard /etc/apparmor.d/agent-home-guard
sudo apparmor_parser -r /etc/apparmor.d/agent-home-guard
```

For another agent, copy the template, choose a unique profile name and update
the executable attachment before loading it. Source installations ship the
template under `~/.local/share/doc/sway-session/contrib/apparmor/`.
The packaged `verify-codex-boundary.sh` checks the supplied Codex hook and
requires the package-owned `/usr/bin/sway-session` executable.

## Integration acceptance

Routine tests cover native-origin translation, v2/v3 compatibility, real
credential/ancestry checks and a stateful fake Herdr that acknowledges ignored
replacements. Opt-in real Herdr acceptance uses disposable configuration/state
roots and a synthetic agent, without OpenAI API calls or productive sessions:

```sh
SWAY_SESSION_HERDR_INTEGRATION=1 GOTOOLCHAIN=go1.26.5 go test ./internal/agentreport -run TestHerdrLive -count=1 -v
```

Keep the shared Codex daemon disabled in the currently accepted workstation
workflow. Forwarding event origin does not fix daemon-inherited pane identity
(LAB-194), expose arbitrary TUI selection changes, or expand Herdr's accepted
origins. In particular, Codex `fork` is not a replacement origin in Herdr 0.9.1.
Test the broker alone before consolidating a user's existing native hook.
