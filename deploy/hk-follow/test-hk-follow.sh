#!/usr/bin/env bash
# Tests for deploy/hk-follow/hk-follow against a fake GitHub, registry, hk,
# shiftgate and healthz. Every external call in the script is one function;
# each case sources the script (HK_FOLLOW_LIB=1) in a subshell and replaces
# those functions with fakes reading and writing files under $FIX.
#
#   bash deploy/hk-follow/test-hk-follow.sh
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/hk-follow"
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
FAILS=0 PASSES=0

SHA_NEW=1111111111111111111111111111111111111111
SHA_OLD=2222222222222222222222222222222222222222
SHA_OTHER=3333333333333333333333333333333333333333
D1=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
D2=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
D3=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc

# --- fixture helpers (run in the case subshell) ----------------------------

key() { printf '%s' "$1" | tr '/?&=' '____'; }

fx_gh() { printf '%s' "$2" >"$FIX/gh/$(key "$1")"; }
fx_manifest() { printf '%s' "$2" >"$FIX/reg/$1"; }
fx_health() { printf '%s' "$2" >"$FIX/health/$(key "$1")"; }
fx_doc() { printf '%s' "$2" >"$FIX/docs/$(key "$1")"; }

index_json() { # <revision or ""> <child digest>...
	local rev=$1
	shift
	jq -cn --arg rev "$rev" --args '{schemaVersion:2, mediaType:"application/vnd.oci.image.index.v1+json",
		manifests: ($ARGS.positional | map({digest:.})),
		annotations: (if $rev == "" then {} else {"org.opencontainers.image.revision": $rev} end)}' "$@"
}

# green <sha>: ci-ok (the ci-ok job of the newest ci.yml main push run) and
# deploy-ok (status) success, and the sha on main.
green() {
	fx_gh "/repos/mgh3326/handoffkeep/commits/$1/status?per_page=100" '{"statuses":[{"context":"deploy-ok","state":"success"}]}'
	ci_run "$1" 501 completed success "2026-11-12T10:00:00Z"
	ci_job 501 success
	fx_gh "/repos/mgh3326/handoffkeep/compare/$1...main" '{"status":"identical"}'
}
ci_run() { # <sha> <run id> <status> <conclusion> <created_at>
	fx_gh "/repos/mgh3326/handoffkeep/actions/workflows/ci.yml/runs?head_sha=$1&event=push&branch=main&per_page=1" \
		"{\"workflow_runs\":[{\"id\":$2,\"status\":\"$3\",\"conclusion\":\"$4\",\"created_at\":\"$5\"}]}"
}
ci_job() { # <run id> <ci-ok conclusion> (the latest attempt)
	fx_gh "/repos/mgh3326/handoffkeep/actions/runs/$1/jobs?filter=latest&per_page=100" \
		"{\"jobs\":[{\"name\":\"test\",\"conclusion\":\"success\"},{\"name\":\"ci-ok\",\"conclusion\":\"$2\"}]}"
}
newest_main() { # <sha> <created_at>
	fx_gh "/repos/mgh3326/handoffkeep/actions/workflows/ci.yml/runs?event=push&branch=main&per_page=1" \
		"{\"workflow_runs\":[{\"head_sha\":\"$1\",\"created_at\":\"$2\"}]}"
}

compare() { # <base> <head> <status> [files-json] [commits-json]
	fx_gh "/repos/mgh3326/handoffkeep/compare/$1...$2" \
		"{\"status\":\"$3\",\"files\":${4:-[]},\"commits\":${5:-[]}}"
}

setup() {
	FIX="$ROOT/case$((PASSES + FAILS))"
	mkdir -p "$FIX"/{gh,reg,health,docs,records,state}
	: >"$FIX/calls"
	export FIX
	export HK_FOLLOW_STATE_DIR="$FIX/state" HK_FOLLOW_LIB=1
	# shellcheck source=hk-follow
	source "$SCRIPT"
	gh_get() { local f="$FIX/gh/$(key "$1")"; [[ -f "$f" ]] && cp "$f" "$2"; }
	gh_status() {
		if [[ -f "$FIX/gh_status_error" ]]; then
			echo "status_failed $1 $2 $3 $4" >>"$FIX/calls"
			return 1
		fi
		echo "status $1 $2 $3 $4" >>"$FIX/calls"
	}
	registry_manifest() {
		local f="$FIX/reg/${1//\//_}"
		[[ -f "$FIX/reg_error" ]] && return 1
		[[ -f "$f" ]] || return 3
		cp "$f" "$2"
	}
	registry_tag() {
		echo "tag $1" >>"$FIX/calls"
		local kids
		mapfile -t kids < <(jq -r '.manifests[].digest' "$FIX/reg/$1")
		index_json "$1" "${kids[@]}" >"$FIX/reg/deploy"
	}
	healthz_revision() { cat "$FIX/health/$(key "$1")" 2>/dev/null || true; }
	# The real hk_doc_get runs against a fake hk CLI: exit 0 with the fixture
	# doc, 1 with not_found on stderr when the doc is missing, and 1 with
	# $FIX/hk_error_text (or a dial error) when $FIX/hk_error exists, so the
	# approval_read_failed log line is exercised, not mocked.
	cat >"$FIX/fake-hk" <<'FAKEHK'
#!/usr/bin/env bash
set -u
fkey() { printf '%s' "$1" | tr '/?&=' '____'; }
if [[ "${1:-}" == doc && "${2:-}" == get ]]; then
	k="${3:-}"
	if [[ -f "$FIX/hk_error" ]]; then
		if [[ -f "$FIX/hk_error_text" ]]; then cat "$FIX/hk_error_text" >&2; else printf 'Get "%s": dial tcp: connect: connection refused\n' "${HANDOFFKEEP_URL:-hk}" >&2; fi
		exit 1
	fi
	f="$FIX/docs/$(fkey "$k")"
	if [[ -f "$f" ]]; then cat "$f"; exit 0; fi
	echo "handoffkeep: not_found" >&2
	exit 1
fi
echo "fake-hk: unsupported $*" >&2
exit 1
FAKEHK
	chmod +x "$FIX/fake-hk"
	export HANDOFFKEEP_CLI="$FIX/fake-hk"
	unset HANDOFFKEEP_URL
	hk_doc_put() {
		[[ -f "$FIX/hk_put_error" ]] && return 1
		cp "$2" "$FIX/records/$(key "$1")"
		echo "record $1" >>"$FIX/calls"
		# HK_FOLLOW_RECORD_OUT: internal/ciworkflow parses every record with
		# the store's own deploy-record parser (TestHKFollowRecordsParse).
		if [[ -n "${HK_FOLLOW_RECORD_OUT:-}" ]]; then
			local d="$HK_FOLLOW_RECORD_OUT/$(basename "$FIX")"
			mkdir -p "$d"
			printf '%s\n' "$1" >"$d/$(key "$1").key"
			cp "$2" "$d/$(key "$1").json"
		fi
	}
	sg_status() {
		[[ -f "$FIX/sg_status_error" ]] && return 1
		cat "$FIX/sg_status" 2>/dev/null || echo '[{"name":"handoffkeep","state":{}},{"name":"handoffkeep-work","state":{}}]'
	}
	sg_rollback() {
		echo "rollback $1" >>"$FIX/calls"
		[[ -f "$FIX/sg_rollback_fail" ]] && return 1
		local url
		url=$(services | awk -v s="$1" '$1 == s {print $2}')
		fx_health "$url" "$(cat "$FIX/prev_$1")"
	}
	sg_deploy() {
		echo "deploy $1 $2" >>"$FIX/calls"
		local rc
		rc=$(cat "$FIX/sg_rc_$1" 2>/dev/null || echo 0)
		local url
		url=$(services | awk -v s="$1" '$1 == s {print $2}')
		healthz_revision "$url" >"$FIX/prev_$1"
		if [[ $rc -eq 0 && -f "$FIX/sg_nohealth_$1" ]]; then
			fx_health "$url" "$SHA_OTHER"
		elif [[ $rc -eq 0 ]]; then
			fx_health "$url" "$(jq -r '.annotations["org.opencontainers.image.revision"]' "$FIX/reg/deploy")"
		fi
		return "$rc"
	}
	notify() { echo "notify $1" >>"$FIX/calls"; }
	sleep() { :; }
	HK_FOLLOW_HEALTH_WAIT=4
}

# A deploy tag on SHA_NEW, its image, and both services on SHA_OLD.
setup_tick() {
	fx_manifest deploy "$(index_json "$SHA_NEW" "$D1" "$D2")"
	fx_manifest "$SHA_NEW" "$(index_json "" "$D2" "$D1")"
	fx_health http://127.0.0.1:8800 "$SHA_OLD"
	fx_health http://127.0.0.1:8801 "$SHA_OLD"
	green "$SHA_NEW"
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/api/api.go","status":"modified","patch":"+x"}]' \
		'[{"commit":{"message":"api: a fix (hk 1) (#12)\n\nbody (#99)"}},{"commit":{"message":"ui: b (#13)"}}]'
}

calls() { cat "$FIX/calls"; }
count() { grep -c -- "$1" "$FIX/calls" || true; }

# check <name> <command...>: run one case in a subshell.
check() {
	local name=$1
	shift
	local rc
	# Neither in an if nor before || or &&: set -e is ignored there, even
	# inside the subshell, and a failed want would not stop the case.
	(set -e; "$@") >"$ROOT/out" 2>&1
	rc=$?
	if [[ $rc -eq 0 ]]; then
		PASSES=$((PASSES + 1))
	else
		FAILS=$((FAILS + 1))
		echo "FAIL: $name" >&2
		sed 's/^/    /' "$ROOT/out" >&2
	fi
}

want() { # <expected> <actual>
	if [[ "$1" != "$2" ]]; then
		echo "want: $1" >&2
		echo " got: $2" >&2
		return 1
	fi
}

# fails <command...>: the command must fail ("! cmd" never trips set -e).
fails() {
	if "$@"; then
		echo "expected failure: $*" >&2
		return 1
	fi
}

run_gate() { gate "$1" "$2"; echo "$GATE_VERDICT $GATE_REASON${GATE_APPROVAL_REF:+ $GATE_APPROVAL_REF}"; }

# --- gate ------------------------------------------------------------------

t_gate_promote() {
	setup; setup_tick
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_identical() {
	setup; setup_tick
	want "noop identical" "$(run_gate "$SHA_NEW" "$SHA_NEW")"
}
t_gate_bad_sha() {
	setup
	want "refuse bad_sha" "$(run_gate "v0.0.1" "$SHA_OLD")"
	want "wait baseline_unknown" "$(run_gate "$SHA_NEW" "")"
}
t_gate_deploy_ok_failure() {
	setup; setup_tick
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" '{"statuses":[{"context":"deploy-ok","state":"failure"}]}'
	want "refuse deploy-ok_failure" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_deploy_ok_missing() {
	setup; setup_tick
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" '{"statuses":[]}'
	want "wait deploy-ok_missing" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_failure() {
	setup; setup_tick
	ci_job 501 failure
	want "refuse ci-ok_failure" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_rerun_failed() {
	# A rerun of the main run that failed overrides its earlier success: the
	# jobs list is the latest attempt, and a ci-ok status does not rescue it.
	setup; setup_tick
	ci_job 501 failure
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" \
		'{"statuses":[{"context":"deploy-ok","state":"success"},{"context":"ci-ok","state":"success"}]}'
	want "refuse ci-ok_failure" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_running() {
	setup; setup_tick
	ci_run "$SHA_NEW" 501 in_progress "" "2026-11-12T10:00:00Z"
	want "wait ci-ok_pending" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_no_main_run() {
	# Only a dispatch or any-ref run: no main verdict.
	setup; setup_tick
	fx_gh "/repos/mgh3326/handoffkeep/actions/workflows/ci.yml/runs?head_sha=$SHA_NEW&event=push&branch=main&per_page=1" '{"workflow_runs":[]}'
	want "wait ci-ok_missing" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_superseded() {
	# A superseded main run: ci-any-ref's ci-ok status decides.
	setup; setup_tick
	ci_run "$SHA_NEW" 501 completed cancelled "2026-11-12T10:00:00Z"
	ci_job 501 cancelled
	newest_main "$SHA_OTHER" "2026-11-12T10:05:00Z"
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" \
		'{"statuses":[{"context":"deploy-ok","state":"success"},{"context":"ci-ok","state":"success"}]}'
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" '{"statuses":[{"context":"deploy-ok","state":"success"}]}'
	want "wait ci-ok_missing" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_failed_job_in_superseded_run() {
	# ci-ok itself failed; the run was cancelled later and superseded. The
	# failed job is final: a ci-any-ref status does not override it.
	setup; setup_tick
	ci_run "$SHA_NEW" 501 completed cancelled "2026-11-12T10:00:00Z"
	ci_job 501 failure
	newest_main "$SHA_OTHER" "2026-11-12T10:05:00Z"
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" \
		'{"statuses":[{"context":"deploy-ok","state":"success"},{"context":"ci-ok","state":"success"}]}'
	want "refuse ci-ok_failure" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_cancelled_not_superseded() {
	# Cancelled with no newer main push: fails closed, a status cannot help.
	setup; setup_tick
	ci_run "$SHA_NEW" 501 completed cancelled "2026-11-12T10:00:00Z"
	ci_job 501 cancelled
	newest_main "$SHA_NEW" "2026-11-12T10:00:00Z"
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" \
		'{"statuses":[{"context":"deploy-ok","state":"success"},{"context":"ci-ok","state":"success"}]}'
	want "refuse ci-ok_failure" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_ci_ok_unreadable() {
	setup; setup_tick
	rm "$FIX/gh/$(key "/repos/mgh3326/handoffkeep/actions/runs/501/jobs?filter=latest&per_page=100")"
	want "wait ci-ok_unknown" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_github_down() {
	setup; setup_tick
	rm "$FIX/gh/$(key "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100")"
	want "wait github_status" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_not_on_main() {
	setup; setup_tick
	fx_gh "/repos/mgh3326/handoffkeep/compare/$SHA_NEW...main" '{"status":"diverged"}'
	want "refuse not_on_main" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_not_newer() {
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" behind
	want "refuse not_newer" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	compare "$SHA_OLD" "$SHA_NEW" diverged
	want "refuse not_newer" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}

MIG_ADDED='[{"filename":"internal/store/migration_new_thing.go","status":"added","patch":"+package store"}]'
t_gate_migration_hold() {
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_migration_approved() {
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	fx_doc "deploy-approval/handoffkeep/$SHA_NEW" "{\"body\":\"# review\\napprove $SHA_NEW\\n\"}"
	want "promote ok deploy-approval/handoffkeep/$SHA_NEW" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_migration_wrong_approval() {
	# The approval names another sha, or says something else: still held.
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	fx_doc "deploy-approval/handoffkeep/$SHA_NEW" "{\"body\":\"approve $SHA_OTHER\\nnot approve $SHA_NEW\"}"
	want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_migration_hk_down() {
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	touch "$FIX/hk_error"
	want "wait approval_unreadable" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_approval_read_failed_logged() {
	# A non-not_found CLI failure logs one approval_read_failed line carrying
	# the redacted text: userinfo, Bearer and secret assignments scrubbed,
	# newlines folded, capped at 300 bytes.
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	{
		printf '%s\n' 'Get "https://operator:s3cr3t-oper@hk.internal/v1/documents/x": dial boom Bearer hunter2'
		printf '%s\n' 'CF-Access-Client-Secret: hunter2 password=hunter3 token=abc'
		printf 'x%.0s' $(seq 1 500)
		printf '%s' TAILMARKER_NEVER_LOGGED
	} >"$FIX/hk_error_text"
	touch "$FIX/hk_error"
	gate "$SHA_NEW" "$SHA_OLD" 2>"$FIX/log"
	want "wait" "$GATE_VERDICT"
	want "approval_unreadable" "$GATE_REASON"
	grep -qF 'hk_follow.approval_read_failed' "$FIX/log"
	# printf %q escapes the redacted text (spaces, <>, quotes), so match the
	# escaped form; the raw secrets must be absent either way.
	grep -qF 'dial\ boom' "$FIX/log"
	grep -qF '\<redacted\>' "$FIX/log"
	grep -qF '://\<redacted\>@' "$FIX/log"
	fails grep -qF 'hunter2' "$FIX/log"
	fails grep -qF 'hunter3' "$FIX/log"
	fails grep -qF 's3cr3t-oper' "$FIX/log"
	fails grep -qF 'TAILMARKER_NEVER_LOGGED' "$FIX/log"
	[[ $(grep -c 'hk_follow.approval_read_failed' "$FIX/log") -eq 1 ]]
}
t_gate_migration_not_found_still_hold() {
	# A not_found CLI failure stays hold migration with no failure log line.
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	gate "$SHA_NEW" "$SHA_OLD" 2>"$FIX/log"
	want "hold" "$GATE_VERDICT"
	want "migration" "$GATE_REASON"
	fails grep -q 'approval_read_failed' "$FIX/log"
}

t_gate_migration_kinds() {
	setup; setup_tick
	local f
	for f in \
		'[{"filename":"internal/store/linear_board_migration.go","status":"added","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_a.go","status":"renamed","patch":"+x"}]' \
		'[{"filename":"internal/store/tasks.go","status":"modified","patch":"@@ -1 +1 @@\n+func init() { registerMigration(migration{ID: \"x\"}) }"}]' \
		'[{"filename":"internal/store/migration_a.go","status":"modified","patch":"@@ -1 +1 @@\n registerMigration(migration{\n-\tID: \"old\"\n+\tID: \"new\"\n }"}]' \
		'[{"filename":"internal/store/big.go","status":"modified"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
	# 300 files: GitHub's cap, the list may be partial.
	compare "$SHA_OLD" "$SHA_NEW" ahead "$(jq -cn '[range(300) | {filename:"docs/\(.).md", status:"added", patch:"+x"}]')"
	want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_not_migrations() {
	setup; setup_tick
	local f
	for f in \
		'[{"filename":"internal/store/migration_a_test.go","status":"added","patch":"+registerMigration("}]' \
		'[{"filename":"internal/store/migration_a.go","status":"modified","patch":"+// comment"}]' \
		'[{"filename":"internal/store/migration_a.go","status":"removed"}]' \
		'[{"filename":"internal/store/sub/migration_a.go","status":"added","patch":"+x"}]' \
		'[{"filename":"internal/store/tasks.go","status":"modified","patch":"-registerMigration(old)\n+x"}]' \
		'[{"filename":"docs/migration_notes.go","status":"added","patch":"+x"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
}
t_gate_mysql_migrations() {
	# hk 5072: an added migration_mysql_*.go feeds the MySQL registry
	# production PostgreSQL never runs, so it does not hold the promote.
	setup; setup_tick
	local f
	for f in \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"added","patch":"+registerMySQLMigration("}]' \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"modified","patch":"@@ -1 +1 @@\n-old\n+new"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
	# The exclusion is by name AND content, and added files only: a
	# PostgreSQL registration inside a MySQL-named file still holds, a
	# renamed or copied migration_mysql_* file keeps the name rule (its
	# patch shows only changed lines, so a carried-over registration would
	# be invisible), a name without the migration_mysql_ prefix is not the
	# MySQL registry, and a MySQL file whose patch GitHub omitted cannot be
	# checked for registerMigration(.
	for f in \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"added","patch":"+func init() { registerMigration(migration{ID: \"x\"}) }"}]' \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"renamed","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"copied","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_mysqlfoo.go","status":"added","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"modified"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
	# A PostgreSQL migration next to MySQL files still holds.
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/store/migration_mysql_tasks.go","status":"added","patch":"+x"},{"filename":"internal/store/migration_pg.go","status":"added","patch":"+x"}]'
	want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
MYSQL_PG_ONLY='{"filename":"internal/store/migration_pg.go","status":"added","patch":"+func init() { registerMigration(migration{ID: \"x\"}) }"}'
t_gate_store_mysql_migrations() {
	# hk 4899 C2: in mysql mode an added or changed migration_mysql_*.go is the
	# production DDL and holds.
	HK_FOLLOW_STORE=mysql
	setup; setup_tick
	local f
	for f in \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"added","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"modified","patch":"@@ -1 +1 @@\n-old\n+new"}]' \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"renamed","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_mysql_tasks.go","status":"modified"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
	# A removed MySQL file is not a new migration.
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/store/migration_mysql_tasks.go","status":"removed"}]'
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_store_mysql_pg_only_migration() {
	# In mysql mode an added PostgreSQL-only migration file does not hold: its
	# patch is the whole file. The same file holds in postgres mode.
	HK_FOLLOW_STORE=mysql
	setup; setup_tick
	local f
	compare "$SHA_OLD" "$SHA_NEW" ahead "[$MYSQL_PG_ONLY]"
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	# A changed, renamed or copied PostgreSQL-only file holds: its patch is
	# partial, so a MySQL step edited outside every shown hunk cannot be ruled
	# out. (A registerMigration-only hunk used to pass here.)
	for f in \
		'[{"filename":"internal/store/migration_pg.go","status":"modified","patch":"@@ -1 +1 @@\n registerMigration(migration{\n-\tID: \"old\"\n+\tID: \"new\"\n }"}]' \
		'[{"filename":"internal/store/migration_pg.go","status":"modified","patch":"@@ -9 +9 @@\n-\tstepMySQL(x)\n+\tstepMySQL(y)"}]' \
		'[{"filename":"internal/store/migration_pg.go","status":"renamed","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_pg.go","status":"copied","patch":"+x"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
	# A removed PostgreSQL-only file adds nothing.
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/store/migration_pg.go","status":"removed"}]'
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	HK_FOLLOW_STORE=postgres
	compare "$SHA_OLD" "$SHA_NEW" ahead "[$MYSQL_PG_ONLY]"
	want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_store_mysql_renamed_out_of_namespace() {
	# A file renamed out of the migration namespace keeps its old name's rule:
	# a dual-registered PostgreSQL file renamed to helpers.go with a MySQL-step
	# edit holds, and so does a MySQL migration renamed away. (Both promoted
	# before the previous_filename check.)
	HK_FOLLOW_STORE=mysql
	setup; setup_tick
	local f
	for f in \
		'[{"filename":"internal/store/helpers.go","previous_filename":"internal/store/migration_pg.go","status":"renamed","patch":"@@ -9 +9 @@\n-\tstepMySQL(x)\n+\tstepMySQL(y)"}]' \
		'[{"filename":"internal/store/helpers.go","previous_filename":"internal/store/migration_mysql_tasks.go","status":"renamed","patch":"+x"}]' \
		'[{"filename":"internal/store/helpers.go","previous_filename":"internal/store/migration_pg.go","status":"copied","patch":"+x"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
}
t_gate_store_renamed_from_outside_store() {
	# A file renamed into internal/store from outside it is not a migration
	# unless its old name was a store migration file.
	HK_FOLLOW_STORE=mysql
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/store/utils.go","previous_filename":"web/console/src/migration_utils.go","status":"renamed","patch":"+x"}]'
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	HK_FOLLOW_STORE=postgres
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/store/utils.go","previous_filename":"web/console/src/migration_utils.go","status":"renamed","patch":"+x"}]'
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_gate_store_mysql_registration() {
	# In mysql mode the registration call that counts is registerMySQLMigration(;
	# a file that registers both dialects still holds, and an unreadable
	# PostgreSQL-only file still holds (the gate cannot check it).
	HK_FOLLOW_STORE=mysql
	setup; setup_tick
	local f
	for f in \
		'[{"filename":"internal/store/migration_pg.go","status":"modified","patch":"@@ -1 +1 @@\n+func init() { registerMySQLMigration(migration{ID: \"x\"}) }"}]' \
		'[{"filename":"internal/store/migration_pg.go","status":"added","patch":"+func init() { registerMigration(migration{ID: \"x\"}); registerMySQLMigration(migration{ID: \"x\"}) }"}]' \
		'[{"filename":"internal/store/migration_pg.go","status":"modified"}]' \
		'[{"filename":"internal/store/tasks.go","status":"modified","patch":"@@ -1 +1 @@\n+func init() { registerMySQLMigration(migration{ID: \"x\"}) }"}]'; do
		compare "$SHA_OLD" "$SHA_NEW" ahead "$f"
		want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	done
}
t_gate_store_mysql_shared_migration_go() {
	# A changed shared migration.go (not migration_*.go) keeps the postgres
	# content rule in both modes: it holds on a registration call.
	HK_FOLLOW_STORE=mysql
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/store/migration.go","status":"modified","patch":"@@ -1 +1 @@\n+func init() { registerMigration(migration{ID: \"x\"}) }"}]'
	want "hold migration" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
	compare "$SHA_OLD" "$SHA_NEW" ahead '[{"filename":"internal/store/migration.go","status":"modified","patch":"+// comment"}]'
	want "promote ok" "$(run_gate "$SHA_NEW" "$SHA_OLD")"
}
t_store_mode_bad_value() {
	# Any value other than postgres or mysql (including empty) is a start error:
	# main logs the refusal and exits 1 before it reads GitHub or writes state.
	local v want_line out rc
	for v in pg MySQL ''; do
		case "$v" in
		'') want_line="hk_follow.refused reason=store_mode value=''" ;;
		*) want_line="hk_follow.refused reason=store_mode value=$v" ;;
		esac
		setup; setup_tick
		HK_FOLLOW_STORE=$v
		rc=0
		out=$(main tick 2>&1) || rc=$?
		want 1 "$rc"
		want "$want_line" "$out"
		want "" "$(calls)"
		fails test -e "$FIX/state/state.json"
	done
}

# --- tick ------------------------------------------------------------------

t_tick_deploys() {
	setup; setup_tick
	tick
	want "deployed" "$(state_field verdict)"
	local dig
	dig=$(state_field digest)
	want "deploy handoffkeep $dig" "$(grep '^deploy handoffkeep ' "$FIX/calls")"
	want "deploy handoffkeep-work $dig" "$(grep '^deploy handoffkeep-work ' "$FIX/calls")"
	want 1 "$(count "status $SHA_NEW deployed success")"
	want 2 "$(count '^record deploy/handoffkeep')"
	want 1 "$(count 'notify .* deployed on oci')"
	local rec
	rec=$(cat "$FIX"/records/deploy_handoffkeep_2*)
	want "deploy-record/v0 handoffkeep $SHA_NEW success hk-follow $SHA_OLD" \
		"$(jq -r '[.schema, .service, .head_sha, .result, .source, .previous_ref] | join(" ")' <<<"$rec")"
	want '["https://github.com/mgh3326/handoffkeep/pull/12","https://github.com/mgh3326/handoffkeep/pull/13"]' \
		"$(jq -c .included_prs <<<"$rec")"
	want "ghcr.io/mgh3326/handoffkeep@$dig" "$(jq -r .deployed_ref <<<"$rec")"
	# I4 and idempotence: the same digest again does nothing at all.
	: >"$FIX/calls"
	tick
	want "" "$(calls)"
}
t_tick_failed_not_retried() {
	setup; setup_tick
	echo 1 >"$FIX/sg_rc_handoffkeep"
	tick
	want "failed shiftgate_handoffkeep_exit_1" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count 'deploy handoffkeep-work')"
	want 1 "$(count "status $SHA_NEW deployed failure")"
	want "failed" "$(jq -r .result "$FIX"/records/deploy_handoffkeep_2*)"
	want 1 "$(count '^record ')"
	# I4: never again for this digest, even after shiftgate recovers.
	rm "$FIX/sg_rc_handoffkeep"
	: >"$FIX/calls"
	tick
	want "" "$(calls)"
	# A new promote (new digest) runs.
	fx_manifest deploy "$(index_json "$SHA_NEW" "$D1" "$D2" | jq -c '.annotations.x="y"')"
	tick
	want 1 "$(count 'deploy handoffkeep ')"
}
t_tick_second_service_fails() {
	setup; setup_tick
	echo 1 >"$FIX/sg_rc_handoffkeep-work"
	tick
	want "failed" "$(state_field verdict)"
	want "success" "$(jq -r .result "$FIX"/records/deploy_handoffkeep_2*)"
	want "failed 6 shiftgate_handoffkeep-work_exit_1" \
		"$(jq -r '.result + " " + (.failed_step|tostring) + " " + .failed_step_name' "$FIX"/records/deploy_handoffkeep-work_*)"
}
t_tick_healthz_mismatch() {
	# shiftgate flipped but the new colour serves another revision: roll it
	# back, verify the old revision serves, stop before the next service.
	setup; setup_tick
	touch "$FIX/sg_nohealth_handoffkeep"
	tick
	want "failed healthz_handoffkeep" "$(state_field verdict) $(state_field reason)"
	want 1 "$(count '^rollback handoffkeep$')"
	want "$SHA_OLD" "$(healthz_revision http://127.0.0.1:8800)"
	want 0 "$(count 'deploy handoffkeep-work')"
	want "rolled_back 7 healthz_handoffkeep" "$(jq -r '.result + " " + (.failed_step|tostring) + " " + .failed_step_name' "$FIX"/records/deploy_handoffkeep_2*)"
}
t_tick_healthz_mismatch_rollback_fails() {
	setup; setup_tick
	touch "$FIX/sg_nohealth_handoffkeep" "$FIX/sg_rollback_fail"
	tick
	want "failed healthz_handoffkeep_rollback_failed" "$(state_field verdict) $(state_field reason)"
	want 1 "$(count 'notify .*rollback did not restore it; act now')"
	want "failed" "$(jq -r .result "$FIX"/records/deploy_handoffkeep_2*)"
}
t_tick_shiftgate_busy() {
	setup; setup_tick
	echo 75 >"$FIX/sg_rc_handoffkeep"
	tick
	want "" "$(state_field verdict)"
	want 0 "$(count '^record ')"
	rm "$FIX/sg_rc_handoffkeep"
	tick
	want "deployed" "$(state_field verdict)"
}
t_tick_shiftgate_hold() {
	setup; setup_tick
	local dig
	dig="sha256:$(sha256sum <"$FIX/reg/deploy" | cut -d' ' -f1)"
	jq -n --arg d "$dig" '[{name:"handoffkeep", state:{hold:{digest:$d, reason:"rolled_back"}}}, {name:"handoffkeep-work", state:{}}]' >"$FIX/sg_status"
	tick
	want "refuse held_handoffkeep" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
}
t_tick_shiftgate_status_unreadable() {
	# No readable hold state is no answer: never deploy (an explicit deploy
	# would clear a rollback's hold), and re-check next tick.
	setup; setup_tick
	touch "$FIX/sg_status_error"
	tick
	want "wait shiftgate_status" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	echo '{"oops":1}' >"$FIX/sg_status"; rm "$FIX/sg_status_error"
	tick
	want 0 "$(count '^deploy ')"
	echo '[{"name":"handoffkeep","state":{}}]' >"$FIX/sg_status"
	tick
	want "wait shiftgate_status" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	# shiftgate names a service whose state.json failed to load, omits
	# state and carries the error (review on #349, round 3).
	echo '[{"name":"handoffkeep","backend":"hk","errors":["state: open /var/lib/shiftgate/handoffkeep/state.json: permission denied"]},{"name":"handoffkeep-work","state":{}}]' >"$FIX/sg_status"
	tick
	want "wait shiftgate_status" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	# No state and no errors (a service shiftgate has never deployed).
	echo '[{"name":"handoffkeep","backend":"hk"},{"name":"handoffkeep-work","state":{}}]' >"$FIX/sg_status"
	tick
	want "wait shiftgate_status" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	# A state present but with an errors entry, or a hold without a digest.
	echo '[{"name":"handoffkeep","state":{},"errors":["haproxy: dial unix: no such file"]},{"name":"handoffkeep-work","state":{}}]' >"$FIX/sg_status"
	tick
	want "wait shiftgate_status" "$(state_field verdict) $(state_field reason)"
	echo '[{"name":"handoffkeep","state":{"hold":{"reason":"rolled_back"}}},{"name":"handoffkeep-work","state":{}}]' >"$FIX/sg_status"
	tick
	want "wait shiftgate_status" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	rm "$FIX/sg_status"
	tick
	want "deployed" "$(state_field verdict)"
}
t_tick_gates_every_baseline() {
	# handoffkeep on OLD, handoffkeep-work on an older A: a migration in
	# A...NEW holds the rollout even though OLD...NEW has none.
	setup; setup_tick
	local A=4444444444444444444444444444444444444444
	fx_health http://127.0.0.1:8801 "$A"
	compare "$A" "$SHA_NEW" ahead "$MIG_ADDED"
	tick
	want "hold migration" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
}
t_tick_never_downgrades_a_service() {
	# handoffkeep-work already serves a newer D: NEW would roll it back.
	setup; setup_tick
	local D=5555555555555555555555555555555555555555
	fx_health http://127.0.0.1:8801 "$D"
	compare "$D" "$SHA_NEW" behind
	tick
	want "refuse not_newer" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
}
t_tick_per_service_previous() {
	setup; setup_tick
	local A=4444444444444444444444444444444444444444
	fx_health http://127.0.0.1:8801 "$A"
	compare "$A" "$SHA_NEW" ahead '[]' '[{"commit":{"message":"x (#7)"}}]'
	tick
	want "deployed" "$(state_field verdict)"
	want "$SHA_OLD" "$(jq -r .previous_ref "$FIX"/records/deploy_handoffkeep_2*)"
	want "$A [\"https://github.com/mgh3326/handoffkeep/pull/7\"]" "$(jq -r '.previous_ref + " " + (.included_prs|tojson)' "$FIX"/records/deploy_handoffkeep-work_*)"
}
t_tick_interrupted() {
	setup; setup_tick
	local dig
	dig="sha256:$(sha256sum <"$FIX/reg/deploy" | cut -d' ' -f1)"
	save_state "$dig" "$SHA_NEW" started ""
	tick
	want "failed interrupted" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	want 1 "$(count "status $SHA_NEW deployed failure")"
}
t_tick_crash_not_retried() {
	# A tick killed inside shiftgate (host reboot, OOM) never re-runs that
	# rollout: the attempt is on disk before the first shiftgate call (I4).
	setup; setup_tick
	(sg_deploy() { echo "deploy $1 $2" >>"$FIX/calls"; exit 137; }; tick) || true
	want 1 "$(count '^deploy ')"
	want "started" "$(state_field verdict)"
	tick
	want "failed interrupted" "$(state_field verdict) $(state_field reason)"
	want 1 "$(count '^deploy ')"
}
t_tick_interrupted_then_tag_moved() {
	# The tick died mid-rollout and a newer promote moved the tag before the
	# next tick: the dead rollout is still reported, then the new tag runs.
	setup; setup_tick
	jq -n --arg d "$D3" --arg s "$SHA_OTHER" '{digest:$d, sha:$s, verdict:"started", reason:"", at:"2026-11-12T01:02:03Z"}' >"$FIX/state/state.json"
	tick
	want 1 "$(count "status $SHA_OTHER deployed failure rollout interrupted")"
	[[ -f "$FIX/records/deploy_handoffkeep_20261112T010203Z" ]]
	want 1 "$(count 'notify .*333333333333 rollout interrupted')"
	want "failed 6 interrupted" "$(jq -r '.result + " " + (.failed_step|tostring) + " " + .failed_step_name' "$FIX"/records/deploy_handoffkeep-work_20261112T010203Z)"
	want "deployed $SHA_NEW" "$(state_field verdict) $(state_field sha)"
	want 1 "$(count 'deploy handoffkeep-work ')"
}
t_tick_tag_not_bound() {
	# The tag says SHA_NEW but carries other images.
	setup; setup_tick
	fx_manifest deploy "$(index_json "$SHA_NEW" "$D3")"
	tick
	want "refuse tag_not_bound" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	want 1 "$(count "status $SHA_NEW deployed failure")"
	: >"$FIX/calls"
	tick
	want "" "$(calls)"
}
t_tick_no_revision() {
	setup; setup_tick
	fx_manifest deploy "$(index_json "" "$D1" "$D2")"
	tick
	want "refuse no_revision" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
}
t_tick_rechecks_github() {
	# The tag mover is not trusted: deploy-ok failure on GitHub stops it.
	setup; setup_tick
	fx_gh "/repos/mgh3326/handoffkeep/commits/$SHA_NEW/status?per_page=100" '{"statuses":[{"context":"deploy-ok","state":"failure"}]}'
	tick
	want "refuse deploy-ok_failure" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
}
t_tick_hold_then_approval() {
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	tick
	want "hold migration" "$(state_field verdict) $(state_field reason)"
	want 1 "$(count "status $SHA_NEW deployed pending deploy-held:migration")"
	want 0 "$(count '^deploy ')"
	# Re-evaluated every tick, reported once.
	tick
	want 1 "$(count 'deploy-held:migration')"
	fx_doc "deploy-approval/handoffkeep/$SHA_NEW" "{\"body\":\"approve $SHA_NEW\"}"
	tick
	want "deployed" "$(state_field verdict)"
	want "deploy-approval/handoffkeep/$SHA_NEW" "$(jq -r .approval_ref "$FIX"/records/deploy_handoffkeep_2*)"
}
t_tick_gate_log_names_store() {
	HK_FOLLOW_STORE=mysql
	setup; setup_tick
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	out=$(tick 2>&1)
	want 1 "$(printf '%s\n' "$out" | grep -c 'hk_follow.gate .*store=mysql')"
}
t_tick_already_serving() {
	setup; setup_tick
	fx_health http://127.0.0.1:8800 "$SHA_NEW"
	fx_health http://127.0.0.1:8801 "$SHA_NEW"
	tick
	want "noop serving" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
	want 0 "$(count '^status ')"
}
t_tick_one_service_current() {
	setup; setup_tick
	fx_health http://127.0.0.1:8800 "$SHA_NEW"
	tick
	want "deployed" "$(state_field verdict)"
	want 0 "$(count 'deploy handoffkeep ')"
	want 1 "$(count 'deploy handoffkeep-work ')"
}
t_tick_baseline_unknown() {
	setup; setup_tick
	rm "$FIX/health/$(key http://127.0.0.1:8800)"
	tick
	want "wait baseline_unknown" "$(state_field verdict) $(state_field reason)"
	want 0 "$(count '^deploy ')"
}
t_tick_dry_run() {
	setup; setup_tick
	DRY_RUN=1
	tick 2>"$FIX/log"
	want "" "$(calls)"
	[[ ! -f "$FIX/state/state.json" ]]
	grep -q 'would_run=' "$FIX/log"
}
t_tick_registry_down() {
	setup; setup_tick
	touch "$FIX/reg_error"
	tick
	want "" "$(calls)"
	[[ ! -f "$FIX/state/state.json" ]]
}
t_tick_lock() {
	# I5: a second tick while one holds the lock exits without work.
	setup; setup_tick
	exec 8>"$HK_FOLLOW_LOCK"
	flock -n 8
	(tick) 2>"$FIX/log"
	want "" "$(calls)"
	grep -q 'hk_follow.busy reason=lock' "$FIX/log"
}

t_gh_get_without_token() {
	# gh_get must not abort under set -u on bash <4.4 when GH_TOKEN is unset.
	# /bin/bash is 3.2 on macOS; the regression only reproduces there.
	setup
	# The fake's own $2 is curl's second argument (-o), not the recording path,
	# so the path is captured before curl is redefined.
	env -u GH_TOKEN /bin/bash -c 'set -u; HK_FOLLOW_LIB=1; source "$1"; argsf=$2; curl() { printf "%s\n" "$@" >"$argsf"; printf 200; }; gh_get /repos/x "$3" && echo ok' _ "$SCRIPT" "$FIX/curl-args" "$FIX/out" >"$FIX/ok" 2>&1
	want "ok" "$(cat "$FIX/ok")"
	fails grep -q Authorization "$FIX/curl-args"
}

t_tick_no_services_refused() {
	# A whitespace-only HK_FOLLOW_SERVICES must refuse, not strand the digest as noop serving.
	setup; setup_tick
	local saved="$HK_FOLLOW_SERVICES"
	HK_FOLLOW_SERVICES="   "
	fails tick
	want "" "$(state_field verdict)"
	want 0 "$(count '^deploy ')"
	want 0 "$(count '^status ')"
	HK_FOLLOW_SERVICES="$saved"
	tick
	want "deployed" "$(state_field verdict)"
}

t_tick_records_retried_after_hk_outage() {
	# Terminal state must not be saved while records are undelivered; a later tick delivers them.
	setup; setup_tick
	touch "$FIX/hk_put_error"
	tick
	want "deployed" "$(state_field verdict)"
	want 0 "$(count '^record ')"
	rm -f "$FIX/hk_put_error"
	tick
	want 2 "$(count '^record deploy/handoffkeep')"
	want 2 "$(count '^deploy ')"
	want 1 "$(count 'notify .* deployed on oci')"
}

t_tick_status_retried() {
	# A failed GitHub status post stays queued and is retried on the next tick.
	setup; setup_tick
	touch "$FIX/gh_status_error"
	tick
	want 0 "$(count "^status $SHA_NEW deployed success")"
	rm -f "$FIX/gh_status_error"
	tick
	want 1 "$(count "^status $SHA_NEW deployed success")"
}

t_outbox_fifo() {
	# hk 4562: a failed older status holds a later one for the same sha
	# and context, so a newer report never overtakes it; the retry
	# delivers both in order.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox"
	mkdir -p "$dir"
	printf '%s\n' "$(jq -cn --arg s "$SHA_OLD" '{kind:"status", sha:$s, context:"deploy-ok", state:"success", description:"older"}')" \
		>"$dir/20260101T000000Z.000001.json"
	printf '%s\n' "$(jq -cn --arg s "$SHA_OLD" '{kind:"status", sha:$s, context:"deploy-ok", state:"success", description:"newer"}')" \
		>"$dir/20260101T000001Z.000001.json"
	gh_status() {
		echo "status $1 $2 $3 $4" >>"$FIX/calls"
		if [[ $4 == older ]]; then return 1; fi
	}
	deliver_outbox
	want "status $SHA_OLD deploy-ok success older" "$(calls)"
	[[ -f "$dir/20260101T000000Z.000001.json" && -f "$dir/20260101T000001Z.000001.json" ]]
	gh_status() { echo "status $1 $2 $3 $4" >>"$FIX/calls"; }
	: >"$FIX/calls"
	deliver_outbox
	want "$(printf 'status %s deploy-ok success older\nstatus %s deploy-ok success newer' "$SHA_OLD" "$SHA_OLD")" "$(calls)"
	[[ -z "$(ls "$dir")" ]]
}

t_outbox_record_fifo() {
	# A failed older put holds back a later record with the same document key,
	# so the retry cannot overwrite the newer record on the next pass.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox" key="deploy/handoffkeep/20260101T000000Z"
	local -a files
	mkdir -p "$dir"
	record handoffkeep "$SHA_NEW" "$D1" "$SHA_OLD" success "" 20260101T000000Z
	record handoffkeep "$SHA_NEW" "$D2" "$SHA_OLD" failed "shiftgate_handoffkeep_exit_1" 20260101T000000Z
	files=("$dir"/*.json)
	mv "${files[0]}" "$dir/20260101T000000Z.000001.json"
	mv "${files[1]}" "$dir/20260101T000001Z.000002.json"
	local attempts=0
	hk_doc_put() {
		attempts=$((attempts + 1))
		if [[ $attempts -eq 1 ]]; then return 1; fi
		cp "$2" "$FIX/records/$(key "$1")"
		echo "record $1" >>"$FIX/calls"
	}
	deliver_outbox
	[[ -f "$dir/20260101T000000Z.000001.json" && -f "$dir/20260101T000001Z.000002.json" ]]
	deliver_outbox
	want failed "$(jq -r .result "$FIX/records/$(key "$key")")"
	[[ -z "$(ls "$dir")" ]]
}

t_outbox_failure_does_not_block_others() {
	# A failed status holds back only a later status for the same sha and
	# context: a status for another sha, a record and a notify are still
	# attempted in the same pass, and an entry that can never be delivered is
	# rejected without blocking the rest (hk 4749).
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox"
	mkdir -p "$dir"
	# The record body comes from the shipped record(), so this fake is one the
	# record parser accepts: TestHKFollowRecordsParse parses every record a
	# successful put writes, and an empty stub body fails it. The outbox is
	# empty here, so the move renames the entry record() queued.
	record handoffkeep "$SHA_NEW" "$D1" "$SHA_OLD" success "" 20260101T000000Z
	mv "$dir"/*.json "$dir/20260101T000001Z.000002.json"
	printf '%s\n' "$(jq -cn --arg s "$SHA_OLD" '{kind:"status", sha:$s, context:"deploy-ok", state:"success", description:"older"}')" \
		>"$dir/20260101T000000Z.000001.json"
	printf '%s\n' "$(jq -cn --arg s "$SHA_NEW" '{kind:"status", sha:$s, context:"deploy-ok", state:"success", description:"newer"}')" \
		>"$dir/20260101T000002Z.000003.json"
	printf '%s\n' '{"kind":"notify", "text":"alert"}' \
		>"$dir/20260101T000003Z.000004.json"
	printf '%s\n' '{"kind":"bogus"}' \
		>"$dir/20260101T000004Z.000005.json"
	gh_status() {
		echo "status $1 $2 $3 $4" >>"$FIX/calls"
		if [[ $1 == "$SHA_OLD" ]]; then return 1; fi
	}
	deliver_outbox
	want "$(printf 'status %s deploy-ok success older\nrecord deploy/handoffkeep/20260101T000000Z\nstatus %s deploy-ok success newer\nnotify alert\nnotify [hk-follow] outbox entry rejected: 20260101T000004Z.000005.json (unknown_kind); kept in %s/outbox/rejected/20260101T000004Z.000005.json' "$SHA_OLD" "$SHA_NEW" "${HK_FOLLOW_STATE_DIR}")" "$(calls)"
	[[ -f "$dir/20260101T000000Z.000001.json" ]]
	[[ -f "$dir/rejected/20260101T000004Z.000005.json" ]]
	[[ ! -f "$dir/20260101T000004Z.000005.json" ]]
	[[ ! -f "$dir/20260101T000001Z.000002.json" && ! -f "$dir/20260101T000002Z.000003.json" && ! -f "$dir/20260101T000003Z.000004.json" ]]
}

# two_same_key_records <dir>: queue two records with one document key, the
# second strictly later in the queue. The bodies come from the shipped
# record(), so TestHKFollowRecordsParse parses them.
two_same_key_records() {
	local -a files
	record handoffkeep "$SHA_NEW" "$D1" "$SHA_OLD" success "" 20260101T000000Z
	record handoffkeep "$SHA_NEW" "$D2" "$SHA_OLD" failed "shiftgate_handoffkeep_exit_1" 20260101T000000Z
	files=("$1"/*.json)
	mv "${files[0]}" "$1/20260101T000000Z.000001.json"
	mv "${files[1]}" "$1/20260101T000001Z.000002.json"
}

t_outbox_rejected_record_unblocks_same_key() {
	# hk 4749: a record hk refuses permanently leaves the queue for
	# rejected/ and holds nothing back, so the later record with the same key
	# is delivered instead of waiting behind it forever.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox" doc_key="deploy/handoffkeep/20260101T000000Z"
	mkdir -p "$dir"
	two_same_key_records "$dir"
	hk_doc_put() {
		if [[ "$1" == "$doc_key" && ! -f "$FIX/refused" ]]; then
			touch "$FIX/refused"
			DELIVER_REASON=invalid_context
			return 2
		fi
		cp "$2" "$FIX/records/$(key "$1")"
		echo "record $1" >>"$FIX/calls"
	}
	deliver_outbox 2>"$FIX/log"
	[[ ! -f "$dir/20260101T000000Z.000001.json" && ! -f "$dir/20260101T000001Z.000002.json" ]]
	[[ -f "$dir/rejected/20260101T000000Z.000001.json" ]]
	want failed "$(jq -r .result "$FIX/records/$(key "$doc_key")")"
	grep -qx "hk_follow.outbox_rejected key=$doc_key reason=invalid_context" "$FIX/log"
	want 1 "$(count 'notify \[hk-follow\] outbox entry rejected')"
}

t_outbox_transient_failure_held() {
	# A failure a later pass may deliver still holds the later record with the
	# same key, and nothing moves to rejected/.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox"
	mkdir -p "$dir"
	two_same_key_records "$dir"
	hk_doc_put() { return 1; }
	deliver_outbox 2>"$FIX/log"
	[[ -f "$dir/20260101T000000Z.000001.json" && -f "$dir/20260101T000001Z.000002.json" ]]
	[[ ! -e "$dir/rejected" ]]
	want 1 "$(grep -c hk_follow.outbox_hold "$FIX/log" || true)"
	want 1 "$(grep -c hk_follow.outbox_retry "$FIX/log" || true)"
	fails grep -q outbox_rejected "$FIX/log"
}

t_outbox_unknown_kind_rejected() {
	# An entry of an unknown kind can never be delivered: it moves to
	# rejected/ at once instead of being retried every tick.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox"
	mkdir -p "$dir"
	printf '%s\n' '{"kind":"bogus"}' >"$dir/20260101T000004Z.000005.json"
	deliver_outbox 2>"$FIX/log"
	[[ ! -f "$dir/20260101T000004Z.000005.json" ]]
	[[ -f "$dir/rejected/20260101T000004Z.000005.json" ]]
	grep -qx 'hk_follow.outbox_rejected key=20260101T000004Z.000005.json reason=unknown_kind' "$FIX/log"
	want 1 "$(count 'notify \[hk-follow\] outbox entry rejected')"
}

t_outbox_reject_failure_does_not_stop_the_pass() {
	# A rejected/ that cannot be created must not abort the pass: the refused
	# entry stays queued, holds its key like a failed retry, and the reports
	# behind it are still attempted.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox" doc_key="deploy/handoffkeep/20260101T000000Z"
	mkdir -p "$dir"
	two_same_key_records "$dir"
	printf '%s\n' '{"kind":"notify", "text":"alert"}' >"$dir/20260101T000002Z.000003.json"
	# rejected/ exists as a file, so mkdir -p refuses it.
	printf '' >"$dir/rejected"
	hk_doc_put() {
		if [[ "$1" == "$doc_key" ]]; then
			DELIVER_REASON=invalid_context
			return 2
		fi
		echo "record $1" >>"$FIX/calls"
	}
	deliver_outbox 2>"$FIX/log"
	[[ -f "$dir/20260101T000000Z.000001.json" && -f "$dir/20260101T000001Z.000002.json" ]]
	[[ -f "$dir/rejected" && ! -d "$dir/rejected" ]]
	want "notify alert" "$(calls)"
	want 1 "$(grep -c hk_follow.outbox_reject_failed "$FIX/log" || true)"
	fails grep -q 'hk_follow.outbox_rejected key=' "$FIX/log"
}

t_outbox_status_rejected_only_on_body_refusal() {
	# Only a 4xx that refuses the body itself (400, 422) is permanent; auth,
	# rate-limit and server errors stay queued. A permanent one moves to
	# rejected/ and holds nothing back.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox"
	mkdir -p "$dir"
	printf '%s\n' "$(jq -cn --arg s "$SHA_OLD" '{kind:"status", sha:$s, context:"deployed", state:"success", description:"refused"}')" \
		>"$dir/20260101T000000Z.000001.json"
	printf '%s\n' "$(jq -cn --arg s "$SHA_NEW" '{kind:"status", sha:$s, context:"deployed", state:"success", description:"retry"}')" \
		>"$dir/20260101T000001Z.000002.json"
	gh_status() {
		echo "status $1 $2 $3 $4" >>"$FIX/calls"
		if [[ $1 == "$SHA_OLD" ]]; then
			DELIVER_REASON=http_422
			return 2
		fi
		return 1
	}
	deliver_outbox 2>"$FIX/log"
	[[ ! -f "$dir/20260101T000000Z.000001.json" ]]
	[[ -f "$dir/rejected/20260101T000000Z.000001.json" ]]
	[[ -f "$dir/20260101T000001Z.000002.json" ]]
	grep -qx "hk_follow.outbox_rejected key=$SHA_OLD/deployed reason=http_422" "$FIX/log"
	# The shipped gh_status: only a body refusal is permanent.
	want "2 http_400" "$(real_status 400)"
	want "2 http_422" "$(real_status 422)"
	want "1 none" "$(real_status 401)"
	want "1 none" "$(real_status 403)"
	want "1 none" "$(real_status 404)"
	want "1 none" "$(real_status 429)"
	want "1 none" "$(real_status 500)"
}

# real_status <http code>: run the shipped gh_status against a curl that
# answers <code>; print "<rc> <reason>".
real_status() {
	(
		# shellcheck source=hk-follow
		source "$SCRIPT"
		# Not `code`: gh_status declares that name local, and bash's dynamic
		# scope would make this one unset (set -u) inside the fake.
		local fake_http=$1
		curl() { printf '%s' "$fake_http"; }
		DELIVER_REASON=""
		rc=0
		gh_status "$SHA_OLD" deployed success x || rc=$?
		printf '%s %s\n' "$rc" "${DELIVER_REASON:-none}"
	)
}

# real_put <cli error>: run the shipped hk_doc_put against a fake hk CLI that
# prints "handoffkeep: <cli error>" and exits 1; print "<rc> <reason>".
real_put() {
	printf '%s\n' "handoffkeep: $1" >"$FIX/cli.err"
	printf '#!/bin/sh\ncat "%s" >&2\nexit 1\n' "$FIX/cli.err" >"$FIX/cli"
	chmod +x "$FIX/cli"
	(
		# shellcheck source=hk-follow
		source "$SCRIPT"
		HANDOFFKEEP_CLI="$FIX/cli"
		DELIVER_REASON=""
		rc=0
		hk_doc_put k "$FIX/record" || rc=$?
		printf '%s %s\n' "$rc" "${DELIVER_REASON:-none}"
	)
}

t_hk_doc_put_classifies_the_cli() {
	# `handoffkeep doc put` exits 1 for every failure, so the wire code in its
	# error line is the signal: invalid_context and secret_like_content mean
	# the server refused this body and a retry cannot succeed.
	setup
	want "2 invalid_context" "$(real_put invalid_context)"
	want "2 secret_like_content" "$(real_put secret_like_content:aws_access_key)"
	want "1 none" "$(real_put 'Post "https://hk.example/v1/documents/k": dial tcp 10.0.0.1:443: connect: connection refused')"
	want "1 none" "$(real_put internal_error)"
	want "1 none" "$(real_put unauthorized)"
}

t_tick_rollback_failed_hk_down_notify_delivered() {
	# Rollback fails and the hk upload fails: the "rollback did not
	# restore it; act now" lane event is still delivered in the same tick.
	# Twenty runs, each in a fresh directory: outbox names carry a
	# sequence, so the queue order is the enqueue order on every run, not
	# mktemp luck.
	local i
	for i in $(seq 1 20); do
		(
		setup
		FIX="$FIX-$i"; export FIX
		export HK_FOLLOW_STATE_DIR="$FIX/state"
		mkdir -p "$FIX"/{gh,reg,health,docs,records,state}
		: >"$FIX/calls"
		setup_tick
		touch "$FIX/sg_nohealth_handoffkeep" "$FIX/sg_rollback_fail" "$FIX/hk_put_error"
		tick
		want "failed healthz_handoffkeep_rollback_failed" "$(state_field verdict) $(state_field reason)"
		want 1 "$(count 'notify .*rollback did not restore it; act now')"
		)
	done
}

t_tick_interrupted_records_retried() {
	# The interrupted-attempt report is queued, not lost, when hk is down.
	setup; setup_tick
	local dig
	dig="sha256:$(sha256sum <"$FIX/reg/deploy" | cut -d' ' -f1)"
	save_state "$dig" "$SHA_NEW" started ""
	touch "$FIX/hk_put_error"
	tick
	want 0 "$(count '^record ')"
	rm -f "$FIX/hk_put_error"
	tick
	want 2 "$(count '^record deploy/handoffkeep')"
}

t_tick_refusal_report_retried() {
	# A refusal whose status post fails is retried; the terminal state is not lost.
	setup; setup_tick
	fx_manifest deploy "$(index_json "$SHA_NEW" "$D3")"
	touch "$FIX/gh_status_error"
	tick
	want "refuse tag_not_bound" "$(state_field verdict) $(state_field reason)"
	rm -f "$FIX/gh_status_error"
	tick
	want 1 "$(count 'status .* deploy-refused:tag_not_bound')"
}

# --- promote ---------------------------------------------------------------

setup_promote() {
	fx_manifest deploy "$(index_json "$SHA_OLD" "$D3")"
	fx_manifest "$SHA_NEW" "$(index_json "" "$D1" "$D2")"
	green "$SHA_NEW"
	compare "$SHA_OLD" "$SHA_NEW" ahead '[]'
}
t_promote_ok() {
	setup; setup_promote
	promote "$SHA_NEW"
	want "tag $SHA_NEW" "$(grep '^tag ' "$FIX/calls")"
	want 1 "$(count "status $SHA_NEW deploy-promote success")"
	want "$SHA_NEW" "$(jq -r '.annotations["org.opencontainers.image.revision"]' "$FIX/reg/deploy")"
}
t_promote_hold() {
	setup; setup_promote
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	promote "$SHA_NEW"
	want 0 "$(count '^tag ')"
	want 1 "$(count "status $SHA_NEW deploy-promote pending deploy-held:migration")"
}
t_promote_refuse() {
	setup; setup_promote
	fx_gh "/repos/mgh3326/handoffkeep/compare/$SHA_NEW...main" '{"status":"behind"}'
	promote "$SHA_NEW"
	want "" "$(calls)"
}
t_promote_private_url_refused() {
	# The private-URL refusal fires only when the approval read fails: the
	# approval lookup is the one gate path that reaches hk.
	setup; setup_promote
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	touch "$FIX/hk_error"
	local u
	for u in 'http://127.0.0.1:8800' 'http://10.0.0.5/' 'http://192.168.1.10:8080/x' 'http://172.16.5.4/' 'http://100.96.0.1/' 'http://localhost:8800/' 'http://[::1]/'; do
		export HANDOFFKEEP_URL="$u"
		fails promote "$SHA_NEW"
		want 1 "$(count 'deploy-refused:approval_unreadable:private_url')"
		want 0 "$(count '^tag ')"
		: >"$FIX/calls"
	done
	# The same hk failure with a public URL keeps the bare reason.
	export HANDOFFKEEP_URL="https://hk.example.com"
	fails promote "$SHA_NEW"
	want 1 "$(count 'deploy-refused:approval_unreadable$')"
	want 0 "$(count 'private_url')"
	want 0 "$(count '^tag ')"
}
t_promote_private_url_no_migration_promotes() {
	# A release with no migration never reads hk: a private URL changes nothing.
	setup; setup_promote
	export HANDOFFKEEP_URL="http://100.96.0.1/"
	promote "$SHA_NEW"
	want "tag $SHA_NEW" "$(grep '^tag ' "$FIX/calls")"
	want 1 "$(count "status $SHA_NEW deploy-promote success")"
}
t_promote_private_url_diagnostic_passes() {
	# A diagnostic dispatch (not on main) with a private URL still passes quiet.
	setup; setup_promote
	export HANDOFFKEEP_URL="http://127.0.0.1:8800/"
	fx_gh "/repos/mgh3326/handoffkeep/compare/$SHA_NEW...main" '{"status":"behind"}'
	promote "$SHA_NEW"
	want "" "$(calls)"
}
t_promote_private_url_no_baseline_pends() {
	# Before the deploy tag exists, a private URL still pends green.
	setup; setup_promote
	rm "$FIX/reg/deploy"
	export HANDOFFKEEP_URL="http://10.0.0.5/"
	promote "$SHA_NEW"
	want 0 "$(count '^tag ')"
	want 1 "$(count 'no baseline: bootstrap')"
}
t_promote_private_url_hold_stays_hold() {
	# A missing approval still holds (pending), not a refusal.
	setup; setup_promote
	compare "$SHA_OLD" "$SHA_NEW" ahead "$MIG_ADDED"
	export HANDOFFKEEP_URL="http://100.96.0.1/"
	promote "$SHA_NEW"
	want 0 "$(count '^tag ')"
	want 1 "$(count 'deploy-held:migration')"
	want 0 "$(count 'deploy-refused')"
}
t_promote_ci_ok_failure() {
	setup; setup_promote
	ci_job 501 failure
	fails promote "$SHA_NEW"
	want 0 "$(count '^tag ')"
	want 1 "$(count "status $SHA_NEW deploy-promote failure deploy-refused:ci-ok_failure")"
}
t_promote_old_sha() {
	# A dispatch of an older head never moves the tag back, and is not red.
	setup; setup_promote
	compare "$SHA_OLD" "$SHA_NEW" behind
	promote "$SHA_NEW"
	want "" "$(calls)"
}
t_promote_no_baseline() {
	# No deploy tag (before bootstrap): pending, exit 0, nothing promoted.
	setup; setup_promote
	rm "$FIX/reg/deploy"
	promote "$SHA_NEW"
	want 0 "$(count '^tag ')"
	want "status $SHA_NEW deploy-promote pending no baseline: bootstrap the deploy tag (README step 6)" "$(calls)"
}
t_promote_baseline_unreadable() {
	# A tag without a revision, or a registry error, still fails.
	setup; setup_promote
	fx_manifest deploy "$(index_json "" "$D3")"
	fails promote "$SHA_NEW"
	want 0 "$(count '^tag ')"
	want 1 "$(count 'deploy-promote failure deploy-refused:baseline_unreadable')"
	touch "$FIX/reg_error"
	fails promote "$SHA_NEW"
	want 2 "$(count 'deploy-refused:baseline_unreadable\|deploy-refused:image_missing')"
}
t_promote_image_missing() {
	setup; setup_promote
	rm "$FIX/reg/$SHA_NEW"
	HK_FOLLOW_IMAGE_WAIT=60
	fails promote "$SHA_NEW"
	want 1 "$(count 'deploy-refused:image_missing')"
}
t_promote_same() {
	setup; setup_promote
	fx_manifest deploy "$(index_json "$SHA_NEW" "$D1" "$D2")"
	promote "$SHA_NEW"
	want "" "$(calls)"
}
t_promote_dry_run() {
	setup; setup_promote
	DRY_RUN=1
	promote "$SHA_NEW"
	want "" "$(calls)"
}
t_promote_readback() {
	setup; setup_promote
	registry_tag() { echo "tag $1" >>"$FIX/calls"; }
	fails promote "$SHA_NEW"
	want 1 "$(count 'deploy-refused:tag_readback')"
}

for t in $(declare -F | awk '{print $3}' | grep '^t_'); do
	check "$t" "$t"
done
echo "hk-follow: $PASSES passed, $FAILS failed"
[[ $FAILS -eq 0 ]]
