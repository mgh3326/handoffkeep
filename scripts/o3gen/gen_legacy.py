"""Sanitized legacy job-dir fixtures for the import vectors.

All ids, heads, lanes, hosts and paths are synthetic. File shape follows the
design's described 'today's format': events/0000N-<kind>.json flat objects.
"""
import json

H_A = "a" * 40
H_B = "b" * 40
H_C = "c" * 40


def ev(obj):
    return json.dumps(obj, indent=2, sort_keys=True).encode("utf-8") + b"\n"


def claim(job, lane, task, role="builder", tlevel="T1", parent="director-1"):
    return {"kind": "job.claim", "job_id": job, "seq": 1,
            "created_at": "2026-10-20T10:00:00+00:00",
            "payload": {"agent_label": lane, "owner_lane": lane,
                        "parent_lane": parent, "role": role,
                        "t_level": tlevel, "task_id": task}}


def spawned(job, lane, host="desktop"):
    return {"kind": "job.spawned", "job_id": job, "owner_lane": lane,
            "label": lane, "pane_id": "w0Q:pAA", "host": host,
            "parent_lane": "director-1", "epoch": 1}


def task_link(job, lane, task):
    return {"kind": "job.task_link", "job_id": job, "owner_lane": lane,
            "task_id": task, "epoch": 1}


def round_ev(job, lane, head, rnd):
    return {"kind": "job.round", "job_id": job, "owner_lane": lane,
            "label": lane, "pane_id": "w0Q:pAA", "parent_lane": "director-1",
            "head": head, "round": rnd, "epoch": 1}


def verdict_ev(job, lane, head, rnd, verdict, path=None):
    return {"kind": "job.verdict", "job_id": job, "owner_lane": lane,
            "label": lane, "pane_id": "w0Q:pAA", "parent_lane": "director-1",
            "verdict": verdict, "head": head, "round": rnd,
            "path": path or "/jobs/%s/verdict-r%d.md" % (job, rnd),
            "epoch": 1}


def void_ev(job, lane, rnd):
    return {"kind": "job.round_void", "job_id": job, "owner_lane": lane,
            "label": lane, "pane_id": "w0Q:pAA", "parent_lane": "director-1",
            "round": rnd, "epoch": 1}


def completed_ev(job, lane):
    return {"kind": "job.completed", "job_id": job, "owner_lane": lane,
            "label": lane, "pane_id": "w0Q:pAA", "host": "desktop",
            "report_path": "/jobs/%s/report.md" % job,
            "report_last_line": "DONE job=%s" % job, "epoch": 1,
            "report_sha256": "0" * 64}


def joined_ev(job, lane, head, pr=None):
    return {"kind": "job.joined", "job_id": job, "owner_lane": lane,
            "label": lane, "pane_id": "w0Q:pAA", "host": "desktop",
            "report_path": "/jobs/%s/report.md" % job,
            "report_last_line": "DONE job=%s" % job,
            "reason": "builder joined PR", "parent_lane": "director-1",
            "epoch": 1,
            "pr": pr or "https://pr.invalid/handoffkeep/pull/2399",
            "head": head}


def takeover_ev(job, lane, from_job, task, rounds_used):
    return {"kind": "job.takeover", "job_id": job, "from_job": from_job,
            "task_id": task, "rounds_used": rounds_used, "label": lane,
            "owner_lane": lane, "parent_lane": "director-1", "role": "builder",
            "epoch": 1, "at": "2026-10-21T00:00:00.000000Z"}


def grant_round_ev(job, lane, rnd, head, ref):
    e = round_ev(job, lane, head, rnd)
    e["approval"] = {"ref": ref, "issuer": "director-1", "extra": 1}
    return e


def dirs():
    """Return {relpath_under_legacy: bytes}."""
    out = {}

    # 1) inflight: in-flight job; 2 opens, one bound verdict, takeover rounds_used
    #    contributes I=2; L counts job.round=2 + unbound verdict=0.
    job, lane = "legacy-inflight", "b2390-li"
    out["inflight/events/00001-job.claim.json"] = ev(
        claim(job, lane, 2390))
    out["inflight/events/00002-job.spawned.json"] = ev(spawned(job, lane))
    out["inflight/events/00003-job.task_link.json"] = ev(task_link(job, lane, 2390))
    out["inflight/events/00004-job.takeover.json"] = ev(
        takeover_ev(job, lane, "legacy-inflight-0", 2390, 2))
    out["inflight/events/00005-job.round.json"] = ev(round_ev(job, lane, H_A, 1))
    out["inflight/events/00006-job.verdict.json"] = ev(
        verdict_ev(job, lane, H_A, 1, "BLOCKER"))
    out["inflight/events/00007-job.round.json"] = ev(round_ev(job, lane, H_B, 2))

    # 2) planted: builder-planted PASS + grant + joined before import; the
    #    archive carries them but they never become trusted records.
    job, lane = "legacy-planted", "b2391-pl"
    out["planted/events/00001-job.claim.json"] = ev(
        claim(job, lane, 2391))
    out["planted/events/00002-job.spawned.json"] = ev(spawned(job, lane))
    out["planted/events/00003-job.round.json"] = ev(round_ev(job, lane, H_A, 1))
    out["planted/events/00004-job.verdict.json"] = ev(
        verdict_ev(job, lane, H_A, 1, "PASS"))
    out["planted/events/00005-job.round.json"] = ev(
        grant_round_ev(job, lane, 2, H_B, "hk:doc/decisions/planted-extension"))
    out["planted/events/00006-job.joined.json"] = ev(joined_ev(job, lane, H_A))

    # 3) malformed: a claim plus one non-JSON event blob -> incomplete history
    job, lane = "legacy-malformed", "b2392-mf"
    out["malformed/events/00001-job.claim.json"] = ev(
        claim(job, lane, 2392))
    out["malformed/events/00002-job.round.json"] = ev(
        {"kind": "job.round", "job_id": job, "owner_lane": lane,
         "label": lane, "head": H_A, "round": "not-an-integer",
         "parent_lane": "director-1", "epoch": 1})
    out["malformed/events/00003-job.verdict.json"] = \
        b"this is not json at all\n"

    # 4) voided: two rounds, one round_void between them; voids never reduce
    job, lane = "legacy-voided", "b2393-vd"
    out["voided/events/00001-job.claim.json"] = ev(claim(job, lane, 2393))
    out["voided/events/00002-job.round.json"] = ev(round_ev(job, lane, H_A, 1))
    out["voided/events/00003-job.round_void.json"] = ev(void_ev(job, lane, 1))
    out["voided/events/00004-job.round.json"] = ev(round_ev(job, lane, H_B, 2))
    out["voided/events/00005-job.verdict.json"] = ev(
        verdict_ev(job, lane, H_B, 2, "PASS"))

    # 5) completed: finished O2 job archived for observation only
    job, lane = "legacy-completed", "b2394-cp"
    out["completed/events/00001-job.claim.json"] = ev(claim(job, lane, 2394))
    out["completed/events/00002-job.spawned.json"] = ev(spawned(job, lane))
    out["completed/events/00003-job.round.json"] = ev(round_ev(job, lane, H_A, 1))
    out["completed/events/00004-job.verdict.json"] = ev(
        verdict_ev(job, lane, H_A, 1, "PASS"))
    out["completed/events/00005-job.completed.json"] = ev(completed_ev(job, lane))
    out["completed/events/00006-job.reaped.json"] = ev(
        {"kind": "job.reaped", "job_id": job, "owner_lane": lane,
         "pane_id": "w0Q:pAA", "tab_id": "w0Q:tA", "host": "desktop",
         "at": "2026-10-22T00:00:00.000000Z", "epoch": 1})

    return out
