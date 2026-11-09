#!/usr/bin/env bash
# Database engines only: MySQL HeatWave (InnoDB FULLTEXT ngram, BOOLEAN and NATURAL)
# and Oracle Autonomous Database (Oracle Text CONTEXT: KOREAN_MORPH, BASIC, WORLD).
#
# Each engine runs in its own process, so one failing engine (connection, TLS,
# missing Instant Client, crash) does not stop the others. A failed engine is
# recorded as <OUT>/<engine>__failed.json and listed under "Failures" in summary.md.
# Exit status: 0 if every DB engine produced results, 1 if any failed, 2 on bad setup.
# `./run-db.sh --smoke`: per engine, build, one query per mode (top 3 printed), and a
# self-probe for local engines; no results written. Run it before a full run: it
# takes the build time (MySQL ingest is minutes) but no repeated query timing.
#
# Settings come from the environment only; values are never printed. The run
# creates and drops only: MySQL database `searchbench`; ADB tables SB_KM, SB_BASIC,
# SB_WORLD (with their CONTEXT indexes) and the lexer preference SB_KM_LEX (dropped
# after its index) in the RB_DB_USER schema.
#
# Required: MYSQL_HOST MYSQL_PORT MYSQL_ADMIN_USER MYSQL_ADMIN_PASSWORD
#           RB_DB_HOST RB_DB_PORT RB_DB_SERVICE RB_DB_USER RB_DB_PASSWORD RB_WALLET_DIR
# Optional: MYSQL_SSL=required|disabled (default required; disabled only for the
#           in-VCN private address), MYSQL_SSL_CA (CA bundle path for HeatWave),
#           OUT (default out/db), RUNS (default 30), ENGINES (space-separated engine
#           names to run; default all of them, see ENGINES below)
# Remote engines (read-only), each needs its own settings:
#   youtrack: YOUTRACK_URL (https), YOUTRACK_API_KEY, optional YOUTRACK_HK_FIELD (default "hk ID")
#   hk-pg:    HK_BIN (default handoffkeep on PATH); the CLI uses its own configuration
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
SMOKE=0
if [[ "${1:-}" == "--smoke" ]]; then
  # Build each engine, run one query per mode, self-probe local engines; writes no results.
  SMOKE=1
fi

export TNS_ADMIN="$RB_WALLET_DIR"
export CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-2}"
OUT="${OUT:-out/db}"
RUNS="${RUNS:-30}"
# Each engine is independent; an engine missing its own settings fails alone.
DEFAULT_ENGINES="mysql-heatwave-ngram oracle-text-korean-morph oracle-text-basic oracle-text-world youtrack hk-pg"
read -r -a ENGINES <<< "${ENGINES:-$DEFAULT_ENGINES}"

mkdir -p "$OUT" work
cargo build --release --features oracle

failed=0
if (( SMOKE )); then
  for e in "${ENGINES[@]}"; do
    set +e
    ./target/release/searchbench run db --smoke --only "$e"
    rc=$?
    set -e
    if (( rc != 0 )); then
      failed=$((failed + 1))
      echo "run-db.sh: $e SMOKE FAILED; continuing" >&2
    fi
  done
  echo "run-db.sh: smoke finished; failed engines: $failed of ${#ENGINES[@]}"
  exit $(( failed > 0 ))
fi
for e in "${ENGINES[@]}"; do
  # Clear this engine's earlier results so a failed rerun cannot show stale numbers.
  rm -f "$OUT/${e}__"*.json
  set +e
  ./target/release/searchbench run db --only "$e" --runs "$RUNS" --out "$OUT" --work work
  rc=$?
  set -e
  if (( rc != 0 )) && [[ ! -f "$OUT/${e}__failed.json" ]]; then
    # The process died before it could record a failure (crash or abort).
    printf '{\n  "engine": "%s",\n  "stage": "process",\n  "error": "exit status %d; see the run log"\n}\n' "$e" "$rc" \
      > "$OUT/${e}__failed.json"
  fi
  if [[ -f "$OUT/${e}__failed.json" ]]; then
    failed=$((failed + 1))
    echo "run-db.sh: $e FAILED (recorded in $OUT/${e}__failed.json); continuing" >&2
  fi
done

./target/release/searchbench summarize --out "$OUT" > /dev/null
echo "run-db.sh: results in $OUT (summary.md); failed engines: $failed of ${#ENGINES[@]}"
exit $(( failed > 0 ))
