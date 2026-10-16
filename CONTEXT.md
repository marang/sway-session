# Sway Session

Sway Session keeps explicit work contexts available across desktop sessions.
Desktop application inventory explains saved intent and observed restore evidence.

## Language

**Persistent context**:
A registered work context with a stable identity and saved lifecycle. A desktop
application context remains registered when its windows are absent.

**Desired open**:
The saved intention to keep a desktop application available for automatic restore.
It is independent of whether a window is currently present.
_Avoid_: Open, running

**Live presence**:
The observed application window group: absent, present with a unique anchor, or
ambiguous. Unknown means that current window evidence is unavailable.
_Avoid_: Desired open, closed

**Pinned restore policy**:
A policy that retains desired-open intent through ordinary window absence.
Explicitly closing the saved intent or archiving the context disables eligibility.

**Next-login restore eligibility**:
Permission from the saved lifecycle and restore policy to attempt automatic
restore. It is independent of launcher validity and completion evidence.
_Avoid_: Will launch, restored

**Launch guard**:
A recorded accepted or uncertain launch attempt that prevents blind duplicate
launches within a compositor lifetime. It is not proof of a pending process.

**Restore outcome**:
The recorded result and separate launch, mapping, placement, and layout proofs
for a restore attempt.
_Avoid_: Window presence

**Pending restore**:
A restore outcome whose requested work has not yet reached a final result.
It does not establish whether a process is currently running.

**Placement-only restore**:
A saved workspace arrangement that retains application placement without a
recoverable exact layout.

### Administrative applications

**Reviewed administrative launcher identity**:
The desktop entry, system helper and application payload reviewed during normal
registration or reapproval. The saved identity is independent of authentication
and authorization by the application's privilege mechanism.
_Avoid_: Password grant, permanent authorization

**Administrative starter**:
A desktop launcher whose supported privilege helper starts a separately
identified application payload. Its registration does not identify an
authentication dialog as an application window.

### Work organization

**Work environment**:
A user-named, nonowning collection of persistent contexts. Its members may also
belong to other work environments.
_Avoid_: Workspace, cloned session

**Primary member**:
The explicitly chosen context through which a work environment is reached.

**Reach**:
The intent to focus a work environment's explicit primary in place while
preserving existing arrangement and lifecycle intent. Missing members require
proven focus safety before they may be opened by Reach.
_Avoid_: Restore layout

### Planned arrangements

**Last-used arrangement**:
The most recently observed outer arrangement of work contexts.

**Named layout**:
A deliberately saved outer arrangement, independent of later observations.

**Template**:
A description of the initial structure of new work, independent of existing
live sessions and their contents.

### Remote work

**Remote binding**:
The explicit association of a persistent terminal context with an SSH target and
a remote Herdr session. A saved-machine binding additionally identifies the
Herdr profile whose target and session were selected.
_Avoid_: Machine label, local session name
