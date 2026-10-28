#!/usr/bin/env python3
"""Kill quota follow-up mutants with narrow tests and Go source overlays.

Store mutants require CI's PostgreSQL service. --no-db runs only the API
scope mutants. No source file in the checkout is modified.
"""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parent.parent
SCOPE = 'if !ok || !s.quotaScope(w, client, r.PathValue("key")) {'
MUTANTS = [
    (
        "dedupe-removed", "store/quota.go", True,
        '\t// The lease row lock above serializes publishers for this account, so a\n'
        '\t// concurrent identical publish observes the committed row here instead of\n'
        '\t// double-appending.\n'
        '\tvar dupID int64\n'
        '\tvar dupValue []byte\n'
        '\terr = tx.QueryRow(ctx, `SELECT o.id,o.value FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 AND l.kind=$2 AND l.window_key=$3 AND o.value=$4`, key, x.Kind, x.Window, data).Scan(&dupID, &dupValue)\n'
        '\tif err == nil {\n'
        '\t\tif err = json.Unmarshal(dupValue, &x); err != nil {\n'
        '\t\t\treturn x, false, err\n'
        '\t\t}\n'
        '\t\tx.ID = dupID\n'
        '\t\treturn x, true, tx.Commit(ctx)\n'
        '\t}\n'
        '\tif !errors.Is(err, pgx.ErrNoRows) {\n'
        '\t\treturn x, false, err\n'
        '\t}\n', "",
        "TestQuotaDedupePostgres", "dedupe",
    ),
    (
        "schema-version-not-idempotent", "store/quota.go", True,
        '`INSERT INTO schema_version(version) VALUES (22) ON CONFLICT DO NOTHING`',
        '`INSERT INTO schema_version(version) VALUES (22)`',
        "TestQuotaPostgres", "idempotent migration",
    ),
    (
        "scope-put-removed", "api/quota.go", False,
        'func (s Server) quotaPut(w http.ResponseWriter, r *http.Request) {\n'
        '\tclient, ok := s.quotaAuth(w, r)\n'
        '\t' + SCOPE + '\n'
        '\t\treturn\n'
        '\t}',
        'func (s Server) quotaPut(w http.ResponseWriter, r *http.Request) {\n'
        '\tclient, ok := s.quotaAuth(w, r)\n'
        '\tif !ok {\n'
        '\t\treturn\n'
        '\t}',
        "TestQuotaWriteScope", "observation: 400",
    ),
    (
        "scope-lease-removed", "api/quota.go", False,
        'func (s Server) quotaLease(w http.ResponseWriter, r *http.Request) {\n'
        '\tclient, ok := s.quotaAuth(w, r)\n'
        '\t' + SCOPE + '\n'
        '\t\treturn\n'
        '\t}',
        'func (s Server) quotaLease(w http.ResponseWriter, r *http.Request) {\n'
        '\tclient, ok := s.quotaAuth(w, r)\n'
        '\tif !ok {\n'
        '\t\treturn\n'
        '\t}',
        "TestQuotaWriteScope", "POST /v1/quota/accounts/codex:0123456789abcdef/lease: 400",
    ),
    (
        "scope-release-removed", "api/quota.go", False,
        'func (s Server) quotaRelease(w http.ResponseWriter, r *http.Request) {\n'
        '\tclient, ok := s.quotaAuth(w, r)\n'
        '\t' + SCOPE + '\n'
        '\t\treturn\n'
        '\t}',
        'func (s Server) quotaRelease(w http.ResponseWriter, r *http.Request) {\n'
        '\tclient, ok := s.quotaAuth(w, r)\n'
        '\tif !ok {\n'
        '\t\treturn\n'
        '\t}',
        "TestQuotaWriteScope", "DELETE /v1/quota/accounts/codex:0123456789abcdef/lease: 400",
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
    parser.add_argument("--no-db", action="store_true")
    args = parser.parse_args()
    if not args.no_db and not os.environ.get("HANDOFFKEEP_TEST_DB_URL"):
        parser.error("HANDOFFKEEP_TEST_DB_URL is required; use --no-db locally")
    selected = [m for m in MUTANTS if not (args.no_db and m[2])]
    with tempfile.TemporaryDirectory(prefix="hk-quota-mutants-") as work:
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
    print(f"Killed {len(selected)}/{len(selected)} quota mutants", flush=True)


if __name__ == "__main__":
    main()
