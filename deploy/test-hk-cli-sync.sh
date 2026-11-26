#!/usr/bin/env bash
# Tests for deploy/hk-cli-sync/hk-cli-sync against fake gh, ssh and scp on PATH.
# The fake ssh runs each remote command under a per-host fake HOME, and the
# fake scp copies into that HOME, so the install steps (checksum, backup,
# atomic mv) really run against files. Connectivity and the serving host's
# healthz are the only things faked.
#
#   bash deploy/test-hk-cli-sync.sh
set -uo pipefail

# A caller's serving-host settings must not leak into the cases below; each
# case sets what it needs.
unset HK_CLI_SERVER HK_CLI_SERVER_HEALTH HK_CLI_NCP HK_CLI_NCP_HEALTH

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/hk-cli-sync"
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
FAILS=0 PASSES=0

REPO=mgh3326/handoffkeep
TAG=v0.0.300
SHA=1111111111111111111111111111111111111111
OLD=2222222222222222222222222222222222222222
HOSTS="local:darwin_amd64 m1b:darwin_arm64 pi:linux_arm64"

# --- fake tools -------------------------------------------------------------

write_fakes() {
	mkdir -p "$FIX/bin"
	cat >"$FIX/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$FIX/calls"
case "$1" in
api)
	f="$FIX/gh/$(printf '%s' "$2" | tr '/' '_')"
	[[ -f "$f" ]] && cat "$f" || exit 1
	;;
release)
	case "$2" in
	view)
		[[ -f "$FIX/release_view.json" ]] && cat "$FIX/release_view.json" || exit 1
		;;
	download)
		[[ ! -f "$FIX/download_fail" ]] || exit 1
		dir=""
		while [[ $# -gt 0 ]]; do
			[[ "$1" == --dir ]] && dir=$2
			shift
		done
		cp "$FIX"/release/* "$dir"/
		;;
	esac
	;;
esac
EOF
	cat >"$FIX/bin/ssh" <<'EOF'
#!/usr/bin/env bash
# Records the call; runs the command with HOME set to the host's fake home.
args=()
while [[ $# -gt 0 ]]; do
	case $1 in
	-o) shift 2 ;;
	-*) shift ;;
	*) args+=("$1"); shift ;;
	esac
done
host=${args[0]}
cmd=${args[1]:-}
echo "ssh $host $cmd" >>"$FIX/calls"
grep -qxF "$host" "$FIX/unreachable" 2>/dev/null && exit 255
case "$cmd" in
*healthz*) cat "$FIX/server_healthz"; exit 0 ;;
esac
mkdir -p "$FIX/remote/$host"
HOME="$FIX/remote/$host" bash -c "$cmd"
EOF
	cat >"$FIX/bin/scp" <<'EOF'
#!/usr/bin/env bash
# scp [-o opt]... [-q] src host:path
args=()
while [[ $# -gt 0 ]]; do
	case $1 in
	-o) shift 2 ;;
	-*) shift ;;
	*) args+=("$1"); shift ;;
	esac
done
src=${args[0]}
host=${args[1]%%:*}
rel=${args[1]#*:}
echo "scp $host $rel" >>"$FIX/calls"
grep -qxF "$host" "$FIX/unreachable" 2>/dev/null && exit 255
# An absolute destination is already a path in the host's fake home; a
# relative one is relative to it.
case $rel in
/*) dest=$rel ;;
*) dest="$FIX/remote/$host/$rel" ;;
esac
mkdir -p "$(dirname "$dest")"
cp "$src" "$dest"
[[ ! -f "$FIX/tamper/$host" ]] || printf 'tampered' >>"$dest"
EOF
	chmod +x "$FIX/bin/gh" "$FIX/bin/ssh" "$FIX/bin/scp"
}

sums() { # <dir>: write SHA256SUMS for the assets in <dir>
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$1" && sha256sum handoffkeep_* >SHA256SUMS)
	else
		(cd "$1" && shasum -a 256 handoffkeep_* >SHA256SUMS)
	fi
}

# version_script <release-tag>: a CLI that prints the version line.
version_script() {
	printf '#!/usr/bin/env bash\necho "handoffkeep rev=%s time=x modified=false module=x go=x release=%s"\n' "$SHA" "$1"
}

fx_gh() { printf '%s' "$2" >"$FIX/gh/$(printf '%s' "$1" | tr '/' '_')"; }

# green: a tag that is on main, deploy-ok success, a complete release, and
# the serving host serving the tag's commit. Every failure case changes one of these.
green() {
	mkdir -p "$FIX/gh" "$FIX/release" "$FIX/remote"
	fx_gh "repos/$REPO/commits/$TAG" "{\"sha\":\"$SHA\"}"
	fx_gh "repos/$REPO/compare/main...$SHA" '{"status":"behind"}'
	fx_gh "repos/$REPO/commits/$SHA/status" '{"statuses":[{"context":"deploy-ok","state":"success"}]}'
	printf '{"assets":[{"name":"handoffkeep_darwin_amd64"},{"name":"handoffkeep_darwin_arm64"},{"name":"handoffkeep_linux_arm64"},{"name":"SHA256SUMS"}]}' >"$FIX/release_view.json"
	# gh release downloads carry no exec bit, so the fake assets are 0644 too.
	for arch in darwin_amd64 darwin_arm64 linux_arm64; do
		version_script "$TAG" >"$FIX/release/handoffkeep_$arch"
		chmod 644 "$FIX/release/handoffkeep_$arch"
	done
	sums "$FIX/release"
	printf '{"vcs_revision":"%s"}' "$SHA" >"$FIX/server_healthz"
}

# setup: a fresh fixture directory for one case.
setup() {
	FIX="$ROOT/case$((PASSES + FAILS))"
	mkdir -p "$FIX/home/.local/bin"
	: >"$FIX/calls"
	write_fakes
	green
}

# run <args...>: run the script with the fakes first on PATH; sets RC.
run() {
	(
		export FIX HOME="$FIX/home" PATH="$FIX/bin:$PATH" HK_CLI_HOSTS="$HOSTS"
		bash "$SCRIPT" "$@"
	) >"$FIX/out" 2>"$FIX/err"
	RC=$?
}

check() { # <name> <cmd...>: pass when the command succeeds
	local name=$1
	shift
	if "$@"; then
		PASSES=$((PASSES + 1))
	else
		FAILS=$((FAILS + 1))
		echo "not ok: $name"
		echo "  stdout: $(tr '\n' '|' <"$FIX/out")"
		echo "  stderr: $(tr '\n' '|' <"$FIX/err")"
		echo "  calls:  $(tr '\n' '|' <"$FIX/calls")"
	fi
}

has() { grep -qF -- "$2" "$1"; }      # has <file> <text>
lacks() { ! grep -qF -- "$2" "$1"; }  # lacks <file> <text>
rc_is() { [[ "$RC" == "$1" ]]; }

# --- cases ------------------------------------------------------------------

echo "== happy path: every host installs, old binary kept"
setup
mkdir -p "$FIX/remote/pi/.local/bin"
printf '#!/usr/bin/env bash\necho old\n' >"$FIX/remote/pi/.local/bin/handoffkeep"
run "$TAG"
check "exit 0" rc_is 0
check "local CLI ok" has "$FIX/out" "CLI local ok"
check "m1b CLI ok" has "$FIX/out" "CLI m1b ok"
check "pi CLI ok" has "$FIX/out" "CLI pi ok"
check "summary has three installs" has "$FIX/out" "summary tag=$TAG installed=3 skipped=0 unreachable=none failed=none"
check "local binary installed" has "$FIX/home/.local/bin/handoffkeep" "release=$TAG"
check "local binary executable" test -x "$FIX/home/.local/bin/handoffkeep"
check "m1b binary installed" has "$FIX/remote/m1b/.local/bin/handoffkeep" "release=$TAG"
check "m1b binary executable" test -x "$FIX/remote/m1b/.local/bin/handoffkeep"
check "pi old binary kept as bak-pre" has "$FIX/remote/pi/.local/bin/handoffkeep.bak-pre-$TAG" "echo old"
check "no staging file left" test -z "$(find "$FIX/remote/m1b/.local/bin" -name "handoffkeep.stage*")"

echo "== dry-run writes nothing"
setup
run "$TAG" --dry-run
check "exit 0" rc_is 0
check "plan lines" has "$FIX/out" "plan m1b install $TAG"
check "no local binary" test ! -e "$FIX/home/.local/bin/handoffkeep"
check "no remote binary" test ! -e "$FIX/remote/m1b/.local/bin/handoffkeep"
check "no scp" lacks "$FIX/calls" "scp "

echo "== refusal: not on origin/main"
setup
fx_gh "repos/$REPO/compare/main...$SHA" '{"status":"ahead"}'
run "$TAG"
check "exit 3" rc_is 3
check "named refusal" has "$FIX/err" "not on origin/main"
check "no host touched" lacks "$FIX/calls" "ssh m1b"
check "no scp" lacks "$FIX/calls" "scp "

echo "== refusal: deploy-ok not success"
setup
fx_gh "repos/$REPO/commits/$SHA/status" '{"statuses":[{"context":"deploy-ok","state":"pending"}]}'
run "$TAG"
check "exit 4" rc_is 4
check "no host touched" lacks "$FIX/calls" "scp "

echo "== refusal: no SHA256SUMS on the release"
setup
printf '{"assets":[{"name":"handoffkeep_darwin_amd64"}]}' >"$FIX/release_view.json"
run "$TAG"
check "exit 5" rc_is 5
check "no download" lacks "$FIX/calls" "gh release download"
check "no scp" lacks "$FIX/calls" "scp "

echo "== default host list no longer names ncp; the serving host is oci"
setup
version_script "$TAG" >"$FIX/release/handoffkeep_linux_amd64"
chmod 644 "$FIX/release/handoffkeep_linux_amd64"
sums "$FIX/release"
printf '{"assets":[{"name":"handoffkeep_darwin_amd64"},{"name":"handoffkeep_darwin_arm64"},{"name":"handoffkeep_linux_amd64"},{"name":"handoffkeep_linux_arm64"},{"name":"SHA256SUMS"}]}' >"$FIX/release_view.json"
(
	export FIX HOME="$FIX/home" PATH="$FIX/bin:$PATH"
	unset HK_CLI_HOSTS
	bash "$SCRIPT" "$TAG" --dry-run
) >"$FIX/out" 2>"$FIX/err"
RC=$?
check "exit 0" rc_is 0
check "oci planned" has "$FIX/out" "plan oci install $TAG"
check "no ncp planned" lacks "$FIX/out" "ncp"
check "healthz read from oci" has "$FIX/calls" "ssh oci curl"

echo "== deprecated HK_CLI_NCP alias still names a distinct serving host"
setup
export HK_CLI_NCP=legacy
run "$TAG"
unset HK_CLI_NCP
check "exit 0" rc_is 0
check "healthz read from the HK_CLI_NCP host" has "$FIX/calls" "ssh legacy curl"
check "CLI installed on m1b" has "$FIX/remote/m1b/.local/bin/handoffkeep" "release=$TAG"

echo "== HK_CLI_SERVER takes precedence over HK_CLI_NCP"
setup
export HK_CLI_SERVER=oci HK_CLI_NCP=legacy
run "$TAG"
unset HK_CLI_SERVER HK_CLI_NCP
check "exit 0" rc_is 0
check "healthz read from the HK_CLI_SERVER host" has "$FIX/calls" "ssh oci curl"
check "legacy host not used for healthz" lacks "$FIX/calls" "ssh legacy curl"

echo "== stale HK_CLI_NCP=ncp warns and falls back to oci"
setup
export HK_CLI_NCP=ncp
run "$TAG"
unset HK_CLI_NCP
check "exit 0" rc_is 0
check "stale alias warning" has "$FIX/err" "HK_CLI_NCP=ncp is stale; using oci"
check "healthz read from oci" has "$FIX/calls" "ssh oci curl"
check "retired host not used" lacks "$FIX/calls" "ssh ncp"

echo "== stale HK_CLI_NCP=ncp ignores the legacy HK_CLI_NCP_HEALTH URL"
setup
export HK_CLI_NCP=ncp HK_CLI_NCP_HEALTH=http://127.0.0.1:9999/healthz
run "$TAG"
unset HK_CLI_NCP HK_CLI_NCP_HEALTH
check "exit 0" rc_is 0
check "warning says the health URL is ignored" has "$FIX/err" "ignoring HK_CLI_NCP_HEALTH"
check "default healthz read from oci" has "$FIX/calls" "ssh oci curl -fsS http://127.0.0.1:8800/healthz"
check "legacy healthz URL not used" lacks "$FIX/calls" "9999"

echo "== stale HK_CLI_NCP=ncp reads HK_CLI_SERVER_HEALTH when set"
setup
export HK_CLI_NCP=ncp HK_CLI_NCP_HEALTH=http://127.0.0.1:9999/healthz HK_CLI_SERVER_HEALTH=http://127.0.0.1:7777/healthz
run "$TAG"
unset HK_CLI_NCP HK_CLI_NCP_HEALTH HK_CLI_SERVER_HEALTH
check "exit 0" rc_is 0
check "explicit healthz URL read from oci" has "$FIX/calls" "ssh oci curl -fsS http://127.0.0.1:7777/healthz"
check "legacy healthz URL not used" lacks "$FIX/calls" "9999"

echo "== deprecated HK_CLI_NCP host keeps the HK_CLI_NCP_HEALTH fallback"
setup
export HK_CLI_NCP=legacy HK_CLI_NCP_HEALTH=http://127.0.0.1:9999/healthz
run "$TAG"
unset HK_CLI_NCP HK_CLI_NCP_HEALTH
check "exit 0" rc_is 0
check "legacy healthz URL read from the HK_CLI_NCP host" has "$FIX/calls" "ssh legacy curl -fsS http://127.0.0.1:9999/healthz"

echo "== deprecated HK_CLI_NCP mismatch refuses naming that host"
setup
printf '{"vcs_revision":"%s"}' "$OLD" >"$FIX/server_healthz"
export HK_CLI_NCP=legacy
run "$TAG"
unset HK_CLI_NCP
check "exit 6" rc_is 6
check "refusal names legacy" has "$FIX/err" "legacy serves"
check "no scp" lacks "$FIX/calls" "scp "

echo "== refusal: the serving host serves another sha"
setup
printf '{"vcs_revision":"%s"}' "$OLD" >"$FIX/server_healthz"
run "$TAG"
check "exit 6" rc_is 6
check "no scp" lacks "$FIX/calls" "scp "

echo "== refusal: the serving host healthz unreadable"
setup
printf 'oci\n' >"$FIX/unreachable"
run "$TAG"
check "exit 6" rc_is 6
check "no scp" lacks "$FIX/calls" "scp "

echo "== refusal: a download fails SHA256SUMS"
setup
{
	printf '%s  handoffkeep_darwin_amd64\n' 0000000000000000000000000000000000000000000000000000000000000000
	grep -v darwin_amd64 "$FIX/release/SHA256SUMS"
} >"$FIX/release/SHA256SUMS.new" && mv "$FIX/release/SHA256SUMS.new" "$FIX/release/SHA256SUMS"
run "$TAG"
check "exit 7" rc_is 7
check "no host touched" lacks "$FIX/calls" "scp "
check "no ssh to a host" lacks "$FIX/calls" "ssh m1b"

echo "== host already at the tag is skipped"
setup
mkdir -p "$FIX/remote/m1b/.local/bin"
version_script "$TAG" >"$FIX/remote/m1b/.local/bin/handoffkeep"
chmod +x "$FIX/remote/m1b/.local/bin/handoffkeep"
run "$TAG"
check "exit 0" rc_is 0
check "skip line" has "$FIX/out" "skip m1b already $TAG"
check "no scp to m1b" lacks "$FIX/calls" "scp m1b"
check "pi still installed" has "$FIX/out" "CLI pi ok"

echo "== a prefix of the tag does not count as at the tag"
setup
mkdir -p "$FIX/remote/m1b/.local/bin"
version_script "v0.0.3000" >"$FIX/remote/m1b/.local/bin/handoffkeep"
chmod +x "$FIX/remote/m1b/.local/bin/handoffkeep"
run "$TAG"
check "m1b installs" has "$FIX/out" "CLI m1b ok"

echo "== unreachable host is a WARN, the run still succeeds"
setup
printf 'pi\n' >"$FIX/unreachable"
run "$TAG"
check "exit 0" rc_is 0
check "WARN line" has "$FIX/out" "WARN pi unreachable"
check "summary lists it" has "$FIX/out" "unreachable=pi"
check "others installed" has "$FIX/out" "CLI m1b ok"

echo "== host-side sha mismatch fails that host only"
setup
mkdir -p "$FIX/tamper" && : >"$FIX/tamper/m1b"
run "$TAG"
check "exit 8" rc_is 8
check "m1b failed" has "$FIX/out" "FAIL m1b sha_mismatch"
check "pi still installed" has "$FIX/out" "CLI pi ok"
check "m1b binary not installed" test ! -e "$FIX/remote/m1b/.local/bin/handoffkeep"
check "m1b staging file removed" test -z "$(find "$FIX/remote/m1b/.local/bin" -name "handoffkeep.stage*")"
check "summary names failure" has "$FIX/out" "failed=m1b"

echo "== backup failure stops before the swap: the old binary stays in place"
setup
mkdir -p "$FIX/remote/pi/.local/bin"
printf '#!/usr/bin/env bash\necho old\n' >"$FIX/remote/pi/.local/bin/handoffkeep"
chmod 000 "$FIX/remote/pi/.local/bin/handoffkeep"
run "$TAG"
check "exit 8" rc_is 8
check "pi failed" has "$FIX/out" "FAIL pi install_failed"
check "m1b still installed" has "$FIX/out" "CLI m1b ok"
check "no backup written" test ! -e "$FIX/remote/pi/.local/bin/handoffkeep.bak-pre-$TAG"
check "old binary not replaced" test -f "$FIX/remote/pi/.local/bin/handoffkeep"
check "pi staging file removed" test -z "$(find "$FIX/remote/pi/.local/bin" -name "handoffkeep.stage*")"
chmod 644 "$FIX/remote/pi/.local/bin/handoffkeep"

echo "== refusal: SHA256SUMS omits an asset a host needs"
setup
grep -v linux_arm64 "$FIX/release/SHA256SUMS" >"$FIX/release/SHA256SUMS.new" && mv "$FIX/release/SHA256SUMS.new" "$FIX/release/SHA256SUMS"
run "$TAG"
check "exit 5" rc_is 5
check "no scp" lacks "$FIX/calls" "scp "

echo "== refusal: an asset does not match its manifest entry"
setup
printf 'corrupted' >>"$FIX/release/handoffkeep_linux_arm64"
run "$TAG"
check "exit 7" rc_is 7
check "no scp" lacks "$FIX/calls" "scp "

echo "== post-install check: a CLI that does not report the tag is restored"
setup
version_script v0.0.299 >"$FIX/release/handoffkeep_linux_arm64"
chmod 644 "$FIX/release/handoffkeep_linux_arm64"
sums "$FIX/release"
mkdir -p "$FIX/remote/pi/.local/bin"
printf '#!/usr/bin/env bash\necho old\n' >"$FIX/remote/pi/.local/bin/handoffkeep"
chmod 755 "$FIX/remote/pi/.local/bin/handoffkeep"
run "$TAG"
check "exit 8" rc_is 8
check "pi version_mismatch" has "$FIX/out" "FAIL pi version_mismatch"
check "pi old CLI restored" has "$FIX/remote/pi/.local/bin/handoffkeep" "echo old"
check "pi backup kept" test -f "$FIX/remote/pi/.local/bin/handoffkeep.bak-pre-$TAG"
check "m1b still installed" has "$FIX/out" "CLI m1b ok"

echo "== fresh install that fails the version check leaves no CLI behind"
setup
version_script v0.0.299 >"$FIX/release/handoffkeep_linux_arm64"
chmod 644 "$FIX/release/handoffkeep_linux_arm64"
sums "$FIX/release"
run "$TAG"
check "exit 8" rc_is 8
check "pi version_mismatch" has "$FIX/out" "FAIL pi version_mismatch"
check "pi has no CLI" test ! -e "$FIX/remote/pi/.local/bin/handoffkeep"
check "pi staging removed" test -z "$(find "$FIX/remote/pi/.local/bin" -name "handoffkeep.stage*")"

echo "== a re-run never overwrites the first per-tag backup"
setup
mkdir -p "$FIX/remote/m1b/.local/bin"
printf '#!/usr/bin/env bash\necho original\n' >"$FIX/remote/m1b/.local/bin/handoffkeep.bak-pre-$TAG"
printf '#!/usr/bin/env bash\necho newer-than-tag\n' >"$FIX/remote/m1b/.local/bin/handoffkeep"
chmod 755 "$FIX/remote/m1b/.local/bin/handoffkeep"
run "$TAG"
check "exit 0" rc_is 0
check "m1b CLI ok" has "$FIX/out" "CLI m1b ok"
check "original backup kept" has "$FIX/remote/m1b/.local/bin/handoffkeep.bak-pre-$TAG" "echo original"

echo "== usage errors"
setup
run
check "no args exit 2" rc_is 2
setup
run "1.2.3"
check "bad tag exit 2" rc_is 2
setup
run "$TAG" extra
check "two positionals exit 2" rc_is 2

echo
echo "passed: $PASSES  failed: $FAILS"
[[ "$FAILS" -eq 0 ]]
