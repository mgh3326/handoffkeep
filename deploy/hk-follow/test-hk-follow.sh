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
	hk_doc_get() {
		[[ -f "$FIX/hk_error" ]] && return 1
		local f="$FIX/docs/$(key "$1")"
		[[ -f "$f" ]] || return 3
		cp "$f" "$2"
	}
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
t_gate_migration_kinds() {
	setup; setup_tick
	local f
	for f in \
		'[{"filename":"internal/store/linear_board_migration.go","status":"added","patch":"+x"}]' \
		'[{"filename":"internal/store/migration_a.go","status":"renamed","patch":"+x"}]' \
		'[{"filename":"internal/store/tasks.go","status":"modified","patch":"@@ -1 +1 @@\n+func init() { registerMigration(migration{ID: \"x\"}) }"}]' \
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
	want 1 "$(count 'notify .* deployed on ncp')"
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
	env -u GH_TOKEN /bin/bash -c 'set -u; HK_FOLLOW_LIB=1; source "$1"; curl() { printf "%s\n" "$@" >"$2"; printf 200; }; gh_get /repos/x "$3" && echo ok' _ "$SCRIPT" "$FIX/curl-args" "$FIX/out" >"$FIX/ok" 2>&1
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
	want 1 "$(count 'notify .* deployed on ncp')"
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
	# hk 4562: a failed older report stops the pass, so a newer one never
	# overtakes it; the retry delivers both in order.
	setup
	local dir="$HK_FOLLOW_STATE_DIR/outbox"
	mkdir -p "$dir"
	printf '%s\n' "$(jq -cn --arg s "$SHA_OLD" '{kind:"status", sha:$s, context:"deploy-ok", state:"success", description:"older"}')" \
		>"$dir/20260101T000000Z.000001.json"
	printf '%s\n' "$(jq -cn --arg s "$SHA_NEW" '{kind:"status", sha:$s, context:"deploy-ok", state:"success", description:"newer"}')" \
		>"$dir/20260101T000001Z.000001.json"
	gh_status() {
		echo "status $1 $2 $3 $4" >>"$FIX/calls"
		[[ $1 == "$SHA_OLD" ]] && return 1
	}
	deliver_outbox
	want "status $SHA_OLD deploy-ok success older" "$(calls)"
	[[ -f "$dir/20260101T000000Z.000001.json" && -f "$dir/20260101T000001Z.000001.json" ]]
	gh_status() { echo "status $1 $2 $3 $4" >>"$FIX/calls"; }
	: >"$FIX/calls"
	deliver_outbox
	want "$(printf 'status %s deploy-ok success older\nstatus %s deploy-ok success newer' "$SHA_OLD" "$SHA_NEW")" "$(calls)"
	[[ -z "$(ls "$dir")" ]]
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
