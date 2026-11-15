#!/usr/bin/env bash
# Tests scripts/previous-release-base.sh: which previous release
# test-previous-release-schema.sh checks the target against. The history is a
# throwaway git repository and the registry read is a stub, so this needs no
# network and no database.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT

# shellcheck source=previous-release-base.sh
source "$repo_root/scripts/previous-release-base.sh"

# The stub stands in for the anonymous registry read. STUB is the revision the
# deploy image tag carries, or rc:<n> for a read that returns status n.
STUB=''
registry_reads=$tmp/registry-reads
: > "$registry_reads"
registry_revision() {
	printf 'read\n' >> "$registry_reads"
	case "$STUB" in
	rc:*) return "${STUB#rc:}" ;;
	*) printf '%s\n' "$STUB" ;;
	esac
}

fail() {
	printf 'test-previous-release-base: %s\n' "$*" >&2
	exit 1
}

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid

# History: A <- B <- C (the target, HEAD). S branches off A and is not an
# ancestor of C. v0.1.0 is A, v0.2.0 and the deploy git tag are B.
repo=$tmp/repo
git init -q -b main "$repo"
cd "$repo"
git commit -q --allow-empty -m a
A=$(git rev-parse HEAD)
git tag v0.1.0
git commit -q --allow-empty -m b
B=$(git rev-parse HEAD)
git tag v0.2.0
git tag deploy
git commit -q --allow-empty -m c
C=$(git rev-parse HEAD)
git checkout -q -b side "$A"
git commit -q --allow-empty -m s
S=$(git rev-parse HEAD)
git checkout -q main
U=1111111111111111111111111111111111111111

# run <stub> [explicit-base]: one selection, its result in rc, base_*, err.
run() {
	STUB=$1
	rc=0
	base_commit=''
	base_rule=''
	select_previous_release "$C" "${2:-}" 2> "$tmp/err" > /dev/null || rc=$?
	err=$(cat "$tmp/err")
}

# expect <name> <rc> <commit> <rule-prefix> [err-substring]
expect() {
	local name=$1 want_rc=$2 want_commit=$3 want_rule=$4 want_err=${5:-}
	[[ $rc -eq $want_rc ]] || fail "$name: rc=$rc, want $want_rc ($err)"
	if [[ $want_rc -eq 0 ]]; then
		[[ "$base_commit" == "$want_commit" ]] || fail "$name: base=$base_commit, want $want_commit ($err)"
		[[ "$base_rule" == "$want_rule"* ]] || fail "$name: rule='$base_rule', want prefix '$want_rule'"
	fi
	if [[ -n "$want_err" ]]; then
		[[ "$err" == *"$want_err"* ]] || fail "$name: stderr lacks '$want_err': $err"
	fi
	printf 'ok - %s\n' "$name"
}

run "$A"
expect 'image revision is an ancestor: chosen' 0 "$A" 'deploy image tag'

run "$C"
expect 'image revision is the target: git deploy tag' 0 "$B" 'deploy tag' 'names the target itself'

run "$S"
expect 'image revision is not an ancestor: git deploy tag' 0 "$B" 'deploy tag' 'not an ancestor of the target'

run "$U"
expect 'image revision is not in the checkout: git deploy tag' 0 "$B" 'deploy tag' 'not in this checkout'

run 'rc:1'
expect 'registry unreachable: git deploy tag, reason logged' 0 "$B" 'deploy tag' 'could not be read from the registry'

run 'rc:3'
expect 'no deploy image tag: git deploy tag' 0 "$B" 'deploy tag' 'no deploy image tag'

run 'not-a-sha'
expect 'tag carries no valid revision: git deploy tag' 0 "$B" 'deploy tag' 'carries no valid revision'

# Without a deploy git tag and with the registry down, the newest v* tag wins.
git tag -d deploy > /dev/null
run 'rc:1'
expect 'registry unreachable and no deploy tag: newest v* tag' 0 "$B" 'newest v* tag' 'there is no deploy tag'
git tag deploy "$B"

# An explicit base wins and never consults the registry.
reads_before=$(wc -l < "$registry_reads")
run "$A" "$B"
expect 'explicit base wins over the registry' 0 "$B" 'explicit argument'
[[ $(wc -l < "$registry_reads") -eq $reads_before ]] || fail 'explicit base still read the registry'
printf 'ok - explicit base does not read the registry\n'

run "$A" "$C"
expect 'explicit base equal to the target is refused' 1 '' '' 'is the target'

run "$A" "$U"
expect 'explicit base not in the checkout is refused' 1 '' '' 'is not a commit in this checkout'

printf 'previous-release base: all cases passed\n'
