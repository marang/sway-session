# Releasing sway-session

This document is for maintainers. The first standalone release is v0.1.0.
Although the repository preserves the complete source history, do not push or
recreate sway-title-animator tags in the sway-session remote.

Use the repository [release-validation skill](../.agents/skills/sway-session-release-validation/SKILL.md)
required by AGENTS.md to select affected user scenarios and record candidate
evidence. The publication gates below remain authoritative.

`sway-session --version` and `sway-session version` print the executing build's
product version and commit on one line. Add `--json` for the normal schema-v1
envelope: `version` is the envelope schema, while `build.product_version`,
`build.commit`, and `build.modified` identify the product. These commands do not
read configuration, session state, or runtime sockets.

GoReleaser embeds the release version and full commit. Arch embeds `pkgver`
and the recipe's pinned `_commit`; the AUR workflow replaces that commit with
the verified release tag commit before building. Local `make build` defaults to
`dev`, records HEAD when available, and marks a dirty checkout as modified.
Source archives can supply immutable inputs explicitly:

~~~sh
make build VERSION=1.2.3 COMMIT=<full-40-character-commit> MODIFIED=false
~~~

The shared build stamp survives stripping, `-trimpath`, and PIE. Read-only
Doctor checks inspect the pinned daemon executable without running it. Older
unstamped executables may have unknown metadata; byte comparison remains
available independently of metadata. No database or broker schema changes are
required for build identification.

## Preconditions

The release commit must be merged to main and associated with a correctly
routed Linear issue in the Sway Session project. Complete:

~~~sh
make verify
~~~

Then follow docs/sway-session-verification.md for the current real Sway/Herdr
check, shared title-indicator fixture comparison, clean GoReleaser snapshot,
temporary local-tarball Arch build, package-content inspection, and package
ownership transition test.

## Automated publication gate

CI, `publish-release.yml`, and `publish-aur.yml` share the read-only
`verify.yml` workflow. Release jobs first resolve the strict version tag to its
full commit, require an exact checkout and main ancestry, then verify that
commit with `make verify` and GoReleaser configuration validation. The verified
commit output is written only after all checks succeed. Publication depends on
successful verification and matching commit outputs; a missing, failed,
cancelled, skipped or running verification cannot authorize publication.

Manual AUR publication is accepted only through `publish-aur.yml` on `main`.
Its requested tag can be older than main: verification and the Arch package
build both check out the resolved tag commit, never the dispatch commit. The
generated package source URL and source directory use that full commit too,
so changing the tag after checkout cannot substitute a different source tree.
Current tag-based and previously pinned package templates are supported;
unrecognized source or directory templates are rejected before publication.
The existing checksum, non-root package build, metadata handoff and SSH host-key
checks still apply. No publishing secrets are passed to verification or CI.
Only the GitHub publisher receives a write-capable GitHub token.

The GitHub publisher passes the resolved tag explicitly as
`GORELEASER_CURRENT_TAG`. When multiple version tags share a verified commit,
GoReleaser therefore builds and publishes the version checked by the release
guard instead of independently selecting the highest tag on that commit.

Repeating a gated run retains its exact commit. GitHub publication checks for
an existing release and leaves already published artifacts intact; an existing
draft requires explicit recovery. AUR publication keeps the existing
no-change guard and serialization. Do not move an existing tag or use a rerun
to replace published assets.

Historical tags retain historical workflow code. Keep `release.yml` (workflow
ID `350831682`) and `aur.yml` (ID `350831683`) on main as inert retirement
placeholders, and keep both IDs in `disabled_manually` state. Deleting the files
is insufficient: GitHub's `deleted` state accepted an isolated historical job
rerun during the LAB-281 cutover, and the disable endpoint rejected that state
as not active. The placeholders have no automatic trigger or token permissions,
and their only job is unconditionally skipped. They allow explicit disabling
of the historical IDs without restoring any publishing code on main.

During cutover, while the restored IDs are active, dispatch each placeholder
from main. Confirm the merged commit, the sole skipped `retired` job and no
executed steps. Then disable both IDs, confirm their `disabled_manually` state,
and confirm no queued or running executions remain. Check full, failed-job and
single-job rerun rejection using those inert runs; never use historical
publishing jobs as probes. Do not
re-enable either retired workflow or remove its placeholder. Use the new
`publish-aur.yml` main-dispatch path for AUR recovery. A read-only probe
established that disabled workflow IDs reject all three rerun endpoints with
HTTP 403; sanitized cutover evidence belongs in LAB-281.

The repository-policy inspection on 2026-10-09 found main protection disabled,
one disabled branch ruleset, and no tag ruleset. These supplemental controls
are not claimed as active. Required CI checks and immutable-tag rules remain
recommended administrative hardening; the implemented job dependencies and
retired workflow IDs provide the publication gate. Recheck live repository
settings before changing this policy.

Required GitHub repository secrets:

~~~text
AUR_PRIVATE_KEY
RELEASE_SYNC_TOKEN
~~~

RELEASE_SYNC_TOKEN must be fine-grained to this repository with read/write
Contents and Pull requests permission so its generated PR runs normal checks.
Optional AUR author identity secrets are AUR_COMMIT_NAME and
AUR_COMMIT_EMAIL.

After creating or changing the sync token, run the workflow's
verify-sync-token operation against main. It creates a uniquely named probe
branch and PR, verifies that pull-request checks run, and always closes and
deletes the exact probe. It does not publish a release or AUR package.

## Honest v0.1.0 bootstrap metadata

No immutable v0.1.0 GitHub tag archive exists before the first release.
PKGBUILD and .SRCINFO therefore carry SKIP as explicit bootstrap metadata.
This is not release metadata and must never be published to the AUR.

The AUR workflow checks out the exact immutable tag, downloads its GitHub source
archive for the resolved commit, calculates its SHA-256, pins the recipe's
source and directories to that commit, writes the checksum, regenerates
.SRCINFO, rejects any remaining SKIP, verifies the source, and builds/tests the
package. Never invent a checksum, checksum a mutable branch archive, or commit a
placeholder that looks real.

## Package ownership transition

The pre-split sway-title-animator package may own /usr/bin/sway-session. The
standalone package must not use an overwrite escape hatch.

Before releasing, build the corresponding animator package that no longer owns
the sway-session binary and this standalone package. In an isolated package
root verify both supported transitions:

1. upgrade sway-title-animator first, then install sway-session; and
2. install both verified packages in one package-manager transaction.

Do not promise that a particular AUR helper performs one atomic transaction
unless that behavior was tested. Do not recommend --overwrite.

## GitHub release

Both publishing workflows reject a tag whose commit is not reachable from
origin/main. After every precondition passes:

~~~sh
git switch main
git pull --ff-only
git tag v0.1.0
git push origin v0.1.0
~~~

GoReleaser publishes Linux amd64 and arm64 tar.gz archives plus DEB and RPM
packages. Each artifact contains only sway-session and its standalone
documentation/integration assets. DEB/RPM runtime metadata contains only Sway;
the pure-Go SQLite driver keeps the binary CGO-free.

Inspect the published release and checksums before treating it as available.

## AUR publication and metadata sync

The tag also starts the AUR workflow:

1. validate the strict vMAJOR.MINOR.PATCH tag and main ancestry;
2. replace version/checksum and pin source/directories to the verified commit;
3. verify and build the source package in Arch Linux;
4. pass only PKGBUILD and .SRCINFO to the isolated publishing job;
5. push exact verified metadata to
   ssh://aur@aur.archlinux.org/sway-session.git; and
6. open a PR syncing those exact files back to main.

Review and merge the metadata-sync PR after its normal checks pass. The
checked-in files then describe the immutable release rather than the bootstrap
SKIP state.

If the AUR path fails after the GitHub release exists, rerun the workflow for
the existing tag:

~~~sh
gh workflow run publish-aur.yml --ref main \
  -f operation=publish-release -f version=v0.1.0
~~~

Never move or replace the tag. The manual path verifies that the requested tag
exists, that checkout HEAD is its commit, and that it is reachable from main.

The publishing job pins the official aur.archlinux.org Ed25519 host key and
fingerprint. If Arch rotates that key, verify it through Arch infrastructure
over an independent trusted path, then update the key, fingerprint, and
packaging assertion in one reviewed change.

## Closeout

After GitHub, DEB/RPM archives, AUR, and the metadata-sync PR are verified:

- record artifact and CI evidence on the Linear issue;
- move the issue to Done only after merge/release completion;
- fast-forward local main;
- remove the completed feature branch when no longer needed; and
- retain no generated archive, package, checksum scratch file, or private test
  state in the repository.
