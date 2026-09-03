#!/bin/sh

set -eu

bash_completion=contrib/completions/bash/sway-session
zsh_completion=contrib/completions/zsh/_sway-session
fish_completion=contrib/completions/fish/sway-session.fish

for completion in "$bash_completion" "$zsh_completion" "$fish_completion"; do
	if [ ! -s "$completion" ]; then
		echo "missing shell completion: $completion" >&2
		exit 1
	fi
	for forbidden in 'contexts.json' 'jq ' 'eval '; do
		if grep -F -- "$forbidden" "$completion" >/dev/null; then
			echo "$completion contains forbidden completion dependency: $forbidden" >&2
			exit 1
		fi
	done
	if ! grep -F -- '--new' "$completion" >/dev/null; then
		echo "$completion omits the fresh persistent terminal option: --new" >&2
		exit 1
	fi
done

bash -n "$bash_completion"
if command -v zsh >/dev/null 2>&1; then
	zsh -n "$zsh_completion"
fi
if command -v fish >/dev/null 2>&1; then
	fish -n "$fish_completion"
fi

temporary=$(mktemp -d)
trap 'find "$temporary" -depth -delete' EXIT HUP INT TERM
sentinel=$temporary/executed-description
mkdir "$temporary/zsh"
mkdir "$temporary/path values"
touch "$temporary/path values/example file"

cat >"$temporary/sway-session" <<'EOF'
#!/bin/sh
if [ "${SWAY_SESSION_COMPLETION_FAIL:-}" = 1 ]; then
	exit 3
fi
if [ "$#" -eq 3 ] && [ "$1" = completion ] && [ "$2" = contexts ]; then
	if [ "$3" = restore-active ]; then
		printf '%s\t%s\n' \
			'11111111-1111-4111-8111-111111111111' 'First $(touch "$SWAY_SESSION_COMPLETION_SENTINEL") · active · herdr:first'
		exit 0
	fi
	printf '%s\t%s\n' \
		'11111111-1111-4111-8111-111111111111' 'First $(touch "$SWAY_SESSION_COMPLETION_SENTINEL") · active · herdr:first' \
		'22222222-2222-4222-8222-222222222222' 'Second · active · herdr:second' \
		'00000000-0000-0000-0000-000000000000' 'Nil UUID must be ignored' \
		'AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA' 'Uppercase UUID must be ignored'
	printf '%s\t%s\t%s\n' '33333333-3333-4333-8333-333333333333' 'Extra' 'field'
	exit 0
fi
exit 2
EOF
chmod 0755 "$temporary/sway-session"

PATH="$temporary:$PATH" \
	SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
	BASH_COMPLETION_SPACED_PATH="$temporary/path values/example" \
	BASH_COMPLETION_FILE="$bash_completion" \
	bash 2>"$temporary/bash-display" <<'EOF'
set -eu
source "$BASH_COMPLETION_FILE"

COMP_WORDS=(sway-session restore 111)
COMP_CWORD=2
COMP_TYPE=9
_sway_session
if [ "${#COMPREPLY[@]}" -ne 1 ] || [ "${COMPREPLY[0]}" != 11111111-1111-4111-8111-111111111111 ]; then
	printf 'bash completion did not insert only the canonical UUID: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMPREPLY=()
cur=$BASH_COMPLETION_SPACED_PATH
_sway_session_files
if [ "${#COMPREPLY[@]}" -ne 1 ] || [ "${COMPREPLY[0]}" != "$BASH_COMPLETION_SPACED_PATH file" ]; then
	printf 'bash path completion split a path containing spaces: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMPREPLY=()
_sway_session_contexts restore ''
if [ "${#COMPREPLY[@]}" -ne 2 ] || [ "${COMPREPLY[0]}" != 11111111-1111-4111-8111-111111111111 ] || [ "${COMPREPLY[1]}" != 22222222-2222-4222-8222-222222222222 ]; then
	printf 'bash multi-completion did not keep UUID-only insertions: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

SWAY_SESSION_COMPLETION_FAIL=1
COMP_WORDS=(sway-session '')
COMP_CWORD=1
COMP_TYPE=9
_sway_session
if [[ " ${COMPREPLY[*]} " != *' restore '* ]] || [[ " ${COMPREPLY[*]} " != *' terminal '* ]]; then
	printf 'bash static completion did not survive dynamic failure: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session restore '')
COMP_CWORD=2
_sway_session
if [[ " ${COMPREPLY[*]} " != *' --socket '* ]] || [[ " ${COMPREPLY[*]} " != *' --require-active '* ]]; then
	printf 'bash static options did not survive dynamic failure: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session terminal '')
COMP_CWORD=2
_sway_session
for expected in manage list status cleanup rename reconfigure --new --project --cwd --label --socket --ephemeral; do
	if [[ " ${COMPREPLY[*]} " != *" $expected "* ]]; then
		printf 'bash terminal completion omitted %s: %q\n' "$expected" "${COMPREPLY[*]-}" >&2
		exit 1
	fi
done

COMP_WORDS=(sway-session terminal manage '')
COMP_CWORD=3
_sway_session
if [[ " ${COMPREPLY[*]} " != *' --socket '* ]] || [[ " ${COMPREPLY[*]} " == *' --json '* ]]; then
	printf 'bash terminal manage completion is invalid: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session terminal rename --label 'Release work' 111)
COMP_CWORD=5
_sway_session
if [ "${#COMPREPLY[@]}" -ne 1 ] || [ "${COMPREPLY[0]}" != 11111111-1111-4111-8111-111111111111 ]; then
	printf 'bash terminal rename completion did not insert the canonical UUID: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session terminal reconfigure '')
COMP_CWORD=3
_sway_session
for expected in --project --socket; do
	if [[ " ${COMPREPLY[*]} " != *" $expected "* ]]; then
		printf 'bash terminal reconfigure completion omitted %s: %q\n' "$expected" "${COMPREPLY[*]-}" >&2
		exit 1
	fi
done

COMP_WORDS=(sway-session terminal status '')
COMP_CWORD=3
_sway_session
if [[ " ${COMPREPLY[*]} " != *' --project '* ]]; then
	printf 'bash terminal status completion omitted --project: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session --config /tmp terminal '')
COMP_CWORD=4
_sway_session
if [[ " ${COMPREPLY[*]} " != *' --project '* ]] || [[ " ${COMPREPLY[*]} " != *' --ephemeral '* ]]; then
	printf 'bash completion failed to skip global --config value: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session terminal cleanup '')
COMP_CWORD=3
_sway_session
if [[ " ${COMPREPLY[*]} " != *' --archived-before '* ]]; then
	printf 'bash terminal cleanup completion omitted --archived-before: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session completion contexts '')
COMP_CWORD=3
_sway_session
if [[ " ${COMPREPLY[*]} " != *' terminal-status '* ]]; then
	printf 'bash completion scope omitted terminal-status: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

unset SWAY_SESSION_COMPLETION_FAIL
COMP_WORDS=(sway-session terminal status 111)
COMP_CWORD=3
_sway_session
if [ "${#COMPREPLY[@]}" -ne 1 ] || [ "${COMPREPLY[0]}" != 11111111-1111-4111-8111-111111111111 ]; then
	printf 'bash terminal status completion did not insert the canonical UUID: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session -- '')
COMP_CWORD=2
_sway_session
if [ "${#COMPREPLY[@]}" -ne 0 ]; then
	printf 'bash completion proposed a command after the global option terminator: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session restore --require-active '')
COMP_CWORD=3
_sway_session
if [[ " ${COMPREPLY[*]} " != *' 11111111-1111-4111-8111-111111111111 '* ]] || [[ " ${COMPREPLY[*]} " == *' 22222222-2222-4222-8222-222222222222 '* ]]; then
	printf 'bash --require-active completion included an archived context: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session restore --socket --json '')
COMP_CWORD=4
_sway_session
if [[ " ${COMPREPLY[*]} " == *' 11111111-1111-4111-8111-111111111111 '* ]]; then
	printf 'bash completion consumed a transparent global option as the socket value: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi
COMP_WORDS=(sway-session restore --socket --json /tmp/sway.sock '')
COMP_CWORD=5
_sway_session
if [[ " ${COMPREPLY[*]} " != *' 11111111-1111-4111-8111-111111111111 '* ]]; then
	printf 'bash completion did not consume the socket value after a transparent global option: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

for invocation in 'restore --' 'app forget --'; do
	read -r -a COMP_WORDS <<<"sway-session $invocation "
	COMP_WORDS+=("")
	COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
	_sway_session
	if [[ " ${COMPREPLY[*]} " != *' 11111111-1111-4111-8111-111111111111 '* ]]; then
		printf 'bash completion omitted a pending context after -- for %s: %q\n' "$invocation" "${COMPREPLY[*]-}" >&2
		exit 1
	fi
	for invalid in --socket --require-active --yes --json --help -h; do
		if [[ " ${COMPREPLY[*]} " == *" $invalid "* ]]; then
			printf 'bash completion proposed %s after -- for %s: %q\n' "$invalid" "$invocation" "${COMPREPLY[*]-}" >&2
			exit 1
		fi
	done
done

for invocation in 'archive --' 'activate --'; do
	read -r -a COMP_WORDS <<<"sway-session $invocation "
	COMP_WORDS+=("")
	COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
	_sway_session
	if [ "${#COMPREPLY[@]}" -ne 0 ]; then
		printf 'bash completion treated -- as a terminator for non-FlagSet command %s: %q\n' "$invocation" "${COMPREPLY[*]-}" >&2
		exit 1
	fi
done

COMP_WORDS=(sway-session register --label --session -- '')
COMP_CWORD=5
_sway_session
if [ "${#COMPREPLY[@]}" -ne 0 ]; then
	printf 'bash completion failed to parse a flag-looking required value before --: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session register --label -- '')
COMP_CWORD=4
_sway_session
if [[ " ${COMPREPLY[*]} " != *' --session '* ]]; then
	printf 'bash completion omitted command options after -- was consumed as a value: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi
for invalid in --json --help -h; do
	if [[ " ${COMPREPLY[*]} " == *" $invalid "* ]]; then
		printf 'bash completion proposed closed global option %s after -- was consumed as a value: %q\n' "$invalid" "${COMPREPLY[*]-}" >&2
		exit 1
	fi
done
for invocation in 'register --label -- --session' 'register --label -- --json'; do
	read -r -a COMP_WORDS <<<"sway-session $invocation "
	COMP_WORDS+=("")
	COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
	_sway_session
	if [ "${#COMPREPLY[@]}" -ne 0 ]; then
		printf 'bash completion mishandled post-marker command grammar for %s: %q\n' "$invocation" "${COMPREPLY[*]-}" >&2
		exit 1
	fi
done

COMP_WORDS=(sway-session restore --require-active -- '')
COMP_CWORD=4
_sway_session
if [[ " ${COMPREPLY[*]} " != *' 11111111-1111-4111-8111-111111111111 '* ]] || [[ " ${COMPREPLY[*]} " == *' 22222222-2222-4222-8222-222222222222 '* ]]; then
	printf 'bash --require-active completion lost its scope across --: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session restore -- 11111111-1111-4111-8111-111111111111 -)
COMP_CWORD=4
_sway_session
if [ "${#COMPREPLY[@]}" -ne 0 ]; then
	printf 'bash completion proposed values after a terminated positional context: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

COMP_WORDS=(sway-session restore -- --json '')
COMP_CWORD=4
_sway_session
if [ "${#COMPREPLY[@]}" -ne 0 ]; then
	printf 'bash completion interpreted a flag-looking post-terminator value as an option: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi

for words_and_index in \
	'register --session|3' \
	'request-start --workspace|3' \
	'app register-focused --desktop-id|4' \
	'completion contexts archive|4'; do
	words=${words_and_index%|*}
	index=${words_and_index##*|}
	read -r -a COMP_WORDS <<<"sway-session $words "
	COMP_WORDS+=("")
	COMP_CWORD=$index
	_sway_session
	if [ "${#COMPREPLY[@]}" -ne 0 ]; then
		printf 'bash completion proposed flags/scopes as a required value for %s: %q\n' "$words" "${COMPREPLY[*]-}" >&2
		exit 1
	fi
done

for invocation in \
	'restore 11111111-1111-4111-8111-111111111111' \
	'purge 11111111-1111-4111-8111-111111111111' \
	'app forget 11111111-1111-4111-8111-111111111111'; do
	read -r -a COMP_WORDS <<<"sway-session $invocation -"
	COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
	_sway_session
	for invalid in --socket --require-active --yes; do
		if [[ " ${COMPREPLY[*]} " == *" $invalid "* ]]; then
			printf 'bash completion proposed %s after the positional context for %s: %q\n' "$invalid" "$invocation" "${COMPREPLY[*]-}" >&2
			exit 1
		fi
	done
done

COMP_WORDS=(sway-session completion contexts archive -)
COMP_CWORD=4
_sway_session
if [[ " ${COMPREPLY[*]} " != *' --json '* ]] || [[ " ${COMPREPLY[*]} " == *' restore '* ]]; then
	printf 'bash completion mishandled globals after a completed scope: %q\n' "${COMPREPLY[*]-}" >&2
	exit 1
fi
EOF

for description in 'First $(touch "$SWAY_SESSION_COMPLETION_SENTINEL") · active · herdr:first' 'Second · active · herdr:second'; do
	if ! grep -F -- "$description" "$temporary/bash-display" >/dev/null; then
		echo "bash completion did not display metadata: $description" >&2
		exit 1
	fi
done

if [ -e "$sentinel" ]; then
	echo 'completion evaluated presentation metadata as shell code' >&2
	exit 1
fi

if command -v zsh >/dev/null 2>&1; then
	PATH="$temporary:$PATH" \
		SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
		ZSH_COMPLETION_FILE="$zsh_completion" \
		ZDOTDIR="$temporary/zsh" \
		zsh -f <<'EOF'
set -eu
source "$ZSH_COMPLETION_FILE"
typeset -ga captured_values captured_descriptions
compadd() {
	if [[ $1 == -d ]]; then
		if [[ $2 != descriptions ]] || [[ $3 != -- ]]; then
			print -u2 -r -- "zsh completion called compadd with an unexpected description contract: $*"
			return 1
		fi
		shift 3
		captured_values+=("$@")
		captured_descriptions+=("${descriptions[@]}")
		return 0
	fi
	while (( $# )); do
		if [[ $1 == -- ]]; then
			shift
			captured_values+=("$@")
			return 0
		fi
		shift
	done
	return 1
}
_files() {
	return 0
}
_sway_session_contexts restore ''
if (( ${#captured_values} != 2 )) || [[ $captured_values[1] != 11111111-1111-4111-8111-111111111111 ]] || [[ $captured_values[2] != 22222222-2222-4222-8222-222222222222 ]]; then
	print -u2 -r -- "zsh completion did not preserve UUID-only insertion: ${(j:,:)captured_values}"
	exit 1
fi
if [[ $captured_descriptions[1] != *'First '* ]] || [[ $captured_descriptions[2] != *'Second '* ]]; then
	print -u2 -r -- "zsh completion did not preserve descriptions: ${(j:,:)captured_descriptions}"
	exit 1
fi

export SWAY_SESSION_COMPLETION_FAIL=1
captured_values=()
captured_descriptions=()
words=(sway-session restore '')
CURRENT=3
_sway-session
if (( ${captured_values[(Ie)--socket]} == 0 )) || (( ${captured_values[(Ie)--require-active]} == 0 )); then
	print -u2 -r -- "zsh static options did not survive dynamic failure: ${(j:,:)captured_values}"
	exit 1
fi
unset SWAY_SESSION_COMPLETION_FAIL

captured_values=()
captured_descriptions=()
words=(sway-session terminal manage '')
CURRENT=${#words}
_sway-session
if (( ${captured_values[(Ie)--socket]} == 0 )) || (( ${captured_values[(Ie)--json]} != 0 )); then
	print -u2 -r -- "zsh terminal manage completion is invalid: ${(j:,:)captured_values}"
	exit 1
fi

captured_values=()
captured_descriptions=()
words=(sway-session terminal rename --label 'Release work' 111)
CURRENT=${#words}
_sway-session
if (( ${captured_values[(Ie)11111111-1111-4111-8111-111111111111]} == 0 )); then
	print -u2 -r -- "zsh terminal rename completion omitted canonical UUID: ${(j:,:)captured_values}"
	exit 1
fi

captured_values=()
captured_descriptions=()
words=(sway-session -- '')
CURRENT=${#words}
_sway-session
if (( ${#captured_values} != 0 )); then
	print -u2 -r -- "zsh completion proposed a command after the global option terminator: ${(j:,:)captured_values}"
	exit 1
fi

captured_values=()
captured_descriptions=()
words=(sway-session restore --require-active '')
CURRENT=4
_sway-session
if (( ${captured_values[(Ie)11111111-1111-4111-8111-111111111111]} == 0 )) || (( ${captured_values[(Ie)22222222-2222-4222-8222-222222222222]} != 0 )); then
	print -u2 -r -- "zsh --require-active completion included an archived context: ${(j:,:)captured_values}"
	exit 1
fi

captured_values=()
captured_descriptions=()
words=(sway-session restore --socket --json '')
CURRENT=${#words}
_sway-session
if (( ${captured_values[(Ie)11111111-1111-4111-8111-111111111111]} != 0 )); then
	print -u2 -r -- "zsh completion consumed a transparent global option as the socket value: ${(j:,:)captured_values}"
	exit 1
fi
captured_values=()
captured_descriptions=()
words=(sway-session restore --socket --json /tmp/sway.sock '')
CURRENT=${#words}
_sway-session
if (( ${captured_values[(Ie)11111111-1111-4111-8111-111111111111]} == 0 )); then
	print -u2 -r -- "zsh completion did not consume the socket value after a transparent global option: ${(j:,:)captured_values}"
	exit 1
fi

for invocation in 'restore --' 'app forget --'; do
	captured_values=()
	captured_descriptions=()
	words=(sway-session ${(z)invocation} '')
	CURRENT=${#words}
	_sway-session
	if (( ${captured_values[(Ie)11111111-1111-4111-8111-111111111111]} == 0 )); then
		print -u2 -r -- "zsh completion omitted a pending context after -- for $invocation: ${(j:,:)captured_values}"
		exit 1
	fi
	for invalid in --socket --require-active --yes --json --help -h; do
		if (( ${captured_values[(Ie)$invalid]} != 0 )); then
			print -u2 -r -- "zsh completion proposed $invalid after -- for $invocation: ${(j:,:)captured_values}"
			exit 1
		fi
	done
done

for invocation in 'archive --' 'activate --'; do
	captured_values=()
	captured_descriptions=()
	words=(sway-session ${(z)invocation} '')
	CURRENT=${#words}
	_sway-session
	if (( ${#captured_values} != 0 )); then
		print -u2 -r -- "zsh completion treated -- as a terminator for non-FlagSet command $invocation: ${(j:,:)captured_values}"
		exit 1
	fi
done

captured_values=()
captured_descriptions=()
words=(sway-session register --label --session -- '')
CURRENT=${#words}
_sway-session
if (( ${#captured_values} != 0 )); then
	print -u2 -r -- "zsh completion failed to parse a flag-looking required value before --: ${(j:,:)captured_values}"
	exit 1
fi

captured_values=()
captured_descriptions=()
words=(sway-session register --label -- '')
CURRENT=${#words}
_sway-session
if (( ${captured_values[(Ie)--session]} == 0 )); then
	print -u2 -r -- "zsh completion omitted command options after -- was consumed as a value: ${(j:,:)captured_values}"
	exit 1
fi
for invalid in --json --help -h; do
	if (( ${captured_values[(Ie)$invalid]} != 0 )); then
		print -u2 -r -- "zsh completion proposed closed global option $invalid after -- was consumed as a value: ${(j:,:)captured_values}"
		exit 1
	fi
done
for invocation in 'register --label -- --session' 'register --label -- --json'; do
	captured_values=()
	captured_descriptions=()
	words=(sway-session ${(z)invocation} '')
	CURRENT=${#words}
	_sway-session
	if (( ${#captured_values} != 0 )); then
		print -u2 -r -- "zsh completion mishandled post-marker command grammar for $invocation: ${(j:,:)captured_values}"
		exit 1
	fi
done

captured_values=()
captured_descriptions=()
words=(sway-session restore --require-active -- '')
CURRENT=${#words}
_sway-session
if (( ${captured_values[(Ie)11111111-1111-4111-8111-111111111111]} == 0 )) || (( ${captured_values[(Ie)22222222-2222-4222-8222-222222222222]} != 0 )); then
	print -u2 -r -- "zsh --require-active completion lost its scope across --: ${(j:,:)captured_values}"
	exit 1
fi

captured_values=()
captured_descriptions=()
words=(sway-session restore -- 11111111-1111-4111-8111-111111111111 -)
CURRENT=${#words}
_sway-session
if (( ${#captured_values} != 0 )); then
	print -u2 -r -- "zsh completion proposed values after a terminated positional context: ${(j:,:)captured_values}"
	exit 1
fi

captured_values=()
captured_descriptions=()
words=(sway-session restore -- --json '')
CURRENT=${#words}
_sway-session
if (( ${#captured_values} != 0 )); then
	print -u2 -r -- "zsh completion interpreted a flag-looking post-terminator value as an option: ${(j:,:)captured_values}"
	exit 1
fi

for invocation in 'register --session' 'request-start --workspace' 'app register-focused --desktop-id' 'completion contexts archive'; do
	captured_values=()
	captured_descriptions=()
	words=(sway-session ${(z)invocation} '')
	CURRENT=${#words}
	_sway-session
	if (( ${#captured_values} != 0 )); then
		print -u2 -r -- "zsh completion proposed flags/scopes as a required value for $invocation: ${(j:,:)captured_values}"
		exit 1
	fi
done

for invocation in \
	'restore 11111111-1111-4111-8111-111111111111' \
	'purge 11111111-1111-4111-8111-111111111111' \
	'app forget 11111111-1111-4111-8111-111111111111'; do
	captured_values=()
	captured_descriptions=()
	words=(sway-session ${(z)invocation} -)
	CURRENT=${#words}
	_sway-session
	for invalid in --socket --require-active --yes; do
		if (( ${captured_values[(Ie)$invalid]} != 0 )); then
			print -u2 -r -- "zsh completion proposed $invalid after the positional context for $invocation: ${(j:,:)captured_values}"
			exit 1
		fi
	done
done

captured_values=()
captured_descriptions=()
words=(sway-session completion contexts archive -)
CURRENT=5
_sway-session
if (( ${captured_values[(Ie)--json]} == 0 )) || (( ${captured_values[(Ie)restore]} != 0 )); then
	print -u2 -r -- "zsh completion mishandled globals after a completed scope: ${(j:,:)captured_values}"
	exit 1
fi
EOF
fi

if [ -e "$sentinel" ]; then
	echo 'zsh completion evaluated presentation metadata as shell code' >&2
	exit 1
fi

if command -v fish >/dev/null 2>&1; then
	output=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
		fish -c "source '$fish_completion'; __sway_session_contexts restore")
	for expected in \
		'11111111-1111-4111-8111-111111111111' \
		'First $(touch "$SWAY_SESSION_COMPLETION_SENTINEL") · active · herdr:first' \
		'22222222-2222-4222-8222-222222222222' \
		'Second · active · herdr:second'; do
		case $output in
		*"$expected"*) ;;
		*)
			echo "fish completion did not preserve candidate field: $expected" >&2
			exit 1
			;;
		esac
	done
	active_output=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
		fish -c "source '$fish_completion'; complete -C 'sway-session restore --require-active '")
	manage_output=$(fish -c "source '$fish_completion'; complete -C 'sway-session terminal manage '")
	case $manage_output in
	*--socket*) ;;
	*)
		echo "fish terminal manage completion omitted --socket: $manage_output" >&2
		exit 1
		;;
	esac
	case $manage_output in
	*--json*)
		echo "fish terminal manage completion proposed --json: $manage_output" >&2
		exit 1
		;;
	esac
	manage_socket_value_output=$(fish -c "source '$fish_completion'; complete -C 'sway-session terminal manage --socket '")
	if printf '%s\n' "$manage_socket_value_output" | grep -Fx -- '--socket' >/dev/null; then
		echo "fish terminal manage completion proposed --socket as its own value: $manage_socket_value_output" >&2
		exit 1
	fi
	if [ -z "$manage_socket_value_output" ]; then
		echo "fish terminal manage completion omitted filesystem socket candidates" >&2
		exit 1
	fi
	manage_json_output=$(fish -c "source '$fish_completion'; complete -C 'sway-session terminal manage --j'")
	case $manage_json_output in
	*--json*)
		echo "fish terminal manage completion proposed unsupported --json prefix: $manage_json_output" >&2
		exit 1
		;;
	esac
	rename_output=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
		fish -c "source '$fish_completion'; complete -C 'sway-session terminal rename --label Release 111'")
	case $rename_output in
	*11111111-1111-4111-8111-111111111111*) ;;
	*)
		echo "fish terminal rename completion omitted canonical UUID: $rename_output" >&2
		exit 1
		;;
	esac
	case $active_output in
	*11111111-1111-4111-8111-111111111111*) ;;
	*)
		echo "fish --require-active completion omitted the active context: $active_output" >&2
		exit 1
		;;
	esac
	pending_socket=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
		fish -c "source '$fish_completion'; complete -C 'sway-session restore --socket --json '")
	case $pending_socket in
	*11111111-1111-4111-8111-111111111111*)
		echo "fish completion consumed a transparent global option as the socket value: $pending_socket" >&2
		exit 1
		;;
	esac
	completed_socket=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
		fish -c "source '$fish_completion'; complete -C 'sway-session restore --socket --json /tmp/sway.sock '")
	case $completed_socket in
	*11111111-1111-4111-8111-111111111111*) ;;
	*)
		echo "fish completion did not consume the socket value after a transparent global option: $completed_socket" >&2
		exit 1
		;;
	esac
	case $active_output in
	*22222222-2222-4222-8222-222222222222*)
		echo "fish --require-active completion included an archived context: $active_output" >&2
		exit 1
		;;
	esac
	for invocation in 'restore -- ' 'app forget -- '; do
		terminated_output=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
			fish -c "source '$fish_completion'; complete -C 'sway-session $invocation'")
		case $terminated_output in
		*11111111-1111-4111-8111-111111111111*) ;;
		*)
			echo "fish completion omitted a pending context after -- for $invocation: $terminated_output" >&2
			exit 1
			;;
		esac
		for invalid in --socket --require-active --yes --json --help; do
			case $terminated_output in
			*"$invalid"*)
				echo "fish completion proposed $invalid after -- for $invocation: $terminated_output" >&2
				exit 1
				;;
			esac
		done
	done
	for invocation in 'archive -- ' 'activate -- '; do
		non_flagset_marker=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
			fish -c "source '$fish_completion'; complete -C 'sway-session $invocation'")
		if [ -n "$non_flagset_marker" ]; then
			echo "fish completion treated -- as a terminator for non-FlagSet command $invocation: $non_flagset_marker" >&2
			exit 1
		fi
	done
	sequential_marker=$(fish -c "source '$fish_completion'; complete -C 'sway-session register --label --session -- '")
	if [ -n "$sequential_marker" ]; then
		echo "fish completion failed to parse a flag-looking required value before --: $sequential_marker" >&2
		exit 1
	fi
	value_marker=$(fish -c "source '$fish_completion'; complete -C 'sway-session register --label -- '")
	case $value_marker in
	*--session*) ;;
	*)
		echo "fish completion omitted command options after -- was consumed as a value: $value_marker" >&2
		exit 1
		;;
	esac
	for invalid in --json --help; do
		case $value_marker in
		*"$invalid"*)
			echo "fish completion proposed closed global option $invalid after -- was consumed as a value: $value_marker" >&2
			exit 1
			;;
		esac
	done
	for invocation in 'register --label -- --session ' 'register --label -- --json '; do
		post_marker=$(fish -c "source '$fish_completion'; complete -C 'sway-session $invocation'")
		if [ -n "$post_marker" ]; then
			echo "fish completion mishandled post-marker command grammar for $invocation: $post_marker" >&2
			exit 1
		fi
	done
	terminated_active=$(PATH="$temporary:$PATH" SWAY_SESSION_COMPLETION_SENTINEL="$sentinel" \
		fish -c "source '$fish_completion'; complete -C 'sway-session restore --require-active -- '")
	case $terminated_active in
	*11111111-1111-4111-8111-111111111111*) ;;
	*)
		echo "fish --require-active completion lost its scope across --: $terminated_active" >&2
		exit 1
		;;
	esac
	case $terminated_active in
	*22222222-2222-4222-8222-222222222222*)
		echo "fish --require-active completion included an archived context after --: $terminated_active" >&2
		exit 1
		;;
	esac
	terminated_exhausted=$(fish -c "source '$fish_completion'; complete -C 'sway-session restore -- 11111111-1111-4111-8111-111111111111 -'")
	if [ -n "$terminated_exhausted" ]; then
		echo "fish completion proposed values after a terminated positional context: $terminated_exhausted" >&2
		exit 1
	fi
	terminated_flag_value=$(fish -c "source '$fish_completion'; complete -C 'sway-session restore -- --json '")
	if [ -n "$terminated_flag_value" ]; then
		echo "fish completion interpreted a flag-looking post-terminator value as an option: $terminated_flag_value" >&2
		exit 1
	fi
	static_output=$(fish -c "source '$fish_completion'; complete -C 'sway-session '")
	for expected in restore app completion; do
		case $static_output in
		*"$expected"*) ;;
		*)
			echo "fish top-level completion is missing: $expected" >&2
			exit 1
			;;
		esac
	done
	terminated_top=$(fish -c "source '$fish_completion'; complete -C 'sway-session -- '")
	if [ -n "$terminated_top" ]; then
		echo "fish completion proposed a command after the global option terminator: $terminated_top" >&2
		exit 1
	fi
	app_output=$(fish -c "source '$fish_completion'; complete -C 'sway-session app '")
	for expected in register-focused rebind-focused forget; do
		case $app_output in
		*"$expected"*) ;;
		*)
			echo "fish app completion is missing: $expected" >&2
			exit 1
			;;
			esac
	done
	required_output=$(fish -c "source '$fish_completion'; complete -C 'sway-session register --session '")
	if [ -n "$required_output" ]; then
		echo "fish completion proposed filesystem/options as a session value: $required_output" >&2
		exit 1
	fi
	exhausted_output=$(fish -c "source '$fish_completion'; complete -C 'sway-session completion contexts archive '")
	if [ -n "$exhausted_output" ]; then
		echo "fish completion proposed another value after the completion scope: $exhausted_output" >&2
		exit 1
	fi
	exhausted_globals=$(fish -c "source '$fish_completion'; complete -C 'sway-session completion contexts archive -'")
	case $exhausted_globals in
	*--json*) ;;
	*)
		echo "fish completion omitted globals after a completed scope: $exhausted_globals" >&2
		exit 1
		;;
	esac
	case $exhausted_globals in
	*restore*)
		echo "fish completion reoffered scopes after a completed scope: $exhausted_globals" >&2
		exit 1
		;;
	esac
	for invocation in \
		'restore 11111111-1111-4111-8111-111111111111 -' \
		'purge 11111111-1111-4111-8111-111111111111 -' \
		'app rebind-focused 11111111-1111-4111-8111-111111111111 -'; do
		post_context=$(fish -c "source '$fish_completion'; complete -C 'sway-session $invocation'")
		for invalid in --socket --require-active --yes --desktop-id; do
			case $post_context in
			*"$invalid"*)
				echo "fish completion proposed $invalid after the positional context for $invocation: $post_context" >&2
				exit 1
				;;
			esac
		done
	done
fi

if [ -e "$sentinel" ]; then
	echo 'fish completion evaluated presentation metadata as shell code' >&2
	exit 1
fi
