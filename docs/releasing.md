# Releasing

This document is for maintainers.

## GitHub Releases

Releases are built with GoReleaser on version tags. Both publishing workflows
reject a tag whose commit is not reachable from `main`:

```sh
git tag v0.1.0
git push origin v0.1.0
```

GoReleaser builds `sway-title-animator` and `sway-session` in Linux archives for
`amd64` and `arm64`, plus Linux `deb` and `rpm` packages. The archives and
packages also carry the Sway, Herdr, Codex-hook, and AppArmor integration
templates plus the live Codex-boundary verification script. Package builds use
the `/usr/bin/sway-session` Codex-hook variant; source installs keep the
`~/.local/bin/sway-session` variant.

## AUR and repository package metadata

The repository includes a source `PKGBUILD` for publishing both binaries and
their integration templates in the `sway-title-animator` AUR package. The AUR
workflow treats it as a release template: it updates `pkgver` and `sha256sums`
from the pushed version tag, resets `pkgrel` to `1`, refuses skipped integrity
checks, builds and tests the resulting package in an isolated job, and generates
`.SRCINFO`. A second job receives only those verified metadata files and pushes
them to:

```text
ssh://aur@aur.archlinux.org/sway-title-animator.git
```

After the AUR push succeeds, a third isolated job opens a pull request against
`main` with the exact `PKGBUILD` and `.SRCINFO` used for the release. Merge that
PR after its normal checks pass. Its version-specific automation branch is
rebuilt from current `main` plus only those two files, so retries cannot retain
unrelated branch content. This keeps the package metadata in this source
repository directly buildable without maintaining another Git repository
beyond the AUR repository itself.

Required GitHub secrets:

```text
AUR_PRIVATE_KEY
RELEASE_SYNC_TOKEN
```

`RELEASE_SYNC_TOKEN` must be a fine-grained token limited to this repository
with read/write access to Contents and Pull requests. A separate token is used
instead of the workflow's `GITHUB_TOKEN` so that the generated pull request runs
the repository's normal pull-request checks.

Optional GitHub secrets:

```text
AUR_COMMIT_NAME
AUR_COMMIT_EMAIL
```

The public key for `AUR_PRIVATE_KEY` must be added to the AUR account allowed to
push to the package.

The release tag is created only after the real Sway/Herdr end-to-end release
check has passed. Pushing the tag starts the GitHub release and AUR workflows;
it is not the point at which the end-to-end check is performed.
