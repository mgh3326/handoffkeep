#!/usr/bin/env python3
"""Generate the hk 1924 O3 contract fixture pack into testdata/o3/.

Usage: gen.py [--out DIR] [--design FILE]

--out defaults to testdata/o3.  --design names the pinned design body
(plain markdown, revision 4); it defaults to design.md next to this
script.  The design body sha256 is checked against the pinned value
before anything is written.
"""
import argparse
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from genlib import *  # noqa
import gen_legacy

_ap = argparse.ArgumentParser()
_ap.add_argument("--out", default="testdata/o3")
_ap.add_argument("--design", default=os.path.join(HERE, "design.md"))
_args = _ap.parse_args()
OUT = _args.out
DESIGN_MD = _args.design

LEGACY_FILES = {}  # relpath -> bytes, filled by write_legacy()


def legacy_sha(rel):
    return sha(LEGACY_FILES["legacy/" + rel])


def legacy_b64(rel):
    return b64(LEGACY_FILES["legacy/" + rel])


def emit(rel, obj):
    path = os.path.join(OUT, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    data = obj if isinstance(obj, (str, bytes)) else json.dumps(
        obj, indent=2,
        default=lambda o: {"$b64": b64(o)}
        if isinstance(o, (bytes, bytearray)) else None) + "\n"
    mode = "wb" if isinstance(data, bytes) else "w"
    with open(path, mode) as f:
        f.write(data)


def imp_record(j, rel, lkind, actor="desktop",
               at="2026-10-20T00:00:00.000000Z"):
    loc = "testdata/o3/legacy/" + rel
    content = LEGACY_FILES["legacy/" + rel]
    return j.add("import",
                 {"source_locator": loc, "source_sha256": sha(content),
                  "original_actor_client_id": actor, "original_at": at,
                  "legacy_kind": lkind},
                 actor="director-1", imported=True,
                 source={"source_locator": loc, "source_sha256": sha(content)})


def manifest_of(j, begin_seq):
    rows = [{"seq": r["seq"],
             "source_locator": r["payload"]["source_locator"],
             "source_sha256": r["payload"]["source_sha256"]}
            for r in j.records if r["kind"] == "import"
            and r["seq"] > begin_seq - 1]
    rows.sort(key=lambda r: r["seq"])
    return rows


def expect_record(given, job, kind, payload, actor):
    """The record a server would append to `job` after `given` records."""
    mine = [r for r in given if r["job_id"] == job]
    seq = (mine[-1]["seq"] + 1) if mine else 1
    claim_seq = seq if kind == "claim" else next(
        (r["seq"] for r in reversed(mine) if r["kind"] == "claim"), 0)
    rec = {
        "job_id": job, "seq": seq,
        "record_id": uuid(800000 + seq), "store_generation": GEN,
        "claim_seq": claim_seq, "actor_client_id": actor,
        "idempotency_key": uuid(800000 + seq + 1000),
        "request_sha256": ZERO64, "imported": False, "source": None,
        "received_at": "2026-10-24T12:00:00.000000Z",
        "previous_hash": mine[-1]["record_hash"] if mine else ZERO64,
        "kind": kind, "payload": payload,
    }
    rec["request_sha256"] = sha({"method": "POST",
                                 "path": "/v1/wrk/jobs/%s/records" % job,
                                 "body": {"kind": kind,
                                          "claim_seq": 0 if kind == "claim"
                                          else claim_seq,
                                          "expected_seq": seq - 1,
                                          "payload": input_view(kind,
                                                                dict(payload))}})
    rec["record_hash"] = sha(rec)
    return rec


CLOCK_RESERVE = 100000


def clock_block(original_count):
    """Build a case whose record count changed without shifting the
    timestamps of later, untouched cases.  The new records take clocks
    from a reserved range; afterwards the global clock advances by the
    number of records the original case consumed."""
    start = Journal.clock[0]
    Journal.clock[0] = start + CLOCK_RESERVE

    def restore():
        Journal.clock[0] = start + original_count
    return restore


def splice(base, claims):
    """Return base records with each reviewer claim moved before the first
    record (round_open/verdict/grant) whose payload names its job_id, so
    every named tester has an earlier-received claim.  Non-claim records
    (an end) keep their place after the base history."""
    out = []
    claims = list(claims)
    moved = [c for c in claims if c["kind"] == "claim"]
    rest = [c for c in claims if c["kind"] != "claim"]
    for r in base:
        for c in moved[:]:
            pl = r.get("payload")
            if isinstance(pl, dict) and pl.get("tester_job_id") == c["job_id"]:
                out.append(c)
                moved.remove(c)
        out.append(r)
    return out + moved + rest


# ============================================================ §5 transition
def build_transition():
    T = "Minimum cross-implementation transition vectors"
    S = "5"
    c = []

    # row 1
    j = activated()
    c.append(case("open-h-round-1", S, T, "transition", 1, j.records,
                  http("desktop", JOB, "round_open", 1, 2, open_input(H)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H, round_no=1)},
                   "summary": {"state": "active", "baseline_used": 0,
                               "used": 1, "next_round_no": 2,
                               "open_rounds": [1]}}))
    j = opened(activated())
    vb = verdict_bytes(H)
    c.append(case("verdict-pass-h-binds-round-1", S, T, "transition", 1,
                  j.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                     vb)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "verdict",
                              "payload": {"round_no": 1, "head": H,
                                          "verdict": "PASS",
                                          "path": "/jobs/job-a/verdict-r1.md",
                                          "content_sha256": sha(vb),
                                          "size_bytes": len(vb),
                                          "tester_job_id": None}},
                   "summary": {"used": 1, "open_rounds": [],
                               "last_verdict_seq": 4,
                               "next_round_no": 2}}))

    # row 2 / row 3: replay after end, same-key-changed-path
    j = verdicted(opened(activated()), full_digest=True)
    j.add("end", end_in(), actor=j.actor)
    v = j.records[3]
    c.append(case("replay-verdict-after-end-200", S, T, "transition", 2,
                  j.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                     vb), key=v["idempotency_key"]),
                  {"http_status": 200, "created": False,
                   "record_ref": {"seq": 4, "record_id": v["record_id"],
                                  "record_hash": v["record_hash"]},
                   "no_new_records": True}))
    c.append(case("same-key-changed-path-idempotency-conflict", S, T,
                  "transition", 3, j.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-rX.md",
                                     vb), key=v["idempotency_key"]),
                  {"http_status": 409, "error": "idempotency_conflict",
                   "no_new_records": True}))

    # row 4: two simultaneous opens at used=2 same expected_seq
    j = opened(opened(activated(), 1, H), 2, H2)
    c.append(case("concurrent-open-winner-used-3", S, T, "transition", 4,
                  j.records,
                  http("desktop", JOB, "round_open", 1, 4, open_input(H3)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H3, round_no=3)},
                   "summary": {"used": 3, "next_round_no": 4,
                               "open_rounds": [1, 2, 3]}}))
    c.append(case("concurrent-open-loser-sequence-conflict", S, T,
                  "transition", 4, j.records,
                  http("desktop", JOB, "round_open", 1, 3, open_input(H3)),
                  {"http_status": 409, "error": "sequence_conflict"}))
    j = opened(opened(opened(activated(), 1, H), 2, H2), 3, H3)
    c.append(case("refreshed-retry-no-grant-round-cap", S, T, "transition", 4,
                  j.records,
                  http("desktop", JOB, "round_open", 1, 5, open_input(H4)),
                  {"http_status": 409, "error": "round_cap"}))

    # row 5: PASS H2 on open H1
    c.append(case("verdict-wrong-head-head-mismatch", S, T, "transition", 5,
                  opened(activated()).records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H2, "PASS",
                                     "/jobs/job-a/verdict-r1.md",
                                     verdict_bytes(H2))),
                  {"http_status": 409, "error": "head_mismatch",
                   "summary": {"open_rounds": [1], "used": 1},
                   "note": "open remains pending"}))

    # row 6: duplicate_path / duplicate_content
    j = opened(verdicted(opened(activated()), 1, H, "PASS"), 2, H2)
    vb2 = verdict_bytes(H2)
    c.append(case("same-path-different-bytes-duplicate-path", S, T,
                  "transition", 6, j.records,
                  http("desktop", JOB, "verdict", 1, 5,
                       verdict_input(2, H2, "PASS",
                                     "/jobs/job-a/verdict-r1.md", vb2)),
                  {"http_status": 409, "error": "duplicate_path"}))
    c.append(case("same-bytes-fresh-path-duplicate-content", S, T,
                  "transition", 6, j.records,
                  http("desktop", JOB, "verdict", 1, 5,
                       verdict_input(2, H2, "PASS",
                                     "/jobs/job-a/verdict-r2.md", vb)),
                  {"http_status": 409, "error": "duplicate_content"}))

    # row 7: second open H - duplicate_head / head_repeat grant
    j = verdicted(opened(activated()), 1, H, "PASS")
    c.append(case("second-open-h-no-grant-duplicate-head", S, T,
                  "transition", 7, j.records,
                  http("desktop", JOB, "round_open", 1, 4, open_input(H)),
                  {"http_status": 409, "error": "duplicate_head"}))
    j = activated()
    opened(j, 1, H)
    verdicted(j, 1, H, "PASS")
    rev = reviewer_journal(REV, JOB, 2)
    granted(j, "head_repeat", round_no=2, head=H, earlier=1, tester=REV,
            reason="repeat_review")
    c.append(case("second-open-h-with-head-repeat", S, T, "transition", 7,
                  splice(j.records, rev.records),
                  http("desktop", JOB, "round_open", 1, 5,
                       open_input(H, tester=REV, rep=5)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H, tester=REV, rep=5,
                                                    round_no=2)},
                   "summary": {"used": 2, "next_round_no": 3,
                               "open_rounds": [2]},
                   "note": "separate numbered round charged against cap"}))

    # row 8: over-cap extension; reuse -> grant_consumed / sequence_conflict
    j = opened(opened(opened(activated(), 1, H), 2, H2), 3, H3)
    granted(j, "extension", round_no=4, head=H4, extra=1,
            reason="cap_extension")
    c.append(case("open-beyond-cap-with-extension", S, T, "transition", 8,
                  j.records,
                  http("desktop", JOB, "round_open", 1, 6,
                       open_input(H4, ext=6)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H4, ext=6, round_no=4)},
                   "summary": {"used": 4, "next_round_no": 5,
                               "open_rounds": [1, 2, 3, 4],
                               "active_grants": []}}))
    j2 = opened(opened(opened(activated(), 1, H), 2, H2), 3, H3)
    granted(j2, "extension", round_no=4, head=H4, extra=1,
            reason="cap_extension")
    j2.add("round_open", {"round_no": 4, **open_input(H4, ext=6)})
    c.append(case("reuse-consumed-extension-grant-consumed", S, T,
                  "transition", 8, j2.records,
                  http("desktop", JOB, "round_open", 1, 7,
                       open_input(H5, ext=6)),
                  {"http_status": 409, "error": "grant_consumed"}))
    c.append(case("reuse-extension-stale-seq-sequence-conflict", S, T,
                  "transition", 8, j2.records,
                  http("desktop", JOB, "round_open", 1, 6,
                       open_input(H5, ext=6)),
                  {"http_status": 409, "error": "sequence_conflict",
                   "note": "stale expected_seq reports sequence_conflict first"}))

    # row 9: void verdict-bearing round
    j = verdicted(opened(activated()), 1, H, "PASS")
    granted(j, "void", round_no=1, reason="void_round")
    c.append(case("void-verdict-bearing-round-has-verdict", S, T,
                  "transition", 9, j.records,
                  http("desktop", JOB, "void", 1, 5, void_in(1, 5)),
                  {"http_status": 409, "error": "round_has_verdict",
                   "note": "grant remains unconsumed"}))

    # row 10: void an open, then verdict on it
    j = opened(activated(), 1, H)
    granted(j, "void", round_no=1, reason="void_round")
    c.append(case("void-open-with-grant-201", S, T, "transition", 10,
                  j.records,
                  http("desktop", JOB, "void", 1, 4, void_in(1, 4)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "void", "payload": void_in(1, 4)},
                   "summary": {"used": 0, "open_rounds": [],
                               "next_round_no": 2}}))
    j2 = opened(activated(), 1, H)
    granted(j2, "void", round_no=1, reason="void_round")
    j2.add("void", void_in(1, 4))
    c.append(case("verdict-on-voided-round-round-voided", S, T, "transition",
                  10, j2.records,
                  http("desktop", JOB, "verdict", 1, 5,
                       verdict_input(1, H, "PASS",
                                     "/jobs/job-a/verdict-r1.md",
                                     verdict_bytes(H))),
                  {"http_status": 409, "error": "round_voided"}))

    # row 11: foreign host without assignment
    jb = activated(job=JOBB, host="mac-dev", owner="b2401-b", task=2401)
    opened(jb, 1, H)
    c.append(case("foreign-host-verdict-host-mismatch", S, T, "transition",
                  11, jb.records,
                  http("desktop", JOBB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS",
                                     "/jobs/job-b/verdict-r1.md",
                                     verdict_bytes(H))),
                  {"http_status": 403, "error": "host_mismatch",
                   "no_new_records": True},
                  job=JOBB))

    # row 12: planted import + activate -> zero_rounds / extension needed
    j = imported_activated()
    c.append(case("imported-activated-join-zero-rounds", S, T, "transition",
                  12, j.records,
                  http("desktop", SHADOW, "joined", 1, j.tail(),
                       joined_in(head=H)),
                  {"http_status": 409, "error": "zero_rounds",
                   "note": "imported PASS/grant never trusted; baseline>=3"},
                  job=SHADOW))
    c.append(case("imported-activated-open-needs-extension", S, T,
                  "transition", 12, j.records,
                  http("desktop", SHADOW, "round_open", 1, j.tail(),
                       open_input(H4)),
                  {"http_status": 409, "error": "round_cap",
                   "note": "a fresh review needs a central extension grant"},
                  job=SHADOW))

    # row 13: mutation after end
    j = verdicted(opened(activated()), 1, H, "PASS")
    j.add("end", end_in())
    c.append(case("new-mutation-after-end-job-ended", S, T, "transition", 13,
                  j.records,
                  http("desktop", JOB, "round_open", 1, 5, open_input(H2)),
                  {"http_status": 409, "error": "job_ended",
                   "note": "successful prior-key replay still returns 200"}))

    # row 14: pagination snapshot
    j = opened(opened(opened(activated(), 1, H), 2, H2), 3, H3)
    j.add("completed", {"report_sha256": REPORT_SHA,
                        "report_size_bytes": len(REPORT), "warnings": []})
    c.append(case("records-page-snapshot-first-page", S, T, "transition", 14,
                  j.records,
                  http_get("desktop", "/v1/wrk/jobs/job-a/records",
                           {"after_seq": 0, "limit": 1, "snapshot_seq": 5}),
                  {"http_status": 200,
                   "records_response": {"snapshot_seq": 5,
                                        "next_after_seq": 1,
                                        "has_more": True,
                                        "record_seqs": [1]},
                   "note": "seq 1..S exactly once across pages; snapshot "
                           "summary is computed at snapshot_seq"}))
    c.append(case("records-page-snapshot-excludes-append", S, T,
                  "transition", 14, j.records,
                  http_get("desktop", "/v1/wrk/jobs/job-a/records",
                           {"after_seq": 4, "limit": 1, "snapshot_seq": 5}),
                  {"http_status": 200,
                   "records_response": {"snapshot_seq": 5,
                                        "next_after_seq": 5,
                                        "has_more": False,
                                        "record_seqs": [5]},
                   "note": "record seq 6 appended during paging is excluded"}))

    # row 15: forged legacy relay / no-job task bypass
    j = activated()
    c.append(case("legacy-relay-completed-no-receipt", S, T, "transition",
                  15, j.records,
                  legacy("desktop", "POST", "/v1/relay/events",
                         {"kind": "job.completed", "job_id": "job-a",
                          "head": H, "report_sha256": REPORT_SHA}),
                  {"http_status": 409, "error": "wrk_receipt_required",
                   "note": "no terminal projection"}))
    c.append(case("legacy-task-no-job-bypass-denied", S, T, "transition",
                  15, j.records,
                  legacy("desktop", "POST", "/v1/tasks/2400/transition",
                         {"to": "dropped", "no_job": True}),
                  {"http_status": 409, "error": "wrk_receipt_required"},
                  legacy=legacy_given(tasks=[legacy_task()])))

    # row 16: asserted digest mismatch
    c.append(case("verdict-asserted-digest-mismatch", S, T, "transition", 16,
                  opened(activated()).records,
                  http("desktop", JOB, "verdict", 1, 3,
                       {"round_no": 1, "head": H, "verdict": "PASS",
                        "path": "/jobs/job-a/verdict-r1.md",
                        "content_sha256": "e" * 64,
                        "content_b64": b64(verdict_bytes(H)),
                        "tester_job_id": None}),
                  {"http_status": 400, "error": "invalid_request",
                   "note": "bytes hash to E; no verdict or consumed grant"}))
    return wrap(T, S, c)


def imported_activated(job=SHADOW, lane="b2402-s", task=2402,
                       planted=True):
    """Shadow job -> import_begin -> imports -> import_seal -> activate."""
    j = Journal(job, "desktop")
    j.add("claim", claim_in(job, lane, "director-1", task, "desktop",
                            "builder", "T2"), actor="director-1")
    j.add("round_open", {"round_no": 1, **open_input(H)})
    j.add("import_begin", {"reason_code": "import_freeze",
                           "shadow_cutoff_seq": 2}, actor="director-1")
    begin_seq = 3
    if planted:
        imp_record(j, "planted/events/00003-job.round.json", "job.round")
        imp_record(j, "planted/events/00004-job.verdict.json", "job.verdict")
        imp_record(j, "planted/events/00005-job.round.json", "job.round")
        imp_record(j, "planted/events/00006-job.joined.json", "job.joined")
    rows = manifest_of(j, begin_seq)
    j.add("import_seal",
          {"begin_seq": begin_seq, "expected_import_count": len(rows),
           "manifest_sha256": sha(rows), "reason_code": "import_seal",
           "shadow_cutoff_seq": 2, "baseline_used": 3, "numbering_floor": 3},
          actor="director-1")
    j.add("activate", {"baseline_used": 3, "mode": "enforced",
                       "reason_code": "activation", "seal_seq": j.tail()},
          actor="director-1")
    return j


# ============================================================ §5 migration
def shadow_open_job():
    j = Journal(SHADOW, "desktop")
    j.add("claim", claim_in(SHADOW, "b2402-s", "director-1", 2402,
                            "desktop", "builder", "T2"), actor="director-1")
    j.add("round_open", {"round_no": 1, **open_input(H)})
    return j


def build_migration():
    T = "Additional migration vectors"
    S = "5"
    c = []

    # row 1: claim-only job activate seal_seq=null -> baseline 0
    j = Journal(SHADOW, "desktop")
    j.add("claim", claim_in(SHADOW, "b2402-s", "director-1", 2402,
                            "desktop", "builder", "T2"), actor="director-1")
    c.append(case("claim-only-activate-baseline-0", S, T, "migration", 1,
                  j.records,
                  http("director-1", SHADOW, "activate", 1, 1,
                       activate_in(None)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "activate",
                              "payload": {"baseline_used": 0,
                                          "mode": "enforced",
                                          "reason_code": "activation",
                                          "seal_seq": None}},
                   "summary": {"state": "active", "baseline_used": 0,
                               "used": 0, "next_round_no": 1}},
                  job=SHADOW))

    # row 2: shadow open then activate w/o seal -> import_unsealed
    j = shadow_open_job()
    c.append(case("activate-with-evidence-no-seal-import-unsealed", S, T,
                  "migration", 2, j.records,
                  http("director-1", SHADOW, "activate", 1, 2,
                       activate_in(None)),
                  {"http_status": 409, "error": "import_unsealed"},
                  job=SHADOW))

    # row 3: begin then racing host verdict -> migration_frozen
    j = shadow_open_job()
    j.add("import_begin", {"reason_code": "import_freeze",
                           "shadow_cutoff_seq": 2}, actor="director-1")
    c.append(case("host-verdict-after-begin-migration-frozen", S, T,
                  "migration", 3, j.records,
                  http("desktop", SHADOW, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-s/v.md",
                                     verdict_bytes(H))),
                  {"http_status": 409, "error": "migration_frozen",
                   "note": "old round stays in the frozen snapshot"},
                  job=SHADOW))

    # row 4: zero-import seal + activate -> baseline 3 floor 3
    j = shadow_open_job()
    j.add("import_begin", {"reason_code": "import_freeze",
                           "shadow_cutoff_seq": 2}, actor="director-1")
    man = manifest_of(j, 3)  # == []
    c.append(case("zero-import-seal-activate-baseline-3", S, T,
                  "migration", 4, j.records,
                  http("director-1", SHADOW, "import_seal", 1, 3,
                       import_seal_in(3, 0, sha(man))),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "import_seal",
                              "payload": {"begin_seq": 3,
                                          "expected_import_count": 0,
                                          "manifest_sha256": sha(man),
                                          "reason_code": "import_seal",
                                          "shadow_cutoff_seq": 2,
                                          "baseline_used": 3,
                                          "numbering_floor": 3}},
                   "summary": {"state": "sealed"}},
                  job=SHADOW))
    j2 = shadow_open_job()
    j2.add("import_begin", {"reason_code": "import_freeze",
                            "shadow_cutoff_seq": 2}, actor="director-1")
    j2.add("import_seal",
           {"begin_seq": 3, "expected_import_count": 0,
            "manifest_sha256": sha([]), "reason_code": "import_seal",
            "shadow_cutoff_seq": 2, "baseline_used": 3,
            "numbering_floor": 3}, actor="director-1")
    c.append(case("activate-exact-seal-baseline-3", S, T, "migration", 4,
                  j2.records,
                  http("director-1", SHADOW, "activate", 1, 4,
                       activate_in(4)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "activate",
                              "payload": {"baseline_used": 3,
                                          "mode": "enforced",
                                          "reason_code": "activation",
                                          "seal_seq": 4}},
                   "summary": {"state": "active", "baseline_used": 3,
                               "used": 3, "next_round_no": 4,
                               "open_rounds": []}},
                  job=SHADOW))

    # row 5: post-activate verdict/void on old round 1 -> round_not_open
    j = imported_activated(planted=False)
    c.append(case("post-activate-verdict-old-round-not-open", S, T,
                  "migration", 5, j.records,
                  http("desktop", SHADOW, "verdict", 1, j.tail(),
                       verdict_input(1, H, "PASS", "/jobs/job-s/v.md",
                                     verdict_bytes(H))),
                  {"http_status": 409, "error": "round_not_open",
                   "summary": {"used": 3}, "note": "no grant consumed"},
                  job=SHADOW))
    j2 = imported_activated(planted=False)
    j2.add("grant", grant_in("void", round_no=1, reason="void_round"),
           actor="director-1")
    c.append(case("post-activate-void-old-round-not-open", S, T,
                  "migration", 5, j2.records,
                  http("desktop", SHADOW, "void", 1, j2.tail(),
                       void_in(1, j2.tail())),
                  {"http_status": 409, "error": "round_not_open"},
                  job=SHADOW))

    # row 6: extension for round 4 + fresh PASS
    j = imported_activated(planted=False)
    granted(j, "extension", round_no=4, head=H4, extra=1,
            reason="cap_extension")
    rev = reviewer_journal(REV, SHADOW, 4, task=2402, owner="b2402-s")
    c.append(case("post-activate-extension-open-round-4", S, T,
                  "migration", 6, j.records + rev.records,
                  http("desktop", SHADOW, "round_open", 1, 6,
                       open_input(H4, tester=REV, ext=6)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H4, tester=REV, ext=6,
                                                    round_no=4)},
                   "summary": {"used": 4, "open_rounds": [4],
                               "next_round_no": 5},
                   "note": "old round 1 is not pending; fresh PASS can satisfy join"},
                  job=SHADOW))

    # row 7: void postactivate round 4 -> used=3
    j = imported_activated(planted=False)
    granted(j, "extension", round_no=4, head=H4, extra=1,
            reason="cap_extension")
    j.add("round_open", {"round_no": 4, **open_input(H4, ext=6)})
    granted(j, "void", round_no=4, reason="void_round")
    c.append(case("void-postactivate-round-4-used-3", S, T, "migration", 7,
                  j.records,
                  http("desktop", SHADOW, "void", 1, 8, void_in(4, 8)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "void", "payload": void_in(4, 8)},
                   "summary": {"used": 3, "baseline_used": 3,
                               "open_rounds": []},
                   "note": "used never 2; baseline remains 3"},
                  job=SHADOW))

    # row 8: manifest says one import, server has two -> import_unsealed
    j = shadow_open_job()
    j.add("import_begin", {"reason_code": "import_freeze",
                           "shadow_cutoff_seq": 2}, actor="director-1")
    imp_record(j, "voided/events/00002-job.round.json", "job.round")
    imp_record(j, "voided/events/00004-job.round.json", "job.round")
    rows = manifest_of(j, 3)
    c.append(case("manifest-undercount-import-unsealed", S, T,
                  "migration", 8, j.records,
                  http("director-1", SHADOW, "import_seal", 1, 5,
                       import_seal_in(3, 1, sha(rows[:1]))),
                  {"http_status": 409, "error": "import_unsealed",
                   "summary": {"state": "importing"},
                   "note": "still importing; no activation"},
                  job=SHADOW))
    return wrap(T, S, c)


# ============================================================ §5 legacy fence
def build_fence():
    T = "Legacy route fence coverage"
    S = "5"
    c = []
    j = activated()
    task_row = legacy_task()
    tasks_only = legacy_given(tasks=[task_row])
    spec = [
        (1, "tasks-create-with-o3-refs-denied",
         legacy("desktop", "POST", "/v1/tasks",
                {"title": "x", "refs": {"job_id": "job-a"}}),
         {"http_status": 409, "error": "wrk_receipt_required"}, None,
         "no legacy row: the route rejects the O3 ref before any task "
         "lookup, so absence is the point"),
        (1, "tasks-create-unrelated-legacy-compatible",
         legacy("desktop", "POST", "/v1/tasks",
                {"title": "x", "refs": {}}),
         {"outcome": "legacy_compatible",
          "detail": "unrelated legacy task creation keeps old behaviour"},
         None,
         "no legacy row: unrelated creation keeps old behaviour, so "
         "absence is the point"),
        (2, "tasks-next-claim-protected-denied",
         legacy("desktop", "POST", "/v1/tasks/next", {}),
         {"http_status": 409, "error": "wrk_receipt_required",
          "note": "explicit claim attempt on a protected task"},
         tasks_only, None),
        (3, "tasks-claim-protected-denied",
         legacy("desktop", "POST", "/v1/tasks/2400/claim",
                {"by": "director-1"}),
         {"http_status": 409, "error": "wrk_receipt_required",
          "note": "refused for central and host, no_job and retry"},
         tasks_only, None),
        (3, "tasks-rebind-protected-denied",
         legacy("desktop", "POST", "/v1/tasks/2400/rebind",
                {"job_id": "job-z"}),
         {"http_status": 409, "error": "wrk_receipt_required"},
         tasks_only, None),
        (4, "tasks-transition-binding-change-denied",
         legacy("desktop", "POST", "/v1/tasks/2400/transition",
                {"to": "claimed", "refs": {"job_id": "job-z"}}),
         {"http_status": 409, "error": "wrk_receipt_required"},
         tasks_only, None),
        (4, "tasks-transition-state-only-without-receipt-denied",
         legacy("director-1", "POST", "/v1/tasks/2400/transition",
                {"to": "merged"}),
         {"http_status": 409, "error": "wrk_receipt_required",
          "note": "state-only changes need central principal + receipt "
                  "naming the current bound subject job"},
         tasks_only, None),
        (5, "tasks-project-or-relane-protected-denied",
         legacy("desktop", "POST", "/v1/tasks/2400/relane",
                {"lane": "other"}),
         {"http_status": 409, "error": "wrk_receipt_required"},
         tasks_only, None),
        (6, "decision-request-resolve-protected-denied",
         legacy("desktop", "POST",
                "/v1/tasks/2400/decision-request/resolve",
                {"answer": "a"}),
         {"http_status": 409, "error": "wrk_receipt_required"},
         tasks_only, None),
        (7, "dispositions-apply-binding-denied",
         legacy("desktop", "POST", "/v1/tasks/dispositions/77/apply",
                {"action": "relink"}),
         {"http_status": 409, "error": "wrk_receipt_required"},
         legacy_given(tasks=[task_row],
                      dispositions=[legacy_disposition()]), None),
        (8, "decisions-resolve-protected-denied",
         legacy("desktop", "POST", "/v1/decisions/resolve",
                {"task_id": 2400, "answer": "a"}),
         {"http_status": 409, "error": "wrk_receipt_required"},
         tasks_only, None),
        (9, "relay-terminal-without-receipt-denied",
         legacy("desktop", "POST", "/v1/relay/events",
                {"kind": "job.joined", "job_id": "job-a", "head": H}),
         {"http_status": 409, "error": "wrk_receipt_required",
          "note": "lane.event text never authorizes completion"}, None,
         "no legacy row: the fence reads job-a's canonical claim, not a "
         "legacy task row"),
    ]
    for rw, name, req, exp, lg, note in spec:
        c.append(case(name, S, T, "fence", rw, j.records, req, exp,
                      legacy=lg, source_note=note))
    return wrap(T, S, c)


# ============================================================ §5 R3
def build_r3():
    T = "R3 regression vectors"
    S = "5"
    c = []

    # row 1: T2 A + assigned reviewer T active at open+verdict; T reaped; join -> 201
    restore = clock_block(7)
    j = activated(tier="T2")
    rev = reviewer_journal(REV, JOB, 1)
    j.add("round_open", {"round_no": 1, **open_input(H, tester=REV)})
    vb = verdict_bytes(H)
    j.add("verdict", {"round_no": 1, "head": H, "verdict": "PASS",
                      "path": "/jobs/job-a-t1/verdict-r1.md",
                      "content_sha256": sha(vb), "size_bytes": len(vb),
                      "tester_job_id": REV}, actor="desktop")
    rev.add("end", end_in(), actor="desktop")  # T ends after accepted PASS
    restore()
    c.append(case("join-after-tester-reaped-201", S, T, "r3", 1,
                  splice(j.records, rev.records),
                  http("desktop", JOB, "joined", 1, j.tail(),
                       joined_in(head=H)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "joined",
                              "payload": {"head": H, "pr": PR_URL,
                                          "report_sha256": REPORT_SHA,
                                          "report_size_bytes": len(REPORT),
                                          "verdict_seqs": [4],
                                          "waiver_grant_seq": None}},
                   "summary": {"state": "joined"},
                   "note": "accepted PASS remains evidence; no tester "
                           "activeness recheck at join"}))

    # row 2: zero-round join with join_waiver
    j = activated(tier="T2")
    granted(j, "join_waiver", head=H, reason="zero_round_waiver")
    c.append(case("zero-round-join-with-waiver-201", S, T, "r3", 2,
                  j.records,
                  http("desktop", JOB, "joined", 1, 3,
                       joined_in(head=H, waiver=3)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "joined",
                              "payload": {"head": H, "pr": PR_URL,
                                          "report_sha256": REPORT_SHA,
                                          "report_size_bytes": len(REPORT),
                                          "verdict_seqs": [],
                                          "waiver_grant_seq": 3}},
                   "note": "no PASS/tester assignment fabricated"}))

    # row 3: T ends before named open / verdict append -> round_not_open
    j = activated(tier="T2")
    revg = reviewer_journal(REV, JOB, 2)
    revg.add("end", end_in("reaped", "cleanup"), actor="desktop")
    c.append(case("open-naming-ended-reviewer-round-not-open", S, T, "r3",
                  3, j.records + revg.records,
                  http("desktop", JOB, "round_open", 1, 2,
                       open_input(H2, tester=REV)),
                  {"http_status": 409, "error": "round_not_open",
                   "note": "the unavailable assignment fails; earlier "
                           "accepted evidence unaffected"}))
    j2 = opened(activated(tier="T2"), 1, H)
    revg2 = reviewer_journal(REV, JOB, 1)
    revg2.add("end", end_in("reaped", "cleanup"), actor="desktop")
    c.append(case("verdict-naming-ended-reviewer-round-not-open", S, T,
                  "r3", 3, j2.records + revg2.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS",
                                     "/jobs/job-a-t1/verdict-r1.md",
                                     verdict_bytes(H), tester=REV)),
                  {"http_status": 409, "error": "round_not_open",
                   "note": "no new verdict"}))

    # row 4: shadow open H (no verdict); freeze/seal/activate; extension open H ok
    j = imported_activated(planted=False)  # had shadow open on H, baseline 3
    granted(j, "extension", round_no=4, head=H, extra=1,
            reason="cap_extension")
    c.append(case("postactivate-open-historical-head-no-repeat", S, T,
                  "r3", 4, j.records,
                  http("desktop", SHADOW, "round_open", 1, 6,
                       open_input(H, ext=6)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H, ext=6, round_no=4)},
                   "note": "preactivation opens hold no head reservation"},
                  job=SHADOW))

    # row 5: historical verdict on H; activated; no grant; open H -> duplicate_head
    j = Journal(SHADOW, "desktop")
    j.add("claim", claim_in(SHADOW, "b2402-s", "director-1", 2402,
                            "desktop", "builder", "T2"), actor="director-1")
    j.add("round_open", {"round_no": 1, **open_input(H)})
    j.add("verdict", verdict_payload_shape(1, H, "PASS",
                                           "/jobs/job-s/v.md",
                                           verdict_bytes(H)))
    j.add("import_begin", {"reason_code": "import_freeze",
                           "shadow_cutoff_seq": 3}, actor="director-1")
    j.add("import_seal", {"begin_seq": 4, "expected_import_count": 0,
                          "manifest_sha256": sha([]),
                          "reason_code": "import_seal",
                          "shadow_cutoff_seq": 3, "baseline_used": 3,
                          "numbering_floor": 3}, actor="director-1")
    j.add("activate", {"baseline_used": 3, "mode": "enforced",
                       "reason_code": "activation", "seal_seq": 5},
          actor="director-1")
    c.append(case("open-historical-verdict-head-no-grant-duplicate-head",
                  S, T, "r3", 5, j.records,
                  http("desktop", SHADOW, "round_open", 1, 6,
                       open_input(H)),
                  {"http_status": 409, "error": "duplicate_head",
                   "note": "head_repeat (+extension at cap) or a new commit"},
                  job=SHADOW))
    badpol = installed_policy()
    badpol["features"]["head_repeat_enabled"] = False
    c.append(case("policy-head-repeat-disabled-invalid", S, T, "r3", 5,
                  [],
                  policy_req(badpol),
                  {"policy_valid": False,
                   "detail": "Q2=a requires head_repeat_enabled=true; "
                             "rejected before installation"}))

    # row 6: two central claims introduce lane X with different parents
    j = activated()  # owner lane b2400-a exists w/ parent director-1
    c.append(case("conflicting-lane-parent-claim-conflict", S, T, "r3", 6,
                  j.records,
                  http("director-1", "job-x", "claim", 0, 0,
                       claim_in("job-x", OWNER, "pa-integrator", 2403,
                                "desktop", "builder", "T2")),
                  {"http_status": 409, "error": "claim_conflict",
                   "note": "lane b2400-a already exists with parent "
                           "director-1; global lock serializes the two "
                           "claim transactions"},
                  job="job-x"))

    # row 7: verdict body with U+00A0/U+3000/embedded CR -> accepted
    body = "review notes there and more\rinside\n"
    vb = verdict_bytes(H, "PASS", body)
    j = opened(activated())
    c.append(case("verdict-body-nonascii-space-and-cr-accepted", S, T,
                  "r3", 7, j.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                     vb)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "verdict",
                              "payload": verdict_payload_shape(
                                  1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                  vb)},
                   "note": "valid UTF-8 body accepted and hashed unchanged"}))

    # row 8: header NBSP / bare CR separator -> invalid_verdict
    bad1 = ("VERDICT: PASS @" + H + " \nbody\n").encode("utf-8")
    c.append(case("verdict-header-nbsp-invalid", S, T, "r3", 8,
                  opened(activated()).records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                     bad1)),
                  {"http_status": 400, "error": "invalid_verdict",
                   "note": "header whitespace is ASCII space/tab only"}))
    bad2 = ("VERDICT: PASS @" + H + "\rbody\n").encode("utf-8")
    c.append(case("verdict-bare-cr-separator-invalid", S, T, "r3", 8,
                  opened(activated()).records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                     bad2)),
                  {"http_status": 400, "error": "invalid_verdict",
                   "note": "CR in the header line is valid only before LF"}))

    # row 9: policy desktop entry grant.write / host_client_id=mac-dev -> invalid
    p1 = installed_policy()
    p1["clients"]["desktop"]["permissions"].append("grant.write")
    c.append(case("policy-desktop-grant-write-invalid", S, T, "r3", 9, [],
                  policy_req(p1),
                  {"policy_valid": False,
                   "detail": "permission outside the host ceiling"}))
    p2 = installed_policy()
    p2["clients"]["desktop"]["host_client_id"] = "mac-dev"
    c.append(case("policy-desktop-wrong-host-invalid", S, T, "r3", 9, [],
                  policy_req(p2),
                  {"policy_valid": False,
                   "detail": "host_client_id must equal the client id"}))
    return wrap(T, S, c)


# ============================================================ §5 R4 claim
def scoped_claim_input(newjob, target, round_no, role="tester",
                       host="desktop", task=TASK, owner=OWNER, tier="T2",
                       pred=None, supers=None):
    return claim_in(newjob, owner, owner, task, host, role, tier,
                    {"job_id": target, "round_no": round_no}, pred, supers)


def build_r4claim():
    T = "R4 normative vectors"
    S = "5"
    c = []
    base = activated(tier="T2")  # job-a active builder on desktop, task 2400

    # row 1: mint tester/reviewer/counsel for own active builder
    for i, role in enumerate(["tester", "reviewer", "counsel"]):
        c.append(case("scoped-mint-%s-accepted" % role, S, T, "r4claim", 1,
                      base.records,
                      http("desktop", REV + chr(97 + i), "claim", 0, 0,
                           scoped_claim_input(REV + chr(97 + i), JOB, 1,
                                              role=role)),
                      {"http_status": 201, "created": True,
                       "record": {"kind": "claim"},
                       "note": "derived fields exact; live assignment at once; "
                               "target used/next_round_no/tail unchanged"},
                      job=REV + chr(97 + i)))

    rest = [
        # row 2 foreign host
        (2, "scoped-mint-foreign-job-host-mismatch",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOBB, 1, task=2401, owner="b2401-b")),
         {"http_status": 403, "error": "host_mismatch"}, REV,
         "foreign_activated", None),
        # row 3 ended target
        (3, "scoped-mint-ended-target-job-ended",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1)),
         {"http_status": 409, "error": "job_ended"}, REV, "ended", None),
        # row 4 different task
        (4, "scoped-mint-different-task-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1, task=9999)),
         {"http_status": 409, "error": "claim_conflict"}, REV, None, None),
        # row 5 builder/worker role
        (5, "scoped-mint-builder-role-scope-denied",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1, role="builder")),
         {"http_status": 403, "error": "scope_denied"}, REV, None, None),
        (5, "scoped-mint-worker-role-scope-denied",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1, role="worker")),
         {"http_status": 403, "error": "scope_denied"}, REV, None, None),
        # row 6 changed derived fields
        (6, "scoped-mint-changed-parent-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              claim_in(REV, OWNER, "director-1", TASK, "desktop", "tester",
                       "T2", {"job_id": JOB, "round_no": 1})),
         {"http_status": 409, "error": "claim_conflict"}, REV, None, None),
        (6, "scoped-mint-changed-owner-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              claim_in(REV, "other-lane", "other-lane", TASK, "desktop",
                       "tester", "T2", {"job_id": JOB, "round_no": 1})),
         {"http_status": 409, "error": "claim_conflict"}, REV, None, None),
        (6, "scoped-mint-changed-tier-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1, tier="T3")),
         {"http_status": 409, "error": "claim_conflict"}, REV, None, None),
        # row 7 another host_client_id
        (7, "scoped-mint-other-host-id-host-mismatch",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1, host="mac-dev")),
         {"http_status": 403, "error": "host_mismatch"}, REV, None, None),
        # row 9 null/self/reviewer-target review_of
        (9, "scoped-mint-null-review-of-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              claim_in(REV, OWNER, OWNER, TASK, "desktop", "tester", "T2",
                       None)),
         {"http_status": 409, "error": "claim_conflict"}, REV, None, None),
        (9, "scoped-mint-self-reference-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, REV, 1)),
         {"http_status": 409, "error": "claim_conflict"}, REV, None, None),
        (9, "scoped-mint-reviewer-as-target-claim-conflict",
         http("desktop", REV2, "claim", 0, 0,
              scoped_claim_input(REV2, REV, 1)),
         {"http_status": 409, "error": "claim_conflict"}, REV2,
         "reviewer_target", None),
        # row 10 reuse id / supersedes / predecessor / stale CAS
        (10, "scoped-mint-reused-job-id-claim-conflict",
         http("desktop", "job-a-old", "claim", 1, 2,
              scoped_claim_input("job-a-old", JOB, 1)),
         {"http_status": 409, "error": "claim_conflict"}, "job-a-old",
         "reused_job", None),
        (10, "scoped-mint-supersedes-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1, supers=1)),
         {"http_status": 409, "error": "claim_conflict"}, REV, None, None),
        (10, "scoped-mint-stale-zero-cas-sequence-conflict",
         http("desktop", JOB, "claim", 0, 0,
              scoped_claim_input(JOB, JOB, 1)),
         {"http_status": 409, "error": "sequence_conflict",
          "note": "stale zero CAS resolves before the id reuse check"},
         JOB, None, None),
        # row 12 joined/shadow/nonexistent/conflicting targets
        (12, "scoped-mint-target-joined-review-sealed",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1)),
         {"http_status": 409, "error": "review_sealed"}, REV,
         "joined", None),
        (12, "scoped-mint-target-shadow-claim-conflict",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, SHADOW, 1, task=2402,
                                 owner="b2402-s")),
         {"http_status": 409, "error": "claim_conflict",
          "note": "nonactive shadow target"}, REV, "shadow", None),
        (12, "scoped-mint-target-nonexistent-not-found",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, "job-nope", 1)),
         {"http_status": 404, "error": "not_found"}, REV, None, None),
        (12, "scoped-mint-conflicting-frozen-tester-claim-conflict",
         http("desktop", REV2, "claim", 0, 0,
              scoped_claim_input(REV2, JOB, 1)),
         {"http_status": 409, "error": "claim_conflict"}, REV2,
         "frozen_tester", None),
        # row 14 replay of successful mint after target end
        (14, "scoped-mint-replay-after-target-end-200",
         http("desktop", REV, "claim", 0, 0,
              scoped_claim_input(REV, JOB, 1), key=replay_key),
         {"http_status": 200, "created": False,
          "no_new_records": True,
          "note": "original claim returned; permission rechecked"},
         REV, "ended_with_reviewer", None),
        # row 17 pa-integrator extension -> scope_denied
        (17, "pa-integrator-extension-scope-denied",
         http("pa-integrator", JOB, "grant", 1, 2,
              grant_in("extension", round_no=2, head=H2, extra=1,
                       reason="pa_extension")),
         {"http_status": 403, "error": "scope_denied"}, JOB, None, None),
        # row 18 token-shaped verdict body -> invalid_request
        (18, "verdict-body-token-shape-invalid-request",
         http("desktop", JOB, "verdict", 1, 3,
              {"round_no": 1, "head": H, "verdict": "PASS",
               "path": "/jobs/job-a/verdict-r1.md",
               "content_sha256": ZERO64,
               "content_b64_construct":
                   "base64('VERDICT: PASS @<head>\\n' + guard-matching "
                   "synthetic token material generated at runtime)",
               "tester_job_id": None}, bad=True),
         {"http_status": 400, "error": "invalid_request",
          "note": "no content, business record or echo; audit carries only "
                  "allowed metadata/code"}, JOB, "opened",
         ["content is constructed at runtime per design row 18; the pack "
          "holds no token-shaped bytes"]),
        # row 19 durability gate
        (19, "p2-gate-unverified-durability-refused",
         none_req("desk operations gate, not an API call"),
         {"outcome": "p2_refused",
          "detail": "missing restore-drill/generation-fence/director-receipt "
                    "evidence keeps the fleet at P1/O2"}, JOB, None, None),
    ]
    for rw, name, req, exp, job, mode, inf in rest:
        recs = r4_given(mode)
        c.append(case(name, S, T, "r4claim", rw, recs, req, exp, job=job,
                      inferred=inf))

    # row 8: grant/unknown field on mint -> invalid_request (schema level)
    bad = scoped_claim_input(REV, JOB, 1)
    bad["grant_seq"] = 3
    c.append(case("scoped-mint-grant-field-invalid-request", S, T,
                  "r4claim", 8, base.records,
                  http("desktop", REV, "claim", 0, 0, bad, bad=True),
                  {"http_status": 400, "error": "invalid_request"},
                  job=REV))
    bad2 = scoped_claim_input(REV, JOB, 1)
    bad2["surprise"] = True
    c.append(case("scoped-mint-unknown-field-invalid-request", S, T,
                  "r4claim", 8, base.records,
                  http("desktop", REV, "claim", 0, 0, bad2, bad=True),
                  {"http_status": 400, "error": "invalid_request"},
                  job=REV))

    # row 11: host touches own review-role job's ops -> scope_denied
    revg = reviewer_journal(REV, JOB, 1)
    for kind, pl, note in [
            ("activate", activate_in(None), "own review job"),
            ("round_open", open_input(H2), "own review job"),
            ("void", void_in(1, 9), "own review job"),
            ("joined", joined_in(head=H2), "own review job")]:
        c.append(case("review-role-own-%s-scope-denied" % kind, S, T,
                      "r4claim", 11, base.records + revg.records,
                      http("desktop", REV, kind, 1, 1, pl),
                      {"http_status": 403, "error": "scope_denied"},
                      job=REV))
    c.append(case("review-role-central-activate-scope-denied", S, T,
                  "r4claim", 11, base.records + revg.records,
                  http("director-1", REV, "activate", 1, 1,
                       activate_in(None)),
                  {"http_status": 403, "error": "scope_denied"},
                  job=REV))

    # row 13: same-host scoped + central cross-host each satisfy Q9
    j = activated(tier="T2")
    rev = reviewer_journal(REV, JOB, 1)
    j.add("round_open", {"round_no": 1, **open_input(H, tester=REV)})
    vb = verdict_bytes(H)
    c.append(case("same-host-scoped-pass-accepted", S, T, "r4claim", 13,
                  splice(j.records, rev.records),
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS",
                                     "/jobs/job-a-t1/verdict-r1.md", vb,
                                     tester=REV)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "verdict"},
                   "note": "satisfies Q9 while live at open and verdict "
                           "acceptance"}))
    # central mints a cross-host reviewer (mac-dev) for desktop's job
    j2 = activated(tier="T2")
    c.append(case("central-cross-host-assignment-201", S, T, "r4claim", 13,
                  j2.records,
                  http("director-1", REVX, "claim", 0, 0,
                       claim_in(REVX, OWNER, OWNER, TASK, "mac-dev",
                                "tester", "T2",
                                {"job_id": JOB, "round_no": 1})),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "claim"},
                   "note": "host cannot mint a cross-host assignment itself"},
                  job=REVX))

    # row 15: policy head_repeat_enabled=false invalid (dup rule, kept per row)
    badpol = installed_policy()
    badpol["features"]["head_repeat_enabled"] = False
    c.append(case("policy-head-repeat-false-invalid", S, T, "r4claim", 15,
                  base.records, policy_req(badpol),
                  {"policy_valid": False,
                   "detail": "installed policy requires true"}))

    # row 16: attribution ids after A1
    for cid in ["at-integrator"]:
        c.append(case("attribution-id-reads-capabilities", S, T, "r4claim",
                      16, base.records,
                      http_get(cid, "/v1/wrk/capabilities"),
                      {"http_status": 200,
                       "capabilities": {"client_id": cid,
                                        "contract": "hk1873.v1"},
                       "note": "reads allowed; grant/claim mint returns "
                               "scope_denied"}))
    c.append(case("attribution-id-grant-scope-denied", S, T, "r4claim", 16,
                  base.records,
                  http("at-integrator", JOB, "grant", 1, 2,
                       grant_in("extension", round_no=2, head=H2, extra=1,
                                reason="cap_extension")),
                  {"http_status": 403, "error": "scope_denied"}))
    c.append(case("attribution-id-claim-scope-denied", S, T, "r4claim", 16,
                  base.records,
                  http("pb-integrator", "job-q", "claim", 0, 0,
                       claim_in("job-q", "b2403-q", "director-1", 2403,
                                "desktop", "builder", "T1")),
                  {"http_status": 403, "error": "scope_denied"},
                  job="job-q"))
    return wrap(T, S, c)


replay_key = uuid(424242)


def r4_given(mode):
    """given records for the r4claim table's varied setups."""
    if mode == "ended":
        j = activated(tier="T2")
        j.add("end", end_in())
        return j.records
    if mode == "joined":
        j = verdicted(opened(activated(tier="T2"), 1, H))
        j.add("joined", {"head": H, "pr": PR_URL,
                         "report_sha256": REPORT_SHA,
                         "report_size_bytes": len(REPORT),
                         "verdict_seqs": [4], "waiver_grant_seq": None})
        return j.records
    if mode == "shadow":
        return shadow_claim_records()
    if mode == "foreign_activated":
        restore = clock_block(4)
        recs = activated(job=JOBB, host="mac-dev", owner="b2401-b",
                         task=2401).records
        restore()
        return recs
    if mode == "reused_job":
        restore = clock_block(2)
        recs = activated(tier="T2").records + activated(
            job="job-a-old", host="desktop", owner=OWNER, task=TASK,
            tier="T2").records
        restore()
        return recs
    if mode == "reviewer_target":
        return activated(tier="T2").records + \
            reviewer_journal(REV, JOB, 1).records
    if mode == "frozen_tester":
        j = activated(tier="T2")
        rev = reviewer_journal("job-a-t9", JOB, 1)
        opened(j, 1, H, tester="job-a-t9")
        return splice(j.records, rev.records)
    if mode == "ended_with_reviewer":
        j = activated(tier="T2")
        j2 = Journal(REV, "desktop")
        j2.add("claim", scoped_claim_input(REV, JOB, 1), key=replay_key)
        j.add("end", end_in())
        return j.records[:2] + j2.records + j.records[2:]
    if mode == "opened":
        return opened(activated()).records
    if mode == "REPLAY_GIVEN":
        return []
    return activated(tier="T2").records


def shadow_claim_records():
    j = Journal(SHADOW, "desktop")
    j.add("claim", claim_in(SHADOW, "b2402-s", "director-1", 2402,
                            "desktop", "builder", "T2"), actor="director-1")
    return j.records


# ============================================================ §5 R4 lifecycle
def build_r4life():
    T = "R4 review job lifecycle vectors"
    S = "5"
    c = []
    base = activated(tier="T2")
    rev = reviewer_journal(REV, JOB, 1)

    c.append(case("tester-cancelled-before-open-activates-and-cancels", S, T,
                  "r4life", 1, base.records + rev.records,
                  http("desktop", REV, "end", 1, 1,
                       end_in("cancelled", "tester cancelled before open")),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "end"},
                   "note": "PASS referencing it no longer satisfies Q9"},
                  job=REV))
    j = opened(activated(tier="T2"), 1, H, tester=REV)
    c.append(case("target-opened-inactive-assignment-round-not-open", S, T,
                  "r4life", 2, j.records + rev.records,
                  http("desktop", JOB, "round_open", 1, 3,
                       open_input(H2, tester=REV)),
                  {"http_status": 409, "error": "round_not_open",
                   "note": "inactive assignment"}, job=JOB))
    # row 3: verdict arrives while assignment active; T reaped; join
    j2 = activated(tier="T2")
    j2.add("round_open", {"round_no": 1, **open_input(H, tester=REV)})
    vb = verdict_bytes(H)
    j2.add("verdict", verdict_payload_shape(1, H, "PASS",
                                            "/jobs/job-a-t1/verdict-r1.md",
                                            vb, tester=REV))
    revg2 = reviewer_journal(REV, JOB, 1)
    revg2.add("end", end_in("reaped", "tester reaped"), actor="desktop")
    c.append(case("verdict-while-active-reaped-after-join-ok", S, T,
                  "r4life", 3, j2.records + revg2.records,
                  http("desktop", JOB, "joined", 1, j2.tail(),
                       joined_in(head=H)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "joined"},
                   "note": "accepted verdict still counted at join"}))
    # row 4: claims ops blocked except cancel on own record (5 sub-rows)
    for i, (kind, pl) in enumerate([
            ("verdict", verdict_input(1, H, "PASS", "/jobs/job-t/v.md", vb)),
            ("round_open", open_input(H)),
            ("grant", grant_in("extension", round_no=1, head=H, extra=1)),
            ("void", void_in(1, 9)),
            ("activate", activate_in(None))]):
        c.append(case("review-job-%s-scope-denied" % kind, S, T, "r4life",
                      4, rev.records,
                      http("desktop", REV, kind, 1, 1, pl),
                      {"http_status": 403, "error": "scope_denied"},
                      job=REV))
    c.append(case("review-job-cancel-on-own-record-allowed", S, T,
                  "r4life", 4, rev.records,
                  http("desktop", REV, "end", 1, 1,
                       end_in("cancelled", "cancel own review job")),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "end"}}, job=REV))
    # row 5: A-bound claim consumed -> claim_conflict
    c.append(case("supersede-unconsumed-claim-conflict", S, T, "r4life", 5,
                  base.records + rev.records,
                  http("desktop", REV2, "claim", 0, 0,
                       scoped_claim_input(REV2, JOB, 1)),
                  {"http_status": 409, "error": "claim_conflict",
                   "note": "unconsumed binding until accepted verdict"},
                  job=REV2))
    # row 6: fresh claim seq1+pred -> sequence_conflict / invalid_request
    c.append(case("fresh-claim-seq-nonzero-sequence-conflict", S, T,
                  "r4life", 6, base.records,
                  http("desktop", "job-new", "claim", 5, 0,
                       claim_in("job-new", "b2403-n", "director-1", 2403,
                                "desktop", "builder", "T1")),
                  {"http_status": 409, "error": "sequence_conflict"},
                  job="job-new"))
    c.append(case("fresh-claim-predecessor-invalid-request", S, T,
                  "r4life", 6, base.records,
                  http("desktop", "job-new", "claim", 0, 0,
                       claim_in("job-new", "b2403-n", "director-1", 2403,
                                "desktop", "builder", "T1", pred=3),
                       bad=True),
                  {"http_status": 400, "error": "invalid_request",
                   "note": "predecessor_seq 0|no predecessor required"},
                  job="job-new"))
    # row 7: terminal review job -> rejected at once
    jend = reviewer_journal(REV, JOB, 1)
    jend.add("end", end_in("reaped", "cleanup"), actor="desktop")
    c.append(case("verdict-with-terminal-reviewer-round-not-open", S, T,
                  "r4life", 7, opened(activated(tier="T2")).records
                  + jend.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS",
                                     "/jobs/job-a-t1/verdict-r1.md",
                                     verdict_bytes(H), tester=REV)),
                  {"http_status": 409, "error": "round_not_open",
                   "note": "assignment rejected at once"}))
    # row 8: revoke submitted verdict request; still no append
    c.append(case("revoked-request-dropped-verdict-round-not-open", S, T,
                  "r4life", 8, base.records + jend.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS",
                                     "/jobs/job-a-t1/verdict-r1.md",
                                     verdict_bytes(H), tester=REV)),
                  {"http_status": 409, "error": "round_not_open",
                   "note": "revoked request is dropped before audit; still "
                           "no verdict record"}))
    return wrap(T, S, c)


# ===================================================== §5 prose vectors
def build_headrepeat():
    T = "Head-repeat binding vector"
    S = "5"
    j = activated()
    granted(j, "head_repeat", round_no=1, head=H, earlier=0, tester=REV,
            reason="repeat_review")
    rev = reviewer_journal(REV, JOB, 1)
    vb = verdict_bytes(H)
    c = [
        case("head-repeat-open-extends-open-or-binds-same-head", S, T,
             "headrepeat", 1, j.records + rev.records,
             http("desktop", JOB, "round_open", 1, 3,
                  open_input(H, tester=REV, rep=3)),
             {"http_status": 201, "created": True,
              "record": {"kind": "round_open",
                         "payload": open_input(H, tester=REV, rep=3,
                                               round_no=1)},
              "summary": {"open_rounds": [1]},
              "note": "the grant extends the open round or binds a new "
                      "numbered round to the same head"}),
        case("head-repeat-verdict-consumes-grant", S, T, "headrepeat", 2,
             j.records + rev.records + [expect_record(
                 j.records, JOB, "round_open",
                 open_input(H, tester=REV, rep=3, round_no=1), "desktop")],
             http("desktop", JOB, "verdict", 1, 4,
                  verdict_input(1, H, "PASS",
                                "/jobs/job-a/verdict-r1.md", vb,
                                tester=REV)),
             {"http_status": 201, "created": True,
              "record": {"kind": "verdict"},
              "note": "the accepted verdict consumes the grant; grant "
                      "remains unconsumed until then"}),
        case("head-repeat-wrong-tester-grant-invalid", S, T, "headrepeat",
             3, j.records + rev.records,
             http("desktop", JOB, "round_open", 1, 3,
                  open_input(H, tester=REV2, rep=3)),
             {"http_status": 409, "error": "grant_invalid",
              "note": "verbatim grant for a different tester job"}),
        case("head-repeat-wrong-head-grant-invalid", S, T, "headrepeat", 4,
             j.records + rev.records,
             http("desktop", JOB, "round_open", 1, 3,
                  open_input(H2, tester=REV, rep=3)),
             {"http_status": 409, "error": "grant_invalid",
              "note": "verbatim grant for a different head"}),
        case("head-repeat-wrong-claim-key-grant-invalid", S, T,
             "headrepeat", 5, j.records + rev.records,
             http("desktop", JOB, "round_open", 1, 3,
                  {"head": H, "tester_job_id": REV,
                   "extension_grant_seq": 5, "repeat_grant_seq": None}),
             {"http_status": 409, "error": "grant_invalid",
              "note": "grant passed in the extension slot is not a "
                      "head_repeat use"}),
    ]
    return wrap(T, S, c)


def build_noop():
    T = "No-op posting vector"
    S = "5"
    j = opened(activated())
    c = [case("round-open-noop-same-key-same-request-200", S, T, "noop",
              1, j.records,
              http("desktop", JOB, "round_open", 1, 2, open_input(H),
                   key=j.records[2]["idempotency_key"]),
              {"http_status": 200, "created": False,
               "record_ref": {"seq": 3,
                              "record_id": j.records[2]["record_id"],
                              "record_hash": j.records[2]["record_hash"]},
               "summary": {"open_rounds": [1], "used": 1},
               "note": "job.journal stays {rounds:[{n:1,head:H,state:open}"
                       ",verdicts:[],voids:[],grants:[],imports:[]]}"})]
    return wrap(T, S, c)


# ============================================================ §6b transport
def build_transport():
    T = "CLI transport and intents vectors"
    S = "6b"
    J = "job-b0debcafebabe00"
    R = "job-a1b2c3d4e5f6a7b"
    key = uuid(555555)
    c = [
        case("path-fake-or-overridden-binary-refused", S, T, "transport",
             1, [],
             cli("job", J, env={
                 "PATH_with_fake": True,
                 "HANDOFFKEEP_BIN": "/evil/handoffkeep"}),
             {"exit_code": 2, "stderr_code": "cli_usage",
              "note": "PR242 guard refuses invalid override/binary before "
                      "exec; fake tool sees zero calls; innocuous fake "
                      "PATH entry alone is ignored"}),
        case("unguarded-hk-bin-lookup-rejected", S, T, "transport", 2, [],
             cli("records", J, "--after-seq", "0"),
             {"outcome": "spy_rejects",
              "note": "C1 test spy rejects unguarded hk_bin; every "
                      "operation incl. later cursor pages uses the same "
                      "canonical resolver"}),
        case("env-token-with-cli-unavailable-no-fallback", S, T,
             "transport", 3, [],
             cli("job", J, env={"HANDOFFKEEP_TOKEN": "x",
                                "_cli_unavailable": True},
                 stub={"response": "unavailable"}),
             {"exit_code": 75, "stderr_code": "transport_unavailable",
              "note": "or trust-path exit 2; no HTTP/curl/env-token "
                      "fallback, no local completion/quota success"}),
        case("server-host-mismatch-exit-77", S, T, "transport", 4, [],
             cli("append", J, "--kind", "round_open",
                 "--idempotency-key", key, "--expected-seq", "1",
                 "--claim-seq", "1",
                 stdin=json.dumps(open_input(H)).encode(),
                 stub={"http_status": 403,
                       "error": "host_mismatch"}),
             {"exit_code": 77, "stderr_code": "host_mismatch",
              "stderr_format": "handoffkeep wrk: CODE",
              "note": "empty stdout; exact one-line stderr; no local "
                      "fallback"}),
        case("server-round-cap-exit-78", S, T, "transport", 4, [],
             cli("append", J, "--kind", "round_open",
                 "--idempotency-key", key, "--expected-seq", "1",
                 "--claim-seq", "1",
                 stdin=json.dumps(open_input(H)).encode(),
                 stub={"http_status": 409,
                       "error": "round_cap"}),
             {"exit_code": 78, "stderr_code": "round_cap",
              "stderr_format": "handoffkeep wrk: CODE",
              "note": "empty stdout; exact one-line stderr; no local "
                      "fallback"}),
        case("server-sequence-conflict-exit-73", S, T, "transport", 4,
             [], cli("append", J, "--kind", "round_open",
                     "--idempotency-key", key, "--expected-seq", "3",
                     "--claim-seq", "1",
                     stdin=json.dumps(open_input(H)).encode(),
                     stub={"http_status": 409,
                           "error": "sequence_conflict"}),
             {"exit_code": 73, "stderr_code": "sequence_conflict",
              "stderr_format": "handoffkeep wrk: CODE",
              "note": "empty stdout; exact one-line stderr; no local "
                      "fallback"}),
        case("commit-then-response-lost-file-changed", S, T, "transport",
             5, [],
             cli("append", J, "--kind", "verdict",
                 "--idempotency-key", key, "--expected-seq", "3",
                 "--claim-seq", "1",
                 stdin=json.dumps(verdict_input(
                     1, H, "PASS", "/jobs/job-a/v.md",
                     verdict_bytes(H))).encode(),
                 stub={"replay": True}),
             {"exit_code": 0,
              "note": "original pending base64/body/key/expected_seq "
                      "resent; 200 created=false identical record/hash; "
                      "exactly one charge; changed file ignored"}),
        case("definitive-sequence-conflict-then-refresh", S, T,
             "transport", 6, [],
             cli("append", J, "--kind", "round_open",
                 "--idempotency-key", key, "--expected-seq", "3",
                 "--claim-seq", "1",
                 stdin=json.dumps(open_input(H2)).encode(),
                 stub={"http_status": 409,
                       "error": "sequence_conflict"}),
             {"exit_code": 73, "stderr_code": "sequence_conflict",
              "note": "keep rejected original intent; new key only "
                      "after revalidation; original request immutable"}),
        case("crash-before-send-after-commit-after-fsync", S, T,
             "transport", 7, [],
             cli("append", J, "--kind", "round_open",
                 "--idempotency-key", key, "--expected-seq", "3",
                 "--claim-seq", "1",
                 stdin=json.dumps(open_input(H3)).encode(),
                 stub={"crash": "any"}),
             {"outcome": "intent_retry",
              "note": "saved intent retries once by key / successful "
                      "replay / GET-based projection repair; no lost "
                      "committed round, new key or double release"}),
        case("intent-symlink-mode-malformed-fsync-fails", S, T,
             "transport", 8, [],
             cli("append", J, "--kind", "round_open",
                 "--idempotency-key", key, "--expected-seq", "3",
                 "--claim-seq", "1",
                 stdin=json.dumps(open_input(H4)).encode(),
                 stub={"intent": "symlink"}),
             {"exit_code": 74,
              "stderr_code": "intent_invalid",
              "stderr_code_alt": "intent_io",
              "note": "exit 74 before send; no event or accepted "
                      "completion"}),
        case("cursor-page-inconsistent-history-regressed", S, T,
             "transport", 9, [],
             cli("records", J, "--after-seq", "3", "--snapshot-seq",
                 "5", stub={"response": "malformed_cursor"}),
             {"exit_code": 75,
              "stderr_code": "protocol_invalid",
              "stderr_code_alt": "history_regressed",
              "note": "different snapshot, missing seq, altered hash or "
                      "generation; no decision on partial pages"}),
        case("redirect-html-namespace-unknown-code", S, T,
             "transport", 10, [],
             cli("job", J, stub={"response": "redirect"}),
             {"exit_code": 75, "stderr_code": "protocol_invalid",
              "stderr_code_alt": "o3_unsupported",
              "note": "one safe line, empty stdout, no body/Location/"
                      "evidence echo; on append keep intent pending, "
                      "exact-key retry"}),
        case("caller-env-overrides-scrubbed", S, T, "transport", 11, [],
             cli("job", J, env={"HANDOFFKEEP_URL": "https://fake",
                                "HANDOFFKEEP_TOKEN": "x",
                                "HANDOFFKEEP_CF_ACCESS_CLIENT_ID": "x",
                                "HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET":
                                    "x"}),
             {"outcome": "env_scrubbed",
              "note": "child has all four override keys absent; no "
                      "values read/echoed; canonical CLI uses installed "
                      "identity; no request reaches fake endpoint"}),
        case("commit-then-proxy-5xx-pending-intent", S, T,
             "transport", 12, [],
             cli("append", J, "--kind", "round_open",
                 "--idempotency-key", key, "--expected-seq", "3",
                 "--claim-seq", "1",
                 stdin=json.dumps(open_input(H5)).encode(),
                 stub={"response": "html_5xx"}),
             {"exit_code": 75, "stderr_code": "protocol_invalid",
              "note": "pending intent unchanged; retry original "
                      "key/body returns accepted replay; recognized "
                      "503 also stays pending"}),
    ]
    return wrap(T, S, c)


# ============================================================ §8 failure
def build_failure():
    T = "Failure and abuse cases"
    S = "8"
    c = []
    spec = [
        ("forged-first-line-multiple-passes", 1,
         {"overlaps": "VERDICT_BODY_GRAMMAR"},
         opened(activated()).records,
         http("desktop", JOB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-a/v.md",
                            verdict_bytes(H, "PASS",
                                          "VERDICT: PASS @" + H + "\n"))),
         {"http_status": 400, "error": "invalid_verdict",
          "note": "second VERDICT: line; still-checked head binding and "
                  "digest"}),
        ("two-simultaneous-opens-cap-exceeded", 2, {"overlaps": "cap"},
         opened(opened(activated(), 1, H), 2, H2).records,
         http("desktop", JOB, "round_open", 1, 4, open_input(H3)),
         {"http_status": 201,
          "note": "winner at used=3; LOSER -> sequence_conflict",
          "loser": http("desktop", JOB, "round_open", 1, 3,
                        open_input(H3)),
          "loser_expect": {"http_status": 409,
                           "error": "sequence_conflict"}}),
        ("verdict-against-different-round-head-mismatch", 3,
         {"overlaps": "binding"},
         opened(activated(), 1, H).records,
         http("desktop", JOB, "verdict", 1, 3,
              verdict_input(1, H2, "PASS", "/jobs/job-a/v.md",
                            verdict_bytes(H2))),
         {"http_status": 409, "error": "head_mismatch"}),
        ("replayed-verdict-changed-path-idempotency-conflict", 4,
         {"overlaps": "idempotency"},
         verdicted(opened(activated()), 1, H, "PASS").records,
         http("desktop", JOB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-a/v2.md",
                            verdict_bytes(H)),
              key=verdicted(opened(activated()), 1, H,
                            "PASS").records[3]["idempotency_key"]),
         {"http_status": 409, "error": "idempotency_conflict"}),
        ("over-cap-extension-replay-rejected", 5,
         {"overlaps": "grant reuse"},
         None, None,
         {"http_status": 409, "error": "grant_consumed",
          "note": "fresh record once then grant_consumed or "
                  "sequence_conflict per Ordering;", "inferred": True}),
        ("cross-host-forgery-host-mismatch", 6,
         {"overlaps": "Q9/Q2"},
         opened(activated(job=JOBB, host="mac-dev", owner="b2401-b",
                          task=2401)).records,
         http("desktop", JOBB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-b/v.md",
                            verdict_bytes(H))),
         {"http_status": 403, "error": "host_mismatch"},
         {"job": JOBB}),
        ("duplicated-verdict-with-undercharge", 7,
         {"overlaps": "joined gate"},
         activated().records,
         http("desktop", JOB, "joined", 1, 2,
              joined_in(head=H,
                        verdict_seqs=[3, 3])),
         {"http_status": 409, "error": "review_incomplete",
          "note": "Q3/Q4/Q10 re-check; no real PASS for open round",
          "inferred": True}),
        ("forged-legacy-record-mint-review-receipt", 8,
         {"overlaps": "authority cutover"},
         activated().records,
         legacy("desktop", "POST", "/v1/relay/events",
                {"kind": "job.verdict", "job_id": "job-a", "head": H,
                 "verdict": "PASS"}),
         {"http_status": 409, "error": "wrk_receipt_required"}),
        ("requests-beyond-limits-413", 9, {"overlaps": "§5 limits"},
         activated().records,
         http("desktop", JOB, "verdict", 1, 2,
              verdict_input(1, H, "PASS", "/jobs/job-a/v.md",
                            verdict_bytes(H)),
              body_bytes=1048577),
         {"http_status": 413, "error": "request_too_large",
          "note": "decoded content limit 262144 applied per stream"}),
        ("malformed-json-invalid-request", 10,
         {"overlaps": "closed envelope"},
         activated().records,
         http("desktop", JOB, "verdict", 1, 2, open_input(H),
              body_bytes=None, raw_body=b"{not json"),
         {"http_status": 400, "error": "invalid_request",
          "note": "unknown kinds rejected"}),
        ("malformed-utf8-invalid-request", 11,
         {"overlaps": "envelope decode"},
         activated().records,
         http("desktop", JOB, "verdict", 1, 2, open_input(H),
              raw_body=b'{"kind":"verdict","\xff"}'),
         {"http_status": 400, "error": "invalid_request",
          "note": "head-byte SHA256 never accepts wrong bytes"}),
        ("void-verdict-bearing-round", 12, {"overlaps": "void rule"},
         None, None,
         {"http_status": 409, "error": "round_has_verdict",
          "note": "grant unconsumed"}),
        ("forged-verdict-digest-mismatch", 13,
         {"overlaps": "Q11"},
         opened(activated()).records,
         http("desktop", JOB, "verdict", 1, 3,
              {"round_no": 1, "head": H, "verdict": "PASS",
               "path": "/jobs/job-a/v.md",
               "content_sha256": "f" * 64,
               "content_b64": b64(verdict_bytes(H)),
               "tester_job_id": None}),
         {"http_status": 400, "error": "invalid_request",
          "note": "audit carries diagnostics only"}),
        ("unknown-kind-invalid-request", 14,
         {"overlaps": "closed union"},
         activated().records,
         http("desktop", JOB, "steal_head", 1, 2, {}, bad=True),
         {"http_status": 400, "error": "invalid_request",
          "note": "closed envelope + closed payload; never lands"}),
        ("canonicalization-digest-excludes-auth", 15,
         {"overlaps": "digest def"},
         opened(activated()).records,
         http("desktop", JOB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-a/v.md",
                            verdict_bytes(H)),
              bearer="different-token"),
         {"http_status": 201, "created": True,
          "note": "digest over method/path/RFC8785 body only; "
                  "idempotency + auth headers excluded"}),
        ("stored-received-verdict-body-rejections", 16,
         {"overlaps": "verdict grammar"},
         opened(activated()).records,
         http("desktop", JOB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-a/v.md",
                            verdict_bytes(H).replace(b"VERDICT: PASS",
                                                     b"VERDICT: PASSX"))),
         {"http_status": 400, "error": "invalid_verdict",
          "note": "content fetched separately if needed"}),
    ]
    for name, rw, tags, given, req, exp, *kw in spec:
        kw = kw[0] if kw else {}
        inf = exp.pop("inferred", None)
        c.append(case(name, S, T, "failure", rw,
                      given if given is not None else activated().records,
                      req if req is not None else
                      http("desktop", JOB, "round_open", 1, 4,
                           open_input(H3)),
                      exp, inferred=["row keeps same meaning as design"],
                      job=kw.get("job", JOB), tags=tags))
    return wrap(T, S, c)


# ============================================================ §4 rules
def build_section4():
    T = "Error map"
    S = "4"
    em = DESIGN["error_map"]
    c = []
    rowno = 0
    for status, codes in em.items():
        rowno += 1
        for code in codes:
            c.append({"name": "errmap-%s-%s" % (status, code),
                      "source": {"section": S, "table": T,
                                 "table_name": "errmap", "row": rowno},
                      "given": {"job_id": JOB, "policy": POLICY,
                                "records": []},
                      "request": {"type": "meta",
                                  "note": "error-map member"},
                      "expect": {"http_status": int(status),
                                 "error": code}})
    # The contract rules as meta cases
    rules = [
        ("any-post-record-uses-request-shape",
         "any POST /records body is a Request object"),
        ("closed-request-union",
         "request union is closed; unknown kind is invalid_request"),
        ("successful-replay-returns-original",
         "idempotent replay returns original record incl. after end"),
        ("digest-rfc8785-method-path-body",
         "request digest = SHA256 over RFC8785 canonical JSON of "
         "{method,path,body}; auth/idempotency headers excluded"),
        ("verdict-first-line-grammar",
         "VERDICT: (PASS|BLOCKER) @<40 hex>; LF/CRLF; ASCII space/tab "
         "only; no BOM; no later VERDICT: line"),
        ("baseline-conservative-accounting",
         "activation/import accounting is conservative; shadow work has "
         "no authority"),
        ("head-repeat-enabled-required",
         "policy head_repeat_enabled must be true; no offline waiver"),
        ("review-role-no-round-budget",
         "review-role assignments have special lifecycle, no independent "
         "round budget"),
        ("protected-transitions-need-receipt",
         "protected task transitions and legacy relay/completion paths "
         "need canonical receipts"),
    ]
    for i, (nm, rule) in enumerate(rules, 1):
        c.append({"name": "rule-%02d-%s" % (i, nm),
                  "source": {"section": S, "table": "contract rules",
                             "table_name": "rules", "row": i,
                             "row_text": rule},
                  "given": {"job_id": JOB, "policy": POLICY,
                            "records": []},
                  "request": {"type": "meta",
                              "note": "normative rule"},
                  "expect": {"rule": rule}})
    return {"pack": "hk1873.v1", "file": "section4-rules.json",
            "section": S, "table": "Error map + contract rules",
            "cases": c}


def build_r4life():
    T = "R4 review-lifecycle and binding regression vectors"
    S = "5"
    c = []
    base = activated(tier="T2")
    rev = reviewer_journal(REV, JOB, 1)  # own summary.state=shadow
    vb = verdict_bytes(H)

    c.append(case("r-done-5xx-intent-pending-exit-75", S, T, "r4life", 1,
                  base.records + rev.records,
                  cli("append", REV, "--kind", "completed",
                      "--idempotency-key", uuid(777000),
                      "--expected-seq", "1", "--claim-seq", "1",
                      stdin=json.dumps(completed_in()).encode(),
                      stub={"response": "unavailable"}),
                  {"exit_code": 75,
                   "stderr_code": "transport_unavailable",
                   "note": "5xx/unavailable: original intent pending; zero "
                           "local job.completed, terminal relay, "
                           "housekeeping or quota release"}))
    c.append(case("r-legacy-receiptless-terminal-relay", S, T, "r4life", 2,
                  base.records + rev.records,
                  legacy("desktop", "POST", "/v1/relay/events",
                         {"kind": "job.completed", "job_id": REV,
                          "head": H, "report_sha256": REPORT_SHA}),
                  {"http_status": 409, "error": "wrk_receipt_required",
                   "note": "no protected terminal publication/counting/"
                           "release"}))
    j2 = base.records + reviewer_journal(REV, JOB, 1).records
    c.append(case("r-canonical-completed-then-end-own-cleanup", S, T,
                  "r4life", 3, j2,
                  http("desktop", REV, "completed", 1, 1,
                       completed_in()),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "completed"},
                   "note": "authorizes only R's own eligible cleanup; later "
                           "canonical end makes R unavailable for new "
                           "target verdict; target used/PASS unchanged"},
                  job=REV))
    j3 = activated(tier="T2")
    rev3 = reviewer_journal(REV, JOB, 1)
    j3.add("round_open", {"round_no": 1, **open_input(H, tester=REV)})
    verdicted(j3, 1, H, "PASS", tester=REV)
    j3.add("joined", {"head": H, "pr": PR_URL,
                      "report_sha256": REPORT_SHA,
                      "report_size_bytes": len(REPORT),
                      "verdict_seqs": [4], "waiver_grant_seq": None})
    c.append(case("r-cleanup-after-subject-join-still-receipt-required",
                  S, T, "r4life", 4,
                  splice(j3.records, rev3.records),
                  http("desktop", REV, "end", 1, 1,
                       end_in("reaped", "cleanup")),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "end"},
                   "note": "R remains receipt-required; no O2 downgrade; "
                           "earlier accepted PASS stays valid"}, job=REV))
    j4 = Journal(SHADOW, "desktop")
    j4.add("claim", claim_in(SHADOW, "b2402-s", "director-1", 2402,
                             "desktop", "builder", "T2"), actor="director-1")
    c.append(case("p1-shadow-host-scoped-mint-claim-conflict", S, T,
                  "r4life", 5, j4.records,
                  http("desktop", REV, "claim", 0, 0,
                       scoped_claim_input(REV, SHADOW, 1, task=2402,
                                          owner="b2402-s")),
                  {"http_status": 409, "error": "claim_conflict",
                   "note": "no fallback; C1 requires a central P1 "
                           "assignment before reviewer start"},
                  job=REV))
    c.append(case("p1-shadow-central-reviewer-claim-starts", S, T,
                  "r4life", 6, j4.records,
                  http("director-1", REV, "claim", 0, 0,
                       scoped_claim_input(REV, SHADOW, 1, task=2402,
                                          owner="b2402-s")),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "claim"},
                   "note": "R starts without activate; own done/end "
                           "fail-closed; target verdict stays untrusted"},
                  job=REV))
    j5 = reviewer_journal(REV, JOB, 1)
    j5.add("end", end_in("reaped", "cleanup"), actor="desktop")
    c.append(case("r-end-receipt-cannot-drop-subject-task", S, T,
                  "r4life", 7, base.records + j5.records,
                  legacy("director-1", "POST",
                         "/v1/tasks/2400/transition",
                         {"to": "dropped",
                          "receipt": {"job_id": REV, "seq": 2}}),
                  {"http_status": 409, "error": "wrk_receipt_required",
                   "note": "requires the current bound subject A end "
                           "receipt; shared task_id is insufficient"},
                  legacy=legacy_given(tasks=[legacy_task()])))
    for kind, pl in [("import_begin", {"reason_code": "import_freeze"}),
                     ("import", {"source_locator": "x",
                                 "source_sha256": ZERO64,
                                 "original_actor_client_id": "a",
                                 "original_at":
                                 "2026-10-20T00:00:00.000000Z",
                                 "legacy_kind": "job.round",
                                 "content_b64": b64(b"{}")}),
                     ("import_seal", {"begin_seq": 2,
                                      "expected_import_count": 0,
                                      "manifest_sha256": sha([]),
                                      "reason_code": "import_seal"})]:
        c.append(case("r-%s-scope-denied" % kind.replace("_", "-"),
                      S, T, "r4life", 8, base.records + rev.records,
                      http("desktop", REV, kind, 1, 1, pl),
                      {"http_status": 403, "error": "scope_denied",
                       "note": "required-mode reviewer never becomes an "
                               "unprotected shadow-import job"}, job=REV))
    return wrap(T, S, c)


def build_failure():
    T = "Failure and abuse cases"
    S = "8"
    c = []
    jv = verdicted(opened(activated()), 1, H, "PASS", full_digest=True)
    v = jv.records[3]
    vb = verdict_bytes(H)
    spec = [
        ("replay-old-verdict-identical-200", 1, jv.records,
         http("desktop", JOB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md", vb),
              key=v["idempotency_key"]),
         {"http_status": 200, "created": False, "no_new_records": True,
          "record_ref": {"seq": 4, "record_id": v["record_id"],
                         "record_hash": v["record_hash"]},
          "note": "no extra cap count"}),
        ("replay-old-verdict-fresh-key-same-path", 2, jv.records,
         http("desktop", JOB, "verdict", 1, 4,
              verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                            verdict_bytes(H2))),
         {"http_status": 409, "error": "duplicate_path",
          "note": "fresh key + already-used path = duplicate_path; the "
                  "row's second clause covers changed-bytes-at-fresh-"
                  "path (duplicate_content) and head reuse blocked at "
                  "round_open by duplicate_head"}),
        ("verdict-foreign-host-job-host-mismatch", 3,
         opened(activated(job=JOBB, host="mac-dev", owner="b2401-b",
                          task=2401)).records,
         http("desktop", JOBB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-b/v.md", vb)),
         {"http_status": 403, "error": "host_mismatch",
          "note": "exception only via centrally minted cross-host "
                  "tester claim"}, JOBB),
        ("grant-for-different-job-head-grant-invalid", 4,
         activated().records + granted0(),
         http("desktop", JOB, "round_open", 1, 2,
              open_input(H, ext=2)),
         {"http_status": 409, "error": "grant_invalid",
          "note": "grant naming different job/claim/action/round/head "
                  "stays unconsumed on its own job"}),
        ("two-testers-one-head-hk1901", 5, two_testers_given(),
         http("desktop", JOB, "round_open", 1, 5,
              open_input(H, tester=REV2, rep=5)),
         {"http_status": 201, "created": True,
          "note": "separate numbered round + distinct tester claim; "
                  "head_repeat naming earlier round+second tester; at "
                  "cap also extension; both verdicts persist; BLOCKER "
                  "after PASS prevents join"}),
        ("forged-content-sha256-invalid-request", 6,
         opened(activated()).records,
         http("desktop", JOB, "verdict", 1, 3,
              {"round_no": 1, "head": H, "verdict": "PASS",
               "path": "/jobs/job-a/v.md", "content_sha256": "f" * 64,
               "content_b64": b64(vb), "tester_job_id": None}),
         {"http_status": 400, "error": "invalid_request"}),
        ("server-loses-acknowledged-write-history-regressed", 7,
         jv.records,
         meta_req("consumer observes lower seq/changed generation/"
                  "different hash at a retained seq"),
         {"client_code": "history_regressed",
          "server_after_freeze": {"http_status": 503,
                                  "error": "wrk_recovery_required"},
          "note": "client/consumer code, not a server response; "
                  "requests operator recovery freeze"}),
        ("clock-skew-server-seq-orders", 8, jv.records,
         meta_req("host clock skew between hosts"),
         {"outcome": "normal_record",
          "note": "server seq orders all ops; grant expiry uses server "
                  "UTC at consumption; skew buys no slots"}),
        ("leaked-host-token-bounded-scope", 9,
         activated().records,
         http("desktop", "job-q", "claim", 0, 0,
              claim_in("job-q", "b2403-q", "director-1", 2403,
                       "desktop", "builder", "T1")),
         {"http_status": 403, "error": "scope_denied",
          "note": "attacker can submit host-attested evidence and "
                  "consume grants for that host's jobs only; cannot "
                  "mint root/builder/worker claims or grants"},
         "job-q"),
        ("leaked-central-token-full-authority", 10,
         activated().records,
         http("director-1", "job-q", "claim", 0, 0,
              claim_in("job-q", "b2403-q", "director-1", 2403,
                       "desktop", "builder", "T1")),
         {"http_status": 201, "created": True,
          "note": "valid privileged records within API checks; "
                  "revoke/rotate and audit central records"},
         "job-q"),
        ("grant-consumer-race-void-reuse", 11,
         grant_race_given(),
         http("desktop", JOB, "round_open", 1, 9,
              open_input(H5, ext=6)),
         {"http_status": 409, "error": "grant_consumed",
          "note": "job lock + unique consumption pick one winner; "
                  "second is sequence_conflict until refresh then "
                  "grant_consumed; void never returns grant capacity"}),
        ("planted-legacy-events-import-untrusted", 12,
         imported_activated(planted=True).records,
         http("desktop", SHADOW, "joined", 1, 9, joined_in(head=H)),
         {"http_status": 409, "error": "zero_rounds",
          "note": "only import records result; asserted authors "
                  "untrusted; baseline>=3; no forged PASS promoted"},
         SHADOW),
        ("deleted-local-events-canonical-unchanged", 13,
         activated().records,
         legacy("desktop", "POST", "/v1/relay/events",
                {"kind": "job.completed", "job_id": "job-a", "head": H,
                 "report_sha256": REPORT_SHA}),
         {"http_status": 409, "error": "wrk_receipt_required",
          "note": "director rejects fake/missing receipts; canonical "
                  "records unchanged"}),
        ("committed-write-lost-reply-replay-repairs", 14, jv.records,
         http("desktop", JOB, "verdict", 1, 3,
              verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                            vb), key=v["idempotency_key"]),
         {"http_status": 200, "created": False,
          "note": "retried original key returns original record; GET "
                  "repairs projections; no duplicated consumption"}),
        ("legacy-joined-direct-or-task-relink-fence", 15,
         activated().records,
         legacy("desktop", "POST", "/v1/relay/events",
                {"kind": "job.joined", "job_id": "job-a", "head": H}),
         {"http_status": 409, "error": "wrk_receipt_required",
          "note": "no terminal relay/task mutation; unknown completion "
                  "cannot release quota"}),
        ("end-races-open-verdict-grant-serialized", 16,
         end_race_given(),
         http("desktop", JOB, "round_open", 1, 5, open_input(H2)),
         {"http_status": 409, "error": "job_ended",
          "note": "serialized journal: if end wins mutation is "
                  "job_ended; if mutation wins its record precedes "
                  "end; no ambiguous timestamp ordering"}),
    ]
    for name, rw, given, req, exp, *rest in spec:
        job = rest[0] if rest else JOB
        c.append(case(name, S, T, "failure", rw, given, req, exp, job=job))
    return wrap(T, S, c)


def granted0():
    j = Journal(JOBB, "mac-dev")
    j.add("claim", claim_in(JOBB, "b2401-b", "director-1", 2401,
                            "mac-dev", "builder", "T2"), actor="director-1")
    j.add("grant", grant_in("extension", round_no=4, head=H4, extra=1,
                            reason="cap_extension"), actor="director-1")
    return j.records


def two_testers_given():
    j = activated(tier="T2")
    rev = reviewer_journal(REV, JOB, 1)
    rev2 = reviewer_journal(REV2, JOB, 2)
    j.add("round_open", {"round_no": 1, **open_input(H, tester=REV)})
    j.add("verdict", verdict_payload_shape(1, H, "PASS",
                                           "/jobs/job-a-t1/v.md",
                                           verdict_bytes(H), tester=REV))
    granted(j, "head_repeat", round_no=2, head=H, earlier=1, tester=REV2,
            reason="repeat_review")
    return splice(splice(j.records, rev.records), rev2.records)


def grant_race_given():
    j = opened(opened(opened(activated(), 1, H), 2, H2), 3, H3)
    granted(j, "extension", round_no=4, head=H4, extra=1,
            reason="cap_extension")
    j.add("round_open", {"round_no": 4, **open_input(H4, ext=6)})
    granted(j, "void", round_no=4, reason="void_round")
    j.add("void", void_in(4, 8))
    return j.records


def end_race_given():
    j = verdicted(opened(activated()), 1, H, "PASS")
    j.add("end", end_in())
    return j.records


def build_headrepeat():
    T = "Head-repeat binding vector"
    S = "5"
    srcrow = "a grant for reviewer B cannot be consumed with " \
        "round_open.tester_job_id=null or reviewer C; both return " \
        "grant_invalid. A valid exact-B open followed by " \
        "verdict.tester_job_id=null/C fails the assignment check. A " \
        "reviewer claim ended/reaped before new verdict append gives " \
        "round_not_open for that assignment; exact successful replay " \
        "remains available. A reviewer ending after accepted PASS has " \
        "no effect on later joined acceptance. Aborted live " \
        "verdict-free open -> valid void -> new same-head open is " \
        "allowed without head_repeat if no other live open or actual " \
        "verdict reserves the head. The void does not allow copying a " \
        "prior verdict path/bytes."
    vb = verdict_bytes(H)
    jg = activated(tier="T2")
    opened(jg, 1, H)
    verdicted(jg, 1, H, "PASS")
    rev = reviewer_journal(REV, JOB, 2)
    rev2 = reviewer_journal(REV2, JOB, 2)
    granted(jg, "head_repeat", round_no=2, head=H, earlier=1, tester=REV,
            reason="repeat_review")
    given = splice(jg.records, rev.records) + rev2.records

    def mk(n, name, given_, req, exp):
        e = case(name, S, T, "headrepeat", n, given_, req, exp)
        e["source"]["row_text"] = srcrow
        return e

    return wrap(T, S, [
        mk(1, "repeat-grant-null-tester-grant-invalid", given,
           http("desktop", JOB, "round_open", 1, 5,
                open_input(H, tester=None, rep=5)),
           {"http_status": 409, "error": "grant_invalid"}),
        mk(2, "repeat-grant-wrong-reviewer-grant-invalid", given,
           http("desktop", JOB, "round_open", 1, 5,
                open_input(H, tester=REV2, rep=5)),
           {"http_status": 409, "error": "grant_invalid"}),
        mk(3, "exact-b-open-verdict-null-tester-fails-assignment",
           given + [expect_record(jg.records, JOB, "round_open",
                                  open_input(H, tester=REV, rep=5,
                                             round_no=2), "desktop")],
           http("desktop", JOB, "verdict", 1, 6,
                verdict_input(2, H, "PASS", "/jobs/job-a/v.md",
                              verdict_bytes(H, body="assignment check\n"),
                              tester=None)),
           {"http_status": 409, "error": "round_not_open",
            "note": "fails the assignment check; same for "
                    "verdict.tester_job_id=C"}),
        mk(4, "reviewer-ended-before-verdict-round-not-open",
           clause4_given(),
           http("desktop", JOB, "verdict", 1, 6,
                verdict_input(2, H, "PASS", "/jobs/job-a/v.md",
                              verdict_bytes(H, body="assignment check\n"),
                              tester=REV)),
           {"http_status": 409, "error": "round_not_open",
            "note": "ended/reaped claim before append; exact successful "
                    "replay remains available"}),
        mk(5, "reviewer-end-after-pass-join-unaffected",
           r_end_after_pass_given(),
           http("desktop", JOB, "joined", 1, 4,
                joined_in(head=H)),
           {"http_status": 201, "created": True,
            "note": "ending after accepted PASS has no effect on later "
                    "joined acceptance"}),
        mk(6, "void-open-then-same-head-open-no-repeat",
           voided_open_given(),
           http("desktop", JOB, "round_open", 1, 5, open_input(H)),
           {"http_status": 201, "created": True,
            "record": {"kind": "round_open",
                       "payload": open_input(H, round_no=2)},
            "note": "no head_repeat needed: no other live open or "
                    "actual verdict reserves H"}),
        mk(7, "voided-open-verdict-cannot-copy-old-path",
           voided_open_prior_verdict_given(),
           http("desktop", JOB, "verdict", 1, 8,
                verdict_input(3, H, "PASS", "/jobs/job-a/verdict-r1.md",
                              vb)),
           {"http_status": 409, "error": "duplicate_path",
            "note": "the void does not allow copying a prior verdict "
                    "path/bytes"}),
    ])


def clause4_given():
    """B-assigned round 2 open, then the reviewer ends before the verdict."""
    restore = clock_block(2)
    j = activated(tier="T2")
    opened(j, 1, H)
    verdicted(j, 1, H, "PASS")
    r = reviewer_journal(REV, JOB, 2)
    granted(j, "head_repeat", round_no=2, head=H, earlier=1, tester=REV,
            reason="repeat_review")
    j.add("round_open", {"round_no": 2,
                         **open_input(H, tester=REV, rep=5)})
    r.add("end", end_in("reaped", "cleanup"), actor="desktop")
    recs = splice(j.records, r.records)
    restore()
    return recs


def r_end_after_pass_given():
    j = activated(tier="T2")
    r = reviewer_journal(REV, JOB, 1)
    j.add("round_open", {"round_no": 1, **open_input(H, tester=REV)})
    j.add("verdict", verdict_payload_shape(1, H, "PASS",
                                           "/jobs/job-a-t1/v.md",
                                           verdict_bytes(H), tester=REV))
    r.add("end", end_in("reaped", "cleanup"), actor="desktop")
    return splice(j.records, r.records)


def voided_open_given():
    j = opened(activated(), 1, H)
    granted(j, "void", round_no=1, reason="void_round")
    j.add("void", void_in(1, 4))
    return j.records


def voided_open_prior_verdict_given():
    """A prior verdict reserves its path; a later voided open reuses H."""
    restore = clock_block(5)
    j = opened(activated(), 1, H2)
    verdicted(j, 1, H2, "PASS", path="/jobs/job-a/verdict-r1.md",
              body="prior review\n")
    j.add("round_open", {"round_no": 2, **open_input(H)})
    granted(j, "void", round_no=2, reason="void_round")
    j.add("void", void_in(2, 6))
    j.add("round_open", {"round_no": 3, **open_input(H)})
    recs = j.records
    restore()
    return recs


# ============================================================ data files
def design_table(start, end):
    """Return [header, sep, data rows...] verbatim markdown lines."""
    out = []
    for i, line in enumerate(open(DESIGN_MD), 1):
        if start <= i <= end and line.startswith("|"):
            out.append(line.rstrip("\n"))
    return out


def row_texts(start, end):
    rows = design_table(start, end)
    return rows[2:]  # skip header + separator


TABLE_LINES = {
    "errmap": (141, 151), "transition": (3966, 3983),
    "permmap": (3991, 4012), "migration": (4037, 4046),
    "fence": (4056, 4066), "r3": (4075, 4085), "r4claim": (4094, 4114),
    "r4life": (4119, 4128), "matrix": (4147, 4155),
    "inventory": (4169, 4201), "cli_cmds": (4213, 4219),
    "cli_exit": (4223, 4234), "transport": (4242, 4255),
    "phases": (4262, 4268), "failure": (4282, 4299),
}


def attach_row_text(files):
    """Fill source.row_text from the design table when not set."""
    cache = {}
    for f in files:
        for c in f["cases"]:
            tn = c["source"].get("table_name")
            r = c["source"].get("row")
            if tn not in TABLE_LINES or not r:
                continue
            if c["source"].get("row_text"):
                continue
            if tn not in cache:
                cache[tn] = row_texts(*TABLE_LINES[tn])
            c["source"]["row_text"] = cache[tn][r - 1]


def build_matrix():
    rows = row_texts(*TABLE_LINES["matrix"])
    hdr = design_table(*TABLE_LINES["matrix"])[0]
    cols = [c.strip() for c in hdr.strip("|").split("|")]
    commands = []
    for i, r in enumerate(rows, 1):
        cells = [c.strip() for c in r.strip("|").split("|")]
        commands.append({
            "row": i, "command": cells[0],
            "modes": dict(zip(cols[1:], cells[1:])),
            "source": {"section": "6",
                       "table": "Enforced/mixed/offline command matrix",
                       "table_name": "matrix", "row": i,
                       "row_text": r}})
    return {"section": "6",
            "table": "Enforced/mixed/offline command matrix",
            "mode_columns": cols[1:], "commands": commands}


def build_cli():
    subs = row_texts(*TABLE_LINES["cli_cmds"])
    subcommands = []
    for i, r in enumerate(subs, 1):
        cells = [c.strip() for c in r.strip("|").split("|")]
        subcommands.append({
            "row": i, "subcommand": cells[0], "stdin": cells[1],
            "request_stdout": cells[2],
            "source": {"section": "6b",
                       "table": "Subcommand and exact arguments after "
                                "handoffkeep",
                       "table_name": "cli_cmds", "row": i,
                       "row_text": r}})
    ex = row_texts(*TABLE_LINES["cli_exit"])
    exit_map = []
    for i, r in enumerate(ex, 1):
        cells = [c.strip() for c in r.strip("|").split("|")]
        import re
        codes = [x.strip() for x in
                 re.sub(r"\([^)]*\)", "", cells[0]).split(",")]
        codes = [x for x in codes if x]
        exit_map.append({
            "row": i, "codes": codes,
            "exit": int(cells[1]),
            "source": {"section": "6b",
                       "table": "Stable code(s) -> CLI/adapter exit",
                       "table_name": "cli_exit", "row": i,
                       "row_text": r}})
    return {"section": "6b",
            "stderr_format": "handoffkeep wrk: CODE",
            "stdout_contract":
                "exactly one newline-terminated JSON object on success; "
                "empty on failure",
            "subcommands": subcommands, "exit_map": exit_map}


def build_inventory():
    rows = row_texts(*TABLE_LINES["inventory"])
    hdr = design_table(*TABLE_LINES["inventory"])[0]
    cols = [c.strip() for c in hdr.strip("|").split("|")]
    entries = []
    for i, r in enumerate(rows, 1):
        cells = [c.strip() for c in r.strip("|").split("|")]
        entries.append({
            "row": i,
            "site": cells[0], "required_change": cells[1],
            "owner": cells[2] if len(cells) > 2 else "",
            "source": {"section": "6a",
                       "table": "Receipt consumer inventory",
                       "table_name": "inventory", "row": i,
                       "row_text": r}})
    return {"section": "6a", "table": "Receipt consumer inventory",
            "columns": cols, "entries": entries}


def build_rollout():
    rows = row_texts(*TABLE_LINES["phases"])
    phases = []
    for i, r in enumerate(rows, 1):
        cells = [c.strip() for c in r.strip("|").split("|")]
        phases.append({
            "row": i, "phase": cells[0], "entry_check": cells[1],
            "rollback": cells[2] if len(cells) > 2 else "",
            "source": {"section": "7",
                       "table": "Migration and rollout phases",
                       "table_name": "phases", "row": i,
                       "row_text": r}})
    return {"section": "7", "table": "Migration and rollout phases",
            "phases": phases}


def build_permmap():
    rows = row_texts(*TABLE_LINES["permmap"])
    perms = []
    for i, r in enumerate(rows, 1):
        cells = [c.strip() for c in r.strip("|").split("|")]
        perms.append({"row": i, "permission": cells[0],
                      "operation": cells[1],
                      "source": {"section": "5",
                                 "table": "Permission-to-operation map",
                                 "table_name": "permmap", "row": i,
                                 "row_text": r}})
    return {"section": "5", "table": "Permission-to-operation map",
            "permissions": perms}


DESIGN_KEY = "designs/hk1873/b1873a-sol-r4"
CONTRACT_SHA = "ce4f6efbe39514bb0bced2d0a66510e646058a66d0d28bbb417af180f375c19d"


def extract_contract():
    """Return the ~~~json fenced schema block, byte-exact."""
    txt = open(DESIGN_MD).read()
    marker = "~~~json"
    start = txt.index(marker) + len(marker)
    if txt[start] == "\r\n":
        start += 2
    elif txt[start] == "\n":
        start += 1
    end = txt.index("~~~", start)
    return txt[start:end]


def check_design():
    """The pack pins one design body; refuse to generate from another."""
    try:
        body = open(DESIGN_MD, "rb").read()
    except OSError:
        raise SystemExit(
            "design body not found at %s; fetch hk:doc %s "
            "(sha %s...) and pass --design FILE"
            % (DESIGN_MD, DESIGN_KEY, CONTRACT_SHA[:8]))
    got = sha(body)
    if got != CONTRACT_SHA:
        raise SystemExit("design body sha256 %s != pinned %s"
                         % (got, CONTRACT_SHA))


def main():
    check_design()
    os.makedirs(OUT, exist_ok=True)
    # legacy fixtures first so their real hashes land in vectors
    for rel, data in gen_legacy.dirs().items():
        LEGACY_FILES["legacy/" + rel] = data
        emit("legacy/" + rel, data)

    emit("contract.json", extract_contract())

    files = [build_section4(), build_transition(), build_migration(),
             build_fence(), build_r3(), build_r4claim(), build_r4life(),
             build_headrepeat(), build_transport(), build_failure()]
    attach_row_text(files)
    for f in files:
        emit("vectors/" + f["file"], f)

    emit("matrix.json", build_matrix())
    emit("cli.json", build_cli())
    emit("inventory.json", build_inventory())
    emit("rollout.json", build_rollout())
    emit("permmap.json", build_permmap())

    emit("README.md", README_TMPL % {"sha": CONTRACT_SHA})

    # MANIFEST.json
    man = {"design": {"key": DESIGN_KEY,
                      "doc": 44185, "revision": 4, "amendment": 1,
                      "body_sha256": CONTRACT_SHA},
           "files": {}}
    found = {}
    for root, _, names in os.walk(OUT):
        for n in names:
            if n == "MANIFEST.json":
                continue
            p = os.path.join(root, n)
            found[os.path.relpath(p, OUT).replace(os.sep, "/")] = p
    for rel in sorted(found):
        p = found[rel]
        man["files"][rel] = {"sha256": sha(open(p, "rb").read()),
                             "bytes": os.path.getsize(p)}
    emit("MANIFEST.json", man)
    total = sum(len(f["cases"]) for f in files)
    print("wrote %d vector cases in %d files + data files to %s"
          % (total, len(files), OUT))


README_TMPL = """# O3 contract fixture pack (hk 1924, M1)

Machine-readable contract for the O3 authority design, generated from a
single pinned design document:

- key: `designs/hk1873/b1873a-sol-r4`
- document: 44185, revision 4, review amendment 1
- body sha256: `%(sha)s`

The pack changes only when a new design revision exists; the manifest
pins the design body sha and the sha256 of every pack file, so any
fixture edit without a matching manifest update fails verification.
Manifest hash changes without a new design revision must be rejected
in review.

## Files

- `contract.json` — the section 5 JSON Schema 2020-12 bundle, copied
  byte-exact from the design's fenced json block. Closed Request union,
  per-kind Input/Payload definitions, Error enumeration, endpoints and
  Policy schema.
- `vectors/*.json` — every normative vector table converted to data,
  one file per table. Each case is `{name, source, given, request,
  expect}`; `source` names the design section, the design's own table
  name, the table row number and the verbatim row text, plus an optional
  `note` when the case's setup needs explaining. `given` holds prior
  journal records and policy as Record objects; `request` is a Request
  object for HTTP cases, `{type:"cli"}` argv/stdin for section
  6b cases, `{type:"legacy"}` for fenced legacy routes, or
  `{type:"policy"|"meta"|"none"}` for non-HTTP expectations.
- `given.legacy` (optional) — the legacy store rows a case needs.
  `tasks` holds task rows `{id, project, state, lane, parent_lane,
  refs}` whose `refs.job_id` names the canonical subject job that
  protects the row; `dispositions` holds disposition rows
  `{id, task_id, state}` whose `task_id` is the legacy task row the
  disposition targets. A case carries exactly the rows its request
  needs. A case whose point is the absence of a row carries none and
  says so in `source.note`.
- `legacy/` — sanitized legacy event fixtures for the import vectors:
  synthetic job dirs with `events/0000N-<kind>.json` in today's format.
  Every id, hash and path is synthetic; no real job dir, inbox, event
  file, hostname or home path was copied.
- `matrix.json` — the section 6 command-by-mode matrix as data.
- `cli.json` — the section 6b subcommand table and stable-code to
  process-exit map as data.
- `permmap.json` — the section 5 permission-to-operation map.
- `inventory.json` — the section 6a receipt-consumer inventory.
- `rollout.json` — the section 7 rollout phases.
- `MANIFEST.json` — the design key, revision and body sha256, plus the
  sha256 and size of every file in the pack.

## Consumers

- S1 (the handoffkeep server, hk 1925) consumes this pack by an in-repo
  Go test: `go test -p 2 ./internal/o3contract/...` verifies the bundle,
  validates every vector's request and expected record against the
  contract schemas, and pins per-table case counts.
- C1 (the wrk client, hk 1926) consumes it later by vendoring the pack
  into agent-skills at a pinned MANIFEST sha.

The pack contains no secret, token value or environment value.
"""


if __name__ == "__main__":
    main()
