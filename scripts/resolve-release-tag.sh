#!/bin/sh
# Resolve a checked-out release tag using only local refs; never fetch or checkout.
set -eu

LC_ALL=C
export LC_ALL

fail() {
    printf '%s\n' "$1" >&2
    exit 1
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    fail 'Usage: resolve-release-tag.sh TAG [EXPECTED_FULL_SHA]'
fi

tag=$1
# Reject newlines as well as shell/Git syntax before matching the whole tag.
# grep alone would accept one valid line in a multiline argument.
case "$tag" in
    ''|*[!v0-9.]*) fail 'Refusing malformed release tag.' ;;
esac
if ! printf '%s\n' "$tag" | \
    grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    fail 'Refusing malformed release tag.'
fi

expected_commit=
if [ "$#" -eq 2 ]; then
    expected_commit=$2
    if [ "${#expected_commit}" -ne 40 ]; then
        fail 'Expected release commit must be a full 40-character hexadecimal SHA.'
    fi
    case "$expected_commit" in
        *[!0-9a-fA-F]*)
            fail 'Expected release commit must be a full 40-character hexadecimal SHA.'
            ;;
    esac
    expected_commit=$(printf '%s' "$expected_commit" | tr 'A-F' 'a-f')
fi

# The AUR container may run under a different UID than actions/checkout.
# Trust only this worktree for each command, without changing Git configuration.
worktree=$(pwd -P)
git_local() {
    git -c safe.directory="$worktree" "$@"
}

tag_ref="refs/tags/$tag"
if ! git_local show-ref --verify --quiet "$tag_ref"; then
    fail "Refusing unknown release tag: $tag"
fi
if ! tag_commit=$(git_local rev-parse --verify --end-of-options \
    "${tag_ref}^{commit}" 2>/dev/null); then
    fail "Release tag does not resolve to a commit: $tag"
fi
if ! head_commit=$(git_local rev-parse --verify --end-of-options \
    'HEAD^{commit}' 2>/dev/null); then
    fail 'Checkout does not resolve to a commit.'
fi
if [ "$head_commit" != "$tag_commit" ]; then
    fail "Checkout does not match release tag $tag."
fi
if [ -n "$expected_commit" ] && [ "$expected_commit" != "$tag_commit" ]; then
    fail "Release tag $tag does not match the expected commit."
fi
if ! main_commit=$(git_local rev-parse --verify --end-of-options \
    'refs/remotes/origin/main^{commit}' 2>/dev/null); then
    fail 'The local origin/main ref is required to validate release ancestry.'
fi
if ! git_local merge-base --is-ancestor "$tag_commit" "$main_commit"; then
    fail 'Refusing to publish a tag whose commit is not on main.'
fi

printf 'tag=%s\ncommit=%s\n' "$tag" "$tag_commit"
