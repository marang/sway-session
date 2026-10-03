# Scenario and evidence record

Use one record for the candidate and one row per applicable scenario. Add the
details needed to distinguish the tested boundary; omit irrelevant fields.
This template records evidence, not release authorization.

```text
Candidate: full commit, product version/build metadata, modified flag
Artifact: executable/package path or public asset name, SHA-256
Scope: pinned comparison and affected responsibilities
Environment: relevant tool versions and disposable fixture conditions
Canonical gates: evidence references or outstanding prerequisites
Independent validator: scope, candidate digest and result; or not run + reason
```

| Scenario | Expected behavior / completion criterion | Status | Observed behavior | Proof level | Candidate / evidence |
| --- | --- | --- | --- | --- | --- |
| Affected user workflow | Concrete observable outcome | passed / failed / not run | Observation or missing prerequisite | Actual boundary exercised | Exact identity and sanitized log/artifact reference |

Record excluded scenarios separately with reasons, such as “branding asset
only; launcher, daemon and lifecycle code unchanged.” An exclusion is not a
passed check. For an applicable scenario that could not run, retain its row as
not run. Distinguish a required gate from additional diagnostic coverage.

Proof levels are descriptive, not interchangeable ranks:

- **Unit simulation:** injected trees/events/processes, including shutdown
  notifications. Proves selected state transitions under those inputs.
- **Real Unix IPC:** actual sockets/protocol framing; injected peers remain
  identified as injected.
- **Private Sway/Herdr:** name which real programs participated and which
  remained injected. A compositor restart is not a machine reboot.
- **Built executable / visible interface:** the candidate command, actual TUI
  viewport or image inspected. Source rendering tests retain their unit label.
- **Package installation:** installed candidate digest, package root and the
  actual transition tested; inspecting a tarball alone is not installation.
- **Actual reboot:** boot identifiers before/after, persisted preboot target,
  target type (disposable VM or explicitly coordinated production target),
  startup ordering and observed postboot outcome. State whether manual layout
  correction occurred. Follow the canonical procedure for the scenario.

Evidence remains attached to its original candidate. For a later candidate,
record the changed inputs and which rows need fresh execution. Carry forward
an unaffected row with its original identity, the scope comparison and reason;
do not claim the new binary was exercised by that older run.

## Interpretation examples

These are illustrative decisions, not measurements from this repository.

**Unavailable reboot target:** the deterministic lifecycle matrix and private
Sway checks pass, but no disposable VM image/reboot coordinator is available.
Record the required actual-reboot scenario as not run and identify the missing
target. If the ticket requires a production reboot, a future VM run would still
leave that production acceptance open. Never summarize this as “reboot tested.”

**Changed candidate:** build A passed an application-close scenario. Build B
changes the close-observation path. Retain A's result for A, mark the B scenario
not run until retested, and rerun the affected close/startup boundary. An
unchanged branding asset can retain its separate visual evidence, with its
original asset digest and the comparison showing it is unchanged.

**Image-only scope:** inspect the actual changed asset and its display context.
Explain why close/upgrade/restore scenarios are excluded. For a release, retain
the packaging and publication gates required by the canonical release guide.

Finish with a short readiness statement, required gaps and next action. Link
the detailed record instead of repeating every successful command.
