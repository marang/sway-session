#!/bin/sh
set -eu

require_fixed() {
	file=$1
	value=$2
	grep -F -- "$value" "$file" >/dev/null || {
		echo "Missing packaging contract in $file: $value" >&2
		exit 1
	}
}

reject_fixed() {
	file=$1
	value=$2
	if grep -F -- "$value" "$file" >/dev/null; then
		echo "Forbidden standalone packaging content in $file: $value" >&2
		exit 1
	fi
}

require_count() {
	file=$1
	value=$2
	expected=$3
	actual=$(grep -F -c -- "$value" "$file" || true)
	if [ "$actual" -ne "$expected" ]; then
		echo "Unexpected count in $file for '$value': got $actual, want $expected" >&2
		exit 1
	fi
}

require_ordered_lines() {
	file=$1
	shift
	previous=0
	for value in "$@"; do
		line=$(grep -n -F -- "$value" "$file" | awk -F: -v previous="$previous" '$1 > previous { print $1; exit }')
		if [ -z "$line" ]; then
			echo "Missing ordered packaging contract in $file after line $previous: $value" >&2
			exit 1
		fi
		previous=$line
	done
}

test -f contrib/sway/50-sway-session.conf
test -f contrib/sway-session/config.toml
test -f contrib/herdr/config.toml
test -f contrib/codex/hooks.json
test -f contrib/codex/hooks-system.json
test -x contrib/codex/report-agent-session.sh
test -f contrib/apparmor/agent-home-guard
test -f scripts/verify-codex-boundary.sh
test -f docs/agent-reporting.md
require_fixed Makefile 'install -m644 docs/agent-reporting.md $(DOC_ROOT)/docs/agent-reporting.md'
require_fixed PKGBUILD 'install -Dm644 docs/agent-reporting.md "$pkgdir/usr/share/doc/$pkgname/docs/agent-reporting.md"'
require_fixed .goreleaser.yaml '      - docs/agent-reporting.md'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-session/docs/agent-reporting.md'

require_fixed .goreleaser.yaml 'project_name: sway-session'
require_count .goreleaser.yaml '  - id: sway-session' 1
require_fixed .goreleaser.yaml '    ids: [sway-session]'
require_fixed .goreleaser.yaml '    package_name: sway-session'
require_fixed .goreleaser.yaml '    homepage: "https://github.com/marang/sway-session"'
require_fixed .goreleaser.yaml '    name: sway-session'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-session/docs/sway-session-plan.md'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-session/docs/adr/0001-sqlite-session-runtime-state.md'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-session/50-sway-session.conf'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-session/contrib/codex/hooks.json'
require_fixed .goreleaser.yaml '        dst: /usr/lib/sway-session/codex-report-agent-session'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-session/contrib/apparmor/agent-home-guard'
require_ordered_lines README.md \
  'sudo apparmor_parser -R /etc/apparmor.d/codex-home-guard' \
  'sudo mv /etc/apparmor.d/codex-home-guard' \
  '/root/codex-home-guard.before-agent-home-guard' \
  'sudo install -m 0644' \
  '/usr/share/doc/sway-session/contrib/apparmor/agent-home-guard' \
  '/etc/apparmor.d/agent-home-guard' \
  'sudo apparmor_parser -r /etc/apparmor.d/agent-home-guard'
require_fixed .goreleaser.yaml '        dst: /usr/share/doc/sway-session/scripts/verify-codex-boundary.sh'
reject_fixed .goreleaser.yaml 'sway-title-animator'
reject_fixed .goreleaser.yaml 'pulseaudio'
reject_fixed .goreleaser.yaml 'parec'
reject_fixed .goreleaser.yaml '      - jq'

require_fixed PKGBUILD 'pkgname=sway-session'
require_fixed PKGBUILD 'url="https://github.com/marang/sway-session"'
require_fixed PKGBUILD "depends=('sway')"
require_fixed PKGBUILD "makedepends=('go>=1.26.5')"
require_fixed PKGBUILD 'CGO_ENABLED=0 go build'
require_fixed PKGBUILD '-o sway-session ./cmd/sway-session'
require_fixed PKGBUILD 'install -Dm755 sway-session "$pkgdir/usr/bin/sway-session"'
require_fixed PKGBUILD '_install_codex_hook'
require_fixed PKGBUILD 'if (( $(vercmp "$pkgver" 0.2.0) <= 0 )); then'
require_fixed PKGBUILD 'install -Dm755 "$hook" "$pkgdir/usr/lib/sway-session/codex-report-agent-session"'
require_fixed PKGBUILD '"$pkgdir/usr/share/doc/$pkgname/50-sway-session.conf"'
reject_fixed PKGBUILD 'optdepends='
reject_fixed PKGBUILD "'jq'"
reject_fixed PKGBUILD 'sway-title-animator'
reject_fixed PKGBUILD './cmd/sway-title-animator'

require_fixed .SRCINFO 'pkgbase = sway-session'
require_fixed .SRCINFO 'pkgname = sway-session'
require_fixed .SRCINFO 'depends = sway'
reject_fixed .SRCINFO 'optdepends ='
reject_fixed .SRCINFO 'sway-title-animator'
reject_fixed .SRCINFO 'depends = jq'

# Before the first v0.1.0 tag, SKIP is honest bootstrap state. Release metadata
# may instead contain only the real 64-hex digest generated from an immutable
# tag archive. Later metadata-sync PRs must continue to pass this gate.
pkgver=$(sed -n 's/^pkgver=//p' PKGBUILD)
printf '%s\n' "$pkgver" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
	echo "PKGBUILD has an invalid semantic version: $pkgver" >&2
	exit 1
}
checksum=$(sed -n "s/^sha256sums=('\\([^']*\\)').*/\\1/p" PKGBUILD)
if [ "$checksum" = SKIP ]; then
	test "$pkgver" = 0.1.0 || {
		echo 'SKIP is allowed only in the pre-v0.1.0 bootstrap template.' >&2
		exit 1
	}
else
	printf '%s\n' "$checksum" | grep -Eq '^[0-9a-f]{64}$' || {
		echo 'PKGBUILD checksum is neither bootstrap SKIP nor a SHA-256 digest.' >&2
		exit 1
	}
fi
require_fixed .github/workflows/aur.yml 'sed -i "s/^sha256sums=.*/sha256sums=('
require_fixed .github/workflows/aur.yml "if grep -F 'SKIP' PKGBUILD"
# Exercise the release substitution, including comments: the workflow's
# fail-closed guard scans the complete recipe, not just the checksum field.
release_test_sum=$(printf '%s' 'packaging regression fixture' | sha256sum | cut -d' ' -f1)
release_recipe=$(sed "s/^sha256sums=.*/sha256sums=('${release_test_sum}')/" PKGBUILD)
if printf '%s\n' "$release_recipe" | grep -F 'SKIP' >/dev/null; then
	echo 'Release checksum substitution leaves a recipe rejected by the AUR guard.' >&2
	exit 1
fi
require_fixed .github/workflows/aur.yml 'PKGNAME: sway-session'
require_fixed .github/workflows/aur.yml 'AUR_REPO: sway-session'
reject_fixed .github/workflows/aur.yml 'sway-title-animator'
require_fixed .github/workflows/aur.yml 'show-ref --verify --quiet "refs/tags/$VERSION"'
require_fixed .github/workflows/aur.yml 'rev-parse HEAD'
require_fixed .github/workflows/aur.yml 'merge-base --is-ancestor "$tag_commit" origin/main'
require_fixed .github/workflows/aur.yml 'curl -fsSL "$tarball" | sha256sum'
require_fixed .github/workflows/aur.yml 'makepkg --syncdeps --cleanbuild --clean --noconfirm'
require_fixed .github/workflows/aur.yml 'makepkg --printsrcinfo'
require_fixed .github/workflows/aur.yml 'aur.archlinux.org ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuBKrPzbawxA/k2g6NcyV5jmqwJ2s+zpgZGZ7tpLIcN'
require_fixed .github/workflows/aur.yml 'SHA256:RFzBCUItH9LZS0cKB5UE6ceAYhBD5C8GeOBip8Z11+4'
require_fixed .github/workflows/aur.yml 'secrets.RELEASE_SYNC_TOKEN'
require_fixed .github/workflows/aur.yml '--force-with-lease='
require_fixed .github/workflows/aur.yml 'gh pr close "$pr_url"'
require_fixed .github/workflows/release.yml 'merge-base --is-ancestor "$GITHUB_SHA" origin/main'
require_fixed .github/workflows/ci.yml 'sudo apt-get update && sudo apt-get install --yes fish jq zsh'

require_count contrib/sway/50-sway-session.conf 'exec --no-startup-id /usr/bin/sway-session daemon' 1
require_count contrib/sway/50-sway-session.conf 'exec --no-startup-id /usr/bin/sway-session restore' 1
require_count contrib/sway/50-sway-session.conf 'bindsym $mod+Return exec --no-startup-id /usr/bin/sway-session terminal --new' 1
require_count contrib/sway/50-sway-session.conf 'bindsym $mod+Shift+Return exec --no-startup-id /usr/bin/sway-session terminal --ephemeral' 1
reject_fixed contrib/sway/50-sway-session.conf 'exec_always'
reject_fixed contrib/sway/50-sway-session.conf 'sway-title-animator'

require_fixed Makefile 'BINARIES := sway-session'
require_fixed Makefile 'CGO_ENABLED=0 go build'
require_fixed Makefile 'install -m755 contrib/codex/report-agent-session.sh $(PREFIX)/lib/sway-session/codex-report-agent-session'
require_fixed Makefile '$(PREFIX)/share/doc/sway-session'
require_fixed Makefile 'install -m644 docs/sway-session-verification.md $(DOC_ROOT)/docs/sway-session-verification.md'
reject_fixed Makefile 'sway-title-animator'

require_fixed contrib/codex/hooks.json '\"$HOME/.local/lib/sway-session/codex-report-agent-session\" \"$HOME/.local/bin/sway-session\"'
require_fixed contrib/codex/hooks-system.json '/usr/lib/sway-session/codex-report-agent-session /usr/bin/sway-session'
reject_fixed contrib/codex/hooks.json 'report-codex-session'
reject_fixed contrib/codex/hooks-system.json 'report-codex-session'
reject_fixed .goreleaser.yaml 'codex-report.sock'

# Exercise both sides of the Arch compatibility guard. The current v0.2.0 tag
# archive may omit the adapter; every later source archive must include it.
bash -c '
	set -eu
	source "$1"
	test_root=$(mktemp -d)
	trap '\''rm -rf "$test_root"'\'' EXIT HUP INT TERM
	mkdir -p "$test_root/source/contrib/codex" "$test_root/pkg"
	cd "$test_root/source"
	pkgdir=$test_root/pkg
	vercmp() {
		case $1:$2 in
			0.1.0:0.2.0) printf '\''%s\n'\'' -1 ;;
			0.2.0:0.2.0) printf '\''%s\n'\'' 0 ;;
			0.2.1:0.2.0) printf '\''%s\n'\'' 1 ;;
			*) return 1 ;;
		esac
	}
	for compatible in 0.1.0 0.2.0; do
		pkgver=$compatible
		_install_codex_hook
	done
	test ! -e "$pkgdir/usr/lib/sway-session/codex-report-agent-session"
	pkgver=0.2.1
	if _install_codex_hook 2>/dev/null; then
		echo '\''Future Arch recipes accepted a missing Codex hook adapter.'\'' >&2
		exit 1
	fi
	printf '\''#!/bin/sh\n'\'' >contrib/codex/report-agent-session.sh
	chmod 755 contrib/codex/report-agent-session.sh
	_install_codex_hook
	test -x "$pkgdir/usr/lib/sway-session/codex-report-agent-session"
' packaging-guard "$PWD/PKGBUILD"

if command -v makepkg >/dev/null 2>&1; then
	generated=$(mktemp)
	trap 'rm -f "$generated"' EXIT HUP INT TERM
	makepkg --printsrcinfo >"$generated"
	if ! cmp -s "$generated" .SRCINFO; then
		echo '.SRCINFO is not the exact output of makepkg --printsrcinfo.' >&2
		diff -u .SRCINFO "$generated" || true
		exit 1
	fi
fi

if command -v goreleaser >/dev/null 2>&1; then
	goreleaser check
fi
