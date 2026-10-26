package wrkrecords_test

import (
	"encoding/base64"
	"encoding/json"
	. "github.com/mgh3326/handoffkeep/internal/wrkrecords"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These scenarios are constructed from design rules, independently of M1.
func designPolicy(t *testing.T) *Policy {
	t.Helper()
	clients := Object{}
	reads := []string{"capabilities.read", "job.read", "records.read", "content.read"}
	for _, host := range []string{"desktop", "mac-dev", "mac-work", "rpi", "mac-personal"} {
		permissions := append(append([]string{}, reads...), "reviewer.claim.write", "round.open", "verdict.write", "void.write", "joined.write", "completed.write", "end.write", "relay.publish")
		clients[host] = Object{"host_client_id": host, "permissions": permissions}
	}
	for _, id := range []string{"director-1", "operator-desk"} {
		permissions := append(append([]string{}, reads...), "reviewer.claim.write", "claim.write", "activate.write", "grant.write", "grant.cancel", "end.write", "import.begin", "import.seal", "import.write", "task.state")
		clients[id] = Object{"host_client_id": nil, "permissions": permissions}
	}
	for _, id := range []string{"pa-integrator", "at-integrator", "pb-integrator", "tt-integrator"} {
		clients[id] = Object{"host_client_id": nil, "permissions": reads}
	}
	raw, _ := json.Marshal(Object{"version": "hk1873.policy.v1", "clients": clients, "lanes": Object{"director-1": nil, "pa-integrator": "director-1"}, "features": Object{"head_repeat_enabled": true, "claim_lane_creation_enabled": true}})
	p, e := ParsePolicy(raw)
	if e != nil {
		t.Fatal(e)
	}
	return p
}

type scenario struct {
	t *testing.T
	e Engine
	s Snapshot
}

func fresh(t *testing.T) *scenario {
	t.Helper()
	c := &scenario{t: t, e: Engine{Policy: designPolicy(t), Mode: "enforce", Now: time.Date(2026, 10, 24, 2, 0, 0, 0, time.UTC), Generation: NewUUID()}, s: Snapshot{Jobs: map[string][]Record{}, Contents: map[string]map[int64][]byte{}}}
	c.must("director-1", "subject", "claim", Object{"job_id": "subject", "owner_lane": "builder-a", "parent_lane": "director-1", "task_id": float64(9001), "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil})
	c.must("director-1", "subject", "activate", Object{"reason_code": "new_job", "seal_seq": nil})
	return c
}
func (c *scenario) request(job, kind string, p Object) Request {
	rs := c.s.Jobs[job]
	seq, claim := int64(len(rs)), int64(0)
	if x := c.s.Claim(job); x != nil {
		claim = x.Seq
	}
	return Request{kind, claim, seq, p}
}
func (c *scenario) send(actor, job, key string, r Request) (WriteResponse, error) {
	out, b, e := c.e.Append(c.s, actor, job, key, r)
	if e == nil && out.Created {
		c.s.Jobs[job] = append(c.s.Jobs[job], out.Record)
		if len(b) > 0 {
			if c.s.Contents[job] == nil {
				c.s.Contents[job] = map[int64][]byte{}
			}
			c.s.Contents[job][out.Record.Seq] = b
		}
	}
	return out, e
}
func (c *scenario) must(actor, job, kind string, p Object) Record {
	c.t.Helper()
	out, e := c.send(actor, job, NewUUID(), c.request(job, kind, p))
	if e != nil {
		c.t.Fatalf("%s %s: %v", job, kind, e)
	}
	return out.Record
}
func (c *scenario) want(actor, job, kind, code string, p Object) {
	c.t.Helper()
	_, e := c.send(actor, job, NewUUID(), c.request(job, kind, p))
	if e == nil || Code(e) != code {
		c.t.Fatalf("%s %s: got %v want %s", job, kind, e, code)
	}
}
func (c *scenario) reviewer(id, host string, round int64) {
	c.t.Helper()
	c.must("director-1", id, "claim", Object{"job_id": id, "owner_lane": "builder-a", "parent_lane": "builder-a", "task_id": float64(9001), "host_client_id": host, "role": "tester", "tier": "T3", "review_of": Object{"job_id": "subject", "round_no": round}, "predecessor_job_id": nil, "supersedes": nil})
}
func opening(head, tester string, extension, repeat any) Object {
	var id any
	if tester != "" {
		id = tester
	}
	return Object{"head": head, "tester_job_id": id, "extension_grant_seq": extension, "repeat_grant_seq": repeat}
}
func evidence(round int64, head, path, note string, tester any) Object {
	b := []byte("VERDICT: PASS @" + head + "\n" + note + "\n")
	return Object{"round_no": round, "head": head, "verdict": "PASS", "path": path, "content_b64": base64.StdEncoding.EncodeToString(b), "content_sha256": SHA(b), "tester_job_id": tester}
}
func report(head string) Object {
	b := []byte("design report\n")
	return Object{"head": head, "pr": "https://example.invalid/pull/1", "report_b64": base64.StdEncoding.EncodeToString(b), "report_sha256": SHA(b), "waiver_grant_seq": nil}
}
func (c *scenario) grant(kind string, round any, head any, earlier any, tester any) Record {
	extra := 0
	if kind == "extension" {
		extra = 1
	}
	return c.must("director-1", "subject", "grant", Object{"grant_type": kind, "round_no": round, "head": head, "extra": extra, "earlier_round_no": earlier, "tester_job_id": tester, "expires_at": c.e.Now.Add(time.Hour).Format(time.RFC3339Nano), "reason_code": "design_case"})
}

const h1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const h2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const h3 = "cccccccccccccccccccccccccccccccccccccccc"
const h4 = "dddddddddddddddddddddddddddddddddddddddd"

func TestDesignReplayBeforeCASAndTerminal(t *testing.T) {
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	r := c.request("subject", "verdict", evidence(1, h1, "/review/one", "first", nil))
	key := NewUUID()
	first, e := c.send("desktop", "subject", key, r)
	if e != nil {
		t.Fatal(e)
	}
	c.must("desktop", "subject", "end", Object{"end_type": "revoked", "reason_code": "done"})
	before := len(c.s.Jobs["subject"])
	replay, e := c.send("desktop", "subject", key, r)
	if e != nil || replay.Created || replay.Record.RecordHash != first.Record.RecordHash || !reflectRecord(replay.Record, first.Record) || len(c.s.Jobs["subject"]) != before {
		t.Fatalf("replay failed: %v", e)
	}
	changed := r
	changed.Payload = Clone(r.Payload)
	changed.Payload["path"] = "/review/two"
	if _, e = c.send("desktop", "subject", key, changed); e == nil || Code(e) != "idempotency_conflict" {
		t.Fatalf("changed-body replay: %v", e)
	}
}

func TestDesignReplaySummaryUsesCommittedClock(t *testing.T) {
	c := fresh(t)
	key := NewUUID()
	request := c.request("subject", "grant", Object{"grant_type": "join_waiver", "round_no": nil, "head": h1, "extra": 0, "earlier_round_no": nil, "tester_job_id": nil, "expires_at": c.e.Now.Add(time.Hour).Format(time.RFC3339Nano), "reason_code": "replay_clock"})
	original, err := c.send("director-1", "subject", key, request)
	if err != nil || !original.Created || !reflect.DeepEqual(original.Summary.Grants, []int64{original.Record.Seq}) {
		t.Fatalf("initial grant response: %v %+v", err, original.Summary)
	}
	c.e.Now = c.e.Now.Add(2 * time.Hour)
	assertReplay := func(stage string) {
		t.Helper()
		replayed, err := c.send("director-1", "subject", key, request)
		if err != nil || replayed.Created || !reflect.DeepEqual(replayed.Record, original.Record) {
			t.Fatalf("exact record replay %s: %v", stage, err)
		}
		if !reflect.DeepEqual(replayed.Summary, original.Summary) {
			t.Errorf("replay changed original committed summary %s: got %+v want %+v", stage, replayed.Summary, original.Summary)
		}
	}
	assertReplay("after expiry")
	join := report(h1)
	join["waiver_grant_seq"] = original.Record.Seq
	c.want("desktop", "subject", "joined", "grant_expired", join)
	b := []byte("later completion\n")
	c.must("desktop", "subject", "completed", Object{"report_b64": base64.StdEncoding.EncodeToString(b), "report_sha256": SHA(b)})
	c.must("desktop", "subject", "end", Object{"end_type": "reaped", "reason_code": "later_end"})
	assertReplay("after later end and stale CAS")
	current, err := c.s.Summary("subject", c.e.Time())
	if err != nil || current.State != "ended" || len(current.Grants) != 0 || current.SnapshotSeq <= original.Summary.SnapshotSeq {
		t.Fatal("fresh read lost current state/expiry")
	}
	c.s.Jobs["subject"][original.Record.Seq-1].ReceivedAt = "invalid"
	if _, err := c.send("director-1", "subject", key, request); err == nil || Code(err) != "wrk_unavailable" {
		t.Fatal("invalid stored replay timestamp did not fail closed")
	}
}

func TestDesignLiveClaimTimestampMatchesRecord(t *testing.T) {
	c := &scenario{t: t, e: Engine{Policy: designPolicy(t), Mode: "enforce", Generation: NewUUID()}, s: Snapshot{Jobs: map[string][]Record{}, Contents: map[string]map[int64][]byte{}}}
	r := c.must("director-1", "live-clock", "claim", Object{"job_id": "live-clock", "owner_lane": "builder-a", "parent_lane": "director-1", "task_id": 9001, "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil})
	if Text(r.Payload, "created_at") != r.ReceivedAt {
		t.Fatal("claim created_at differs from its accepted record timestamp")
	}
}
func reflectRecord(a, b Record) bool {
	x, _ := Canonical(a)
	y, _ := Canonical(b)
	return string(x) == string(y)
}
func TestDesignReviewerEndAfterPASSAndIdentity(t *testing.T) {
	c := fresh(t)
	c.reviewer("tester-a", "desktop", 1)
	c.must("desktop", "subject", "round_open", opening(h1, "tester-a", nil, nil))
	c.must("desktop", "subject", "verdict", evidence(1, h1, "/review/frozen", "first", "tester-a"))
	c.must("desktop", "tester-a", "end", Object{"end_type": "reaped", "reason_code": "cleanup"})
	c.must("desktop", "subject", "joined", report(h1))
	c.want("director-1", "subject", "joined", "scope_denied", report(h1))
}
func TestDesignDuplicatePathSurvivesVoid(t *testing.T) {
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	c.must("desktop", "subject", "verdict", evidence(1, h1, "/review/preserved", "first", nil))
	c.must("desktop", "subject", "round_open", opening(h2, "", nil, nil))
	g := c.grant("void", int64(2), nil, nil, nil)
	c.must("desktop", "subject", "void", Object{"round_no": 2, "grant_seq": g.Seq, "reason_code": "aborted"})
	c.must("desktop", "subject", "round_open", opening(h3, "", nil, nil))
	c.want("desktop", "subject", "verdict", "duplicate_path", evidence(3, h3, "/review/./preserved", "different", nil))
}
func TestDesignScopedClaimsNoEscalation(t *testing.T) {
	c := fresh(t)
	p := Object{"job_id": "review-host", "owner_lane": "builder-a", "parent_lane": "builder-a", "task_id": float64(9001), "host_client_id": "desktop", "role": "tester", "tier": "T3", "review_of": Object{"job_id": "subject", "round_no": 1}, "predecessor_job_id": nil, "supersedes": nil}
	c.must("desktop", "review-host", "claim", p)
	c.want("desktop", "review-host", "claim", "claim_conflict", p)
	p = Clone(p)
	p["job_id"] = "self"
	p["review_of"] = Object{"job_id": "self", "round_no": 1}
	c.want("desktop", "self", "claim", "claim_conflict", p)
	p = Clone(p)
	p["job_id"] = "builder-forged"
	p["role"] = "builder"
	p["review_of"] = nil
	c.want("desktop", "builder-forged", "claim", "scope_denied", p)
	c.must("director-1", "foreign", "claim", Object{"job_id": "foreign", "owner_lane": "builder-b", "parent_lane": "director-1", "task_id": float64(9002), "host_client_id": "mac-dev", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil})
	c.must("director-1", "foreign", "activate", Object{"reason_code": "fresh", "seal_seq": nil})
	p["job_id"] = "foreign-review"
	p["role"] = "tester"
	p["review_of"] = Object{"job_id": "foreign", "round_no": 1}
	c.want("desktop", "foreign-review", "claim", "host_mismatch", p)
	p["review_of"] = Object{"job_id": "missing", "round_no": 1}
	c.want("desktop", "foreign-review", "claim", "not_found", p)
}
func TestDesignRepeatGrantAndAssignment(t *testing.T) {
	c := fresh(t)
	c.reviewer("tester-a", "desktop", 1)
	c.must("desktop", "subject", "round_open", opening(h1, "tester-a", nil, nil))
	c.must("desktop", "subject", "verdict", evidence(1, h1, "/review/a", "a", "tester-a"))
	c.reviewer("tester-b", "desktop", 2)
	g := c.grant("head_repeat", int64(2), h1, int64(1), "tester-b")
	c.want("desktop", "subject", "round_open", "grant_invalid", opening(h1, "tester-b", nil, int64(1)))
	c.must("desktop", "subject", "round_open", opening(h1, "tester-b", nil, g.Seq))
	c.want("desktop", "subject", "verdict", "round_not_open", evidence(2, h1, "/review/b", "b", "tester-a"))
	c.must("desktop", "tester-b", "end", Object{"end_type": "reaped", "reason_code": "cleanup"})
	c.want("desktop", "subject", "verdict", "round_not_open", evidence(2, h1, "/review/b", "b", "tester-b"))
}
func TestDesignExtensionNotRefundedAndBeforeCap(t *testing.T) {
	c := fresh(t)
	g := c.grant("extension", int64(1), h1, nil, nil)
	c.want("desktop", "subject", "round_open", "grant_before_cap", opening(h1, "", g.Seq, nil))
	for _, h := range []string{h1, h2, h3} {
		c.must("desktop", "subject", "round_open", opening(h, "", nil, nil))
	}
	g = c.grant("extension", int64(4), h4, nil, nil)
	c.must("desktop", "subject", "round_open", opening(h4, "", g.Seq, nil))
	v := c.grant("void", int64(4), nil, nil, nil)
	c.must("desktop", "subject", "void", Object{"round_no": 4, "grant_seq": v.Seq, "reason_code": "abort"})
	c.want("desktop", "subject", "round_open", "grant_consumed", opening(h4, "", g.Seq, nil))
}
func TestDesignJoinPendingAndGuard(t *testing.T) {
	c := fresh(t)
	c.reviewer("tester-a", "desktop", 1)
	c.must("desktop", "subject", "round_open", opening(h1, "tester-a", nil, nil))
	c.must("desktop", "subject", "verdict", evidence(1, h1, "/review/a", "pass", "tester-a"))
	c.must("desktop", "subject", "round_open", opening(h2, "", nil, nil))
	c.want("desktop", "subject", "joined", "review_incomplete", report(h1))
	p := evidence(2, h2, "/review/b", "sk-"+strings.Repeat("X", 32), nil)
	c.want("desktop", "subject", "verdict", "invalid_request", p)
	if len(c.s.Contents["subject"]) != 1 {
		t.Fatal("rejected evidence stored")
	}
}
func TestDesignImportFloorAndNoPlantedTrust(t *testing.T) {
	c := fresh(t)
	delete(c.s.Jobs, "subject")
	delete(c.s.Contents, "subject")
	c.must("director-1", "subject", "claim", Object{"job_id": "subject", "owner_lane": "builder-a", "parent_lane": "director-1", "task_id": 9001, "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil})
	begin := c.must("director-1", "subject", "import_begin", Object{"reason_code": "archive"})
	b := []byte(`{"kind":"job.round","round":1,"head":"` + h1 + `"}`)
	imported := c.must("director-1", "subject", "import", Object{"source_locator": "fixture/events/one.json", "source_sha256": SHA(b), "original_actor_client_id": nil, "original_at": nil, "legacy_kind": "job.round", "content_b64": base64.StdEncoding.EncodeToString(b)})
	manifestRows := []Object{{"seq": imported.Seq, "source_locator": imported.Payload["source_locator"], "source_sha256": imported.Payload["source_sha256"]}}
	for _, event := range []Object{
		{"kind": "job.verdict", "round": 1, "head": h1, "verdict": "PASS", "path": "/planted/pass", "content_sha256": strings.Repeat("a", 64)},
		{"kind": "job.grant", "grant_type": "join_waiver", "head": h1},
	} {
		bytes, _ := json.Marshal(event)
		r := c.must("director-1", "subject", "import", Object{"source_locator": "fixture/" + Text(event, "kind"), "source_sha256": SHA(bytes), "original_actor_client_id": nil, "original_at": nil, "legacy_kind": Text(event, "kind"), "content_b64": base64.StdEncoding.EncodeToString(bytes)})
		manifestRows = append(manifestRows, Object{"seq": r.Seq, "source_locator": r.Payload["source_locator"], "source_sha256": r.Payload["source_sha256"]})
	}
	manifest, _ := Digest(manifestRows)
	seal := c.must("director-1", "subject", "import_seal", Object{"begin_seq": begin.Seq, "expected_import_count": len(manifestRows), "manifest_sha256": manifest, "reason_code": "sealed"})
	if Num(seal.Payload, "baseline_used") != 3 || Num(seal.Payload, "numbering_floor") != 3 {
		t.Fatal("incorrect sealed floor")
	}
	c.must("director-1", "subject", "activate", Object{"seal_seq": seal.Seq, "reason_code": "enforce"})
	c.want("desktop", "subject", "joined", "zero_rounds", report(h1))
	g := c.grant("extension", int64(4), h4, nil, nil)
	r := c.must("desktop", "subject", "round_open", opening(h4, "", g.Seq, nil))
	if Num(r.Payload, "round_no") != 4 {
		t.Fatal("round 4 required")
	}
}

func TestDesignStoredSealFailsClosed(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "sealed", true: "activated"}[active], func(t *testing.T) {
			c := fresh(t)
			claim := c.s.Jobs["subject"][0]
			c.s.Jobs["subject"] = []Record{claim, {JobID: "subject", Seq: 2, Kind: "import_seal", Payload: Object{"baseline_used": 3, "numbering_floor": 1}}}
			if active {
				c.s.Jobs["subject"] = append(c.s.Jobs["subject"], Record{JobID: "subject", Seq: 3, Kind: "activate", Payload: Object{"baseline_used": 3}})
			}
			if _, e := c.s.Summary("subject", c.e.Now); Code(e) != "import_unsealed" {
				t.Fatalf("corrupt seal read: %v", e)
			}
			before := len(c.s.Jobs["subject"])
			c.want("desktop", "subject", "round_open", "import_unsealed", opening(h4, "", nil, nil))
			if len(c.s.Jobs["subject"]) != before {
				t.Fatal("allocated from corrupt seal")
			}
		})
	}
}

func TestDesignReviewerMustExistBeforeNamedOpen(t *testing.T) {
	c := fresh(t)
	c.want("desktop", "subject", "round_open", "round_not_open", opening(h1, "late-reviewer", nil, nil))
	if len(c.s.Jobs["subject"]) != 2 {
		t.Fatal("open accepted before reviewer claim")
	}
	c.reviewer("late-reviewer", "desktop", 1)
	c.must("desktop", "subject", "round_open", opening(h1, "late-reviewer", nil, nil))
}

func TestDesignP1AssignmentUsesSubjectPhase(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	// Global enforce must still permit a central P1 assignment on a shadow subject.
	c.reviewer("p1-reviewer", "desktop", 1)
	c.must("desktop", "subject", "round_open", opening(h1, "p1-reviewer", nil, nil))
	c.must("desktop", "subject", "verdict", evidence(1, h1, "/p1/review", "shadow observation", "p1-reviewer"))
	if !c.s.ReceiptRequired("p1-reviewer") || c.s.ProtectedTask(9001) {
		t.Fatal("P1 lifecycle/subject protection mismatch")
	}
}

func TestDesignWaiverStrandedAtJoin(t *testing.T) {
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	g := c.grant("join_waiver", nil, h1, nil, nil)
	sum, e := c.s.Summary("subject", c.e.Now)
	if e != nil || len(sum.Grants) != 0 || len(sum.Warnings) != 1 || sum.Warnings[0] != "grant_stranded" {
		t.Fatalf("waiver summary: %+v %v", sum, e)
	}
	p := report(h1)
	p["waiver_grant_seq"] = g.Seq
	c.want("desktop", "subject", "joined", "review_incomplete", p)
}

func TestDesignVerdictMustMatchOpenHead(t *testing.T) {
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	c.want("desktop", "subject", "verdict", "head_mismatch", evidence(1, h2, "/review/wrong-head", "different head", nil))
}
func TestDesignLegacyRelayReceiptRequired(t *testing.T) {
	c := fresh(t)
	for _, kind := range []string{"job.completed", "job.joined", "job.revoked", "job.reaped", "job.handed_off", "job.takeover", "end"} {
		if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{JobID: "subject", Kind: kind}); Code(e) != "wrk_receipt_required" {
			t.Fatalf("%s: %v", kind, e)
		}
	}
	if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{JobID: "subject", Kind: "job.lost"}); e != nil {
		t.Fatalf("notification fenced: %v", e)
	}
	c.reviewer("review-role", "desktop", 1)
	if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{JobID: "review-role", Kind: "job.completed"}); Code(e) != "wrk_receipt_required" {
		t.Fatal("claim-only reviewer bypass")
	}
}

func TestDesignHostCannotMintBuilder(t *testing.T) {
	c := fresh(t)
	c.want("desktop", "forged-builder", "claim", "scope_denied", Object{"job_id": "forged-builder", "owner_lane": "forged-lane", "parent_lane": "director-1", "task_id": 9002, "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": nil, "supersedes": nil})
}

func TestDesignSuccessorInheritsNumbersAndIdentities(t *testing.T) {
	c := fresh(t)
	c.must("desktop", "subject", "round_open", opening(h1, "", nil, nil))
	c.must("desktop", "subject", "verdict", evidence(1, h1, "/review/inherited", "original", nil))
	c.must("desktop", "subject", "round_open", opening(h2, "", nil, nil))
	g := c.grant("void", int64(2), nil, nil, nil)
	c.must("desktop", "subject", "void", Object{"round_no": 2, "grant_seq": g.Seq, "reason_code": "abort"})
	c.must("desktop", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	successor := func(job, predecessor string) {
		previous, err := c.s.Summary(predecessor, c.e.Time())
		if err != nil {
			t.Fatal(err)
		}
		c.must("director-1", job, "claim", Object{"job_id": job, "owner_lane": "builder-a", "parent_lane": "director-1", "task_id": 9001, "host_client_id": "desktop", "role": "builder", "tier": "T3", "review_of": nil, "predecessor_job_id": predecessor, "supersedes": nil})
		activated := c.must("director-1", job, "activate", Object{"seal_seq": nil, "reason_code": "successor"})
		current, err := c.s.Summary(job, c.e.Time())
		if err != nil || Num(activated.Payload, "baseline_used") != previous.Used || current.Baseline != previous.Used || current.Used != previous.Used {
			t.Fatalf("successor activation lost predecessor used: got %+v want %d (%v)", current, previous.Used, err)
		}
	}
	successor("successor-one", "subject")
	r := c.must("desktop", "successor-one", "round_open", opening(h3, "", nil, nil))
	if Num(r.Payload, "round_no") != 3 {
		t.Fatal("recycled predecessor void number")
	}
	c.must("desktop", "successor-one", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	successor("successor-two", "successor-one")
	c.want("desktop", "successor-two", "round_open", "duplicate_head", opening(h3, "", nil, nil))
	c.want("desktop", "successor-two", "round_open", "duplicate_head", opening(h1, "", nil, nil))
	r = c.must("desktop", "successor-two", "round_open", opening(h4, "", nil, nil))
	if Num(r.Payload, "round_no") != 4 {
		t.Fatal("successor numbering floor lost")
	}
	c.want("desktop", "successor-two", "verdict", "duplicate_path", evidence(4, h4, "/review/inherited", "changed bytes", nil))
	c.want("desktop", "successor-two", "verdict", "duplicate_content", evidence(4, h1, "/review/unique", "original", nil))
}

func TestDesignReviewPermissionCannotUseCentralClaimPermission(t *testing.T) {
	c := fresh(t)
	var wire Object
	for _, v := range loadVectors(t) {
		if len(v.Given.Policy) > 0 {
			if json.Unmarshal(v.Given.Policy, &wire) != nil {
				t.Fatal("policy")
			}
			break
		}
	}
	clients := Obj(wire, "clients")
	director := Obj(clients, "director-1")
	permissions := []any{}
	for _, v := range director["permissions"].([]any) {
		if v != "reviewer.claim.write" {
			permissions = append(permissions, v)
		}
	}
	director["permissions"] = permissions
	raw, _ := json.Marshal(wire)
	p, e := ParsePolicy(raw)
	if e != nil {
		t.Fatal(e)
	}
	c.e.Policy = p
	c.want("director-1", "central-reviewer", "claim", "scope_denied", Object{"job_id": "central-reviewer", "owner_lane": "builder-a", "parent_lane": "builder-a", "task_id": 9001, "host_client_id": "desktop", "role": "tester", "tier": "T3", "review_of": Object{"job_id": "subject", "round_no": 1}, "predecessor_job_id": nil, "supersedes": nil})
}

func TestDesignImportGuardAndIncompleteHistory(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	begin := c.must("director-1", "subject", "import_begin", Object{"reason_code": "archive"})
	input := func(b []byte) Object {
		return Object{"source_locator": "synthetic/event", "source_sha256": SHA(b), "original_actor_client_id": nil, "original_at": nil, "legacy_kind": "job.verdict", "content_b64": base64.StdEncoding.EncodeToString(b)}
	}
	c.want("director-1", "subject", "import", "invalid_request", input([]byte("sk-"+strings.Repeat("X", 32))))
	r := c.must("director-1", "subject", "import", input([]byte("malformed legacy bytes")))
	manifest, _ := Digest([]Object{{"seq": r.Seq, "source_locator": r.Payload["source_locator"], "source_sha256": r.Payload["source_sha256"]}})
	seal := c.must("director-1", "subject", "import_seal", Object{"begin_seq": begin.Seq, "expected_import_count": 1, "manifest_sha256": manifest, "reason_code": "seal"})
	if Num(seal.Payload, "baseline_used") != 3 || Num(seal.Payload, "numbering_floor") != 3 {
		t.Fatal("incomplete history lowered floor")
	}
	sum, e := c.s.Summary("subject", c.e.Now)
	if e != nil || len(sum.Warnings) != 1 || sum.Warnings[0] != "migration_history_incomplete" {
		t.Fatalf("missing migration warning: %+v %v", sum, e)
	}
}

func TestDesignPolicyGraphKeepsReferencedEdges(t *testing.T) {
	c := fresh(t)
	var wire Object
	for _, v := range loadVectors(t) {
		if len(v.Given.Policy) > 0 {
			_ = json.Unmarshal(v.Given.Policy, &wire)
			break
		}
	}
	lanes := Obj(wire, "lanes")
	lanes["builder-a"] = "director-1"
	raw, _ := json.Marshal(wire)
	old, e := ParsePolicy(raw)
	if e != nil {
		t.Fatal(e)
	}
	if e = old.ValidateGraph(c.s, nil); e != nil {
		t.Fatal(e)
	}
	delete(lanes, "builder-a")
	raw, _ = json.Marshal(wire)
	replacement, e := ParsePolicy(raw)
	if e != nil {
		t.Fatal(e)
	}
	if e = replacement.ValidateGraph(c.s, old); Code(e) != "claim_conflict" {
		t.Fatal("removed referenced policy edge")
	}
	lanes["builder-a"] = "pa-integrator"
	raw, _ = json.Marshal(wire)
	replacement, e = ParsePolicy(raw)
	if e != nil {
		t.Fatal(e)
	}
	if e = replacement.ValidateGraph(c.s, old); Code(e) != "claim_conflict" {
		t.Fatal("changed referenced canonical parent")
	}
}

func TestDesignImportedVerdictKeepsNegativeIdentity(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	begin := c.must("director-1", "subject", "import_begin", Object{"reason_code": "archive"})
	original := []byte("VERDICT: PASS @" + h1 + "\narchived PASS\n")
	bytes, _ := json.Marshal(Object{"kind": "job.verdict", "payload": Object{"round": 1, "head": strings.ToUpper(h1), "path": "/review/../negative/path", "content_sha256": strings.ToUpper(SHA(original)), "verdict": "PASS"}})
	imported := c.must("director-1", "subject", "import", Object{"source_locator": "synthetic/verdict", "source_sha256": SHA(bytes), "original_actor_client_id": nil, "original_at": nil, "legacy_kind": "job.verdict", "content_b64": base64.StdEncoding.EncodeToString(bytes)})
	manifest, _ := Digest([]Object{{"seq": imported.Seq, "source_locator": imported.Payload["source_locator"], "source_sha256": imported.Payload["source_sha256"]}})
	seal := c.must("director-1", "subject", "import_seal", Object{"begin_seq": begin.Seq, "expected_import_count": 1, "manifest_sha256": manifest, "reason_code": "seal"})
	c.must("director-1", "subject", "activate", Object{"seal_seq": seal.Seq, "reason_code": "activate"})
	c.want("desktop", "subject", "round_open", "duplicate_head", opening(h1, "", nil, nil))
	grant := c.grant("extension", int64(4), h2, nil, nil)
	c.must("desktop", "subject", "round_open", opening(h2, "", grant.Seq, nil))
	c.want("desktop", "subject", "verdict", "duplicate_path", evidence(4, h2, "/negative/path", "fresh bytes", nil))
	p := evidence(4, h2, "/negative/unique", "fresh bytes", nil)
	p["content_b64"], p["content_sha256"] = base64.StdEncoding.EncodeToString(original), SHA(original)
	c.want("desktop", "subject", "verdict", "duplicate_content", p)
	c.want("desktop", "subject", "joined", "review_incomplete", report(h2))
}

func TestDesignShadowLegacyFenceStartsAtActivate(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	binding := LegacyIntent{TaskID: 9001, CurrentJob: "subject", JobID: "subject", Binding: true}
	state := LegacyIntent{TaskID: 9001, CurrentJob: "subject", To: "claimed"}
	for _, in := range []LegacyIntent{binding, state} {
		if e := c.s.CheckLegacy(c.e.Policy, "desktop", in); e != nil {
			t.Fatalf("P1 legacy operation fenced: %v", e)
		}
	}
	c.reviewer("p1-reviewer", "desktop", 1)
	if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{JobID: "p1-reviewer", Binding: true}); Code(e) != "wrk_receipt_required" {
		t.Fatal("review role was not protected immediately")
	}
	c.must("director-1", "subject", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	for _, in := range []LegacyIntent{binding, state} {
		if e := c.s.CheckLegacy(c.e.Policy, "desktop", in); Code(e) != "wrk_receipt_required" {
			t.Fatalf("activated operation escaped: %v", e)
		}
	}
}

func TestDesignImportNumberingOverflowIsRefused(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	begin := c.must("director-1", "subject", "import_begin", Object{"reason_code": "archive"})
	b, _ := json.Marshal(Object{"kind": "job.round", "round": MaxInt})
	r := c.must("director-1", "subject", "import", Object{"source_locator": "synthetic/overflow", "source_sha256": SHA(b), "original_actor_client_id": nil, "original_at": nil, "legacy_kind": "job.round", "content_b64": base64.StdEncoding.EncodeToString(b)})
	manifest, _ := Digest([]Object{{"seq": r.Seq, "source_locator": r.Payload["source_locator"], "source_sha256": r.Payload["source_sha256"]}})
	c.want("director-1", "subject", "import_seal", "invalid_request", Object{"begin_seq": begin.Seq, "expected_import_count": 1, "manifest_sha256": manifest, "reason_code": "seal"})
	sum, e := c.s.Summary("subject", c.e.Now)
	if e != nil || sum.State != "importing" {
		t.Fatal("overflow committed a seal")
	}
}

func TestDesignCompletedRelayRequiresExactMetadata(t *testing.T) {
	c := fresh(t)
	b := []byte("report metadata\n")
	r := c.must("desktop", "subject", "completed", Object{"report_b64": base64.StdEncoding.EncodeToString(b), "report_sha256": SHA(b)})
	receipt := r.Receipt()
	in := LegacyIntent{JobID: "subject", Kind: "job.completed", Digest: SHA(b), Receipt: &receipt}
	if e := c.s.CheckLegacy(c.e.Policy, "desktop", in); e != nil {
		t.Fatal(e)
	}
	in.Head = h1
	if e := c.s.CheckLegacy(c.e.Policy, "desktop", in); Code(e) != "wrk_receipt_required" {
		t.Fatal("noncanonical completion head accepted")
	}
}

func TestDesignShadowCompletionReceiptBoundary(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	c.reviewer("p1-reviewer", "desktop", 1)
	b := []byte("P1 completion observation\n")
	p := Object{"report_b64": base64.StdEncoding.EncodeToString(b), "report_sha256": SHA(b)}
	old := c.must("desktop", "subject", "completed", p)
	reviewer := c.must("desktop", "p1-reviewer", "completed", p)
	ended := c.must("desktop", "p1-reviewer", "end", Object{"end_type": "reaped", "reason_code": "cleanup"})
	check := func(r Record, kind, code string) {
		t.Helper()
		receipt := r.Receipt()
		e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{JobID: r.JobID, Kind: kind, Digest: Text(r.Payload, "report_sha256"), Receipt: &receipt})
		if (code == "" && e != nil) || (code != "" && Code(e) != code) {
			t.Fatalf("%s seq %d receipt: %v want %s", r.JobID, r.Seq, e, code)
		}
	}
	check(old, "job.completed", "") // Ordinary P1 legacy compatibility.
	check(reviewer, "job.completed", "")
	check(ended, "job.revoked", "")
	begin := c.must("director-1", "subject", "import_begin", Object{"reason_code": "freeze"})
	manifest, _ := Digest([]Object{})
	seal := c.must("director-1", "subject", "import_seal", Object{"begin_seq": begin.Seq, "expected_import_count": 0, "manifest_sha256": manifest, "reason_code": "seal"})
	c.must("director-1", "subject", "activate", Object{"seal_seq": seal.Seq, "reason_code": "enforce"})
	check(old, "job.completed", "wrk_receipt_required")
	check(reviewer, "job.completed", "") // Subject phase never downgrades reviewer lifecycle.
	check(ended, "job.revoked", "")
	fresh := c.must("desktop", "subject", "completed", p)
	check(fresh, "job.completed", "")
}

func TestDesignOnlyCurrentClaimEnrollsTask(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	claim := Clone(c.s.Claim("subject").Payload)
	delete(claim, "created_at")
	delete(claim, "policy_sha256")
	claim["task_id"], claim["supersedes"] = 9002, int64(1)
	c.must("director-1", "subject", "claim", claim)
	c.must("director-1", "subject", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	assertTasks := func() {
		t.Helper()
		if c.s.ProtectedTask(9001) || !c.s.ProtectedTask(9002) {
			t.Fatal("superseded claim enrolled the mistaken task")
		}
		if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{TaskID: 9001, Binding: true}); e != nil {
			t.Fatalf("detached task lost legacy compatibility: %v", e)
		}
		if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{TaskID: 9002, Binding: true}); Code(e) != "wrk_receipt_required" {
			t.Fatalf("current task was not fenced: %v", e)
		}
	}
	assertTasks()
	c.must("desktop", "subject", "end", Object{"end_type": "handed_off", "reason_code": "successor"})
	claim["job_id"], claim["predecessor_job_id"], claim["supersedes"] = "successor", "subject", nil
	c.must("director-1", "successor", "claim", claim)
	assertTasks() // Durable predecessor enrollment also protects a shadow successor.
	c.must("director-1", "successor", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	assertTasks()
	claim["job_id"], claim["owner_lane"], claim["task_id"], claim["predecessor_job_id"] = "other", "builder-b", 9001, nil
	c.must("director-1", "other", "claim", claim)
	c.must("director-1", "other", "activate", Object{"seal_seq": nil, "reason_code": "enforce"})
	if !c.s.ProtectedTask(9001) {
		t.Fatal("other activated job did not enroll its current task")
	}
}

func TestDesignReviewClaimEnrollsNoTask(t *testing.T) {
	c := fresh(t)
	c.s.Jobs["subject"] = c.s.Jobs["subject"][:1]
	c.reviewer("p1-reviewer", "desktop", 1)
	if !c.s.ReceiptRequired("p1-reviewer") || c.s.ProtectedTask(9001) {
		t.Fatal("review assignment enrolled a subject task")
	}
	if e := c.s.CheckLegacy(c.e.Policy, "desktop", LegacyIntent{TaskID: 9001, CurrentJob: "subject", Binding: true}); e != nil {
		t.Fatal("review claim changed shadow subject's task protection")
	}
}
