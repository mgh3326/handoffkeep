#!/usr/bin/env bash
# Print "yes" when any go test pattern in "$@" can select a DB-backed
# package, else "no" (hk 4342). ./... always can while the tree has
# DB-backed tests; ./dir/... can when dir or a package below it is
# DB-backed; ./dir can only when dir itself is. An empty or unreadable
# DB-backed set answers "yes", the same fail-safe pr-store-db takes, so
# PostgreSQL starts rather than skipping tests it needs.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
dbdirs=$("$here/ci-db-dirs.sh" || true)
if [ -z "$dbdirs" ]; then
  printf 'yes\n'
  exit 0
fi

needed=no
for pat in "$@"; do
  subtree=no
  case "$pat" in */...) subtree=yes ;; esac
  p=${pat#./}
  p=${p%/...}
  # validate-ci-packages.sh accepts a trailing slash; ./internal/store/ is
  # the same package as ./internal/store.
  p=${p%/}
  if [ "$p" = "..." ] || [ "$p" = "." ]; then
    needed=yes
    break
  fi
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    if [ "$subtree" = yes ]; then
      if [ "$d" = "$p" ] || [[ "$d" == "$p/"* ]]; then
        needed=yes
        break 2
      fi
    elif [ "$d" = "$p" ]; then
      needed=yes
      break 2
    fi
  done <<< "$dbdirs"
done
printf '%s\n' "$needed"
