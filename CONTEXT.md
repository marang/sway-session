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
