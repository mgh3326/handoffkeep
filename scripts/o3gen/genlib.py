"""Shared helpers for the hk 1924 O3 pack generator."""
import base64
import hashlib
import json
import os

ZERO64 = "0" * 64
GEN = "00000000-0000-4000-8000-ffff00000001"

TASK = 2400
JOB = "job-a"          # fresh activated desktop subject
JOBB = "job-b"         # mac-dev foreign subject
SHADOW = "job-s"       # shadow / importing subject
P1JOB = "job-p1"       # P1 shadow subject
REV = "job-a-t1"       # same-host review job
REVX = "job-b-t1"      # cross-host review job on job-b
OWNER = "b2400-a"      # job-a builder lane; parent director-1
PR_URL = "https://pr.invalid/handoffkeep/pull/2400"

H = "1" * 40
H2 = "2" * 40
H3 = "3" * 40
H4 = "4" * 40
H5 = "5" * 40

TABLES = json.load(open(os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "tables.json")))


def row(tab, n):
    return TABLES[tab]["rows"][n - 1].strip("|").strip()


def canon(o):
    return json.dumps(o, separators=(",", ":"), sort_keys=True,
                      ensure_ascii=False).encode("utf-8")


def sha(o):
    if isinstance(o, (dict, list)):
        o = canon(o)
    if isinstance(o, str):
        o = o.encode("utf-8")
    return hashlib.sha256(o).hexdigest()


def b64(raw):
    if isinstance(raw, str):
        raw = raw.encode("utf-8")
    return base64.standard_b64encode(raw).decode("ascii")


def uuid(n):
    return "00000000-0000-4000-8000-%012d" % n


def ts(n):
    # n counts synthetic microseconds past 2026-10-24T00:00:00Z
    sec, micro = divmod(n, 1000000)
    return "2026-10-24T00:%02d:%02d.%06dZ" % (sec // 60, sec % 60, micro)


# --------------------------------------------------------------- policy
def host_perms():
    return ["capabilities.read", "job.read", "records.read", "content.read",
            "round.open", "verdict.write", "void.write", "joined.write",
            "completed.write", "end.write", "relay.publish",
            "reviewer.claim.write"]


def central_perms():
    return ["capabilities.read", "job.read", "records.read", "content.read",
            "claim.write", "activate.write", "grant.write", "grant.cancel",
            "end.write", "import.begin", "import.seal", "import.write",
            "task.state", "reviewer.claim.write"]


def readonly_perms():
    return ["capabilities.read", "job.read", "records.read", "content.read"]


HOSTS = ["desktop", "mac-dev", "mac-work", "rpi", "mac-personal"]
CENTRALS = ["director-1", "operator-desk"]
READERS = ["pa-integrator", "at-integrator", "pb-integrator", "tt-integrator"]


def installed_policy():
    clients = {h: {"host_client_id": h, "permissions": host_perms()}
               for h in HOSTS}
    clients.update({c: {"host_client_id": None, "permissions": central_perms()}
                    for c in CENTRALS})
    clients.update({r: {"host_client_id": None, "permissions": readonly_perms()}
                    for r in READERS})
    return {"version": "hk1873.policy.v1", "clients": clients,
            "lanes": {"director-1": None, "pa-integrator": "director-1"},
            "features": {"head_repeat_enabled": True,
                         "claim_lane_creation_enabled": True}}


POLICY = installed_policy()
POLICY_SHA = sha(POLICY)


def verdict_bytes(head, verdict="PASS", body="review notes\n"):
    return ("VERDICT: %s @%s\n%s" % (verdict, head, body)).encode("utf-8")


REPORT = b"pack report bytes\n"
REPORT_SHA = sha(REPORT)
REPORT_B64 = b64(REPORT)


def verdict_input(round_no, head, verdict, path, content, tester=None):
    return {"round_no": round_no, "head": head, "verdict": verdict,
            "path": path, "content_sha256": sha(content),
            "content_b64": b64(content), "tester_job_id": tester}


def open_input(head, tester=None, ext=None, rep=None, round_no=None):
    p = {"head": head, "tester_job_id": tester,
         "extension_grant_seq": ext, "repeat_grant_seq": rep}
    if round_no is not None:
        return {"round_no": round_no, **p}
    return p


# --------------------------------------------------------------- journal
class Journal:
    """Synthetic canonical journal: builds well-formed Record objects."""

    clock = [0]

    def __init__(self, job_id, actor):
        self.job = job_id
        self.actor = actor
        self.records = []
        self._k = 0

    def claim_seq(self):
        for r in reversed(self.records):
            if r["kind"] == "claim":
                return r["seq"]
        return 0

    def tail(self):
        return len(self.records)

    def add(self, kind, payload, actor=None, imported=False,
            source=None, key=None, req_payload=None):
        seq = len(self.records) + 1
        self._k += 1
        Journal.clock[0] += 1
        actor = actor if actor is not None else self.actor
        rec = {
            "job_id": self.job,
            "seq": seq,
            "record_id": uuid(100000 + 41 * Journal.clock[0]),
            "store_generation": GEN,
            "claim_seq": seq if kind == "claim" else self.claim_seq(),
            "actor_client_id": actor,
            "idempotency_key": key or uuid(200000 + 41 * Journal.clock[0]),
            "request_sha256": ZERO64,
            "imported": imported,
            "source": source,
            "received_at": ts(Journal.clock[0]),
            "previous_hash": self.records[-1]["record_hash"]
            if self.records else ZERO64,
            "kind": kind,
            "payload": payload,
        }
        if kind == "claim":
            payload["policy_sha256"] = POLICY_SHA
            payload["created_at"] = rec["received_at"]
        body = {"kind": kind,
                "claim_seq": 0 if kind == "claim" else rec["claim_seq"],
                "expected_seq": seq - 1,
                "payload": req_payload if req_payload is not None
                else input_view(kind, payload)}
        rec["request_sha256"] = sha({"method": "POST",
                                   "path": "/v1/wrk/jobs/%s/records" % self.job,
                                   "body": body})
        rec["record_hash"] = sha(rec)
        self.records.append(rec)
        return rec


def input_view(kind, payload):
    """Persisted payload minus server-added fields == the Input_* shape."""
    p = dict(payload)
    if kind == "claim":
        for k in ("policy_sha256", "created_at"):
            p.pop(k, None)
    elif kind == "round_open":
        p.pop("round_no", None)  # server-assigned, forbidden in Input_round_open
    elif kind == "verdict":
        p.pop("size_bytes", None)
        p["content_b64"] = "AAAA"
    elif kind == "completed":
        p.pop("warnings", None)
        p["report_b64"] = "AAAA"
    elif kind == "joined":
        for k in ("report_size_bytes", "verdict_seqs"):
            p.pop(k, None)
        p["report_b64"] = "AAAA"
    elif kind == "activate":
        for k in ("baseline_used", "mode"):
            p.pop(k, None)
    elif kind == "import_begin":
        p.pop("shadow_cutoff_seq", None)
    elif kind == "import_seal":
        for k in ("shadow_cutoff_seq", "baseline_used", "numbering_floor"):
            p.pop(k, None)
    elif kind == "import":
        p["content_b64"] = "AAAA"
    return p


def claim_in(job, owner, parent, task, host, role, tier,
             review_of=None, pred=None, supers=None):
    return {"job_id": job, "owner_lane": owner, "parent_lane": parent,
            "task_id": task, "host_client_id": host, "role": role,
            "tier": tier, "review_of": review_of,
            "predecessor_job_id": pred, "supersedes": supers}


def verdict_in(round_no, head, verdict, path, content, tester=None):
    return verdict_input(round_no, head, verdict, path, content, tester)


def void_in(round_no, grant_seq, reason="void_round"):
    return {"round_no": round_no, "grant_seq": grant_seq,
            "reason_code": reason}


def grant_in(gtype, round_no=None, head=None, extra=0, earlier=None,
             tester=None, expires="2026-10-31T00:00:00.000000Z",
             reason="grant_reason"):
    return {"grant_type": gtype, "round_no": round_no, "head": head,
            "extra": extra, "earlier_round_no": earlier,
            "tester_job_id": tester, "expires_at": expires,
            "reason_code": reason}


def end_in(etype="reaped", reason="cleanup"):
    return {"end_type": etype, "reason_code": reason}


def activate_in(seal_seq=None, reason="activation"):
    return {"reason_code": reason, "seal_seq": seal_seq}


def completed_in(report=REPORT):
    return {"report_b64": b64(report), "report_sha256": sha(report)}


def joined_in(head=H, waiver=None, pr=PR_URL, report=REPORT):
    return {"head": head, "pr": pr, "report_b64": b64(report),
            "report_sha256": sha(report), "waiver_grant_seq": waiver}


def import_begin_in(reason="import_freeze"):
    return {"reason_code": reason}


def import_seal_in(begin_seq, count, manifest_sha, reason="import_seal"):
    return {"begin_seq": begin_seq, "expected_import_count": count,
            "manifest_sha256": manifest_sha, "reason_code": reason}


def import_in(locator, sha256, actor, at, lkind, content):
    return {"source_locator": locator, "source_sha256": sha256,
            "original_actor_client_id": actor, "original_at": at,
            "legacy_kind": lkind, "content_b64": b64(content)}


def cancel_in(grant_seq, reason="grant_cancel"):
    return {"grant_seq": grant_seq, "reason_code": reason}


# ------------------------------------------------------------- requests
def http(actor, job, kind, claim_seq, expected_seq, payload, key=None,
         bad=False):
    return {
        "type": "http_raw" if bad else "http",
        "actor": actor,
        "method": "POST",
        "path": "/v1/wrk/jobs/%s/records" % job,
        "idempotency_key": key or uuid(999),
        "body": {"kind": kind, "claim_seq": claim_seq,
                 "expected_seq": expected_seq, "payload": payload},
    }


def http_get(actor, path, query=None):
    r = {"type": "http", "actor": actor, "method": "GET", "path": path}
    if query is not None:
        r["query"] = query
    return r


def legacy(actor, method, path, body=None):
    r = {"type": "legacy", "actor": actor, "method": method,
         "path": path}
    if body is not None:
        r["body"] = body
    return r


def policy_req(doc):
    return {"type": "policy", "document": doc}


def cli(*argv, stdin="", stub=None, env=None):
    r = {"type": "cli", "argv": list(argv), "stdin": stdin}
    if stub is not None:
        r["stub"] = stub
    if env is not None:
        r["env"] = env
    return r


def none_req(note):
    return {"type": "none", "note": note}


# ------------------------------------------------------------- givens
def activated(job=JOB, host="desktop", owner=OWNER, task=TASK, tier="T2"):
    j = Journal(job, host)
    j.add("claim", claim_in(job, owner, "director-1", task, host, "builder",
                            tier), actor="director-1")
    j.add("activate", {"baseline_used": 0, "mode": "enforced",
                       "reason_code": "initial_activation", "seal_seq": None},
          actor="director-1")
    return j


def opened(j, round_no=1, head=H, tester=None):
    j.add("round_open", {"round_no": round_no,
                         **open_input(head, tester)}, actor=j.actor)
    return j


def verdicted(j, round_no=1, head=H, verdict="PASS", tester=None,
              path=None, body="review notes\n", full_digest=False):
    content = verdict_bytes(head, verdict, body)
    path = path or "/jobs/%s/verdict-r%d.md" % (j.job, round_no)
    j.add("verdict", {"round_no": round_no, "head": head, "verdict": verdict,
                      "path": path, "content_sha256": sha(content),
                      "size_bytes": len(content), "tester_job_id": tester},
          actor=tester_actor(j, tester),
          req_payload=verdict_input(round_no, head, verdict, path, content,
                                    tester) if full_digest else None)
    return j


def tester_actor(j, tester):
    if tester is None:
        return j.actor
    return j.actor


def granted(j, gtype, actor="director-1", **kw):
    j.add("grant", grant_in(gtype, **kw), actor=actor)
    return j


def reviewer_journal(job, target, round_no, host="desktop", role="tester",
                     actor=None, owner=OWNER, task=TASK, tier="T2"):
    """A review-role job's own journal: seq1 claim pinned per §2."""
    actor = actor or host
    j = Journal(job, actor)
    pl = claim_in(job, owner, owner, task, host, role, tier,
                  review_of={"job_id": target, "round_no": round_no})
    j.add("claim", pl, actor=actor)
    return j


def case(name, section, table, tabkey, row, given, request, expect,
         job=JOB, inferred=None, tags=None, legacy=None, source_note=None):
    source = {"section": section, "table": table,
              "table_name": tabkey, "row": row}
    if source_note:
        source["note"] = source_note
    g = {"job_id": job, "policy": POLICY, "records": given}
    if legacy is not None:
        g["legacy"] = legacy
    c = {"name": name,
         "source": source,
         "given": g,
         "request": request,
         "expect": expect}
    if inferred:
        c["inferred"] = inferred
    if tags:
        c["tags"] = tags
    return c


# ------------------------------------------------------- legacy store rows
LEGACY_PROJECT = "handoffkeep"


def legacy_task(tid=TASK, state="in_progress", project=LEGACY_PROJECT,
                job=JOB, lane=OWNER):
    """A protected legacy task row bound to a canonical subject job."""
    return {"id": tid, "project": project, "state": state, "lane": lane,
            "parent_lane": "director-1", "refs": {"job_id": job}}


def legacy_disposition(did=77, task=TASK, state="claimed"):
    """A legacy disposition row targeting a legacy task row."""
    return {"id": did, "task_id": task, "state": state}


def legacy_given(tasks=None, dispositions=None):
    d = {}
    if tasks is not None:
        d["tasks"] = tasks
    if dispositions is not None:
        d["dispositions"] = dispositions
    return d


def summary(state="active", baseline=0, used=0, next_round=1, opens=None,
            last_verdict=None, grants=None, warnings=None):
    return {"state": state, "baseline_used": baseline, "used": used,
            "next_round_no": next_round, "open_rounds": opens or [],
            "last_verdict_seq": last_verdict,
            "active_grants": grants or [], "warnings": warnings or []}


FILE_NAMES = {
    "Minimum cross-implementation transition vectors":
        "transition.json",
    "Additional migration vectors": "migration.json",
    "Legacy route fence coverage": "legacy-route-fence.json",
    "R3 regression vectors": "r3-regression.json",
    "R4 normative vectors": "r4-scoped-claim.json",
    "R4 review-lifecycle and binding regression vectors":
        "r4-review-lifecycle.json",
    "Head-repeat binding vector": "head-repeat-binding.json",
    "CLI transport and intents vectors": "transport-intents.json",
    "Failure and abuse cases": "failure-abuse.json",
    "Error map + contract rules": "section4-rules.json",
}


def wrap(table, section, cases):
    return {"pack": "hk1873.v1", "section": section, "table": table,
            "file": FILE_NAMES[table], "cases": cases}

REV2 = "job-a-t2"

DESIGN = {"error_map": {
    400: ["invalid_request", "invalid_cursor", "invalid_verdict",
          "invalid_path", "grant_before_cap"],
    401: ["unauthorized"],
    403: ["scope_denied", "host_mismatch"],
    404: ["not_found"],
    409: ["idempotency_conflict", "sequence_conflict", "claim_conflict",
          "job_ended", "review_sealed", "round_cap", "round_not_open",
          "round_has_verdict", "round_voided", "head_mismatch",
          "duplicate_path", "duplicate_content", "duplicate_head",
          "grant_invalid", "grant_consumed", "grant_expired",
          "zero_rounds", "review_incomplete", "blocker_unresolved",
          "wrk_receipt_required", "import_unsealed", "import_sealed",
          "migration_frozen"],
    413: ["request_too_large"],
    405: ["method_not_allowed"],
    415: ["unsupported_media_type"],
    503: ["wrk_unavailable", "wrk_recovery_required"]}}

def verdict_payload_shape(round_no, head, verdict, path, content,
                          tester=None):
    """Persisted Payload_verdict (no content_b64)."""
    return {"round_no": round_no, "head": head, "verdict": verdict,
            "path": path, "content_sha256": sha(content),
            "size_bytes": len(content), "tester_job_id": tester}


def meta_req(note):
    return {"type": "meta", "note": note}
