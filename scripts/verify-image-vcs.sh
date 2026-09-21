#!/bin/sh
# verify-image-vcs.sh — assert that a handoffkeep image or bare binary carries
# the expected Go VCS stamp, using the same signal the deploy runbook checks
# (vcs.revision in the binary; /healthz exposes it as vcs_revision at runtime).
#
#   scripts/verify-image-vcs.sh <image-ref|binary-path> [expected-sha]
#
# <image-ref> requires docker (the binary is extracted via docker create/cp).
# <binary-path> works anywhere. expected-sha defaults to `git rev-parse HEAD`.
# Exit 0 only when vcs.revision equals the expected SHA exactly; vcs.modified
# is printed so dirty-tree builds are visible (runbook expects "false").
set -eu

if [ $# -lt 1 ]; then
  echo "usage: $0 <image-ref|binary-path> [expected-sha]" >&2
  exit 2
fi
target=$1
want=${2:-}
if [ -z "$want" ]; then
  want=$(git rev-parse HEAD)
fi

tmpd=
if [ -f "$target" ]; then
  bin=$target
else
  command -v docker >/dev/null 2>&1 || {
    echo "verify-image-vcs: docker is required for image target '$target'" >&2
    exit 2
  }
  tmpd=$(mktemp -d)
  trap 'rm -rf "$tmpd"' EXIT
  cid=$(docker create "$target")
  docker cp "$cid:/handoffkeep" "$tmpd/handoffkeep"
  docker rm "$cid" >/dev/null
  bin=$tmpd/handoffkeep
fi

if command -v go >/dev/null 2>&1; then
  go version -m "$bin" | grep -E 'vcs\.(revision|time|modified)=' || true
fi

rev=$(grep -a -o 'vcs\.revision=[0-9a-f]*' "$bin" | head -n1 | cut -d= -f2)
if [ -z "$rev" ]; then
  echo "verify-image-vcs: FAIL — no vcs.revision stamp in $target (built without .git or with -buildvcs=false?)" >&2
  exit 1
fi
if [ "$rev" != "$want" ]; then
  echo "verify-image-vcs: FAIL — vcs.revision=$rev, want $want" >&2
  exit 1
fi
echo "verify-image-vcs: OK vcs.revision=$rev"
