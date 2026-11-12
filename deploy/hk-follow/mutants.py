#!/usr/bin/env python3
"""Mutants for deploy/hk-follow/hk-follow (hk 3509).

Each mutant removes one invariant check from a copy of the script; the test
suite (test-hk-follow.sh) must turn red on every one. Not run in CI (about two
minutes); run it after changing the gate or the rollout loop:

    python3 deploy/hk-follow/mutants.py [mutant-name ...]
"""
import pathlib
import shutil
import subprocess
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent

MUTANTS = {
    "I1-revision-unchecked": ('if ! valid_sha "$sha"; then\n\t\tverdict=refuse reason=no_revision',
                              'if false; then\n\t\tverdict=refuse reason=no_revision'),
    "I1-children-unchecked": ('elif [[ "$CAND_CHILDREN" != "$TAG_CHILDREN" ]]; then', "elif false; then"),
    "I1-ci-ok-ignored": ("for c in ci-ok deploy-ok; do", "for c in deploy-ok; do"),
    "I1-deploy-ok-ignored": ("for c in ci-ok deploy-ok; do", "for c in ci-ok; do"),
    "I1-main-ignored": ("ahead | identical) ;;\n\t*) verdict refuse not_on_main",
                        "*) ;;\n\tahead) verdict refuse not_on_main"),
    "forward-only-dropped": ("\t*) verdict refuse not_newer; return ;;", "\t*) ;;"),
    "I2-no-migration-check": ('[[ -z "$(migrations "$WORK/compare.json")" ]]', "true"),
    "I2-any-approval-body": ('grep -qxF "approve ${sha}"', "true"),
    "I2-300-cap": ('[[ "$n" -lt 300 ]] &&', "true &&"),
    "I3-no-healthz": ('while [[ "$(healthz_revision "$url")" != "$sha" ]]; do', "while false; do"),
    "I3-continue-after-failure": (
        'failed_step="shiftgate_${svc}_exit_${rc}"\n\t\t\tbreak',
        'failed_step="shiftgate_${svc}_exit_${rc}"\n\t\t\tcontinue'),
    "I4-terminal-retry": ("deployed | failed | refuse | noop)", "deployed | refuse | noop)"),
    "I4-no-started-state": ('save_state "$TAG_DIGEST" "$sha" started ""', "true"),
    "I4-shiftgate-hold-ignored": ('if [[ "$held" == "$TAG_DIGEST" ]]; then', "if false; then"),
    "I5-no-lock": ("if ! flock -n 9; then", "if false; then"),
    "promote-no-readback": (
        'if [[ $rc -ne 0 || "${NEW_REV:-}" != "$sha" || "${NEW_CHILDREN:-}" != "$CAND_CHILDREN" ]]; then',
        "if false; then"),
}


def main() -> int:
    src = (HERE / "hk-follow").read_text()
    chosen = sys.argv[1:] or list(MUTANTS)
    survivors = []
    with tempfile.TemporaryDirectory() as d:
        shutil.copy(HERE / "test-hk-follow.sh", d)
        for name in chosen:
            old, new = MUTANTS[name]
            if src.count(old) != 1:
                print(f"{name}: anchor matches {src.count(old)} times", file=sys.stderr)
                return 2
            pathlib.Path(d, "hk-follow").write_text(src.replace(old, new))
            r = subprocess.run(["bash", f"{d}/test-hk-follow.sh"], capture_output=True, text=True)
            red = r.returncode != 0
            print(f"{name}: {'red' if red else 'SURVIVED'}", flush=True)
            if not red:
                survivors.append(name)
    if survivors:
        print("surviving mutants: " + ", ".join(survivors), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
