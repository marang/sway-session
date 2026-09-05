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
the hook client. No socket override or command arguments are accepted.

Integrations must translate their native hook event to these two fields; this
command does not install or infer provider hooks. The existing Codex command
continues to parse its native SessionStart event and check its UUID and
CODEX_THREAD_ID agreement.

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

## Compatibility and limitations

LAB-122 adds protocol v2 on a new canonical socket. The legacy
`codex-report.sock` keeps its v1 request/response shape and the installed
`report-codex-session` hook remains supported. Both use the same transport and
normalized service rather than independent implementations. Existing hooks
need no changes. Stored state and configuration require no migration.

The AppArmor template includes both endpoint paths. It remains experimental:
pathname socket-connect mediation and unconfined broker-created panes have the
limitations described in the README. Supporting more agent kinds is not a claim
that those agents are sandboxed. This change does not install or reload the
host's AppArmor policy.
