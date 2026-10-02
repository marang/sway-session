# Durable lifecycle operations

Application registration and rebind coordinate SQLite state with effects in
Sway; terminal purge coordinates it with Herdr. Those systems cannot participate
in one transaction. Each operation therefore records its exact intent before the
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
| Purge, forward | Recorded Herdr directory identity and native session listing | Stop a running named session or delete a stopped one, with fresh checks before each command | Remove the operation only after confirming absence; its context and activity were removed atomically with intent |
| Any operation, retryable failure | Fresh state after the retry deadline | Reconsider the next action; never replay an unobserved command blindly | Retain intent until completion |
| Any operation, conflict | Reused identity, changed context, ambiguous ownership, or incompatible evidence | Report the operation and a supported operator action | Retain the reservation; do not guess |

Application operations reserve their context identities and approved launchers.
Other lifecycle changes cannot silently overtake an unresolved operation.
Launcher cleanup retains files referenced by pending operations as well as by
registered contexts. Unrelated contexts remain independently usable.

Application forget holds the lifecycle lock through removal and any error
compensation, so a concurrent rebind cannot reserve a replacement between them.
Compensation rechecks the unchanged registration, pending reservations, compositor
lifetime, original window identity and mark ownership under the registry lock.
It restores only a missing mark on that same window; a closed window needs no
inverse command. Conflicting evidence leaves marks untouched and reports the
original error together with the compensation failure. Forget retains its
synchronous behavior and does not create a durable operation journal entry.

Purge reserves the original context ID, launcher and typed terminal identity
(when present). Recreating or restoring that identity cannot overtake pending
cleanup. The operation records the original Herdr root, so a changed environment
does not redirect retries to a different root.

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

## Terminal purge recovery and its boundary

Purge uses Herdr's native `session list`, `session stop` and `session delete`
commands. sway-session does not remove or quarantine Herdr files itself. The
operation begins under the lifecycle lock, compares the confirmed context with
the current registry and atomically saves intent while removing the context and
its activity. Database transactions remain separate from subprocess calls.

Foreground purge and operation retry/cancel commands hold the shared state
access gate across all their steps. Database recovery therefore cannot replace
their journal between intent, effects and final observation. The daemon already
holds this gate for its lifetime.

Each pass verifies that the Herdr root is owner-only and its child directories
belong to the user and deny write access to others. Native Herdr's `0755` children are accepted
inside that private root; symlinks and nested session mounts are refused.
Device/inode evidence, plus directory birth time where the filesystem
supports it, distinguishes an observed replacement from the authorized target.
Without birth time, device/inode evidence is weaker because inodes can be reused.
A replacement blocks the operation as a conflict; retry retains the original
evidence and never adopts the replacement. Socket absence alone does not prove
that a session can be deleted.

Only a bounded amount of work runs per pass. A refused stop or uncertain command
result leaves intent intact and starts retry backoff. A new pass observes the
session again rather than blindly replaying the last command. If deletion
succeeded but its acknowledgement or the final database commit was lost,
confirmed absence permits completion without another delete command. The daemon
resumes pending operations; `state operations --retry OPERATION_UUID` also works
without a Sway connection. Repeating `purge --yes CONTEXT_UUID` finds the same
pending intent even though the context has left the registry.

Purge cannot be cancelled or rolled back once its intent is recorded. An
interrupted native command may already have removed session data. Restoring the
old context would falsely suggest that its session is still recoverable. Pending
and conflicted purges remain listed with their retry command, without a rollback
suggestion. Applications still support the compensating cancellation above.

Herdr 0.9.2 exposes deletion by name, without a caller-supplied generation or a
lock spanning observation and deletion. The repeated checks detect replacements
that become visible between passes or before commands; they do not make native
deletion atomic with these checks. An independent start, server handoff or
delete/recreate can still race the final check and native command. This is the
explicitly accepted compatibility boundary for [LAB-208](https://linear.app/riotbox/issue/LAB-208),
not a guarantee of exclusion. A stronger guarantee would require an upstream
Herdr API change. No plugin or upstream modification is required by this
implementation. See the [Herdr investigation](research/herdr-plugin-session-deletion.md)
for the inspected interfaces and isolated identity evidence.
