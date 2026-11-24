#!/usr/bin/env bash
# Print the Go packages of shard <index> of <count> for the race suite (hk 6156):
# every package of ./... except internal/store, which store-test-shard.sh splits
# by test. Packages are sorted with LC_ALL=C and dealt round-robin like the
# store tests, one import path per line. A shard can be empty when there are
# fewer packages than shards. internal/ciworkflow's TestCIShardsPartitionTheSuites
# proves the shards cover the list exactly once.
# Run from the module root being tested. The script itself may come from any
# checkout (CI takes it from the workflow revision, hk 6156): it reads the
# module through the go tool in the current directory, so it never cd's.
set -euo pipefail

usage() {
  echo "usage: $0 <index> <count>" >&2
  exit 1
}

index="${1:-}"
count="${2:-}"
case "$index" in '' | *[!0-9]*) usage ;; esac
case "$count" in '' | *[!0-9]*) usage ;; esac
if [ "$count" -lt 1 ] || [ "$index" -lt 1 ] || [ "$index" -gt "$count" ]; then
  usage
fi

pkgs=$(go list ./... | awk '!/\/internal\/store$/' | LC_ALL=C sort)

# NR is 1-based; the same mapping as store-test-shard.sh.
awk -v i="$index" -v n="$count" 'NR % n == i % n' <<< "$pkgs"
