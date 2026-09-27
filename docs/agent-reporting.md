# Agent-session reporting

The reporting broker associates an agent's own session ID with a registered
terminal context and a Herdr pane. Herdr owns agent startup and resume state;
sway-session validates and forwards one association, without implementing a
second agent manager.

## Hook interface

Run `sway-session report-agent-session` in the reporting agent's managed pane.
Supply one JSON object on stdin with exactly `agent` and `agent_session_id`.
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

Integrations must translate their native hook event to these two fields; this
command does not install or infer provider hooks. For Codex, use
`sway-session report-agent-session --codex-hook` as the SessionStart hook
command. This CLI mode accepts Codex's native JSON on stdin, checks its UUID
and CODEX_THREAD_ID agreement, then uses the generic report path. It ignores
unrelated events, calls outside Herdr, and calls with a visibly stale
HERDR_SOCKET_PATH. If AppArmor hides the socket path, the broker still verifies
the reporting process and pane association.
Provider parsing is not part of the daemon or wire protocol.

## Wire and backend boundary

The canonical endpoint is `$XDG_RUNTIME_DIR/sway-session/agent-report.sock`.
Its version-2 newline-delimited JSON request contains `version`, `context_id`,
`pane_id`, `agent`, and `agent_session_id`. Peer PID comes from Unix credentials,
never from a request field. The daemon validates a registered Herdr context,
queries the selected pane's shell process and checks the reporter's process
ancestry before calling Herdr's fixed `pane.report_agent_session` operation.
The source field is constructed by the adapter, not supplied by callers.

The socket is owner-only, messages and concurrent work are bounded, and calls
have deadlines. Replies contain protocol status only, never registry contents,
transcripts, or general Herdr responses. No Sway or Herdr operation runs inside
a SQLite transaction.

## Upgrade the Codex hook

LAB-125 intentionally removes `report-codex-session`, `codex-report.sock`, and
the protocol-v1 report adapter. The generic protocol v2 and session-start
protocol v1 are unchanged. Stored state and configuration require no migration.

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
5. Restart the sway-session daemon to retire the legacy listener. Restart or
   resume the relevant Codex process so its new SessionStart hook is used;
   already running agents do not retroactively emit that event.

The CLI mode reads at most 16 KiB of provider input, rejects malformed or
mismatched identities, and reports only the actual session ID. It never reads
the registry, starts an agent, or accepts a destination socket. Keep Codex's
normal hook timeout; no hook should wait indefinitely for a broker.

## Security limitations

The optional `agent-home-guard` AppArmor template includes session-start and
agent-report paths. Its ready-to-use attachment is Codex; another agent needs a
separate copied profile with its executable attachment changed. It remains
experimental: pathname socket-connect mediation and unconfined broker-created
panes have the limitations described in the README. Supporting more agent kinds
is not a claim that those agents are sandboxed. This change does not install or
reload the host's AppArmor policy.
