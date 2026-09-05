# Sway Session workflow conventions

Version: 1.0
Status: Active
Audience: contributors, reviewers, and coding agents

## Ownership

This document is canonical for Linear routing, branches, pull requests, CI,
reviews, releases, and cleanup.

- AGENTS.md owns non-negotiable engineering and safety rules.
- docs/sway-session-plan.md owns durable architecture and compatibility.
- docs/sway-session-verification.md owns automated and live verification.
- docs/releasing.md owns release publication and package metadata.
- Linear owns active sequencing and executable follow-ups.
- Pull requests, CI, tests, and Git history own implementation evidence.

Do not leave an important decision only in chat or a Linear comment.

## Linear routing

Work is coordinated in the shared Linear team Lab under key LAB. Every issue
for this repository must:

1. belong to the
   [Sway Session project](https://linear.app/riotbox/project/sway-session-74dd95a8e064);
2. carry the mutually exclusive Codebase → Sway Session label; and
3. describe one executable, reviewable slice with acceptance criteria.

Additional labels such as Feature, Bug, Improvement, workflow, or
review-followup may describe an orthogonal dimension. Team membership alone is
not sufficient routing.

Move one issue to In Progress before implementation and keep the near-term
backlog bounded and honest. Existing session follow-ups include LAB-89,
LAB-92, LAB-93, LAB-94, LAB-95, and LAB-116.

## Delivery loop

Normal work follows:

Linear issue → In Progress → current main → focused LAB branch → implementation
and tests → local review → commit and push → PR and In Review → CI and review →
merge → Done → sync main → cleanup.

1. Select exactly one correctly routed issue.
2. Start from current main and inspect the worktree.
3. Create a narrow branch containing the issue key, such as
   feature/lab-123-short-name or fix/lab-123-short-name.
4. Implement one coherent slice with tests and lasting documentation.
5. Run the smallest checks while iterating, then make verify.
6. Review the complete diff and run the code-review skill.
7. Commit with a concise English Conventional Commit subject and push.
8. Open a ready PR against main, link the Linear issue, and move it to In
   Review.
9. Inspect every CI job and every review surface for the current head.
10. Address in-scope findings and repeat affected checks after every push.
11. Merge only when the gate below passes.
12. Move the issue to Done, sync local main by fast-forward, and clean up the
    branch when it is no longer needed.

Draft PRs are for deliberately incomplete collaboration. Their issue remains In
Progress until local implementation, verification, and review are complete.

## Local validation

The shared local/CI gate is:

~~~sh
make verify
~~~

It runs formatting checks, uncached unit and race tests, vet, staticcheck,
CGO-disabled build, AppArmor checks, completion checks, packaging assertions,
the standalone package/source boundary, and whitespace checks. CI also validates
the GoReleaser configuration.

Use docs/sway-session-verification.md to select additional evidence:

- storage or migration work: disposable XDG roots and SQLite checks;
- Sway or terminal lifecycle work: private compositor and workspace 98+;
- desktop application work: disposable identity and high workspace;
- mark-wire changes: golden test and byte comparison with sway-title-animator;
- security work: static AppArmor plus explicit live boundary result;
- release or packaging work: GoReleaser snapshot, local-tarball Arch build, and
  package ownership transition.

Never claim a real Sway, Herdr, AppArmor, or package-manager check that did not
run. Never test against live user session state.

## Pull request content

A PR description states:

- user-visible behavior and why the slice exists;
- important Sway, terminal, state, protocol, security, and compatibility
  consequences;
- fallback and unsupported behavior;
- local automated and relevant live evidence;
- packaging/release consequences when applicable;
- intentionally deferred evidence or follow-ups; and
- the associated Linear issue.

CI status and review status are separate gates. Inspect conversation comments,
submitted reviews, inline threads and resolution state, requested reviewers,
and the overall decision. A review of an older head is stale after a push.

Classify findings as an in-scope fix, verified out-of-scope follow-up, stale or
duplicate observation, or an explicit trade-off. Do not silently ignore a
valid finding.

## Merge gate

Merge only when:

- the diff still represents one coherent Linear issue;
- make verify passes locally;
- required CI passes for the current head;
- required review is complete and actionable threads are resolved;
- public behavior, paths, protocols, packaging, and durable docs agree;
- no secret, private state, pane history, socket, generated binary, package, or
  transient log is included; and
- deferred work is explicitly bounded in Linear.

Release branches additionally satisfy docs/releasing.md before an immutable
tag is created.

## Review cadence

- Run code-review for every substantive branch before PR handoff.
- Run review-codebase after every fifth substantive feature/fix branch or at a
  meaningful project checkpoint, whichever comes first.
- Documentation-only and mechanical maintenance do not advance the counter
  unless they alter architecture or product contracts.
- Verify old findings against current main and existing Linear issues before
  creating another follow-up.

## Parallel work and safety

When multiple agents or branches are active, use separate branches or
worktrees. Establish file ownership before edits, inspect likely overlap, and
re-review conflicts instead of overwriting another contributor.

The sway-session and sway-title-animator repositories intentionally duplicate
only the minimal Sway IPC and title-indicator implementation needed for
independent binaries. Changes to the v1 title-indicator wire fixture are
coordinated and compared across both repositories. Do not introduce a third
runtime or module dependency for this contract.

Never commit credentials, GitHub or Linear tokens, Sway socket paths, private
desktop identity snapshots, database contents, pane history, or terminal
output. Keep diagnostic logs and local release artifacts outside the
repository.

Real compositor work must use isolated roots and workspace 98 or higher.
Inspect focus before every move or close. Delete only the test contexts,
windows, processes, and roots created by the current procedure.

## Release workflow

Standalone releases begin at v0.1.0. Preserve the imported source history, but
do not push old sway-title-animator tags to the new remote.

PKGBUILD and .SRCINFO are source-build metadata. Sway is the sole runtime
dependency; Go is the sole build dependency. Do not add optional dependency
metadata for runtime-discovered integrations.

Before the first tag exists, SKIP is explicit bootstrap state rather than a
fabricated checksum. The release workflow must replace it from the immutable
tag archive and refuse publication if SKIP remains.

The old combined package may own /usr/bin/sway-session. Validate the split with
real packages: upgrade the animator package that removes the file before
installing sway-session, or install both replacement packages in one
package-manager transaction. Never use --overwrite.
