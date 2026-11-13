#!/usr/bin/env python3
"""Kill reopen regression mutants with narrow tests and Go source overlays.

Every mutant needs CI's PostgreSQL service. No source file in the checkout is
modified.
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
        "reopen-keeps-previous-pr", "store/task_flow.go",
        '\t\tx.Refs.PR = ""\n', "",
        "TestReopenClearsCycleRefs", "stale cycle refs after reopen",
    ),
    (
        "disposition-can-reopen", "store/task_flow.go",
        '\t\tif reopen {\n'
        '\t\t\treturn Task{}, fmt.Errorf("%w: disposition items cannot be reopened", ErrTaskConflict)\n'
        '\t\t}\n', "",
        "TestReopenRefusesDisposition", "disposition reopen refusal",
    ),
    (
        "disposition-can-claim", "store/task_flow.go",
        '\tif hasDisposition {\n'
        '\t\treturn Task{}, fmt.Errorf("%w: disposition items cannot be claimed directly", ErrTaskConflict)\n'
        '\t}\n', "",
        "TestClaimRefusesDisposition", "disposition claim refusal",
    ),
    (
        "reopen-ignores-caller", "api/api.go",
        "s.Service.ReopenTask(r.Context(), id, client, input.Reason)",
        # Keep client referenced so the mutant compiles; only the caller passed
        # to ReopenTask changes.
        's.Service.ReopenTask(r.Context(), id, func() string { _ = client; return "api" }(), input.Reason)',
        "TestTaskReopenAPI", "reopen caller binding",
    ),
]


def run_test(package, test, overlay=None):
    command = ["go", "test", "-p", "1", "-count=1", "-timeout=2m"]  # bounded by go test
    if overlay:
        command += ["-overlay", str(overlay)]
    command += ["./internal/" + package, "-run", "^" + test + "$"]
    env = dict(os.environ, GOMAXPROCS="2")
    return subprocess.run(command, cwd=ROOT, env=env, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    if not os.environ.get("HANDOFFKEEP_TEST_DB_URL"):
        parser.error("HANDOFFKEEP_TEST_DB_URL is required")
    selected = MUTANTS
    with tempfile.TemporaryDirectory(prefix="hk-reopen-mutants-") as work:
        for name, relative, old, new, test, assertion in selected:
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
