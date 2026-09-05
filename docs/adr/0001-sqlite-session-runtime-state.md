# Store sway-session runtime state in SQLite

Status: accepted

## Context

`sway-session` originally persisted its context registry, layout, terminal
activity, and per-compositor application launch attempts as separate JSON
documents. Cross-document transitions required filesystem locks and
compensation, registry capacity was coupled to planner bounds through an
arbitrary 128-context ceiling, and independent CLI and daemon processes needed
a clearer concurrency boundary.

Configuration has different operational needs: it is human-authored, reviewed,
and deployed as strict text. Approved user-local desktop entries are immutable
source snapshots. Neither belongs in the runtime database.

Sway IPC, Herdr commands, process inspection, and application launches are
external effects. SQLite cannot make those effects atomic with durable state,
and holding a writer transaction across them would turn compositor or process
latency into database contention.

## Decision

Store persistent session runtime state in the owner-only
`${XDG_STATE_HOME:-$HOME/.local/state}/sway-session/state.sqlite3` database.
Database schema 1 contains registry metadata and preferences, versioned context
payloads, terminal activity, the captured layout, the compositor identity, and
application launch attempts. Use the pure-Go SQLite driver so all release
builds continue to support `CGO_ENABLED=0`.

Run SQLite in WAL mode with full synchronous commits, foreign keys, a
database-wide integrity check at daemon startup, cheap per-connection and
per-row validation, a 250 ms busy timeout, and context cancellation.
Serialize the one-time persistent PRAGMAs and schema creation with a separate
cross-process lock on the validated database inode. Bootstrap contenders close
their provisional SQLite connection before waiting, recheck the schema after
acquiring the lock, and wait for at most two seconds or their earlier caller
deadline. This longer bound applies only while creating an uninitialized
database; normal runtime contention retains the 250 ms limit.
Registry operations therefore open and, when necessary, initialize the
database before acquiring the registry lifecycle lock. Their registry read,
revision check, mutation or inspection, and commit remain inside that lock.
`state.sqlite3` and any pathname-reachable WAL, SHM, or rollback-journal
sidecar must be regular, single-link, current-user-owned files with mode
`0600`. A sidecar inode which SQLite unlinks after pathname lookup may be
observed with zero links; treat that transient state as absent, while the main
database must remain linked and every object with multiple links is rejected.
The Codex AppArmor profile denies the complete default `sway-session` state
root, which covers the database and its sidecars. SQLite's Unix VFS
canonicalizes its input path, so relocating or replacing the state root while
operations run is not a supported workflow. Unconfined same-UID processes are
trusted state owners and can already modify the owner-only database directly.
The supplied AppArmor profile additionally denies writes to the default `.local/` and
`.local/state/` directory objects, preventing a confined process from swapping
an ancestor around the protected state tree.
Reject a main database beyond the configured page budget before opening it, but
do not impose a pre-open size ceiling on WAL, SHM, or rollback-journal files. A
valid sidecar may temporarily exceed the database while a reader pins old frames
or recovery is pending after a crash; SQLite must be allowed to replay and
checkpoint it.

Keep transactions short and limited to database validation and mutation. Run
Sway, Herdr, process, desktop-catalog, and launcher operations outside database
transactions. Coordinate them with explicit observe/record/act/reconcile sagas,
durable intent before ambiguous launch, fresh observation after ambiguous IPC,
and compensation or later reconciliation after partial failure.

Do not impose a numeric limit on total stored contexts. Rotate bounded placement
and indicator action batches plus bounded application preflight across daemon
passes, and make layout restore re-observe after every mutation and yield after
bounded in-memory transitions. Resume remaining work from fresh Sway
observation; these operational guards are not storage-capacity limits.

Keep `sway-session` configuration in strict TOML text files and desktop
approval snapshots as separate immutable files. For this pre-1.0 transition,
recognized legacy JSON blocks ordinary database creation until the operator
explicitly presses `m` in `terminal manage`. That action strictly decodes every
legacy runtime document and imports the coherent result in the same transaction
that initializes database schema 1. The source locks remain held through the
commit; stale dependent runtime rows are skipped with explicit counts. It is
idempotent and leaves every source file byte-identical as a backup. This narrow transition action is not a general
schema-migration framework and may be removed after the transition release.

## Consequences

Related registry, activity, and launch-attempt changes can now commit atomically.
Registry revisions let the daemon reuse a validated snapshot until a writer
commits, and unchanged context rows are not rewritten. Application-session
revisions likewise protect prepared launch-attempt deltas, including foreign-key
cascades, so normal writes touch only changed rows inside `BEGIN IMMEDIATE`.
WAL permits ordinary
concurrent readers while short writes remain serialized and bounded contention
is visible to callers.

External behavior remains eventually reconciled rather than transactionally
atomic. Each saga therefore needs idempotent actions, explicit ambiguous-outcome
handling, and tests that prove no external call occurs inside a transaction.
The pre-existing hard-crash gaps around application mark/rebind and Herdr purge
need an additional durable operation/tombstone state machine and are tracked
separately in [LAB-116](https://linear.app/riotbox/issue/LAB-116/make-sway-mark-and-herdr-purge-sagas-crash-recoverable);
they are not silently claimed as solved by this storage migration.

Operators upgrading pre-1.0 state must explicitly choose migration; it is never
triggered by a daemon or ordinary command. WAL/SHM files become part
of permissions, backup, confinement, cleanup, and diagnostics even though they
may appear or disappear during normal database use.
