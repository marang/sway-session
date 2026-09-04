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

The Arch package keeps only `sway` as a runtime dependency. Go is a
`makedepends` entry because AUR builds compile from source, but installed
GoReleaser binaries do not require Go. Audio (`libpulse`), terminal adapters
(`alacritty` or `foot`), Herdr, Flatpak/GIO application restore, and the
AppArmor verification boundary remain feature-specific `optdepends`. The
SQLite driver is pure Go and built with `CGO_ENABLED=0`, so neither SQLite nor
a C runtime/toolchain is a package dependency. DEB and RPM recommendations
carry the same Herdr 0.8.2 minimum as the Arch metadata.

If the AUR workflow infrastructure fails after a GitHub release has already
been published, rerun the fixed workflow for the existing immutable tag rather
than moving the tag or creating a replacement release:

```sh
gh workflow run aur.yml --ref main -f version=v0.3.0
```

The manual path accepts only a strict `vMAJOR.MINOR.PATCH` tag, checks out that
exact tag, verifies that `HEAD` matches it, and requires its commit to be
reachable from `origin/main` before building or publishing. The container uses
a command-scoped `safe.directory` for the Actions workspace only; it does not
persist a broad Git trust exception.

Required GitHub secrets:

```text
AUR_PRIVATE_KEY
RELEASE_SYNC_TOKEN
```

`RELEASE_SYNC_TOKEN` must be a fine-grained token limited to this repository
with read/write access to Contents and Pull requests. A separate token is used
instead of the workflow's `GITHUB_TOKEN` so that the generated pull request runs
the repository's normal pull-request checks.

After creating or changing that token, verify both required permissions without
publishing a release:

```sh
gh workflow run aur.yml --ref main -f operation=verify-sync-token
```

The verification job creates a uniquely named temporary branch and pull
request, requires its normal pull-request checks to start and pass without
manual approval, then uses an always-running cleanup step to close it and delete
the exact probe commit even when a later verification step fails. It never
publishes to the AUR or changes `main`.

Optional GitHub secrets:

```text
AUR_COMMIT_NAME
AUR_COMMIT_EMAIL
```

The public key for `AUR_PRIVATE_KEY` must be added to the AUR account allowed to
push to the package.

The publishing job pins the official `aur.archlinux.org` Ed25519 host key and
requires fingerprint
`SHA256:RFzBCUItH9LZS0cKB5UE6ceAYhBD5C8GeOBip8Z11+4`; it never trusts a key
learned during the release run. If Arch rotates that key, verify the new value
against Arch Linux infrastructure's
[`docs/ssh-hostkeys.txt`](https://gitlab.archlinux.org/archlinux/infrastructure/-/blob/main/docs/ssh-hostkeys.txt)
through a separate trusted path, then update both the workflow key and
fingerprint plus the packaging regression check in one reviewed change.

The release tag is created only after the real Sway/Herdr end-to-end release
check has passed. That check exercises both a fresh schema-1 SQLite state root
and the explicit, source-preserving pre-1.0 JSON migration. Pushing the tag starts the
GitHub release and AUR workflows; it is not the point at which the end-to-end
check is performed.
