---
name: sway-session-release-validation
description: Validate sway-session release candidates through affected user workflows and artifact evidence. Use for release preparation or readiness assessments; ordinary diff reviews use code-review.
---

# Sway Session Release Validation

Assess the candidate through observed behavior. Keep code-review and behavioral
validation as separate gates. Invoking this skill adds no publication authority
or new approval policy; apply the user's existing authorization and repository
workflow.

## Select scenarios

Read [workflow conventions](../../../docs/workflow_conventions.md) and
[releasing](../../../docs/releasing.md), then the affected sections of
[verification](../../../docs/sway-session-verification.md). Those documents own
the release gates and executable procedures. Use the
[scenario/evidence template](references/scenario-evidence.md) for this run.

Pin the comparison, changed responsibilities and actual candidate: commit,
build metadata and executable/package digest. Before testing, translate each
affected user workflow into expected observations and a completion criterion.
Account for each affected responsibility; record exclusions with a concrete
reason. Image/text-only changes do not acquire terminal-lifecycle scenarios.
Canonical release gates still apply when preparing an actual release.

Use existing tests and the LAB-134 lifecycle runner through the verification
guide. Select relevant close, next-login, daemon-update or restore scenarios;
this skill does not implement a second harness. For Doctor/TUI changes, include
an ordinary working configuration and a narrow terminal. For executable or
packaging changes, exercise the built executable/package rather than relying
only on source tests. Inspect the visible interface or image when affected.

## Validate the candidate

Run selected checks within the existing authorization. Live fixtures use
disposable state and workspace 98 or higher, preserving production work.
Record an unavailable boundary as **not run**, with the missing prerequisite;
use the evidence level actually exercised. Unit simulation, real Unix IPC,
private Sway/Herdr, package installation and actual reboot are distinct proof.
The lifecycle runner's simulated shutdown or private compositor restart does
not prove a real reboot. An acceptance requirement for production reboots must
be demonstrated on that production target, not substituted with a VM result.

When subagents are available and authorized, assign independent behavioral
validation of the built candidate. Give the validator artifact identity,
expected behavior, relevant procedures and safe execution scope. Let it choose
its conclusion; a second diff review does not satisfy this step. Record absent
independent validation as not run, rather than claiming it occurred.

For every applicable scenario, record **passed**, **failed** or **not run** and
the expected/observed behavior, candidate identity, environment, proof level
and evidence reference. Keep raw diagnostics outside the checkout; tracked or
published records contain only sanitized evidence. Exclude private registry
contents, pane output, secrets and user-specific environment dumps.

After a candidate change, identify which tested inputs or outcomes changed.
Retire relevant old evidence and rerun those scenarios on the new candidate.
Reuse unaffected evidence only with its original identity and an explicit
scope/input comparison; never relabel old artifact results as new executions.

## Report readiness

Give a concise decision supported by the scenario records: verified behavior,
remaining defects and untested boundaries. A failed or missing required check
keeps its gate open. Name the next concrete action needed. Review approval,
test counts and confidence percentages do not replace behavioral evidence.
