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

require_sequence() {
	file=$1
	first=$2
	second=$3
	third=$4
	if ! awk -v first="$first" -v second="$second" -v third="$third" '
		$0 == first {
			if ((getline next_line) > 0 && next_line == second &&
			    (getline final_line) > 0 && final_line == third) {
				found = 1
			}
		}
		END { exit(found ? 0 : 1) }
	' "$file"; then
		echo "$file is missing required associated packaging block: $first / $second / $third" >&2
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
require_fixed .goreleaser.yaml 'ids: [sway-title-animator, sway-session]'
require_count .goreleaser.yaml 'dependencies:' 1
require_sequence .goreleaser.yaml \
	'    dependencies:' \
	'      - sway' \
	'    contents:'
reject_fixed .goreleaser.yaml 'sway-herdr-init'
reject_fixed .gitignore 'sway-herdr-init'
require_fixed PKGBUILD '# Maintainer: marang <1550038+marang@users.noreply.github.com>'
require_fixed PKGBUILD '# Release template:'
require_fixed PKGBUILD "depends=('sway')"
require_fixed PKGBUILD "makedepends=('go>=1.26.5')"
require_fixed PKGBUILD "options=('!debug')"
require_fixed .SRCINFO 'depends = sway'
require_fixed .SRCINFO 'makedepends = go>=1.26.5'
reject_regex PKGBUILD '^[[:space:]]*optdepends[[:space:]]*(\+)?='
reject_regex PKGBUILD '^[[:space:]]*depends\+'
reject_regex .SRCINFO '^[[:space:]]*optdepends[[:space:]]*='
reject_regex PKGBUILD '^depends=.*(alacritty|apparmor|flatpak|foot|glib2|go|herdr|libpulse|noto|python|sqlite)'
reject_fixed PKGBUILD 'noto-fonts:'
reject_fixed .SRCINFO 'optdepends = noto-fonts:'
reject_fixed PKGBUILD 'sqlite:'
reject_fixed .SRCINFO 'depends = sqlite'
require_fixed PKGBUILD '_go_build_flags=(-buildmode=pie -trimpath -buildvcs=false -mod=readonly -modcacherw)'
require_fixed PKGBUILD '_go_ldflags=(-s -w -buildid=)'
require_fixed PKGBUILD 'export GOTOOLCHAIN=local'
require_fixed PKGBUILD 'CGO_ENABLED=0 go build "${_go_build_flags[@]}" -ldflags="${_go_ldflags[*]}" -o sway-session ./cmd/sway-session'
require_fixed PKGBUILD 'CGO_ENABLED=0 go test "${_go_build_flags[@]}" -count=1 ./...'
require_fixed PKGBUILD 'install -Dm755 sway-session "$pkgdir/usr/bin/sway-session"'
reject_fixed PKGBUILD 'sway-herdr-init'
require_fixed PKGBUILD 'install -Dm644 contrib/completions/bash/sway-session "$pkgdir/usr/share/bash-completion/completions/sway-session"'
require_fixed PKGBUILD 'install -Dm644 contrib/completions/zsh/_sway-session "$pkgdir/usr/share/zsh/site-functions/_sway-session"'
require_fixed PKGBUILD 'install -Dm644 contrib/completions/fish/sway-session.fish "$pkgdir/usr/share/fish/vendor_completions.d/sway-session.fish"'
require_count contrib/sway/45-title-animator.conf 'exec_always --no-startup-id /usr/bin/sway-title-animator --replace --fps 25' 1
require_count contrib/sway/45-title-animator.conf 'exec --no-startup-id /usr/bin/sway-session daemon' 1
require_count contrib/sway/45-title-animator.conf 'exec --no-startup-id /usr/bin/sway-session restore' 1
require_fixed contrib/sway/45-title-animator.conf '# exec, not exec_always: a config reload must not request another restore.'
require_count contrib/sway/45-title-animator.conf '# bindsym $mod+Ctrl+p exec --no-startup-id /usr/bin/sway-session app register-focused' 1
require_count contrib/sway/45-title-animator.conf 'bindsym $mod+Return exec --no-startup-id /usr/bin/sway-session terminal --new' 1
require_count contrib/sway/45-title-animator.conf 'bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral' 1
reject_fixed contrib/sway/45-title-animator.conf 'exec_always --no-startup-id /usr/bin/sway-session daemon'
reject_fixed contrib/sway/45-title-animator.conf 'exec_always --no-startup-id /usr/bin/sway-session restore'
reject_regex contrib/sway/45-title-animator.conf '^[[:space:]]*bindsym \$mod\+Ctrl\+p exec --no-startup-id /usr/bin/sway-session app register-focused[[:space:]]*$'
require_fixed .goreleaser.yaml 'contrib/codex/hooks.json'
require_fixed .goreleaser.yaml 'contrib/codex/hooks-system.json'
require_fixed PKGBUILD 'contrib/codex/hooks-system.json'
reject_regex .goreleaser.yaml '^[[:space:]]+(recommends|suggests):'
reject_fixed .goreleaser.yaml '      - sqlite'
reject_fixed .goreleaser.yaml '      - golang'
require_fixed .goreleaser.yaml '      - src: ./contrib/sway/45-title-animator.conf'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/45-title-animator.conf'
require_fixed .goreleaser.yaml '      - src: ./contrib/herdr/config.toml'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/contrib/herdr/config.toml'
require_fixed .goreleaser.yaml '      - src: ./contrib/sway-session/config.toml'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/contrib/sway-session/config.toml'
require_fixed .goreleaser.yaml '      - src: ./contrib/codex/hooks-system.json'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/contrib/codex/hooks.json'
require_fixed .goreleaser.yaml '      - src: ./contrib/completions/bash/sway-session'
require_fixed .goreleaser.yaml '        dst: /usr/share/bash-completion/completions/sway-session'
require_fixed .goreleaser.yaml '      - src: ./contrib/completions/zsh/_sway-session'
require_fixed .goreleaser.yaml '        dst: /usr/share/zsh/vendor-completions/_sway-session'
require_fixed .goreleaser.yaml '        dst: /usr/share/zsh/site-functions/_sway-session'
require_fixed .goreleaser.yaml '        packager: deb'
require_fixed .goreleaser.yaml '        packager: rpm'
require_fixed .goreleaser.yaml '      - src: ./contrib/completions/fish/sway-session.fish'
require_fixed .goreleaser.yaml '        dst: /usr/share/fish/vendor_completions.d/sway-session.fish'
require_count .goreleaser.yaml 'contrib/completions/bash/sway-session' 2
require_count .goreleaser.yaml 'contrib/completions/zsh/_sway-session' 3
require_count .goreleaser.yaml 'contrib/completions/fish/sway-session.fish' 2
require_count .goreleaser.yaml '        dst: /usr/share/zsh/vendor-completions/_sway-session' 1
require_count .goreleaser.yaml '        dst: /usr/share/zsh/site-functions/_sway-session' 1
require_count .goreleaser.yaml '        packager: deb' 1
require_count .goreleaser.yaml '        packager: rpm' 1
require_sequence .goreleaser.yaml \
	'      - src: ./contrib/completions/zsh/_sway-session' \
	'        dst: /usr/share/zsh/vendor-completions/_sway-session' \
	'        packager: deb'
require_sequence .goreleaser.yaml \
	'      - src: ./contrib/completions/zsh/_sway-session' \
	'        dst: /usr/share/zsh/site-functions/_sway-session' \
	'        packager: rpm'
reject_fixed .goreleaser.yaml '      - src: ./contrib/codex/hooks.json'
require_fixed .goreleaser.yaml '      - src: ./contrib/apparmor/codex-home-guard'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/contrib/apparmor/codex-home-guard'
require_fixed .goreleaser.yaml '      - src: ./scripts/verify-codex-boundary.sh'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-title-animator/scripts/verify-codex-boundary.sh'
require_fixed PKGBUILD 'install -Dm644 contrib/sway/45-title-animator.conf "$pkgdir/usr/share/doc/$pkgname/45-title-animator.conf"'
require_fixed PKGBUILD 'install -Dm644 contrib/herdr/config.toml "$pkgdir/usr/share/doc/$pkgname/contrib/herdr/config.toml"'
require_fixed PKGBUILD 'install -Dm644 contrib/sway-session/config.toml "$pkgdir/usr/share/doc/$pkgname/contrib/sway-session/config.toml"'
require_fixed PKGBUILD 'install -Dm644 contrib/codex/hooks-system.json "$pkgdir/usr/share/doc/$pkgname/contrib/codex/hooks.json"'
require_fixed PKGBUILD 'install -Dm644 contrib/apparmor/codex-home-guard "$pkgdir/usr/share/doc/$pkgname/contrib/apparmor/codex-home-guard"'
require_fixed .goreleaser.yaml 'mode: 0755'
require_fixed PKGBUILD 'install -Dm755 scripts/verify-codex-boundary.sh'
require_fixed Makefile 'install -m644 contrib/completions/bash/sway-session $(PREFIX)/share/bash-completion/completions/sway-session'
require_fixed Makefile 'install -m644 contrib/completions/zsh/_sway-session $(PREFIX)/share/zsh/site-functions/_sway-session'
require_fixed Makefile 'install -m644 contrib/completions/fish/sway-session.fish $(PREFIX)/share/fish/vendor_completions.d/sway-session.fish'
require_fixed Makefile 'install -m644 contrib/sway-session/config.toml $(PREFIX)/share/doc/sway-title-animator/contrib/sway-session/config.toml'
require_fixed contrib/sway-session/config.toml 'version = 2'
require_fixed contrib/sway-session/config.toml 'session_manager = "herdr"'
reject_regex contrib/sway-session/config.toml '(^|[[:space:]])(command|executable|args|environment)[[:space:]]*='
require_fixed scripts/verify-codex-boundary.sh 'PATH=/usr/bin'
require_fixed scripts/verify-codex-boundary.sh 'session_binary=/usr/bin/sway-session'
reject_fixed scripts/verify-codex-boundary.sh 'sway-herdr-init'
require_fixed scripts/verify-codex-boundary.sh 'require_packaged_binary "$session_binary"'
require_fixed .github/workflows/aur.yml "grep -F 'SKIP' PKGBUILD"
require_fixed .github/workflows/aur.yml 'sed -i "s/^pkgrel=.*/pkgrel=1/" PKGBUILD'
require_fixed .github/workflows/aur.yml "grep -Fx 'pkgrel=1' PKGBUILD"
require_fixed .github/workflows/aur.yml 'makepkg --syncdeps --cleanbuild --clean --noconfirm'
require_fixed .github/workflows/aur.yml 'workflow_dispatch:'
require_fixed .github/workflows/aur.yml '          - verify-sync-token'
require_fixed .github/workflows/aur.yml "if: \${{ github.event_name == 'push' || inputs.operation == 'publish-release' }}"
require_fixed .github/workflows/aur.yml "if: \${{ github.event_name == 'workflow_dispatch' && inputs.operation == 'verify-sync-token' }}"
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
require_fixed .github/workflows/aur.yml 'branch="automation/release-sync-check-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"'
require_fixed .github/workflows/aur.yml 'gh pr close "$pr_url" --repo "$GITHUB_REPOSITORY"'
require_fixed .github/workflows/aur.yml 'printf '\''probe_sha=%s\n'\'' "$(git rev-parse HEAD)" >> "$GITHUB_OUTPUT"'
require_fixed .github/workflows/aur.yml 'if ! remote_ref=$(git ls-remote --heads origin \'
require_fixed .github/workflows/aur.yml 'if [ -n "$remote_sha" ] && [ "$remote_sha" != "$PROBE_SHA" ]; then'
require_fixed .github/workflows/aur.yml '--force-with-lease="refs/heads/${PROBE_BRANCH}:${PROBE_SHA}"'
require_fixed .github/workflows/aur.yml 'origin ":refs/heads/${PROBE_BRANCH}"; then'
require_fixed .github/workflows/aur.yml 'GH_TOKEN: ${{ github.token }}'
require_fixed .github/workflows/aur.yml 'timeout 10m gh run watch "$run_id" \'
require_fixed .github/workflows/aur.yml 'cp release-metadata/SRCINFO .SRCINFO'
require_fixed .github/workflows/aur.yml 'git checkout -B "$branch" origin/main'
require_fixed .github/workflows/aur.yml '--force-with-lease="refs/heads/${branch}:${remote_sha}"'
require_fixed .github/workflows/aur.yml 'aur.archlinux.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuBKrPzbawxA/k2g6NcyV5jmqwJ2s+zpgZGZ7tpLIcN'
require_fixed .github/workflows/aur.yml 'SHA256:RFzBCUItH9LZS0cKB5UE6ceAYhBD5C8GeOBip8Z11+4'
require_fixed .github/workflows/aur.yml 'StrictHostKeyChecking=yes'
reject_fixed .github/workflows/aur.yml 'ssh-keyscan'
reject_fixed .github/workflows/aur.yml 'StrictHostKeyChecking=accept-new'

if git check-ignore --no-index --quiet .SRCINFO; then
	echo '.SRCINFO must remain trackable for release metadata sync PRs.' >&2
	exit 1
fi

for asset in \
	contrib/sway/45-title-animator.conf \
	contrib/herdr/config.toml \
	contrib/sway-session/config.toml \
	contrib/apparmor/codex-home-guard \
	scripts/verify-codex-boundary.sh
do
	require_fixed .goreleaser.yaml "$asset"
	require_fixed PKGBUILD "$asset"
done
