"""Vector builders for design §5 tables (transition, migration, fence, r3,
r4 scoped-claim, r4 review-lifecycle, head-repeat binding vector)."""
from genlib import *


# ---------------------------------------------------------------- transition
def build_transition():
    T = "Minimum cross-implementation transition vectors"
    S = "5"
    c = []

    # row 1: open H then PASS H with matching digest
    j = activated()
    c.append(case("open-h-round-1", S, T, "transition", 1, j.records,
                  http("desktop", JOB, "round_open", 1, 2, open_input(H)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H, round_no=1)},
                   "summary": summary(used=1, next_round=2, opens=[1])}))
    j = opened(activated())
    c.append(case("verdict-pass-h-binds-round-1", S, T, "transition", 1,
                  j.records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                     verdict_bytes(H))),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "verdict",
                              "payload": verdict_payload_shape(1, H, "PASS",
                                    "/jobs/job-a/verdict-r1.md",
                                    verdict_bytes(H))},
                   "summary": summary(used=1, next_round=2,
                                      last_verdict=4)}))

    # row 2: exact replay after end -> 200 created=false
    j = verdicted(opened(activated()))
    j.add("end", end_in(), actor=j.actor)
    v = j.records[3]
    replay = http("desktop", JOB, "verdict", 1, 3,
                  verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                verdict_bytes(H)),
                  key=v["idempotency_key"])
    c.append(case("replay-verdict-after-end-created-false", S, T,
                  "transition", 2, j.records, replay,
                  {"http_status": 200, "created": False,
                   "record_ref": {"seq": 4, "record_id": v["record_id"],
                                  "record_hash": v["record_hash"]},
                   "no_new_records": True}))

    # row 3: same key changed path -> idempotency_conflict
    changed = http("desktop", JOB, "verdict", 1, 3,
                   verdict_input(1, H, "PASS", "/jobs/job-a/verdict-rX.md",
                                 verdict_bytes(H)),
                   key=v["idempotency_key"])
    c.append(case("same-key-changed-path-idempotency-conflict", S, T,
                  "transition", 3, j.records, changed,
                  {"http_status": 409, "error": "idempotency_conflict",
                   "no_new_records": True}))

    # row 4: two simultaneous opens at used=2 with same expected_seq
    j = opened(opened(activated(), 1, H), 2, H2)
    c.append(case("concurrent-open-winner-used-3", S, T, "transition", 4,
                  j.records,
                  http("desktop", JOB, "round_open", 1, 4, open_input(H3)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H3, round_no=3)},
                   "summary": summary(used=3, next_round=4,
                                      opens=[1, 2, 3])}))
    c.append(case("concurrent-open-loser-sequence-conflict", S, T,
                  "transition", 4, j.records,
                  http("desktop", JOB, "round_open", 1, 3, open_input(H3)),
                  {"http_status": 409, "error": "sequence_conflict"}))
    j = opened(opened(opened(activated(), 1, H), 2, H2), 3, H3)
    c.append(case("refreshed-retry-no-grant-round-cap", S, T, "transition", 4,
                  j.records,
                  http("desktop", JOB, "round_open", 1, 5, open_input(H4)),
                  {"http_status": 409, "error": "round_cap"}))

    # row 5: PASS H2 for open H1 -> head_mismatch
    c.append(case("verdict-head-mismatch", S, T, "transition", 5,
                  opened(activated()).records,
                  http("desktop", JOB, "verdict", 1, 3,
                       verdict_input(1, H2, "PASS", "/jobs/job-a/verdict-r1.md",
                                     verdict_bytes(H2))),
                  {"http_status": 409, "error": "head_mismatch",
                   "summary": summary(used=1, next_round=2, opens=[1]),
                   "note": "open remains pending"}))

    # row 6: duplicate_path / duplicate_content
    j = opened(verdicted(opened(activated()), 1, H, "PASS"), 2, H2)
    c.append(case("same-path-different-bytes-duplicate-path", S, T,
                  "transition", 6, j.records,
                  http("desktop", JOB, "verdict", 1, 5,
                       verdict_input(2, H2, "PASS", "/jobs/job-a/verdict-r1.md",
                                     verdict_bytes(H2))),
                  {"http_status": 409, "error": "duplicate_path"}))
    c.append(case("same-bytes-fresh-path-duplicate-content", S, T,
                  "transition", 6, j.records,
                  http("desktop", JOB, "verdict", 1, 5,
                       verdict_input(2, H2, "PASS", "/jobs/job-a/verdict-r2.md",
                                     verdict_bytes(H))),
                  {"http_status": 409, "error": "duplicate_content"}))

    # row 7: second open H: no grant -> duplicate_head; head_repeat grant -> 201
    j = verdicted(opened(activated()), 1, H, "PASS")
    c.append(case("second-open-h-no-grant-duplicate-head", S, T,
                  "transition", 7, j.records,
                  http("desktop", JOB, "round_open", 1, 4, open_input(H)),
                  {"http_status": 409, "error": "duplicate_head"}))
    j = verdicted(opened(activated()), 1, H, "PASS")
    granted(j, "head_repeat", round_no=2, head=H, earlier=1, tester=REV,
            reason="repeat_review")
    rev = reviewer_journal(REV, JOB, 2)
    c.append(case("second-open-h-with-head-repeat-grant", S, T, "transition",
                  7, j.records + rev.records,
                  http("desktop", JOB, "round_open", 1, 5,
                       open_input(H, tester=REV, rep=5)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "round_open",
                              "payload": open_input(H, tester=REV, rep=5,
                                                    round_no=2)},
                   "summary": summary(used=2, next_round=3, opens=[2])}))

    # row 8: over-cap open with extension; reuse -> grant_consumed / seq conflict
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
                   "summary": summary(used=4, next_round=5,
                                      opens=[1, 2, 3, 4])}))
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

    # row 9: void verdict-bearing round -> round_has_verdict
    j = verdicted(opened(activated()), 1, H, "PASS")
    granted(j, "void", round_no=1, reason="void_round")
    c.append(case("void-verdict-bearing-round-has-verdict", S, T,
                  "transition", 9, j.records,
                  http("desktop", JOB, "void", 1, 5, void_in(1, 5)),
                  {"http_status": 409, "error": "round_has_verdict",
                   "note": "no grant consumed"}))

    # row 10: void an open with grant -> 201 used decremented; verdict -> round_voided
    j = opened(activated(), 1, H)
    granted(j, "void", round_no=1, reason="void_round")
    c.append(case("void-open-with-grant-201", S, T, "transition", 10,
                  j.records, http("desktop", JOB, "void", 1, 4, void_in(1, 4)),
                  {"http_status": 201, "created": True,
                   "record": {"kind": "void",
                              "payload": void_in(1, 4)},
                   "summary": summary(used=0, next_round=2, opens=[])}))
    j2 = opened(activated(), 1, H)
    granted(j2, "void", round_no=1, reason="void_round")
    j2.add("void", void_in(1, 4))
    c.append(case("verdict-on-voided-round-round-voided", S, T, "transition",
                  10, j2.records,
                  http("desktop", JOB, "verdict", 1, 5,
                       verdict_input(1, H, "PASS", "/jobs/job-a/verdict-r1.md",
                                     verdict_bytes(H))),
                  {"http_status": 409, "error": "round_voided"}))

    # row 11: foreign-host submit without assignment -> host_mismatch
    jb = activated(job=JOBB, host="mac-dev", owner="b2401-b", task=2401)
    opened(jb, 1, H)
    c.append(case("foreign-host-verdict-host-mismatch", S, T, "transition",
                  11, jb.records,
                  http("desktop", JOBB, "verdict", 1, 3,
                       verdict_input(1, H, "PASS", "/jobs/job-b/verdict-r1.md",
                                     verdict_bytes(H))),
                  {"http_status": 403, "error": "host_mismatch",
                   "no_new_records": True},
                  job=JOBB))

    # row 12: planted PASS/grant import + activate -> join zero_rounds; open needs extension
    j = imported_activated()
    c.append(case("activated-imported-job-join-zero-rounds", S, T,
                  "transition", 12, j.records,
                  http("director-1", SHADOW, "joined", 1, j.tail(),
                       joined_in(head=H)),
                  {"http_status": 409, "error": "zero_rounds"},
                  job=SHADOW))
    c.append(case("activated-imported-job-open-round-cap", S, T,
                  "transition", 12, j.records,
                  http("desktop", SHADOW, "round_open", 1, j.tail(),
                       open_input(H4)),
                  {"http_status": 409, "error": "round_cap",
                   "note": "baseline>=3: fresh review needs a central extension grant"},
                  job=SHADOW))

    # row 13: new mutation after end -> job_ended; prior-key replay still 200
    j = verdicted(opened(activated()), 1, H, "PASS")
    j.add("end", end_in())
    c.append(case("mutation-after-end-job-ended", S, T, "transition", 13,
                  j.records,
                  http("desktop", JOB, "round_open", 1, 5, open_input(H2)),
                  {"http_status": 409, "error": "job_ended",
                   "note": "successful prior-key replay still returns 200"}))

    # row 14: pagination snapshot
    j = opened(opened(opened(activated(), 1, H), 2, H2), 3, H3)
    c.append(case("records-page-first-of-snapshot", S, T, "transition", 14,
                  j.records,
                  http_get("desktop", "/v1/wrk/jobs/job-a/records",
                           {"after_seq": 0, "limit": 1, "snapshot_seq": 5}),
                  {"http_status": 200,
                   "records_response": {"snapshot_seq": 5, "next_after_seq": 1,
                                        "has_more": True,
                                        "record_seqs": [1]},
                   "note": "full seq 1..S exactly once across pages"}))
    c.append(case("records-page-excludes-post-snapshot-append", S, T,
                  "transition", 14, j.records,
                  http_get("desktop", "/v1/wrk/jobs/job-a/records",
                           {"after_seq": 4, "limit": 1, "snapshot_seq": 5}),
                  {"http_status": 200,
                   "records_response": {"snapshot_seq": 5, "next_after_seq": 5,
                                        "has_more": False,
                                        "record_seqs": [5]},
                   "note": "the record appended during paging is excluded"}))

    # row 15: forged legacy relay / no-job task bypass -> wrk_receipt_required
    j = activated()
    c.append(case("legacy-relay-without-receipt-denied", S, T, "transition",
                  15, j.records,
                  legacy("desktop", "POST", "/v1/relay/events",
                         {"kind": "job.completed", "job_id": "job-a",
                          "head": H, "report_sha256": REPORT_SHA}),
                  {"http_status": 409, "error": "wrk_receipt_required",
                   "note": "no terminal projection"}))
    c.append(case("legacy-task-no-job-transition-denied", S, T, "transition",
                  15, j.records,
                  legacy("desktop", "POST", "/v1/tasks/2400/transition",
                         {"to": "dropped", "no_job": True}),
                  {"http_status": 409, "error": "wrk_receipt_required"}))

    # row 16: digest mismatch -> invalid_request
    c.append(case("verdict-asserted-digest-mismatch", S, T, "transition", 16,
                  opened(activated()).records,
                  http("desktop", JOB, "verdict", 1, 3,
                       {"round_no": 1, "head": H, "verdict": "PASS",
                        "path": "/jobs/job-a/verdict-r1.md",
                        "content_sha256": "e" * 64,
                        "content_b64": b64(verdict_bytes(H)),
                        "tester_job_id": None}),
                  {"http_status": 400, "error": "invalid_request",
                   "note": "no verdict written, no grant consumed"}))
    return wrap(T, S, c)


def verdict_payload_shape(round_no, head, verdict, path, content, tester=None):
    return {"round_no": round_no, "head": head, "verdict": verdict,
            "path": path, "content_sha256": sha(content),
            "size_bytes": len(content), "tester_job_id": tester}


def imported_activated():
    """Shadow job-s: claim, shadow open, import_begin, two planted imports,
    import_seal, activate with baseline_used=3."""
    j = Journal(SHADOW, "desktop")
    j.add("claim", claim_in(SHADOW, "b2402-s", "director-1", 2402, "desktop",
                            "builder", "T2"), actor="director-1")
    j.add("round_open", {"round_no": 1, **open_input(H)})
    j.add("import_begin", {"reason_code": "import_freeze",
                           "shadow_cutoff_seq": 2}, actor="director-1")
    for i, f in enumerate(["00004-job.verdict.json",
                           "00005-job.grant.json"]):
        loc = "testdata/o3/legacy/planted/events/" + f
        raw = open("../" + "handoffkeep.1924/" + loc, "rb").read() \
            if False else None
        # content bytes filled by write_legacy(); import record carries loc/sha
        j.add("import", {"source_locator": loc, "source_sha256": ZERO64,
                         "original_actor_client_id": "desktop",
                         "original_at": "2026-10-20T00:00:00.000000Z",
                         "legacy_kind": f.split("-", 1)[1].split(".json")[0]},
              actor="director-1", imported=True,
              source={"source_locator": loc, "source_sha256": ZERO64})
    j.add("import_seal", {"begin_seq": 3, "expected_import_count": 2,
                          "manifest_sha256": ZERO64,
                          "reason_code": "import_seal",
                          "shadow_cutoff_seq": 2, "baseline_used": 3,
                          "numbering_floor": 1}, actor="director-1")
    j.add("activate", {"baseline_used": 3, "mode": "enforced",
                       "reason_code": "activation", "seal_seq": 6},
          actor="director-1")
    return j
