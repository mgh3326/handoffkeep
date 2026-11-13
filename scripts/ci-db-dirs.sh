#!/usr/bin/env bash
# Print the package directories whose tests are DB-backed, one per line:
# a _test.go file that imports internal/testdb belongs to the nearest
# enclosing directory that holds Go files, and internal/testdb itself
# counts (hk 3528). Shared by ci.yml's pr-store-db decision step and
# main-linux's package selection (hk 4342); keep the marker in step with
# the contract in AGENTS.md §Checks. Exits non-zero when the tree cannot
# be read, so callers can fail safe; an empty answer is also a failure
# signal for callers, because this repository always has DB-backed tests.
set -euo pipefail

# The DB-backed marker: a _test.go file importing internal/testdb.
marker='^[[:space:]]*(import[[:space:]]+)?([[:alnum:]_.]+[[:space:]]+)?"[^"]*/internal/testdb"'
# Directory of each path; a top-level file is in ".".
dirs() { sed -e '/^$/d' -e 's|^[^/]*$|./&|' -e 's|/[^/]*$||' | sort -u; }

gofiles=$(git ls-files -- '*.go')
dbfiles=$(git grep -l -E "$marker" -- '*_test.go' || [ $? -eq 1 ])
pkgdirs=$(dirs <<< "$gofiles")
dbdirs=$(dirs <<< "$dbfiles")
if grep -Fxq internal/testdb <<< "$pkgdirs"; then
  dbdirs=$(printf '%s\ninternal/testdb\n' "$dbdirs" | sed '/^$/d' | sort -u)
fi
printf '%s\n' "$dbdirs" | sed '/^$/d'
