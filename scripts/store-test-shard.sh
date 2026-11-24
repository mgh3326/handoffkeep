#!/usr/bin/env bash
# Print the anchored -run regex for shard <index> of <count> of the
# internal/store tests (hk 3478, hk 6156). The shard space is every Test,
# Example and Fuzz function `go test -list` names, sorted with LC_ALL=C. Test k
# (counted from 0) belongs to shard k % count + 1, so the shards are disjoint,
# their union is the whole list, and a test added later lands in a shard with
# no edit here. The optional third argument "mysql" lists the tests under the
# mysql build tag, which is the list the MySQL suite runs. The partition is
# proved by internal/ciworkflow's TestCIShardsPartitionTheSuites.
# Run from the module root being tested. The script itself may come from any
# checkout (CI takes it from the workflow revision, hk 6156): it reads the
# module through the go tool in the current directory, so it never cd's.
set -euo pipefail

usage() {
  echo "usage: $0 <index> <count> [mysql]" >&2
  exit 1
}

index="${1:-}"
count="${2:-}"
tags="${3:-}"
case "$index" in '' | *[!0-9]*) usage ;; esac
case "$count" in '' | *[!0-9]*) usage ;; esac
if [ "$count" -lt 1 ] || [ "$index" -lt 1 ] || [ "$index" -gt "$count" ]; then
  usage
fi
case "$tags" in
  '' | mysql) ;;
  *) usage ;;
esac

tagargs=()
if [ "$tags" = mysql ]; then
  tagargs=(-tags=mysql)
fi
tests=$(go test "${tagargs[@]}" -list . ./internal/store | grep -E '^(Test|Example|Fuzz)' | LC_ALL=C sort)

# NR is 1-based, so NR % count == index % count is the 0-based k % count == index - 1.
selected=$(awk -v i="$index" -v n="$count" 'NR % n == i % n' <<< "$tests")
if [ -z "$selected" ]; then
  echo "shard $index of $count selects no store tests" >&2
  exit 1
fi

printf '^(%s)$\n' "$(paste -sd '|' <<< "$selected")"
