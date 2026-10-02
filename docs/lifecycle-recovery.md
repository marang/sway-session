# Durable lifecycle operations

Application registration and rebind coordinate SQLite state with effects in
Sway. Those systems cannot participate in one transaction. Each operation therefore records its exact intent before the
first external effect, observes the effect after ambiguous acknowledgements,
and commits completion separately.

The operation journal belongs in the private state database. Configuration,
approved launcher files, and Herdr session data keep their existing owners.
Database transactions contain no Sway, Herdr, process, or launcher calls.

## Transitions

| Operation and durable phase | Fresh observation | Next action | Durable completion |
| --- | --- | --- | --- |
| Register, forward | Exact selected windows and context marks | Add only missing marks to their captured targets | Insert the complete approved batch and remove its operation together |
| Rebind, forward | Original and replacement windows, mark ownership | Remove the original mark, then add it to the approved replacement, observing between effects | Replace the unchanged original context and remove its operation together |
| Register or rebind, rollback | Exact targets and any acknowledged or ambiguous marks | Restore the original mark arrangement without touching another identity | Keep the original registry state and remove the operation |
| Any operation, retryable failure | Fresh state after the retry deadline | Reconsider the next action; never replay an unobserved command blindly | Retain intent until completion |
| Any operation, conflict | Reused identity, changed context, ambiguous ownership, or incompatible evidence | Report the operation and a supported operator action | Retain the reservation; do not guess |

Application operations reserve their context identities and approved launchers.
Other lifecycle changes cannot silently overtake an unresolved operation.
Launcher cleanup retains files referenced by pending operations as well as by
registered contexts. Unrelated contexts remain independently usable.

## Identity and interruption

Window evidence combines the compositor socket lifetime identity with the Sway
container ID and the observed application/process identity. Sway allocates
container IDs monotonically within a compositor process; a socket lifetime
change invalidates that numbering domain. See [Sway node initialization](https://github.com/swaywm/sway/blob/master/sway/tree/node.c).

A matching application name alone is insufficient evidence. Unknown or
identity-drifted `persist:` marks are not automatically safe to remove. A fresh
observation precedes every effect, and a subsequent observation determines
whether an effect succeeded even when its IPC acknowledgement was lost.

Returned errors may start compensation only after the rollback phase is
durable. If recording that phase fails, the operation remains pending and the
caller receives an actionable error instead of performing an unrecorded
inverse action. Abrupt process death cannot discard durable intent.

Daemon reconciliation processes bounded work with continuation and retry
backoff. Pending contexts are excluded from ordinary placement and restore;
that filtered view is never persisted as a replacement registry. Explicit
retries select one operation rather than silently acting on unrelated work.

Suspended restoration retains its progress and cleanup ownership while other
workspaces continue. Fresh layout observations detect intervening user changes
before reconstruction resumes. Such changes cancel reconstruction, while
cleanup of the daemon's temporary staging remains available after the
reservation is released.

## Verification

Use disposable databases and persisted fake-effect fixtures for deterministic
process-death tests around intent, mark, unmark, rollback, and commit
boundaries. Cover lost acknowledgements, repeated restart, conflicting retries,
reused target identities and database contention. Real compositor tests use
isolated state and workspace 98 or higher.

## Operator recovery

`sway-session state operations` lists unfinished operations and their stable
IDs. It is read-only and does not retry an external action. The JSON form is
available through the existing `--json` option. A conflict retains its resource
reservations, even when the associated context is absent from the registry.

After resolving the reported cause, use
`sway-session state operations --retry OPERATION_UUID`. This releases the retry
backoff for that exact intent; it does not approve a different window.
For application registration or rebind,
`sway-session state operations --cancel OPERATION_UUID` requests durable
rollback to the captured mark arrangement. Cancellation is complete only after
fresh observations confirm compensation. It never removes an unrelated mark.

## Persistence and compatibility

The journal and resource reservations extend the existing private SQLite
schema. They do not alter context IDs, `persist:` marks, launcher forms, broker
protocols, or Herdr's ownership of session contents. A backup includes pending
operations and validates their reservations on recovery; restoring a backup
never replays external effects as part of its database transaction.

Do not run an older sway-session binary against a database with unfinished
operations. Older releases cannot interpret these reservations. Complete or
safely cancel application operations before a planned downgrade. A current
binary also refuses incompatible operation versions rather than treating them
as completed work.

## Deferred terminal purge recovery

Terminal purge keeps its existing behavior and does not use this journal yet.
[LAB-208](https://linear.app/riotbox/issue/LAB-208) owns durable purge recovery;
its parent LAB-116 remains open. The user chose this split so application
recovery can ship independently.

Herdr 0.9.2 does not expose a session-lifetime lock or conditional deletion
capability. During live handoff it removes public sockets while live pane
processes are transferred. Socket absence therefore cannot authorize deletion.
A native Herdr capability must exclude concurrent startup and handoff, verify
the exact session generation, and make cleanup retryable before sway-session
can safely implement this extension. No experimental purge adapter is included.
