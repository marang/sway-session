#!/bin/sh

set -eu

require_fixed() {
	file=$1
	value=$2
	if ! grep -F -- "$value" "$file" >/dev/null; then
		echo "$file is missing required packaging entry: $value" >&2
		exit 1
	fi
}

require_count() {
	file=$1
	value=$2
	want=$3
	got=$(grep -F -c -- "$value" "$file" || true)
	if [ "$got" -ne "$want" ]; then
		echo "$file has $got copies of required packaging entry (want $want): $value" >&2
		exit 1
	fi
}

reject_fixed() {
	file=$1
	value=$2
	if grep -F -- "$value" "$file" >/dev/null; then
		echo "$file contains forbidden packaging entry: $value" >&2
		exit 1
	fi
}

reject_regex() {
	file=$1
	pattern=$2
	if grep -E -- "$pattern" "$file" >/dev/null; then
		echo "$file contains forbidden packaging line: $pattern" >&2
		exit 1
	fi
}

require_fixed .goreleaser.yaml 'id: sway-session'
require_fixed .goreleaser.yaml 'main: ./cmd/sway-session'
require_fixed .goreleaser.yaml 'binary: sway-session'
require_fixed .goreleaser.yaml 'id: sway-herdr-init'
require_fixed .goreleaser.yaml 'main: ./cmd/sway-herdr-init'
require_fixed .goreleaser.yaml 'binary: sway-herdr-init'
require_fixed .goreleaser.yaml 'ids: [sway-title-animator, sway-session, sway-herdr-init]'
require_fixed PKGBUILD '# Maintainer: marang <1550038+marang@users.noreply.github.com>'
require_fixed PKGBUILD '# Release template:'
require_fixed PKGBUILD "makedepends=('go>=1.26.5')"
require_fixed PKGBUILD "options=('!debug')"
require_fixed PKGBUILD 'flatpak: restore explicitly registered Flatpak desktop applications'
require_fixed PKGBUILD 'glib2: restore desktop-entry applications through gio'
require_fixed PKGBUILD 'noto-fonts: recommended Noto Sans Mono glyph coverage for desktop-app indicators'
require_fixed PKGBUILD 'apparmor: secure Codex resume boundary'
require_fixed PKGBUILD '_go_build_flags=(-buildmode=pie -trimpath -buildvcs=false -mod=readonly -modcacherw)'
require_fixed PKGBUILD '_go_ldflags=(-s -w -buildid=)'
require_fixed PKGBUILD 'export GOTOOLCHAIN=local'
require_fixed PKGBUILD 'CGO_ENABLED=0 go build "${_go_build_flags[@]}" -ldflags="${_go_ldflags[*]}" -o sway-session ./cmd/sway-session'
require_fixed PKGBUILD 'CGO_ENABLED=0 go build "${_go_build_flags[@]}" -ldflags="${_go_ldflags[*]}" -o sway-herdr-init ./cmd/sway-herdr-init'
require_fixed PKGBUILD 'CGO_ENABLED=0 go test "${_go_build_flags[@]}" -count=1 ./...'
require_fixed PKGBUILD 'install -Dm755 sway-session "$pkgdir/usr/bin/sway-session"'
require_fixed PKGBUILD 'install -Dm755 sway-herdr-init "$pkgdir/usr/bin/sway-herdr-init"'
require_count contrib/sway/45-title-animator.conf 'exec_always --no-startup-id /usr/bin/sway-title-animator --replace --fps 25' 1
require_count contrib/sway/45-title-animator.conf 'exec --no-startup-id /usr/bin/sway-session daemon' 1
require_count contrib/sway/45-title-animator.conf 'exec --no-startup-id /usr/bin/sway-session restore' 1
require_fixed contrib/sway/45-title-animator.conf '# exec, not exec_always: a config reload must not request another restore.'
require_count contrib/sway/45-title-animator.conf '# bindsym $mod+Ctrl+p exec --no-startup-id /usr/bin/sway-session app register-focused' 1
reject_fixed contrib/sway/45-title-animator.conf 'exec_always --no-startup-id /usr/bin/sway-session daemon'
reject_fixed contrib/sway/45-title-animator.conf 'exec_always --no-startup-id /usr/bin/sway-session restore'
reject_regex contrib/sway/45-title-animator.conf '^[[:space:]]*bindsym \$mod\+Ctrl\+p exec --no-startup-id /usr/bin/sway-session app register-focused[[:space:]]*$'
require_fixed .goreleaser.yaml 'contrib/codex/hooks.json'
require_fixed .goreleaser.yaml 'contrib/codex/hooks-system.json'
require_fixed PKGBUILD 'contrib/codex/hooks-system.json'
require_fixed .goreleaser.yaml '      - flatpak'
require_fixed .goreleaser.yaml '      - apparmor'
require_fixed .goreleaser.yaml '      - python3'
require_fixed .goreleaser.yaml '          - libglib2.0-bin'
require_fixed .goreleaser.yaml '          - fonts-noto-mono'
require_fixed .goreleaser.yaml '          - glib2'
require_fixed .goreleaser.yaml '          - google-noto-sans-mono-fonts'
require_fixed .goreleaser.yaml '      - src: ./contrib/sway/45-title-animator.conf'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/45-title-animator.conf'
require_fixed .goreleaser.yaml '      - src: ./contrib/herdr/config.toml'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/contrib/herdr/config.toml'
require_fixed .goreleaser.yaml '      - src: ./contrib/codex/hooks-system.json'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/contrib/codex/hooks.json'
reject_fixed .goreleaser.yaml '      - src: ./contrib/codex/hooks.json'
require_fixed .goreleaser.yaml '      - src: ./contrib/apparmor/codex-home-guard'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/contrib/apparmor/codex-home-guard'
require_fixed .goreleaser.yaml '      - src: ./scripts/verify-codex-boundary.sh'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/scripts/verify-codex-boundary.sh'
require_fixed PKGBUILD 'install -Dm644 contrib/sway/45-title-animator.conf "$pkgdir/usr/share/doc/$pkgname/45-title-animator.conf"'
require_fixed PKGBUILD 'install -Dm644 contrib/herdr/config.toml "$pkgdir/usr/share/doc/$pkgname/contrib/herdr/config.toml"'
require_fixed PKGBUILD 'install -Dm644 contrib/codex/hooks-system.json "$pkgdir/usr/share/doc/$pkgname/contrib/codex/hooks.json"'
require_fixed PKGBUILD 'install -Dm644 contrib/apparmor/codex-home-guard "$pkgdir/usr/share/doc/$pkgname/contrib/apparmor/codex-home-guard"'
require_fixed .goreleaser.yaml 'mode: 0755'
require_fixed PKGBUILD 'install -Dm755 scripts/verify-codex-boundary.sh'
require_fixed scripts/verify-codex-boundary.sh 'PATH=/usr/bin'
require_fixed scripts/verify-codex-boundary.sh 'session_binary=/usr/bin/sway-session'
require_fixed scripts/verify-codex-boundary.sh 'initializer_binary=/usr/bin/sway-herdr-init'
require_fixed scripts/verify-codex-boundary.sh 'require_packaged_binary "$session_binary"'
require_fixed scripts/verify-codex-boundary.sh 'LD_PRELOAD="$preload_probe"'
require_fixed .github/workflows/aur.yml "grep -F 'SKIP' PKGBUILD"
require_fixed .github/workflows/aur.yml 'sed -i "s/^pkgrel=.*/pkgrel=1/" PKGBUILD'
require_fixed .github/workflows/aur.yml "grep -Fx 'pkgrel=1' PKGBUILD"
require_fixed .github/workflows/aur.yml 'makepkg --syncdeps --cleanbuild --clean --noconfirm'
require_fixed .github/workflows/aur.yml 'workflow_dispatch:'
require_fixed .github/workflows/aur.yml 'VERSION: ${{ github.event.inputs.version || github.ref_name }}'
require_fixed .github/workflows/aur.yml 'ref: ${{ github.event.inputs.version || github.ref_name }}'
require_fixed .github/workflows/aur.yml 'show-ref --verify --quiet "refs/tags/$VERSION"'
require_fixed .github/workflows/aur.yml 'git -c safe.directory="$GITHUB_WORKSPACE" \'
require_fixed .github/workflows/aur.yml 'merge-base --is-ancestor "$tag_commit" origin/main'
require_fixed .github/workflows/release.yml 'git merge-base --is-ancestor "$GITHUB_SHA" origin/main'
require_fixed .github/workflows/aur.yml 'actions/upload-artifact@v4'
require_fixed .github/workflows/aur.yml 'actions/download-artifact@v4'
require_fixed .github/workflows/aur.yml 'group: aur-release'
require_fixed .github/workflows/aur.yml 'persist-credentials: false'
require_fixed .github/workflows/aur.yml 'needs: publish-aur'
require_fixed .github/workflows/aur.yml 'RELEASE_SYNC_TOKEN'
require_fixed .github/workflows/aur.yml 'gh pr create'
require_fixed .github/workflows/aur.yml 'cp release-metadata/SRCINFO .SRCINFO'
require_fixed .github/workflows/aur.yml 'git checkout -B "$branch" origin/main'
require_fixed .github/workflows/aur.yml '--force-with-lease="refs/heads/${branch}:${remote_sha}"'

if git check-ignore --no-index --quiet .SRCINFO; then
	echo '.SRCINFO must remain trackable for release metadata sync PRs.' >&2
	exit 1
fi

for asset in \
	contrib/sway/45-title-animator.conf \
	contrib/herdr/config.toml \
	contrib/apparmor/codex-home-guard \
	scripts/verify-codex-boundary.sh
do
	require_fixed .goreleaser.yaml "$asset"
	require_fixed PKGBUILD "$asset"
done
