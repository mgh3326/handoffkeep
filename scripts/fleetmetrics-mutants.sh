#!/usr/bin/env bash
# Runs every mutant in internal/fleetmetrics/testdata/mutants/*.json against a
# throwaway copy of the module and requires each one to turn a test RED by a
# failed assertion ("--- FAIL" with no panic and no build error). The working
# tree is never modified. Exit 0 only when every mutant is killed.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
dir="$root/internal/fleetmetrics/testdata/mutants"
mutants=("$dir"/*.json)
echo "mutants on disk: ${#mutants[@]} ($dir)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

killed=0
for spec in "${mutants[@]}"; do
  rm -rf "$work/m"
  mkdir -p "$work/m"
  cp "$root/go.mod" "$root/go.sum" "$work/m/"
  cp -R "$root/internal" "$work/m/internal"
  name="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$spec")"
  test="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["test"])' "$spec")"
  python3 - "$spec" "$work/m/internal/fleetmetrics" <<'PY'
import json, sys
spec = json.load(open(sys.argv[1]))
path = sys.argv[2] + "/" + spec["file"]
src = open(path).read()
n = src.count(spec["old"])
if n != 1:
    sys.exit(f"{spec['name']}: expected exactly one match in {spec['file']}, found {n}")
open(path, "w").write(src.replace(spec["old"], spec["new"]))
PY
  set +e
  out="$(cd "$work/m" && go test ./internal/fleetmetrics -run "$test" -count=1 2>&1)"
  rc=$?
  set -e
  if [[ $rc -ne 0 ]] && grep -q -- '--- FAIL' <<<"$out" && ! grep -q -e 'panic:' -e '\[build failed\]' <<<"$out"; then
    killed=$((killed + 1))
    first="$(grep -m1 -E '_test\.go:[0-9]+:' <<<"$out" | sed 's/^[[:space:]]*//')"
    echo "RED   $name ($test): $first"
  else
    echo "ALIVE $name ($test) rc=$rc"
    echo "$out" | tail -5
  fi
done
echo "killed $killed/${#mutants[@]}"
[[ $killed -eq ${#mutants[@]} ]]
