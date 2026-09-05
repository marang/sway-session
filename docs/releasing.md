# Releasing sway-session

This document is for maintainers. The first standalone release is v0.1.0.
Although the repository preserves the complete source history, do not push or
recreate sway-title-animator tags in the sway-session remote.

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
archive, calculates its SHA-256, writes that checksum into PKGBUILD, regenerates
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
2. replace version/checksum in the release template;
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
gh workflow run aur.yml --ref main \
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
