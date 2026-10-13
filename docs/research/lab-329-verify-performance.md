# LAB-329 verification measurements

The gate keeps `go test -count=1 ./...`, `go test -race -count=1 ./...`,
formatting, vet, staticcheck, AppArmor, completions, packaging, standalone
boundaries, both Python harness suites, the `CGO_ENABLED=0` build and whitespace
checks. A successful cache restore reuses compiled code, never test results.
Local `make verify` remains the complete gate and runs sequentially by default.

## Baseline on GitHub-hosted Ubuntu

The successful baseline runs used Go 1.26.5. Timings below come from UTC command
timestamps in `gh run view RUN --log`; each row ends at the next command start.
The full-gate interval begins at the `make verify` step and ends at GoReleaser
setup. This includes small recipe/shell overhead. The main runs checked the
same commit, `e1f58f5e7f2c3fa6a279de7b5eb8bf7ade2acd40`.

| Baseline run | Full make verify | Ordinary tests | Race tests | Vet | Staticcheck |
| --- | ---: | ---: | ---: | ---: | ---: |
| [PR #93, 38438210314](https://github.com/marang/sway-session/actions/runs/38438210314) | 235.887 s | 64.012 s | 141.350 s | 4.822 s | 10.628 s |
| [Main, 38439014286](https://github.com/marang/sway-session/actions/runs/38439014286) | 306.900 s | 80.583 s | 185.310 s | 6.372 s | 13.803 s |
| [Same main commit, 38440367785](https://github.com/marang/sway-session/actions/runs/38440367785) | 308.850 s | 86.079 s | 181.434 s | 6.321 s | 14.363 s |

All three restored the same approximately 236 MB setup-go cache. At the end,
setup-go reported a primary-key hit and skipped saving the cache. That key used
the Go version and dependency hash, so source changes and additional compiled
race artifacts did not update the restored snapshot. Repeating the exact main
commit therefore did not provide a progressively warmed build cache.

## Cache and scheduling changes

The verifier disables setup-go's combined cache and manages three caches:

- Module downloads have an immutable key for the OS, architecture, Go toolchain,
  `go.mod` and `go.sum`. A miss saves downloads after the required checks succeed.
- Compiled ordinary and race artifacts share Go's native content-addressed
  cache. Each run and attempt has a fresh primary key; restoration selects the
  most recent cache with the same OS, architecture, toolchain and dependency
  prefix. A successful run saves the updated cache even if it restored a
  preceding successful run.
- The staticcheck executable has its own toolchain/version key. Its expensive
  `go install` runs only on a binary-cache miss.

CI invokes the same `make verify` target with two jobs. Its Go prerequisite lane
serializes ordinary tests, race tests, vet, staticcheck and the static build.
The second lane serializes the independent configuration/harness checks. This
bounds command concurrency and overlaps those checks with the longer Go lane.
GoReleaser's configuration check is pinned to v2.18.2, the version used to build
shared release archives.

For historical recovery targets, the workflow stages profiling and proof tools
from its immutable workflow source before checking out the application commit.
An older Makefile without the profiling contract runs its complete gate with a
single job. This preserves old release recovery and avoids accidentally running
its ordinary and race test commands concurrently.

## Measurement method

`scripts/verify-profile.py` writes a summary and raw events into a fresh directory
under the runner's temporary root. It returns the actual `make verify` exit code.
The per-command wrapper adds `-json` to the unchanged Go test command, retaining
`-count=1` and `-race`. It records command wall time, each package's start delay
and Go's package execution duration. A start delay includes compilation,
linking, implicit vet and scheduler waits; it is not presented as pure compiler
CPU time. Test binaries can run while other packages are still building, so
package execution times must not be added to infer command wall time.

The uploaded `verification-profile` artifact includes the actual Go toolchain,
checked-out commit, CPU count, run ID/attempt, per-check JSON and raw Go events.
The GitHub step summary reports the complete gate and each command. This makes
the first new-cache run and its same-commit rerun comparable without using test
result caching or adding a second diagnostic-only test execution.

The focused profiler tests verify exit-code propagation, failed-test diagnostic
preservation, retained race/uncached flags, bounded complete-gate invocation,
serial historical gates and rejection of stale profile output. A local Go 1.26.5 ordinary-test profile ran
all 13 packages successfully in 19.880 s; it is workstation evidence, not a
GitHub-runner speed comparison.

Cold and warm GitHub-runner profiles must be recorded after the integrated
branch is pushed. No speed claim is based on the local timing above.

## Metadata-only PR investigation

The AUR sync changes only PKGBUILD and .SRCINFO, but those files control source
identity, checksums, version stamping and installed package metadata. Keep the
complete canonical gate for these PRs. A branch-name or two-path filter alone
would not establish equivalent main verification and could turn mutable package
metadata into an unsupported test bypass. The new compiled cache reuses the
unchanged Go code while tests still execute afresh, and the packaging checks
continue to validate both synced files. No new special verification mode is
introduced for automatically generated PRs.

## Release verification/build counts

Before this change a tag performed make verify separately in GitHub publication
and AUR publication, in addition to successful main CI. Now a tag uses one shared
accepted verification: zero new full gates when exact compatible main evidence
is retained, or one fresh full gate otherwise. GitHub packages have one read-only
GoReleaser build and are passed unchanged to handoff and publication. A complete
matching published release can be restored without another package build. The
separate Arch source/PIE build and its package tests remain mandatory; they are
not counted as redundant binary packaging. Isolated validation dispatches skip
both publishers and demonstrate this graph without creating a public version.
