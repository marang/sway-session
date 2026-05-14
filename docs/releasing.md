# Releasing

This document is for maintainers.

## GitHub Releases

Releases are built with GoReleaser on version tags:

```sh
git tag v0.1.0
git push origin v0.1.0
```

GoReleaser builds Linux archives for `amd64` and `arm64`, plus Linux `deb` and
`rpm` packages.

## AUR

The repository includes a source `PKGBUILD` for publishing `sway-title-animator`
to the AUR. The AUR workflow updates `pkgver` and `sha256sums`, generates
`.SRCINFO`, and pushes to:

```text
ssh://aur@aur.archlinux.org/sway-title-animator.git
```

Required GitHub secret:

```text
AUR_PRIVATE_KEY
```

Optional GitHub secrets:

```text
AUR_COMMIT_NAME
AUR_COMMIT_EMAIL
```

The public key for `AUR_PRIVATE_KEY` must be added to the AUR account allowed to
push to the package.
