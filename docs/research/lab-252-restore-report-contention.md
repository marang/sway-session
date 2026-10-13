# LAB-252 restore-report contention evidence

Restore-report writes use the existing owner-only SQLite state store with short
`BEGIN IMMEDIATE` transactions, a 250 ms normal busy timeout and earlier caller
cancellation. The accepted SQLite ADR makes bounded contention visible to
callers; it does not promise that every racing call waits until success.
`IsStateDatabaseBusy` recognizes the public sentinel and wrapped SQLite
BUSY/LOCKED errors. An uncertain COMMIT is a distinct outcome requiring fresh
observation, even when its underlying cause also looks busy.

## Historical observations and bounded reproduction

The original [race CI failure](https://github.com/marang/sway-session/actions/runs/37134426063)
rejected `TestRestoreReportConcurrentReplacementRejectsOldWriter` with
`begin state database transaction: database is locked (5) (SQLITE_BUSY)`.
That test required both physical calls to succeed before comparing the complete
replacement record. Its unconditional success requirement exceeded the
bounded-contention contract; the error alone did not demonstrate corruption.

Preflight on `bf0a38730a05b7812ae09c96cec605e65fa43c87` used Go 1.26.5,
GOMAXPROCS=2 and uncached race tests. Restore-report runtime code is unchanged
between that source and merged main `94fb517295c8eac17d3b37c20faa242ed85f66d6`.

| Observation | Result |
| --- | --- |
| Original replacement race | 20/20 passed; package 2.294 s |
| Same race with one bounded CPU worker sharing one CPU | 100/100 passed; fixture 12.503 s |
| Original 65-context batching integration | 3/3 passed; package 4.446 s |
| Same batching test with the bounded CPU worker | 3/3 passed; individual tests 2.38, 2.31 and 2.37 s |
| Deliberately held private writer, original replacement success assertions | Failed 3/3 with the same BEGIN SQLITE_BUSY error; about 0.44 s each |
| Held-writer public Begin/Update probe with a bounded 2 s caller context | Passed 3/3: both calls returned known busy around 260 ms and left the original row unchanged; release permitted replacement, then stale-token Update returned false |
| Existing deadline/rollback test | Passed 3/3; about 0.11 s each |

The deliberate third writer establishes the permitted contention boundary,
not the historical two-writer scheduling trigger. The original trigger remains
unreproduced after 120 focused repetitions. The separate release-validation
batching observation also remains unreproduced after six focused repetitions.
These results do not establish a kernel cause, production starvation, disk
failure or corruption. No production wait, deadline or batching assertion was
increased.

Regenerating the same 400 ms held-writer fixture from the corrected concurrency
test then passed 3/3 under the same race/CPU/toolchain settings (0.56, 0.55 and
0.55 s; package 2.676 s). Both logical operations and the complete replacement
comparison remain in that fixture. The committed contention/uncertain-COMMIT
cases passed three race repetitions (package 2.817 s); the unchanged batching
integration and new injected-yield guard together passed three repetitions
(package 16.090 s). These were the first revision's local results, before the
runner exposed the additional test timing assumption below.

## Scoped contract and regression changes

Concurrent proof/replacement tests now replay only known rejected busy writes,
using the same immutable token and input, at most eight attempts and within a
2 s test-only context. Both logical operations must still finish, all concurrent
proofs must remain, and the complete final replacement must equal the newer
record. Ordinary storage failures, cancellation/deadline errors and uncertain
COMMIT outcomes stop immediately. Production operations gain no retry policy.

The committed held-writer fixture waits for both actual public operations to
report busy, verifies that neither changed the original row, releases the
private writer, and requires successful reconciliation plus rejection of a
subsequent old-token update. It coordinates release through observed errors,
without assuming a fixed scheduling sleep.

A real commit-acknowledgement-loss fixture commits the update and then returns
an error. It requires the uncertain-outcome classification, exactly one public
operation and a visible committed row. This protects against repeating work
whose COMMIT outcome is uncertain. Additional policy cases retain cancellation
and ordinary errors and enforce the attempt bound.

The original real-store 65-context batching test retains its 250 ms contexts,
first-pass count bound and ten subsequent passes. Separate injected-I/O-yield
coverage commits one actual pending-token CAS per fresh observation, then
rejects the next update with an injected deadline. It checks every durable
completion, unchanged failed token, retained cursor and exact next-pass
resumption until all 65 contexts finish. This proves cursor/resumption behavior
under controlled I/O rejection, not the original wall-time trigger. It uses
Go's [controlled test clock](https://go.dev/pkg/testing/synctest/) for this
semantic scenario, retaining real public SQLite writes while elapsed filesystem
or scheduling latency cannot replace the chosen injected yield. The original
batching integration runs outside that clock and retains real-deadline coverage.

## Runner-observed test timing correction

[Initial PR CI](https://github.com/marang/sway-session/actions/runs/38459898727)
at `cd372fbf2ca8b97233d03ff26c8f126232b406c3` rejected the new injected-yield
test on pass 27. It had committed the 27th outcome successfully; then the real
250 ms context expired before the intended second updater callback. The observer
correctly yielded with no error, but the test incorrectly required that callback
to occur. This was a timing assumption in the newly added test, not evidence of
lost progress or reproduction of the older batch-starvation symptom.

A temporary overlay inserted one 300 ms syscall pause immediately after the
first successful public update, outside its SQLite transaction. The same
one-callback failure then reproduced locally. Wrapping only the semantic test
in `testing/synctest.Test` removed that dependence without changing the updater,
cursor, token or durable-state assertions, deadlines or production code. The
corrected focused race test passed three repetitions (package 12.760 s); the
same physical-pause overlay also passed three (package 14.856 s). Both
independent reviewers checked this delta. The actual busy-wait fixtures and
original batching integration continue to use real time.
The complete Go 1.26.5 `make verify` gate passed again after this correction;
independent delta reviews retained no findings.

## Focused commands

~~~sh
GOTOOLCHAIN=go1.26.5 GOMAXPROCS=2 GOFLAGS=-p=1 go test -race ./internal/session -run '^TestRestoreReportConcurrentReplacementRejectsOldWriter$' -count=20 -timeout=45s
GOTOOLCHAIN=go1.26.5 go test -race ./internal/session -run '^TestRestoreReport(Concurrent|ReplacementReconciles|TestReplay)' -count=3 -timeout=30s
GOTOOLCHAIN=go1.26.5 go test -race ./cmd/sway-session -run '^(TestRestoreReportBatchesAllContextsWithoutTotalLimit|TestRestoreReport.*Yield)' -count=3 -timeout=45s
~~~

All fixtures use disposable private state. Preflight scripts, overlays and raw
sanitized logs remain outside the checkout; no production state, compositor,
registry contents or terminal history is included in this record.
