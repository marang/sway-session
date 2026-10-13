# Releasing sway-session

This document is for maintainers. Standalone releases began at v0.1.0.
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

A tag starts one `publish-release.yml` pipeline. It resolves the strict version
tag to its full commit and requires an exact checkout and main ancestry.
`verify-release.yml` first looks for compatible completed main-CI evidence for
that exact commit. It accepts only the canonical repository's successful
`ci.yml` push to main, the current run attempt, and an unexpired proof artifact
whose archive digest, workflow source, verification contract and actual pinned
Go toolchain match. Pull-request checks, cache hits, missing artifacts and
failed, skipped or running checks cannot authorize publication.

If compatible evidence is unavailable, the pipeline calls the complete
read-only `verify.yml` gate once. The aggregation job publishes a commit output
only when the reused proof or fresh successful verification matches the
resolved commit. GitHub and AUR jobs depend on this same output. CI still runs
fresh unit and race tests with `-count=1`; compiled caches never substitute for
successful test execution. Profiling and cache measurements are recorded in
[the LAB-329 evidence](research/lab-329-verify-performance.md).

The read-only artifact job uses GoReleaser 2.18.2 with
`release --clean --skip=publish,announce`, the explicit
`GORELEASER_CURRENT_TAG`, and the commit timestamp. It builds Linux amd64 and
arm64 archives, DEBs and RPMs once. The staged manifest binds version, full
commit, source tree and build inputs, toolchain, GoReleaser version, target
architectures and every file's SHA-256. The checker inspects each package's ELF
architecture, embedded build identity and Go toolchain, and requires the same
binary bytes across package formats for each architecture.

Jobs transfer only inspected assets and their manifest using the immutable
Actions artifact ID. An independent read-only handoff job compares the manifest
digest from the producing job and verifies the transferred packages again.
The GitHub publisher performs the same checks and revalidates the tag before
uploading those bytes; it does not rebuild. Only that job receives a
write-capable GitHub token. Verification and builds receive no publication
secrets; AUR and metadata-sync secrets stay in their respective publishing jobs.

Manual AUR publication remains `publish-aur.yml` on main. It invokes the common
pipeline with target `aur`, which performs no unnecessary GoReleaser build.
Its requested tag can be older than main: verification and the Arch package
build both check out the resolved tag commit, never the dispatch commit.
New workflow helpers are staged from the immutable workflow-source commit,
so historical source checkouts do not need to contain those helpers. Historical
Makefiles still run their complete original verification gate serially.

`aur-package.yml` retains the separate non-root Arch source build, package
checks, checksum and commit-pinned source URL/directories, metadata handoff and
SSH host-key checks. Its PIE source build is required and is not replaced by
GoReleaser's binary packages. Current tag-based and previously pinned recipe
templates are supported; unknown templates are rejected before publication.

For recovery, rerun failed jobs in the original pipeline so they consume the
successful build job's retained artifact ID. Release artifacts are retained
for 14 days: resume a partial publication within that window. After expiry,
a partial release requires explicit recovery; a complete matching release
can be restored directly without rebuilding. The publisher hashes every
existing release asset before mutation and uploads only missing matching
files; it never deletes or replaces published assets. An existing draft,
`starter` asset or conflicting file requires explicit recovery. AUR keeps
serialization and its no-change guard: older versions are rejected, identical
same-version package files are a no-op, and changed published files require a
reviewed package revision. An empty repository with neither package file may
receive its first publication.

An isolated handoff run is available without creating a public tag or release:

~~~sh
gh workflow run publish-release.yml --ref <candidate-branch> \
  -f validation-only=true
~~~

It uses a checkout-local `v0.0.0` tag, executes verification, builds and checks
the actual package artifact across jobs, and skips both publishers. Publication
from a manual dispatch still requires main and an existing immutable tag.

The comparison reads `.SRCINFO` as data: numeric epoch (absent means zero),
dotted numeric version, then numeric package revision with an optional
subrevision. Unsupported versions, incomplete metadata and mismatched package
identities fail before mutation. The incoming version must match the resolved
release tag; no PKGBUILD is executed by this guard.

The metadata-sync job independently rejects older versions against main,
including direct job reruns. Equal-version changes remain allowed there so
main can receive the exact commit-pinned package files already published.
Do not move an existing tag or use a rerun to replace published assets.

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

## Historical v0.1.0 bootstrap metadata

Before the first standalone release, no immutable tag archive existed.
The initial PKGBUILD and .SRCINFO therefore carried `SKIP` as explicit
bootstrap metadata. Current checked-in metadata describes a released archive
with a concrete SHA-256 checksum. `SKIP` is not release metadata and must never
be published to the AUR.

The AUR workflow checks out the exact immutable tag, downloads its GitHub source
archive for the resolved commit, calculates its SHA-256, pins the recipe's
source and directories to that commit, writes the checksum, regenerates
.SRCINFO, rejects any remaining SKIP, verifies the source, and builds/tests the
package. Never invent a checksum, checksum a mutable branch archive, or commit a
placeholder that looks real.

## Package ownership transition

The historical pre-split sway-title-animator package may own
/usr/bin/sway-session. The standalone package must not use an overwrite escape
hatch.

Before releasing, build the corresponding animator package that no longer owns
the sway-session binary and this standalone package. In an isolated package
root verify both supported transitions:

1. upgrade sway-title-animator first, then install sway-session; and
2. install both verified packages in one package-manager transaction.

Do not promise that a particular AUR helper performs one atomic transaction
unless that behavior was tested. Do not recommend --overwrite.

## GitHub release

The common pipeline rejects a tag whose commit is not reachable from
origin/main. After every precondition passes:

~~~sh
git switch main
git pull --ff-only
release_tag=v1.2.3 # Example: replace with the approved, unused release version.
git tag "$release_tag"
git push origin "$release_tag"
~~~

GoReleaser publishes Linux amd64 and arm64 tar.gz archives plus DEB and RPM
packages. Each artifact contains only sway-session and its standalone
documentation/integration assets. DEB/RPM runtime metadata contains only Sway;
the pure-Go SQLite driver keeps the binary CGO-free.

Inspect the published release and checksums before treating it as available.

## AUR publication and metadata sync

The shared pipeline also calls the AUR source-package workflow:

1. validate the strict vMAJOR.MINOR.PATCH tag and main ancestry;
2. replace version/checksum and pin source/directories to the verified commit;
3. verify and build the source package in Arch Linux;
4. pass only PKGBUILD and .SRCINFO to the isolated publishing job;
5. push exact verified metadata to
   ssh://aur@aur.archlinux.org/sway-session.git; and
6. open a PR syncing those exact files back to main.

Review and merge the metadata-sync PR after its normal checks pass. The
checked-in files then describe that immutable release and its verified source
checksum.

If the AUR path fails after the GitHub release exists, rerun the workflow for
the existing tag:

~~~sh
gh workflow run publish-aur.yml --ref main \
  -f operation=publish-release -f version="$release_tag"
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
