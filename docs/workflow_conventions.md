# Sway Title Animator workflow conventions

Version: 1.0
Status: Active
Audience: contributors, reviewers, and coding agents

## 1. Purpose and ownership

This document is the canonical operational workflow for planning, branches,
pull requests, CI, reviews, merges, and cleanup.

Source-of-truth ownership is split deliberately:

- `AGENTS.md` defines non-negotiable engineering and safety rules.
- This document defines Linear coordination and the Git/GitHub loop.
- Repository plans such as `docs/sound-presets-plan.md` own durable technical
  and behavioral decisions.
- Linear owns active status, sequencing, and actionable follow-ups.
- Pull requests, CI, tests, and Git history own implementation and validation
  evidence.

Do not leave an important technical decision only in chat or a Linear comment.

## 2. Linear routing

Work is coordinated in the shared Linear team `Lab` under key `LAB`. Because
that team contains multiple codebases, team membership alone is not sufficient
routing.

Every issue for this repository must:

1. belong to the
   [Sway Title Animator P001 project](https://linear.app/riotbox/project/sway-title-animator-or-p001-or-sound-reactive-presets-e8a4308a9902);
2. carry the mutually exclusive `Codebase` → `Sway Title Animator` label; and
3. describe one executable, reviewable slice with acceptance criteria.

Use additional labels such as `Feature`, `Bug`, `Improvement`, `workflow`, or
`review-followup` only when they add orthogonal information.

The issue-first exception is limited to initial project creation and draft plan
shaping. Once a slice will change repository behavior or workflow, select or
create its issue and move it to `In Progress` before starting its branch.

Keep the near-term backlog small and honest. One active issue and roughly one
to five already-shaped follow-ups are preferable to a large speculative tree.

Current explicit follow-ups:

- [LAB-25: Investigate native PipeWire capture without CGO](https://linear.app/riotbox/issue/LAB-25/investigate-native-pipewire-capture-without-cgo)
- [LAB-26: Update the project to Go 1.26.5](https://linear.app/riotbox/issue/LAB-26/update-the-project-to-go-1265)
- [LAB-28: Expand the Bubble Tea preview into an interactive animation browser](https://linear.app/riotbox/issue/LAB-28/expand-the-bubble-tea-preview-into-an-interactive-animation-browser)

## 3. Default delivery loop

Normal work follows this loop:

`Linear issue → In Progress → current main → focused branch → implementation
and tests → local review → commit and push → draft PR → CI → ready PR and In
Review → review feedback → merge → Done → sync main → cleanup`

1. Select exactly one correctly routed Linear issue.
2. Move it to `In Progress`.
3. Start from current `main` and inspect the worktree before editing. Preserve
   unrelated user changes.
4. Create a narrow branch containing the issue key:
   `feature/lab-123-short-name`, `fix/lab-123-short-name`, or
   `docs/lab-123-short-name`.
5. Implement one coherent slice, including tests and lasting documentation.
6. Run `make verify` plus any relevant manual Sway/audio check.
7. Review the complete diff and run the `code-review` skill.
8. Commit with a concise English Conventional Commit message and push.
9. Open a draft PR against `main`, link the Linear issue, and include the
   behavior, risk, fallback, and verification evidence.
10. Inspect every CI job. Fix branch-owned failures on the same branch.
11. Mark the PR ready only when the branch is locally complete and CI is
    healthy; move the issue to `In Review`.
12. Inspect all review surfaces, address actionable findings, and repeat
    affected checks after every review-driven push.
13. Merge only when the gate in section 6 is satisfied.
14. Move the issue to `Done`, record bounded follow-ups, sync local `main`, and
    remove the completed branch when it is no longer needed.

Normal product and workflow changes do not go directly to `main`.

## 4. Local validation

The shared local/CI gate is:

```sh
make verify
```

It runs:

- `gofmt` verification;
- unit tests with a fresh count;
- race-detector tests;
- `go vet`;
- `staticcheck`;
- the production-style `CGO_ENABLED=0` build; and
- `git diff --check`.

Use the smallest relevant checks while iterating, then run the complete gate
before handoff or PR readiness. A local green run does not replace CI.

For animation changes, also inspect `--preview` at useful narrow and wide
terminal sizes. Verify fixed-seed determinism, exact output width, zero-width
safety, and the all-pairs similarity guard.

For sound-reactive changes, verify these states separately:

- capture unavailable: base visual plus one actionable process-lifetime
  warning;
- capture available but silent: unchanged, complete base animation;
- active audio: bounded, smooth response without frame-rate-dependent motion.

Never claim a real Sway or sound check that was not actually run.

## 5. Pull request content and review

A PR description must state:

- what changes for the user;
- why the slice exists;
- important Sway, terminal, audio, or compatibility consequences;
- fallback and unsupported behavior;
- local validation and relevant manual checks;
- intentionally deferred evidence or follow-ups; and
- the associated Linear issue.

CI status and review status are separate gates. Inspect check summaries first
and fetch full logs only for failed, canceled, or suspicious jobs.

Review all available surfaces: conversation comments, submitted reviews,
inline threads and their resolution state, requested reviewers, and the overall
review decision. A review of an older head commit is stale after a push.

Classify findings as in-scope fixes, verified out-of-scope follow-ups, stale or
duplicate observations, or decisions requiring an explicit trade-off. Do not
silently ignore a valid finding.

## 6. Merge gate

Merge only when all of the following are true:

- the diff still matches one coherent Linear issue;
- `make verify` passes locally;
- required GitHub Actions checks pass for the current head;
- required review has completed and actionable threads are resolved;
- public behavior, configuration, and lasting plans are synchronized;
- no secret, transient log, generated binary, or local runtime artifact is
  included; and
- deferred work is explicitly bounded in Linear.

After merge, move the issue to `Done`, sync local `main` by fast-forward, verify
the merge, and clean up the feature branch. Never continue new work from a
stale local `main`.

## 7. Review cadence

Branch review catches local defects but not gradual architecture drift.

- Run `code-review` for every substantive branch before PR handoff.
- Run `review-codebase` after every fifth substantive feature/fix branch or at
  a meaningful project checkpoint, whichever comes first.
- Documentation-only and mechanical maintenance branches do not advance the
  counter unless they change architecture or product contracts.
- Verify older review findings against current `main` and existing Linear
  issues before creating a follow-up.

Record durable review-driven decisions in repository documentation and create
only the bounded Linear issues that are still actionable.

## 8. Parallel work and safety

When multiple agents or branches are active, use separate branches and
worktrees. Inspect likely overlap before editing and re-review conflicts instead
of overwriting another contributor's work.

Never commit credentials, Linear tokens, Sway socket paths, captured audio,
clipboard contents, or private desktop artifacts. Keep transient diagnostic
logs outside the repository and summarize only the useful, non-sensitive
finding.
