#!/usr/bin/env python3
"""Kill reopen regression mutants with narrow tests and Go source overlays.

The three store mutants require CI's PostgreSQL service. --no-db runs only
the fleetmetrics mutant. No source file in the checkout is modified.
"""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parent.parent
MUTANTS = [
    (
        "reopen-keeps-previous-pr", "store/store.go", True,
        '\t\tx.Refs.PR = ""\n', "",
        "TestReopenClearsCycleRefs", "stale cycle refs after reopen",
    ),
    (
        "disposition-can-reopen", "store/store.go", True,
        '\t\tif to == "reopened" {\n'
        '\t\t\treturn Task{}, fmt.Errorf("%w: disposition items cannot be reopened", ErrTaskConflict)\n'
        '\t\t}\n', "",
        "TestReopenRefusesDisposition", "disposition reopen refusal",
    ),
    (
        "disposition-can-claim", "store/store.go", True,
        '\tif hasDisposition {\n'
        '\t\treturn Task{}, fmt.Errorf("%w: disposition items cannot be claimed directly", ErrTaskConflict)\n'
        '\t}\n', "",
        "TestClaimRefusesDisposition", "disposition claim refusal",
    ),
    (
        "lead-time-inherits-previous-cycle", "fleetmetrics/metrics.go", False,
        "prs := ix.leadTimePRs(t)", "prs := ix.taskPRs[id]",
        "TestLeadTimeReopenedCycleLinks", "current cycle lead time=",
    ),
]


def run_test(package, test, overlay=None):
    command = ["go", "test", "-p", "1", "-count=1", "-timeout=2m"]
    if overlay:
        command += ["-overlay", str(overlay)]
    command += ["./internal/" + package, "-run", "^" + test + "$"]
    env = dict(os.environ, GOMAXPROCS="2")
    return subprocess.run(command, cwd=ROOT, env=env, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--no-db", action="store_true")
    args = parser.parse_args()
    if not args.no_db and not os.environ.get("HANDOFFKEEP_TEST_DB_URL"):
        parser.error("HANDOFFKEEP_TEST_DB_URL is required; use --no-db locally")
    selected = [m for m in MUTANTS if not (args.no_db and m[2])]
    with tempfile.TemporaryDirectory(prefix="hk-reopen-mutants-") as work:
        for name, relative, _, old, new, test, assertion in selected:
            source = ROOT / "internal" / relative
            original = source.read_text()
            if original.count(old) != 1:
                raise SystemExit(f"{name}: expected exactly one mutation target")
            package = relative.split("/")[0]
            baseline = run_test(package, test)
            if baseline.returncode != 0:
                raise SystemExit(f"{name}: baseline failed\n{baseline.stdout}")
            replacement = Path(work) / "mutant.go"
            replacement.write_text(original.replace(old, new))
            overlay = Path(work) / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {str(source): str(replacement)}}))
            result = run_test(package, test, overlay)
            output = result.stdout
            if (result.returncode == 0 or "--- FAIL: " + test not in output
                    or assertion not in output or "panic:" in output
                    or "[build failed]" in output):
                raise SystemExit(f"ALIVE/invalid {name}\n{output}")
            print(f"KILLED {name}: {test} failed its regression assertion", flush=True)
    print(f"Killed {len(selected)}/{len(selected)} reopen mutants", flush=True)


if __name__ == "__main__":
    main()
