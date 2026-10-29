#!/usr/bin/env bash
set -euo pipefail
: "${HANDOFFKEEP_TEST_DB_URL:?}"
tmp=$(mktemp -d)
pid=''
admin_url=$HANDOFFKEEP_TEST_DB_URL
db_name=task_expected_migration
createdb --maintenance-db="$admin_url" "$db_name"
export HANDOFFKEEP_TEST_DB_URL=$(python3 -c 'import sys,urllib.parse; u=urllib.parse.urlsplit(sys.argv[1]); print(urllib.parse.urlunsplit((u.scheme,u.netloc,"/"+sys.argv[2],u.query,u.fragment)))' "$admin_url" "$db_name")
cleanup() { if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; dropdb --if-exists --maintenance-db="$admin_url" "$db_name"; rm -rf "$tmp"; }
trap cleanup EXIT
printf 'HANDOFFKEEP_TOKEN_test=test-token\n' > "$tmp/auth"
# Build the actual previous schema from the PR base, not a hand-maintained copy.
base=$(git merge-base HEAD origin/main)
mkdir "$tmp/base"
git archive "$base" | tar -x -C "$tmp/base"
(cd "$tmp/base" && go build -o "$tmp/old" ./cmd/handoffkeep)
go build -o "$tmp/new" ./cmd/handoffkeep
export HANDOFFKEEP_AUTH_FILE="$tmp/auth"
export HANDOFFKEEP_DB_URL="$HANDOFFKEEP_TEST_DB_URL"
MIGRATION_TEST_LISTEN=$(python3 -c 'import socket,sys,urllib.parse; print(socket.gethostbyname(urllib.parse.urlsplit(sys.argv[1]).hostname)+":18802")' "$HANDOFFKEEP_TEST_DB_URL")
# The CI service gives this step its own empty database. A server start runs
# migrations before opening the health endpoint.
start() {
  "$1" serve --listen "$2" > "$tmp/server.log" 2>&1 & pid=$!
  for _ in {1..100}; do
    if curl -fsS "http://$2/healthz" > /dev/null; then return; fi
    if ! kill -0 "$pid" 2>/dev/null; then cat "$tmp/server.log"; return 1; fi
    sleep 0.1
  done
  cat "$tmp/server.log"
  return 1
}
stop() { kill "$pid"; wait "$pid" 2>/dev/null || true; pid=''; }
# First start exercises the fresh database, second the idempotent restart.
start "$tmp/new" "$MIGRATION_TEST_LISTEN"
stop
start "$tmp/new" "$MIGRATION_TEST_LISTEN"
stop
# Recreate the database, then let the previous binary establish its actual
# schema before inserting a legacy task and starting the new binary.
dropdb --maintenance-db="$admin_url" "$db_name"
createdb --maintenance-db="$admin_url" "$db_name"
start "$tmp/old" "$MIGRATION_TEST_LISTEN"
stop
psql "$HANDOFFKEEP_TEST_DB_URL" -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at)
VALUES('migration-test','legacy','implement','backlog','test',now(),now());
SQL
start "$tmp/new" "$MIGRATION_TEST_LISTEN"
psql "$HANDOFFKEEP_TEST_DB_URL" -v ON_ERROR_STOP=1 -tAc "SELECT 1/(CASE WHEN EXISTS (SELECT 1 FROM tasks WHERE title='legacy' AND expected IS NULL) AND EXISTS (SELECT 1 FROM schema_version WHERE version=22) THEN 1 ELSE 0 END)"
stop
