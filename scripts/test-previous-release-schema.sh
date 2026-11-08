#!/usr/bin/env bash
# The previous release must keep working on the schema this release migrates.
# Blue-green starts the new colour, which migrates the shared database, while
# the old colour keeps serving: a migration may only expand the schema (see
# AGENTS.md §Migrations). This script proves the observable part of that rule
# for one target checkout:
#
#   1. build the target binary from the current checkout
#   2. migrate a fresh database with it and seed one task through the API
#   3. start the previous release's server against that schema and exercise a
#      smoke set with a test token: healthz, task list/show/create, comment
#      create and list, decision list
#
# The previous release is the newest v* tag reachable from HEAD (the target
# sha), or an explicit base commit/tag.
#
#   scripts/test-previous-release-schema.sh [base-sha-or-tag]
#
# Needs HANDOFFKEEP_TEST_DB_URL, a maintenance URL with CREATE DATABASE
# (see AGENTS.md). The deploy tier runs this job; locally it needs a running
# PostgreSQL 17, same as every other DB-backed test.
set -euo pipefail

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

admin_url=${HANDOFFKEEP_TEST_DB_URL:?HANDOFFKEEP_TEST_DB_URL must name a maintenance database}
base_input=${1:-''}

tmp=$(mktemp -d)
db_name=schema_compat_${GITHUB_RUN_ID:-$$}
server_pid=''
token=test-token

stop_server() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
    server_pid=''
  fi
}

cleanup() {
  stop_server
  dropdb --if-exists --maintenance-db="$admin_url" "$db_name" > /dev/null 2>&1 || true
  rm -rf -- "$tmp"
}
trap cleanup EXIT

fail() {
  printf 'schema-compat: %s\n' "$*" >&2
  exit 1
}

say() {
  printf '\n==> %s\n' "$*"
}

# --- pick the previous release -------------------------------------------

target_commit=$(git rev-parse --verify 'HEAD^{commit}')

if [[ -n "$base_input" ]]; then
  git cat-file -e "$base_input^{commit}" 2>/dev/null || fail "base '$base_input' is not a commit in this checkout"
  base_commit=$(git rev-parse --verify "$base_input^{commit}")
  base_desc=$base_input
  [[ "$base_commit" == "$target_commit" ]] && fail "base '$base_input' is the target; there is no previous release to check"
else
  # Newest version among the v* tags whose commit is reachable from the
  # target. The target's own tag is skipped: the previous release is the one
  # that was deployed before this build. v0.0.0 is auto-tag's placeholder,
  # never a release.
  tags=$(git tag --merged "$target_commit" --sort=-v:refname)
  base_commit=''
  base_desc=''
  for tag in $tags; do
    # Only released tags count; v0.0.0 is auto-tag's placeholder.
    [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
    [[ $tag == v0.0.0 ]] && continue
    [[ "$(git rev-parse --verify "$tag^{commit}")" == "$target_commit" ]] && continue
    base_commit=$(git rev-parse --verify "$tag^{commit}")
    base_desc=$tag
    break
  done
  [[ -n "$base_commit" ]] || fail "no v* release tag before $target_commit; pass its commit as the argument"
fi

say "target $target_commit, previous release $base_desc ($base_commit)"

# --- build both binaries --------------------------------------------------

go build -o "$tmp/target" ./cmd/handoffkeep

mkdir -p "$tmp/previous-src"
git archive "$base_commit" | tar -x -C "$tmp/previous-src"
(cd "$tmp/previous-src" && go build -o "$tmp/previous" ./cmd/handoffkeep)

# --- a fresh database and a token ----------------------------------------

dropdb --if-exists --maintenance-db="$admin_url" "$db_name"
createdb --maintenance-db="$admin_url" "$db_name"
db_url=$(python3 -c 'import sys, urllib.parse; u = urllib.parse.urlsplit(sys.argv[1]); print(urllib.parse.urlunsplit((u.scheme, u.netloc, "/" + sys.argv[2], u.query, u.fragment)))' "$admin_url" "$db_name")
printf 'HANDOFFKEEP_TOKEN_test=%s\n' "$token" > "$tmp/auth"

export HANDOFFKEEP_DB_URL=$db_url
export HANDOFFKEEP_AUTH_FILE=$tmp/auth

start_server() { # binary label
  # A fresh free loopback port per start, so the two servers never collide
  # with each other or with a lingering socket.
  local port
  port=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
  base_url=http://127.0.0.1:$port
  "$1" serve --listen "127.0.0.1:$port" > "$tmp/$2.log" 2>&1 &
  server_pid=$!
  for _ in {1..300}; do
    if curl -fsS --max-time 2 "$base_url/healthz" > /dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$server_pid" 2>/dev/null; then
      printf 'schema-compat: %s server exited before answering healthz\n' "$2" >&2
      cat "$tmp/$2.log" >&2
      return 1
    fi
    sleep 0.1
  done
  printf 'schema-compat: %s server did not answer healthz\n' "$2" >&2
  cat "$tmp/$2.log" >&2
  return 1
}

api() { # METHOD PATH [JSON-BODY]
  local method=$1 path=$2 body=${3:-} code
  if [[ -n "$body" ]]; then
    code=$(curl -sS --max-time 10 -o "$tmp/response.json" -w '%{http_code}' \
      -X "$method" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
      --data-binary "$body" "$base_url$path")
  else
    code=$(curl -sS --max-time 10 -o "$tmp/response.json" -w '%{http_code}' \
      -X "$method" -H "Authorization: Bearer $token" "$base_url$path")
  fi
  if [[ "$code" != 2?? ]]; then
    printf 'schema-compat: %s %s answered %s\n' "$method" "$path" "$code" >&2
    cat "$tmp/response.json" >&2
    return 1
  fi
}

# expect DESCRIPTION EXPRESSION [EXTRA]: EXPRESSION is Python over the parsed
# response as d, and EXTRA as a. It travels through the environment so its
# quotes survive the shell.
expect() { # description python-expression [extra-arg]
  local what=$1 extra=${3:-}
  if ! EXPECT_EXPR=$2 python3 - "$tmp/response.json" "$extra" <<'PY'; then
import json, os, sys
d = json.load(open(sys.argv[1]))
a = sys.argv[2] if len(sys.argv) > 2 else ''
sys.exit(0 if eval(os.environ["EXPECT_EXPR"]) else 1)
PY
    printf 'schema-compat: %s: unexpected response\n' "$what" >&2
    cat "$tmp/response.json" >&2
    return 1
  fi
}

task_id() {
  python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["id"])' "$tmp/response.json"
}

# --- migrate with the target and seed a row the previous release must read -

say 'migrate a fresh database with the target binary and seed a task'
start_server "$tmp/target" target
api POST /v1/tasks '{"lane":"schema-compat","title":"seed from the target release","kind":"implement","new":true}'
seed_id=$(task_id)
expect 'seed task is backlog with the target kind' 'd["state"] == "backlog" and d["kind"] == "implement"'
stop_server

# --- run the previous release on that schema ------------------------------

say "run the previous release $base_desc on the target schema"
start_server "$tmp/previous" previous

api GET /healthz
expect 'healthz reports ok' 'd.get("status") == "ok"'

api GET '/v1/tasks?limit=50'
expect 'task list includes the row the target created' 'any(t.get("id") == int(a) for t in d.get("tasks", []))' "$seed_id"

api GET "/v1/tasks/$seed_id"
expect 'task show returns the row the target created' 'd.get("id") == int(a) and d.get("title") == "seed from the target release"' "$seed_id"

api POST /v1/tasks '{"lane":"schema-compat","title":"written by the previous release","kind":"implement","new":true}'
expect 'the previous release creates a task' 'd.get("id") > 0 and d.get("title") == "written by the previous release"'
expect 'the created task is owned by the confirming client' 'd.get("created_by") == "test"'

api POST "/v1/tasks/$seed_id/comments" '{"body":"comment from the previous release"}'
expect 'comment create succeeds' 'd.get("task_id") == int(a) and d.get("body") == "comment from the previous release"' "$seed_id"
expect 'the comment is authored by the confirming client' 'd.get("author") == "test"'

api GET "/v1/tasks/$seed_id/comments"
expect 'comment list shows the comment the previous release wrote' 'any(c.get("body") == "comment from the previous release" for c in d.get("comments", []))'

api GET /v1/decision-requests
expect 'decision list answers a list' 'isinstance(d.get("decision_requests"), list)'

stop_server

printf '\nschema-compat: previous release %s still works on the target schema\n' "$base_desc"
