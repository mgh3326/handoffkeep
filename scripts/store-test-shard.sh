#!/usr/bin/env bash
# Print anchored -run regex for internal/store test shard 1 or 2 (hk 3478).
# Deterministically sorts test names listed by `go test -list . ./internal/store`
# and selects the first or second half.
set -euo pipefail

shard="${1:-}"
if [ "$shard" != "1" ] && [ "$shard" != "2" ]; then
  echo "usage: $0 <1|2>" >&2
  exit 1
fi

here=$(cd "$(dirname "$0")/.." && pwd)
cd "$here"

tests=$(go test -list . ./internal/store | grep "^Test" | LC_ALL=C sort)
total=$(wc -l <<< "$tests")
mid=$(( total / 2 ))

if [ "$shard" = "1" ]; then
  selected=$(head -n "$mid" <<< "$tests")
else
  selected=$(tail -n +"$(( mid + 1 ))" <<< "$tests")
fi

regex="^($(paste -sd '|' <<< "$selected"))$"
printf '%s\n' "$regex"
