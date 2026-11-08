#!/usr/bin/env bash
# All engines in one run: local (tantivy default and lindera, SQLite FTS5 unicode61
# and trigram) then the database engines via run-db.sh. Results go flat into OUT
# so summary.md covers every engine. Database settings are checked before anything
# runs (see run-db.sh for names); with LOCAL_ONLY=1 only the local engines run.
# RUNS (default 100) is for the local engines; RUNS_DB (default 30) for the DB engines.
# A failing DB engine does not stop the run: summary.md lists it under Failures,
# and the script exits with run-db.sh's status once the summary is written.
set -euo pipefail
cd "$(dirname "$0")"

export CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}"
OUT="${OUT:-out/all}"
RUNS="${RUNS:-100}"
RUNS_DB="${RUNS_DB:-30}"

if [[ ! -f corpus/documents.jsonl || ! -f queries.jsonl ]]; then
  echo "run-all.sh: corpus/ and queries.jsonl are required (corpus is not in git; copy it in first)" >&2
  exit 2
fi

if [[ "${LOCAL_ONLY:-0}" != "1" ]]; then
  ./run-db.sh --check   # fail before any run if the database settings are missing
fi

cargo build --release --features oracle

./target/release/searchbench run local --runs "$RUNS" --out "$OUT" --work work

db_rc=0
if [[ "${LOCAL_ONLY:-0}" != "1" ]]; then
  OUT="$OUT" RUNS="$RUNS_DB" ./run-db.sh || db_rc=$?
fi

./target/release/searchbench summarize --out "$OUT" > /dev/null
echo "run-all.sh: done, see $OUT/summary.md (run-db.sh status $db_rc)"
exit "$db_rc"
