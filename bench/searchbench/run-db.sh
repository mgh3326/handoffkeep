#!/usr/bin/env bash
# Database engines only: MySQL HeatWave (InnoDB FULLTEXT ngram, BOOLEAN and NATURAL)
# and Oracle Autonomous Database (Oracle Text CONTEXT: KOREAN_MORPH, BASIC, WORLD).
#
# Settings come from the environment only; values are never printed. The run
# creates and drops only: MySQL database `searchbench`; ADB tables SB_KM, SB_BASIC,
# SB_WORLD (with their CONTEXT indexes) in the RB_DB_USER schema.
#
# Required: MYSQL_HOST MYSQL_PORT MYSQL_ADMIN_USER MYSQL_ADMIN_PASSWORD
#           RB_DB_HOST RB_DB_PORT RB_DB_SERVICE RB_DB_USER RB_DB_PASSWORD RB_WALLET_DIR
# Optional: MYSQL_SSL_CA (CA bundle path for HeatWave), OUT (default out/db), RUNS (default 100)
# Oracle needs Oracle Instant Client on the library path (e.g. LD_LIBRARY_PATH).
set -euo pipefail
cd "$(dirname "$0")"

required=(MYSQL_HOST MYSQL_PORT MYSQL_ADMIN_USER MYSQL_ADMIN_PASSWORD RB_DB_HOST RB_DB_PORT RB_DB_SERVICE RB_DB_USER RB_DB_PASSWORD RB_WALLET_DIR)
missing=()
for v in "${required[@]}"; do
  [[ -n "${!v:-}" ]] || missing+=("$v")
done
if (( ${#missing[@]} )); then
  echo "run-db.sh: missing environment: ${missing[*]}" >&2
  exit 2
fi
if [[ ! -f corpus/documents.jsonl || ! -f queries.jsonl ]]; then
  echo "run-db.sh: corpus/ and queries.jsonl are required (corpus is not in git; copy it in first)" >&2
  exit 2
fi
if [[ "${1:-}" == "--check" ]]; then
  echo "run-db.sh: environment and corpus present"
  exit 0
fi

export TNS_ADMIN="$RB_WALLET_DIR"
export CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}"
OUT="${OUT:-out/db}"
RUNS="${RUNS:-100}"

cargo build --release --features oracle
./target/release/searchbench run db --runs "$RUNS" --out "$OUT" --work work
./target/release/searchbench summarize --out "$OUT" > /dev/null
echo "run-db.sh: results in $OUT (summary.md)"
