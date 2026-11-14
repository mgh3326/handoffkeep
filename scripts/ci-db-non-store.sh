#!/usr/bin/env bash
# Print DB-backed packages other than internal/store (hk 3478).
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
cd "$here"

while IFS= read -r d; do
  [ -n "$d" ] || continue
  if [ "$d" != "internal/store" ]; then
    printf './%s\n' "$d"
  fi
done < <(bash scripts/ci-db-dirs.sh)
