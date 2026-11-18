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
        "balance-negative-accepted", "store/quota.go", False,
        "\tif x.Balance != nil && (math.IsNaN(*x.Balance) || math.IsInf(*x.Balance, 0) || *x.Balance < 0) {\n\t\treturn false\n\t}\n",
        "",
        "TestQuotaBalanceValidation", "balance",
    ),
    (
        "dedupe-removed", "store/quota.go", True,
        "\tif err == nil {\n\t\tvar existing QuotaObservation\n",
        "\terr = ErrNoRows // mutant: the duplicate lookup always misses\n\tif err == nil {\n\t\tvar existing QuotaObservation\n",
        "TestQuotaDedupePostgres", "dedupe",
    ),
    (
        "confirmed-at-client-supplied", "store/quota.go", True,
        "\t// confirmed_at is server-owned: never store a client-supplied value.\n"
        "\tx.ConfirmedAt = nil\n",
        "",
        "TestQuotaDedupePostgres", "confirmed_at",
    ),
    (
        "dedupe-includes-measured-at", "store/quota.go", True,
        "(o.value - 'measured_at') = ($4::jsonb - 'measured_at')",
        "o.value = $4::jsonb",
        "TestQuotaDedupePostgres", "dedupe freshness",
    ),
    (
        "confirmed-at-legacy-floor-removed", "store/quota.go", True,
        "GREATEST(confirmed_at,measured_at,$4)",
        "GREATEST(confirmed_at,$4)",
        "TestQuotaDedupePostgres", "dedupe legacy floor",
    ),
    (
        "grant-generic-guard-first", "api/api.go", False,
        '\t\tk, v, ok := strings.Cut(line, "=")\n'
        '\t\tif id, isQuota := strings.CutPrefix(k, "HANDOFFKEEP_QUOTA_"); isQuota {\n',
        '\t\tk, v, ok := strings.Cut(line, "=")\n'
        '\t\tif !ok || v == "" {\n'
        '\t\t\treturn nil, nil, errors.New("invalid HANDOFFKEEP_AUTH_FILE")\n'
        '\t\t}\n'
        '\t\tif id, isQuota := strings.CutPrefix(k, "HANDOFFKEEP_QUOTA_"); isQuota {\n',
        "TestQuotaGrantFile", "quota grant refused startup",
    ),
    (
        "prune-flag-not-reset", "store/quota.go", True,
        " PERFORM set_config('handoffkeep.quota_prune', 'off', true);",
        "",
        "TestQuotaRetentionPostgres", "prune flag leaked",
    ),
    (
        "grant-unknown-client-refuses", "api/api.go", False,
        '\t\t\tfmt.Fprintf(os.Stderr, "handoffkeep: ignoring HANDOFFKEEP_QUOTA_ grants for unknown client %q\\n", id)\n'
        '\t\t\tdelete(grants, id)',
        '\t\t\treturn nil, nil, errors.New("quota grant for unknown client")',
        "TestQuotaGrantFile", "quota grant refused startup",
    ),
    (
        "grant-malformed-refuses", "api/api.go", False,
        '\t\t\t\tfmt.Fprintf(os.Stderr, "handoffkeep: ignoring invalid quota grant line %q\\n", line)\n'
        '\t\t\t\tcontinue',
        '\t\t\t\treturn nil, nil, errors.New("invalid quota grant line")',
        "TestQuotaGrantFile", "quota grant refused startup",
    ),
    (
        "schema-version-not-idempotent", "store/quota.go", True,
        '`INSERT INTO schema_version(version) VALUES (23) ON CONFLICT DO NOTHING`',
        '`INSERT INTO schema_version(version) VALUES (23)`',
        "TestQuotaPostgres", "idempotent migration",
    ),
    (
        "retention-deletes-referenced", "store/quota.go", True,
        "interval '90 days' AND id NOT IN (SELECT observation_id FROM quota_latest)", "interval '90 days'",
        "TestQuotaRetentionPostgres", "retention",
    ),
    (
        "trigger-allows-arbitrary-delete", "store/quota.go", True,
        "IF TG_OP = 'DELETE' AND current_setting('handoffkeep.quota_prune', true) = 'on' THEN",
        "IF TG_OP = 'DELETE' THEN",
        "TestQuotaRetentionPostgres", "retention arbitrary delete allowed",
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
