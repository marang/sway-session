---
status: accepted
---

# Restore administrative starters through normal registration

On 2026-10-16 the user authorized LAB-94 to restore supported administrative
desktop applications with their normal system authentication. LAB-383 corrects
the initial implementation: ordinary registration and reapproval, including
`--yes`, review the supported launcher without a separate administrative flag
or permission step. The existing preview explains possible authentication.
Follow and Pinned then restore eligible work, including at next login, without
an extra sway-session launch confirmation.

This supersedes the per-launch consent and login-deferral proposal in the
[historical audit](https://github.com/marang/sway-session/blob/main/docs/research/privileged-desktop-restore-gate.md).
Policy, cache and the session authentication agent remain upstream
responsibilities. Registration records launch identity, not an authorization
result.

Support is bounded to direct pkexec/sudo desktop starters with a root-owned
helper and direct root-owned payload. Approval pins the complete desktop bytes
and both executable identities, including for system entries; changes require
ordinary reapproval. Pending approval also binds that identity.
The existing persistent daemon starts the pinned helper directly so its caller
can remain available during authentication; ordinary desktop launch retains GIO.
Bare helper names explicitly select their canonical `/usr/bin` paths. Arguments
come only from approved desktop bytes through a bounded field-code expansion,
with no file/URI inputs. No command language, arbitrary argv interface,
credential handling, authentication telemetry, new broker capability, askpass
injection or global policy modification is introduced.
Process-start acceptance cannot prove authentication or cancellation. An accepted or
uncertain attempt keeps its compositor-scoped duplicate-launch guard through
daemon restart; a newly started compositor may try again under the saved policy.
A daemon exit can cancel pending pkexec authentication; uninterrupted
authentication or survival of already started work is not a generic guarantee.
Possibly accepted work is never killed or rolled back to enforce a timeout.

The optional strict context and approval-token fields retain existing schema
numbers and ordinary payloads. Older readers reject administrative registrations,
even when archived. Before downgrade, use the newer binary to forget those
registrations or restore a private pre-feature backup; do not mix writer
versions. [The plan](../sway-session-plan.md#administrative-desktop-starters)
owns the supported syntax and lifecycle limits; [verification](../sway-session-verification.md#administrative-desktop-restore-acceptance)
owns candidate evidence requirements.
