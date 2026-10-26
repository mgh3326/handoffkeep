package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// Design-derived rule tests (hk 1925). Each one is built from the design r4
// text, not from the M1 pack, and covers one rule the director listed or
// one mutant the brief names. Line numbers cite the design body.

// Director rule 1 (L79, L128): a verdict path used by an earlier verdict on
// the job is never reusable, even after a void.
func TestWrkDuplicatePathSurvivesVoid(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-p", "b2400-p", 2400, "T1")
	s.ok(s.post("desktop", "job-p", "round_open", openPayload(head('1'), nil, nil, nil)), "open 1")
	s.ok(s.post("desktop", "job-p", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/job-p/v.md", nil, verdictBytes("PASS", head('1'), "r1"))), "verdict 1")
	s.ok(s.post("desktop", "job-p", "round_open", openPayload(head('2'), nil, nil, nil)), "open 2")
	g := s.ok(s.post("director-1", "job-p", "grant", grantPayload("void", 2, nil, 0, nil, nil)), "void grant")
	gseq := num(g["record"].(map[string]any)["seq"])
	s.ok(s.post("desktop", "job-p", "void", map[string]any{"round_no": 2, "grant_seq": gseq, "reason_code": "aborted"}), "void 2")
	s.ok(s.post("desktop", "job-p", "round_open", openPayload(head('2'), nil, nil, nil)), "open 3")
	s.refused(s.post("desktop", "job-p", "verdict", verdictPayload(3, "PASS", head('2'), "/jobs/job-p/v.md", nil, verdictBytes("PASS", head('2'), "r3"))), "duplicate_path", "reused path after void")
	s.refused(s.post("desktop", "job-p", "verdict", verdictPayload(3, "PASS", head('2'), "/jobs/job-p/./x/../v.md", nil, verdictBytes("PASS", head('2'), "r3"))), "duplicate_path", "lexical alias of the used path")
	s.ok(s.post("desktop", "job-p", "verdict", verdictPayload(3, "PASS", head('2'), "/jobs/job-p/v3.md", nil, verdictBytes("PASS", head('2'), "r3"))), "fresh path")
}

// Director rule 2 (L130): a reviewer ended after its PASS was accepted does
// not block joined; joined reads the assignment frozen at verdict time.
func TestWrkReviewerEndAfterPassDoesNotBlockJoin(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-r", "b2401-r", 2401, "T2")
	s.ok(s.post("desktop", "job-r-t1", "claim", reviewPayload("job-r-t1", "b2401-r", 2401, "desktop", "tester", "T2", "job-r", 1)), "mint")
	s.ok(s.post("desktop", "job-r", "round_open", openPayload(head('a'), "job-r-t1", nil, nil)), "open")
	s.ok(s.post("desktop", "job-r", "verdict", verdictPayload(1, "PASS", head('a'), "/jobs/job-r-t1/v.md", "job-r-t1", verdictBytes("PASS", head('a'), "ok"))), "pass")
	s.ok(s.post("desktop", "job-r-t1", "end", map[string]any{"end_type": "reaped", "reason_code": "cleanup"}), "reviewer end")
	j := s.ok(s.post("desktop", "job-r", "joined", reportPayload(head('a'), nil)), "joined")
	if got := j["record"].(map[string]any)["payload"].(map[string]any)["verdict_seqs"]; fmt.Sprint(got) != "[4]" {
		t.Fatalf("verdict_seqs %v", got)
	}
	// A T2 PASS with no assigned reviewer cannot satisfy join.
	s2 := newScenario(t)
	s2.activeBuilder("job-q", "b2402-q", 2402, "T2")
	s2.ok(s2.post("desktop", "job-q", "round_open", openPayload(head('b'), nil, nil, nil)), "open")
	s2.ok(s2.post("desktop", "job-q", "verdict", verdictPayload(1, "PASS", head('b'), "/jobs/job-q/v.md", nil, verdictBytes("PASS", head('b'), "ok"))), "pass")
	s2.refused(s2.post("desktop", "job-q", "joined", reportPayload(head('b'), nil)), "review_incomplete", "T2 join without assignment")
}

// Director rule 3 (L4088): a scoped mint reusing an existing job id is
// claim_conflict, and a self-reference is claim_conflict before any target
// lookup (so not not_found even for a target that does not exist).
func TestWrkScopedMintReuseAndSelfReference(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-m", "b2403-m", 2403, "T2")
	s.ok(s.post("desktop", "job-m-t1", "claim", reviewPayload("job-m-t1", "b2403-m", 2403, "desktop", "tester", "T2", "job-m", 1)), "mint")
	s.refused(s.post("desktop", "job-m-t1", "claim", reviewPayload("job-m-t1", "b2403-m", 2403, "desktop", "reviewer", "T2", "job-m", 1)), "claim_conflict", "reuse with current CAS")
	s.refused(s.postBody("desktop", "job-m-t1", s.key(), map[string]any{"kind": "claim", "claim_seq": 0, "expected_seq": 0, "payload": reviewPayload("job-m-t1", "b2403-m", 2403, "desktop", "tester", "T2", "job-m", 1)}), "sequence_conflict", "reuse with stale zero CAS")
	s.refused(s.post("desktop", "job-new", "claim", reviewPayload("job-new", "b2403-m", 2403, "desktop", "tester", "T2", "job-new", 1)), "claim_conflict", "self reference with no such target")
}

// Director rule 4 (L77, L3975) and mutant "let an extension be consumed
// before the cap" companion: a consumed extension gives grant_consumed,
// and two racing consumers pick exactly one winner.
func TestWrkConsumedExtensionAndRace(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-x", "b2404-x", 2404, "T1")
	for _, h := range []byte{'1', '2', '3'} {
		s.ok(s.post("desktop", "job-x", "round_open", openPayload(head(h), nil, nil, nil)), "open")
	}
	g := s.ok(s.post("director-1", "job-x", "grant", grantPayload("extension", 4, head('4'), 1, nil, nil)), "extension")
	gseq := num(g["record"].(map[string]any)["seq"])
	claim, tail := s.cas("job-x")
	var wg sync.WaitGroup
	results := make([]packResponse, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.postBody("desktop", "job-x", fmt.Sprintf("00000000-0000-4000-9000-00000000000%d", i), map[string]any{"kind": "round_open", "claim_seq": claim, "expected_seq": tail, "payload": openPayload(head('4'), nil, gseq, nil)})
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, r := range results {
		switch {
		case r.status == 201:
			wins++
		case r.body["error"] == "sequence_conflict":
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("race: %d wins %d conflicts", wins, conflicts)
	}
	s.refused(s.post("desktop", "job-x", "round_open", openPayload(head('5'), nil, gseq, nil)), "grant_consumed", "refreshed retry")
}

// Mutant "let an extension be consumed before the cap".
func TestWrkExtensionBeforeCapRefused(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-e", "b2405-e", 2405, "T1")
	g := s.ok(s.post("director-1", "job-e", "grant", grantPayload("extension", 1, head('1'), 1, nil, nil)), "extension")
	s.refused(s.post("desktop", "job-e", "round_open", openPayload(head('1'), nil, num(g["record"].(map[string]any)["seq"]), nil)), "grant_before_cap", "extension under cap")
}

// Director rule 5 (L4033 import freeze/seal): planted legacy events earn no
// trust; an imported activated job with zero post-activate rounds gets
// zero_rounds on joined, and a fresh review needs an extension.
func TestWrkPlantedLegacyEarnsNoTrust(t *testing.T) {
	s := newScenario(t)
	s.ok(s.post("director-1", "job-s", "claim", claimPayload("job-s", "b2406-s", "director-1", 2406, "desktop", "builder", "T2")), "claim")
	s.ok(s.post("desktop", "job-s", "round_open", openPayload(head('1'), nil, nil, nil)), "shadow open")
	begin := s.ok(s.post("director-1", "job-s", "import_begin", map[string]any{"reason_code": "import_freeze"}), "begin")
	beginSeq := num(begin["record"].(map[string]any)["seq"])
	var manifest []any
	for _, name := range []string{"00003-job.round.json", "00004-job.verdict.json", "00005-job.round.json", "00006-job.joined.json"} {
		b := readLegacy(t, "planted", name)
		kind := strings.TrimSuffix(name[6:], ".json")
		r := s.ok(s.post("director-1", "job-s", "import", map[string]any{"source_locator": "planted/" + name, "source_sha256": wrk.ContentSHA256(b), "original_actor_client_id": "desktop", "original_at": "2026-10-20T00:00:00Z", "legacy_kind": kind, "content_b64": base64.StdEncoding.EncodeToString(b)}), "import "+name)
		manifest = append(manifest, map[string]any{"seq": int64(num(r["record"].(map[string]any)["seq"])), "source_locator": "planted/" + name, "source_sha256": wrk.ContentSHA256(b)})
	}
	seal := s.ok(s.post("director-1", "job-s", "import_seal", map[string]any{"begin_seq": beginSeq, "expected_import_count": 4, "manifest_sha256": wrk.ContentSHA256(wrk.MustCanonical(manifest)), "reason_code": "import_seal"}), "seal")
	sp := seal["record"].(map[string]any)["payload"].(map[string]any)
	if num(sp["baseline_used"]) < 3 || num(sp["numbering_floor"]) < num(sp["baseline_used"]) {
		t.Fatalf("seal payload %v", sp)
	}
	sealSeq := num(seal["record"].(map[string]any)["seq"])
	s.ok(s.post("director-1", "job-s", "activate", map[string]any{"reason_code": "activation", "seal_seq": sealSeq}), "activate")
	s.refused(s.post("desktop", "job-s", "joined", reportPayload(head('a'), nil)), "zero_rounds", "joined on planted PASS")
	s.refused(s.post("desktop", "job-s", "round_open", openPayload(head('c'), nil, nil, nil)), "round_cap", "fresh review without extension")
	// The planted verdict's head stays negatively reserved.
	next := num(summaryOf(s.do("desktop", "GET", "/v1/wrk/jobs/job-s", nil, nil))["next_round_no"])
	g := s.ok(s.post("director-1", "job-s", "grant", grantPayload("extension", next, head('a'), 1, nil, nil)), "extension")
	s.refused(s.post("desktop", "job-s", "round_open", openPayload(head('a'), nil, num(g["record"].(map[string]any)["seq"]), nil)), "duplicate_head", "planted head reserved")
}

// Director rule 6 (L103, L4003): director-1 has no joined.write.
func TestWrkDirectorJoinedScopeDenied(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-d", "b2407-d", 2407, "T1")
	s.refused(s.post("director-1", "job-d", "joined", reportPayload(head('1'), nil)), "scope_denied", "director joined")
}

// Director rule 7 (L77, L131): a second tester on the same head through the
// right repeat_grant_seq succeeds; a pointer to a non-grant record is
// grant_invalid.
func TestWrkSecondTesterViaRepeatGrant(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-h", "b2408-h", 2408, "T2")
	s.ok(s.post("desktop", "job-h-t1", "claim", reviewPayload("job-h-t1", "b2408-h", 2408, "desktop", "tester", "T2", "job-h", 1)), "t1")
	s.ok(s.post("desktop", "job-h", "round_open", openPayload(head('1'), "job-h-t1", nil, nil)), "open 1")
	v := s.ok(s.post("desktop", "job-h", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/job-h-t1/v.md", "job-h-t1", verdictBytes("PASS", head('1'), "t1"))), "pass 1")
	vseq := num(v["record"].(map[string]any)["seq"])
	s.ok(s.post("desktop", "job-h-t2", "claim", reviewPayload("job-h-t2", "b2408-h", 2408, "desktop", "reviewer", "T2", "job-h", 2)), "t2")
	g := s.ok(s.post("director-1", "job-h", "grant", grantPayload("head_repeat", 2, head('1'), 0, 1, "job-h-t2")), "repeat grant")
	gseq := num(g["record"].(map[string]any)["seq"])
	s.refused(s.post("desktop", "job-h", "round_open", openPayload(head('1'), "job-h-t2", nil, vseq)), "grant_invalid", "pointer to the verdict record")
	s.refused(s.post("desktop", "job-h", "round_open", openPayload(head('1'), "job-h-t2", nil, nil)), "duplicate_head", "no repeat grant")
	o := s.ok(s.post("desktop", "job-h", "round_open", openPayload(head('1'), "job-h-t2", nil, gseq)), "open 2")
	if num(o["record"].(map[string]any)["payload"].(map[string]any)["round_no"]) != 2 || num(summaryOf(o)["used"]) != 2 {
		t.Fatalf("second tester round %v", o)
	}
}

// Director rule 8 (L128, L4052): a verdict for an open whose assigned tester
// differs fails the assignment check; a reviewer ended before its verdict
// gives round_not_open.
func TestWrkVerdictAssignmentChecks(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-v", "b2409-v", 2409, "T2")
	s.ok(s.post("desktop", "job-v-t1", "claim", reviewPayload("job-v-t1", "b2409-v", 2409, "desktop", "tester", "T2", "job-v", 1)), "t1")
	s.ok(s.post("desktop", "job-v-t2", "claim", reviewPayload("job-v-t2", "b2409-v", 2409, "desktop", "tester", "T2", "job-v", 1)), "t2")
	s.ok(s.post("desktop", "job-v", "round_open", openPayload(head('1'), "job-v-t1", nil, nil)), "open")
	content := verdictBytes("PASS", head('1'), "x")
	s.refused(s.post("desktop", "job-v", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/v/a.md", nil, content)), "round_not_open", "null tester on assigned open")
	s.refused(s.post("desktop", "job-v", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/v/a.md", "job-v-t2", content)), "round_not_open", "other tester on assigned open")
	s.ok(s.post("desktop", "job-v-t1", "end", map[string]any{"end_type": "reaped", "reason_code": "cleanup"}), "t1 end")
	s.refused(s.post("desktop", "job-v", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/v/a.md", "job-v-t1", content)), "round_not_open", "ended reviewer")
}

// Director rule 9 (L4088): for a scoped mint, a missing reviewed job is
// not_found and another host's existing job is host_mismatch.
func TestWrkScopedMintHostVersusNotFound(t *testing.T) {
	s := newScenario(t)
	s.ok(s.post("director-1", "job-b", "claim", claimPayload("job-b", "b2410-b", "director-1", 2410, "mac-dev", "builder", "T2")), "mac-dev claim")
	s.ok(s.post("director-1", "job-b", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate")
	s.refused(s.post("desktop", "job-b-t1", "claim", reviewPayload("job-b-t1", "b2410-b", 2410, "desktop", "tester", "T2", "job-b", 1)), "host_mismatch", "other host's job")
	s.refused(s.post("desktop", "job-z-t1", "claim", reviewPayload("job-z-t1", "b2410-b", 2410, "desktop", "tester", "T2", "job-zzz", 1)), "not_found", "missing job")
	s.refused(s.post("mac-dev", "job-b-t2", "claim", reviewPayload("job-b-t2", "b2410-b", 2410, "desktop", "tester", "T2", "job-b", 1)), "host_mismatch", "payload names another host")
}

// Director rule 10 (L4028-4030, L4044): after an import with baseline 3 the
// next round is 4 = 1 + max(numbering_floor, post-activate rounds); an
// extension at round 4 is consumable. A stored seal with floor below
// baseline is an integrity error: it fails closed and is never clamped.
func TestWrkPostImportNumberingAndBrokenSeal(t *testing.T) {
	s := newScenario(t)
	s.ok(s.post("director-1", "job-n", "claim", claimPayload("job-n", "b2411-n", "director-1", 2411, "desktop", "builder", "T1")), "claim")
	s.ok(s.post("desktop", "job-n", "round_open", openPayload(head('1'), nil, nil, nil)), "shadow open")
	begin := s.ok(s.post("director-1", "job-n", "import_begin", map[string]any{"reason_code": "import_freeze"}), "begin")
	seal := s.ok(s.post("director-1", "job-n", "import_seal", map[string]any{"begin_seq": num(begin["record"].(map[string]any)["seq"]), "expected_import_count": 0, "manifest_sha256": wrk.ContentSHA256([]byte("[]")), "reason_code": "import_seal"}), "seal")
	sp := seal["record"].(map[string]any)["payload"].(map[string]any)
	if num(sp["baseline_used"]) != 3 || num(sp["numbering_floor"]) != 3 {
		t.Fatalf("seal %v", sp)
	}
	a := s.ok(s.post("director-1", "job-n", "activate", map[string]any{"reason_code": "activation", "seal_seq": num(seal["record"].(map[string]any)["seq"])}), "activate")
	if num(summaryOf(a)["next_round_no"]) != 4 || num(summaryOf(a)["used"]) != 3 {
		t.Fatalf("summary %v", summaryOf(a))
	}
	s.refused(s.post("desktop", "job-n", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/n/v.md", nil, verdictBytes("PASS", head('1'), "old"))), "round_not_open", "historical round")
	g := s.ok(s.post("director-1", "job-n", "grant", grantPayload("extension", 4, head('1'), 1, nil, nil)), "extension")
	o := s.ok(s.post("desktop", "job-n", "round_open", openPayload(head('1'), nil, num(g["record"].(map[string]any)["seq"]), nil)), "round 4")
	if num(o["record"].(map[string]any)["payload"].(map[string]any)["round_no"]) != 4 || num(summaryOf(o)["used"]) != 4 {
		t.Fatalf("round 4 %v", o)
	}

	// Broken seal: floor 1 below baseline 3, seeded as unreachable history.
	b := newScenario(t)
	b.ok(b.post("director-1", "job-k", "claim", claimPayload("job-k", "b2412-k", "director-1", 2412, "desktop", "builder", "T1")), "claim")
	b.ok(b.post("desktop", "job-k", "round_open", openPayload(head('1'), nil, nil, nil)), "shadow open")
	bb := b.ok(b.post("director-1", "job-k", "import_begin", map[string]any{"reason_code": "import_freeze"}), "begin")
	seedBrokenSeal(t, b.mem, "job-k", int64(num(bb["record"].(map[string]any)["seq"])))
	b.refused(b.post("director-1", "job-k", "activate", map[string]any{"reason_code": "activation", "seal_seq": int64(4)}), "import_unsealed", "activate on broken seal")
	b.ok(b.post("director-1", "job-k", "end", map[string]any{"end_type": "revoked", "reason_code": "broken_seal"}), "end stays allowed")
}

// seedBrokenSeal appends an import_seal whose numbering_floor (1) is below
// its baseline_used (3), chained correctly, as fixture history.
func seedBrokenSeal(t *testing.T, mem *wrk.Memory, job string, beginSeq int64) {
	t.Helper()
	recs := mem.Records(job)
	last := recs[len(recs)-1]
	rec := wrk.Record{JobID: job, Seq: last.Seq + 1, RecordID: wrk.NewUUID(), StoreGeneration: packGeneration, ClaimSeq: last.ClaimSeq, ActorClientID: "director-1", IdempotencyKey: wrk.NewUUID(), RequestSHA256: strings.Repeat("a", 64), ReceivedAt: wrk.FormatTime(packNow), PreviousHash: last.RecordHash, Kind: "import_seal",
		Payload: map[string]any{"begin_seq": beginSeq, "expected_import_count": int64(0), "manifest_sha256": wrk.ContentSHA256([]byte("[]")), "reason_code": "import_seal", "shadow_cutoff_seq": beginSeq - 1, "baseline_used": int64(3), "numbering_floor": int64(1)}}
	h, err := rec.ComputeHash()
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordHash = h
	mem.Seed(rec, nil)
}

// Director rule 11 (L125) and mutant "check expected_seq before
// successful-key replay": an identical replay returns 200 created=false with
// the original record even after end, before terminal and CAS checks.
func TestWrkReplayPrecedesCAS(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-y", "b2413-y", 2413, "T1")
	s.ok(s.post("desktop", "job-y", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	claim, tail := s.cas("job-y")
	body := map[string]any{"kind": "verdict", "claim_seq": claim, "expected_seq": tail, "payload": verdictPayload(1, "PASS", head('1'), "/jobs/y/v.md", nil, verdictBytes("PASS", head('1'), "y"))}
	key := s.key()
	first := s.ok(s.postBody("desktop", "job-y", key, body), "verdict")
	// The reply is lost; the journal moves on and the job ends.
	s.ok(s.post("desktop", "job-y", "end", map[string]any{"end_type": "reaped", "reason_code": "cleanup"}), "end")
	again := s.postBody("desktop", "job-y", key, body)
	if again.status != 200 || again.body["created"] != false {
		t.Fatalf("replay: %d %s", again.status, again.raw)
	}
	if again.body["record"].(map[string]any)["record_hash"] != first["record"].(map[string]any)["record_hash"] {
		t.Fatal("replay returned a different record")
	}
	if num(summaryOf(again)["snapshot_seq"]) != num(first["record"].(map[string]any)["seq"]) {
		t.Fatalf("replay summary is not the original snapshot: %v", summaryOf(again))
	}
	// Permission is rechecked on replay: another host's key namespace is
	// separate, and the replayed key cannot be used by mac-dev.
	if r := s.postBody("mac-dev", "job-y", key, body); r.body["error"] != "host_mismatch" {
		t.Fatalf("foreign replay: %s", r.raw)
	}
}

// Director rule 12 (L125, L4048): the same key with a changed body is
// idempotency_conflict, including a changed expected_seq only.
func TestWrkSameKeyChangedBody(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-i", "b2414-i", 2414, "T1")
	claim, tail := s.cas("job-i")
	key := s.key()
	body := map[string]any{"kind": "round_open", "claim_seq": claim, "expected_seq": tail, "payload": openPayload(head('1'), nil, nil, nil)}
	s.ok(s.postBody("desktop", "job-i", key, body), "open")
	changed := map[string]any{"kind": "round_open", "claim_seq": claim, "expected_seq": tail, "payload": openPayload(head('2'), nil, nil, nil)}
	s.refused(s.postBody("desktop", "job-i", key, changed), "idempotency_conflict", "changed head")
	shifted := map[string]any{"kind": "round_open", "claim_seq": claim, "expected_seq": tail + 1, "payload": openPayload(head('1'), nil, nil, nil)}
	s.refused(s.postBody("desktop", "job-i", key, shifted), "idempotency_conflict", "changed expected_seq")
	// A failed attempt reserves no key.
	failKey := s.key()
	s.refused(s.postBody("desktop", "job-i", failKey, map[string]any{"kind": "round_open", "claim_seq": claim, "expected_seq": 0, "payload": openPayload(head('3'), nil, nil, nil)}), "sequence_conflict", "stale")
	c2, t2 := s.cas("job-i")
	s.ok(s.postBody("desktop", "job-i", failKey, map[string]any{"kind": "round_open", "claim_seq": c2, "expected_seq": t2, "payload": openPayload(head('3'), nil, nil, nil)}), "same key after a refusal")
}

// Mutant "let a host write a builder claim".
func TestWrkHostCannotMintBuilderClaim(t *testing.T) {
	s := newScenario(t)
	for _, role := range []string{"builder", "worker"} {
		s.refused(s.post("desktop", "job-w-"+role, "claim", claimPayload("job-w-"+role, "b2415-w", "director-1", 2415, "desktop", role, "T1")), "scope_denied", role)
	}
	if len(s.records("job-w-builder")) != 0 || s.mem.TaskBinding(2415) != "" {
		t.Fatal("host claim left a record or a task binding")
	}
}

// Mutant "accept a verdict whose head differs from its open".
func TestWrkVerdictHeadMustEqualOpen(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-g", "b2416-g", 2416, "T1")
	s.ok(s.post("desktop", "job-g", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	s.refused(s.post("desktop", "job-g", "verdict", verdictPayload(1, "PASS", head('2'), "/jobs/g/v.md", nil, verdictBytes("PASS", head('2'), "g"))), "head_mismatch", "head differs from open")
	s.refused(s.post("desktop", "job-g", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/g/v.md", nil, verdictBytes("PASS", head('2'), "g"))), "head_mismatch", "header differs from asserted head")
	s.refused(s.post("desktop", "job-g", "verdict", verdictPayload(1, "BLOCKER", head('1'), "/jobs/g/v.md", nil, verdictBytes("PASS", head('1'), "g"))), "invalid_request", "verdict word mismatch")
	if sum := summaryOf(s.do("desktop", "GET", "/v1/wrk/jobs/job-g", nil, nil)); fmt.Sprint(sum["open_rounds"]) != "[1]" {
		t.Fatalf("open not pending after refusals: %v", sum)
	}
}

// Mutant "let a void return a consumed grant's capacity".
func TestWrkVoidNeverRefundsGrant(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-u", "b2417-u", 2417, "T1")
	for _, h := range []byte{'1', '2', '3'} {
		s.ok(s.post("desktop", "job-u", "round_open", openPayload(head(h), nil, nil, nil)), "open")
	}
	g := s.ok(s.post("director-1", "job-u", "grant", grantPayload("extension", 4, head('4'), 1, nil, nil)), "extension")
	ext := num(g["record"].(map[string]any)["seq"])
	s.ok(s.post("desktop", "job-u", "round_open", openPayload(head('4'), nil, ext, nil)), "round 4")
	vg := s.ok(s.post("director-1", "job-u", "grant", grantPayload("void", 4, nil, 0, nil, nil)), "void grant")
	v := s.ok(s.post("desktop", "job-u", "void", map[string]any{"round_no": 4, "grant_seq": num(vg["record"].(map[string]any)["seq"]), "reason_code": "aborted"}), "void")
	if num(summaryOf(v)["used"]) != 3 {
		t.Fatalf("used after void %v", summaryOf(v))
	}
	s.refused(s.post("desktop", "job-u", "round_open", openPayload(head('5'), nil, ext, nil)), "grant_consumed", "voided round's extension")
	s.refused(s.post("desktop", "job-u", "round_open", openPayload(head('5'), nil, nil, nil)), "round_cap", "no fresh extension")
}

// Mutant "accept a joined with a pending open round".
func TestWrkJoinRefusesPendingRound(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-j", "b2418-j", 2418, "T1")
	s.ok(s.post("desktop", "job-j", "round_open", openPayload(head('1'), nil, nil, nil)), "open 1")
	s.ok(s.post("desktop", "job-j", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/j/v1.md", nil, verdictBytes("PASS", head('1'), "j"))), "pass")
	s.ok(s.post("desktop", "job-j", "round_open", openPayload(head('2'), nil, nil, nil)), "open 2 pending")
	s.refused(s.post("desktop", "job-j", "joined", reportPayload(head('1'), nil)), "review_incomplete", "pending round")
	s.ok(s.post("desktop", "job-j", "verdict", verdictPayload(2, "BLOCKER", head('2'), "/jobs/j/v2.md", nil, verdictBytes("BLOCKER", head('2'), "j"))), "blocker")
	s.refused(s.post("desktop", "job-j", "joined", reportPayload(head('1'), nil)), "blocker_unresolved", "later blocker")
	s.refused(s.post("desktop", "job-j", "joined", reportPayload(head('2'), nil)), "blocker_unresolved", "blocker head")
}

// Guard vector (design R4 row 18, Q10 S-c): a generated token-shaped string
// in a verdict is invalid_request; nothing is stored or echoed and the
// attempt keeps only allowed metadata.
func TestWrkGuardRejectsTokenShapedVerdict(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-k", "b2419-k", 2419, "T1")
	s.ok(s.post("desktop", "job-k", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	secret := syntheticSecret(t)
	content := append(verdictBytes("PASS", head('1'), "notes"), secret...)
	before := len(s.records("job-k"))
	r := s.post("desktop", "job-k", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/k/v.md", nil, content))
	s.refused(r, "invalid_request", "guard")
	if strings.Contains(string(r.raw), strings.TrimSpace(string(secret))) || string(r.raw) != `{"error":"invalid_request"}`+"\n" {
		t.Fatalf("response echoes content: %q", r.raw)
	}
	if len(s.records("job-k")) != before {
		t.Fatal("refused verdict was stored")
	}
	atts := s.mem.Attempts()
	last := atts[len(atts)-1]
	if last.Code != wrk.InvalidRequest || last.JobID != "job-k" || last.Kind != "verdict" || last.ClientID != "desktop" {
		t.Fatalf("attempt %+v", last)
	}
	raw, _ := json.Marshal(atts)
	if strings.Contains(string(raw), strings.TrimSpace(string(secret))) {
		t.Fatal("attempt carries content")
	}
}

// successor appends a central successor claim for pred on the same task,
// host and lanes.
func (s *scenario) successor(job, pred, lane string, task int64, tier string) packResponse {
	p := claimPayload(job, lane, "director-1", task, "desktop", "builder", tier)
	p["predecessor_job_id"] = pred
	return s.post("director-1", job, "claim", p)
}

// Tester round 1 finding 1 (design §2/§4 successors): a lineage takes any
// number of non-branching successors; each inherits the whole budget.
func TestWrkSuccessorChain(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-c1", "b2400-c", 2400, "T1")
	s.ok(s.post("desktop", "job-c1", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	s.ok(s.post("director-1", "job-c1", "end", map[string]any{"end_type": "handed_off", "reason_code": "takeover"}), "end A")
	s.ok(s.successor("job-c2", "job-c1", "b2400-c", 2400, "T1"), "successor B")
	b := s.ok(s.post("director-1", "job-c2", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate B")
	if num(summaryOf(b)["used"]) != 1 || num(summaryOf(b)["next_round_no"]) != 2 {
		t.Fatalf("B summary %v", summaryOf(b))
	}
	s.ok(s.post("director-1", "job-c2", "end", map[string]any{"end_type": "handed_off", "reason_code": "takeover"}), "end B")
	s.ok(s.successor("job-c3", "job-c2", "b2400-c", 2400, "T1"), "second successor C")
	c := s.ok(s.post("director-1", "job-c3", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate C")
	if num(summaryOf(c)["used"]) != 1 || num(summaryOf(c)["next_round_no"]) != 2 {
		t.Fatalf("C summary %v", summaryOf(c))
	}
	if s.mem.TaskBinding(2400) != "job-c3" {
		t.Fatalf("task bound to %q", s.mem.TaskBinding(2400))
	}
	s.refused(s.successor("job-c4", "job-c2", "b2400-c", 2400, "T1"), "claim_conflict", "branching successor")
	s.refused(s.post("director-1", "job-c5", "claim", claimPayload("job-c5", "b2400-c", "director-1", 2400, "desktop", "builder", "T1")), "claim_conflict", "competing root claim on the task")
}

// Tester round 1 finding 2 (design §2/§4, esc-2 #18): a successor keeps the
// inherited head identity and the explicit head_repeat route for it.
func TestWrkSuccessorRepeatGrantOnInheritedHead(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-ia", "b2401-i", 2401, "T1")
	s.ok(s.post("desktop", "job-ia", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	s.ok(s.post("desktop", "job-ia", "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/ia/v.md", nil, verdictBytes("PASS", head('1'), "a"))), "pass")
	s.ok(s.post("director-1", "job-ia", "end", map[string]any{"end_type": "handed_off", "reason_code": "takeover"}), "end A")
	s.ok(s.successor("job-ib", "job-ia", "b2401-i", 2401, "T1"), "successor")
	s.ok(s.post("director-1", "job-ib", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate")
	s.ok(s.post("desktop", "job-ib-t2", "claim", reviewPayload("job-ib-t2", "b2401-i", 2401, "desktop", "reviewer", "T1", "job-ib", 2)), "mint")
	s.refused(s.post("desktop", "job-ib", "round_open", openPayload(head('1'), "job-ib-t2", nil, nil)), "duplicate_head", "inherited head")
	g := s.ok(s.post("director-1", "job-ib", "grant", grantPayload("head_repeat", 2, head('1'), 0, 1, "job-ib-t2")), "repeat grant on inherited round 1")
	o := s.ok(s.post("desktop", "job-ib", "round_open", openPayload(head('1'), "job-ib-t2", nil, num(g["record"].(map[string]any)["seq"]))), "repeat open")
	if num(o["record"].(map[string]any)["payload"].(map[string]any)["round_no"]) != 2 {
		t.Fatalf("round %v", o["record"])
	}
	s.refused(s.post("desktop", "job-ib", "verdict", verdictPayload(2, "PASS", head('1'), "/jobs/ia/v.md", "job-ib-t2", verdictBytes("PASS", head('1'), "b"))), "duplicate_path", "inherited path identity")
}

// Tester round 1 finding 4 (design §3/§5 attenuation): central identity is
// the fixed role, and each operation checks its own permission.
func TestWrkAttenuatedCentralKeepsOtherPermissions(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-at", "b2402-a", 2402, "T1")
	var doc map[string]any
	_ = json.Unmarshal(standardPolicy(t), &doc)
	d := doc["clients"].(map[string]any)["director-1"].(map[string]any)
	var kept []any
	for _, p := range d["permissions"].([]any) {
		if p != "claim.write" {
			kept = append(kept, p)
		}
	}
	d["permissions"] = kept
	raw, _ := json.Marshal(doc)
	policy, err := wrk.ParsePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.server.Wrk.Policy, s.server.Wrk.Engine.Policy = policy, policy
	s.ok(s.post("director-1", "job-at", "grant", grantPayload("extension", 1, head('2'), 1, nil, nil)), "grant.write kept")
	s.ok(s.post("director-1", "job-at", "end", map[string]any{"end_type": "revoked", "reason_code": "attenuated"}), "end.write kept")
	s.refused(s.post("director-1", "job-at2", "claim", claimPayload("job-at2", "b2403-a", "director-1", 2403, "desktop", "builder", "T1")), "scope_denied", "claim.write removed")
}

// Tester round 1 finding 3 (RFC 8785 §3.2.2.2): an invalid Unicode escape is
// refused, never repaired into a different string.
func TestWrkLoneSurrogateRefused(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-ls", "b2403-l", 2403, "T1")
	s.ok(s.post("desktop", "job-ls", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	claim, tail := s.cas("job-ls")
	content := verdictBytes("PASS", head('1'), "x")
	body, _ := json.Marshal(map[string]any{"kind": "verdict", "claim_seq": claim, "expected_seq": tail, "payload": verdictPayload(1, "PASS", head('1'), "/PLACEHOLDER", nil, content)})
	raw := strings.Replace(string(body), "/PLACEHOLDER", `/\ud800`, 1)
	r := s.do("desktop", "POST", "/v1/wrk/jobs/job-ls/records", map[string]string{"Idempotency-Key": s.key()}, []byte(raw))
	s.refused(r, "invalid_request", "lone surrogate in path")
	if int64(len(s.records("job-ls"))) != tail {
		t.Fatal("lone-surrogate verdict was stored")
	}
}

// importLegacyRound imports one generated legacy job.round numbered n into
// an open import and returns its manifest entry.
func (s *scenario) importLegacyRound(job string, n int64, h string) map[string]any {
	s.t.Helper()
	b, err := json.Marshal(map[string]any{"epoch": 1, "head": h, "job_id": "legacy-" + job, "kind": "job.round", "label": "legacy", "owner_lane": "legacy", "parent_lane": "director-1", "round": n})
	if err != nil {
		s.t.Fatal(err)
	}
	loc := fmt.Sprintf("generated/%s-round-%d.json", job, n)
	r := s.ok(s.post("director-1", job, "import", map[string]any{"source_locator": loc, "source_sha256": wrk.ContentSHA256(b), "original_actor_client_id": "desktop", "original_at": "2026-10-20T00:00:00Z", "legacy_kind": "job.round", "content_b64": base64.StdEncoding.EncodeToString(b)}), "import round")
	return map[string]any{"seq": int64(num(r["record"].(map[string]any)["seq"])), "source_locator": loc, "source_sha256": wrk.ContentSHA256(b)}
}

func endHandedOff(s *scenario, job string) {
	s.t.Helper()
	s.ok(s.post("director-1", job, "end", map[string]any{"end_type": "handed_off", "reason_code": "takeover"}), "end "+job)
}

// Tester round 2 finding B1 (design §2/§4/§5): a successor of an ended,
// never-activated predecessor inherits its committed seal baseline and
// numbering floor, or the conservative budget of its unsealed work; never a
// fresh zero budget or recycled numbering.
func TestWrkSuccessorOfSealedPredecessorKeepsBudget(t *testing.T) {
	s := newScenario(t)
	s.ok(s.post("director-1", "job-sa", "claim", claimPayload("job-sa", "b2420-s", "director-1", 2420, "desktop", "builder", "T1")), "claim A")
	begin := s.ok(s.post("director-1", "job-sa", "import_begin", map[string]any{"reason_code": "import_freeze"}), "begin")
	m := s.importLegacyRound("job-sa", 9, head('9'))
	seal := s.ok(s.post("director-1", "job-sa", "import_seal", map[string]any{"begin_seq": num(begin["record"].(map[string]any)["seq"]), "expected_import_count": 1, "manifest_sha256": wrk.ContentSHA256(wrk.MustCanonical([]any{m})), "reason_code": "import_seal"}), "seal")
	sp := seal["record"].(map[string]any)["payload"].(map[string]any)
	if num(sp["baseline_used"]) != 3 || num(sp["numbering_floor"]) != 9 {
		t.Fatalf("seal %v", sp)
	}
	endHandedOff(s, "job-sa")
	s.ok(s.successor("job-sb", "job-sa", "b2420-s", 2420, "T1"), "successor B")
	b := s.ok(s.post("director-1", "job-sb", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate B")
	if num(summaryOf(b)["used"]) != 3 || num(summaryOf(b)["next_round_no"]) != 10 {
		t.Fatalf("B summary %v", summaryOf(b))
	}
	s.refused(s.post("desktop", "job-sb", "round_open", openPayload(head('1'), nil, nil, nil)), "round_cap", "ordinary open over the inherited budget")
	g := s.ok(s.post("director-1", "job-sb", "grant", grantPayload("extension", 10, head('1'), 1, nil, nil)), "extension")
	o := s.ok(s.post("desktop", "job-sb", "round_open", openPayload(head('1'), nil, num(g["record"].(map[string]any)["seq"]), nil)), "round 10")
	if num(o["record"].(map[string]any)["payload"].(map[string]any)["round_no"]) != 10 {
		t.Fatalf("round %v", o["record"])
	}

	// Unsealed shadow work: the successor reserves the migration baseline.
	w := newScenario(t)
	w.ok(w.post("director-1", "job-wa", "claim", claimPayload("job-wa", "b2421-w", "director-1", 2421, "desktop", "builder", "T1")), "claim A")
	w.ok(w.post("desktop", "job-wa", "round_open", openPayload(head('1'), nil, nil, nil)), "shadow open")
	endHandedOff(w, "job-wa")
	w.ok(w.successor("job-wb", "job-wa", "b2421-w", 2421, "T1"), "successor B")
	wb := w.ok(w.post("director-1", "job-wb", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate B")
	if num(summaryOf(wb)["used"]) != 3 || num(summaryOf(wb)["next_round_no"]) != 4 {
		t.Fatalf("shadow successor summary %v", summaryOf(wb))
	}
	w.refused(w.post("desktop", "job-wb", "round_open", openPayload(head('2'), nil, nil, nil)), "round_cap", "ordinary open after shadow work")
}

// Tester round 2 finding B2 (design §2/§4): a predecessor's nonvoid
// post-activation open keeps its head reserved across end and successor;
// only the head_repeat route reuses it.
func TestWrkSuccessorKeepsPendingHeadReservation(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-pa", "b2422-p", 2422, "T1")
	s.ok(s.post("desktop", "job-pa", "round_open", openPayload(head('1'), nil, nil, nil)), "pending open")
	endHandedOff(s, "job-pa")
	s.ok(s.successor("job-pb", "job-pa", "b2422-p", 2422, "T1"), "successor")
	s.ok(s.post("director-1", "job-pb", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate")
	s.refused(s.post("desktop", "job-pb", "round_open", openPayload(head('1'), nil, nil, nil)), "duplicate_head", "inherited pending head")
	s.ok(s.post("desktop", "job-pb-t2", "claim", reviewPayload("job-pb-t2", "b2422-p", 2422, "desktop", "reviewer", "T1", "job-pb", 2)), "mint")
	g := s.ok(s.post("director-1", "job-pb", "grant", grantPayload("head_repeat", 2, head('1'), 0, 1, "job-pb-t2")), "repeat grant")
	o := s.ok(s.post("desktop", "job-pb", "round_open", openPayload(head('1'), "job-pb-t2", nil, num(g["record"].(map[string]any)["seq"]))), "repeat open")
	if num(o["record"].(map[string]any)["payload"].(map[string]any)["round_no"]) != 2 {
		t.Fatalf("round %v", o["record"])
	}
}

// Tester round 2 finding B3 (design §3/§4): a successor's claim correction
// cannot change the lineage's task.
func TestWrkSuccessorCorrectionCannotChangeTask(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-ka", "b2423-k", 2423, "T1")
	endHandedOff(s, "job-ka")
	s.ok(s.successor("job-kb", "job-ka", "b2423-k", 2423, "T1"), "successor")
	corr := claimPayload("job-kb", "b2423-k", "director-1", 2424, "desktop", "builder", "T1")
	corr["predecessor_job_id"] = "job-ka"
	corr["supersedes"] = 1
	s.refused(s.post("director-1", "job-kb", "claim", corr), "claim_conflict", "correction changes the lineage task")
	if s.mem.TaskBinding(2423) != "job-kb" || s.mem.TaskBinding(2424) != "" {
		t.Fatalf("bindings %q %q", s.mem.TaskBinding(2423), s.mem.TaskBinding(2424))
	}
}

// Tester round 2 finding B4 (design §2/§4): a lineage has no length limit;
// a long non-branching chain keeps the whole budget.
func TestWrkLongSuccessorChain(t *testing.T) {
	s := newScenario(t)
	s.activeBuilder("job-l0", "b2425-l", 2425, "T1")
	s.ok(s.post("desktop", "job-l0", "round_open", openPayload(head('1'), nil, nil, nil)), "open")
	prev := "job-l0"
	for i := 1; i <= 70; i++ {
		job := fmt.Sprintf("job-l%d", i)
		endHandedOff(s, prev)
		s.ok(s.successor(job, prev, "b2425-l", 2425, "T1"), "successor "+job)
		a := s.ok(s.post("director-1", job, "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate "+job)
		if num(summaryOf(a)["used"]) != 1 || num(summaryOf(a)["next_round_no"]) != 2 {
			t.Fatalf("%s summary %v", job, summaryOf(a))
		}
		prev = job
	}
	if s.mem.TaskBinding(2425) != prev {
		t.Fatalf("task bound to %q", s.mem.TaskBinding(2425))
	}
}

// A broken seal anywhere in the lineage stays an integrity error for the
// successor: it fails closed instead of inheriting a clamped budget.
func TestWrkSuccessorOfBrokenSealFailsClosed(t *testing.T) {
	s := newScenario(t)
	s.ok(s.post("director-1", "job-xa", "claim", claimPayload("job-xa", "b2426-x", "director-1", 2426, "desktop", "builder", "T1")), "claim A")
	s.ok(s.post("desktop", "job-xa", "round_open", openPayload(head('1'), nil, nil, nil)), "shadow open")
	bb := s.ok(s.post("director-1", "job-xa", "import_begin", map[string]any{"reason_code": "import_freeze"}), "begin")
	seedBrokenSeal(t, s.mem, "job-xa", int64(num(bb["record"].(map[string]any)["seq"])))
	endHandedOff(s, "job-xa")
	s.ok(s.successor("job-xb", "job-xa", "b2426-x", 2426, "T1"), "successor claim")
	s.refused(s.post("director-1", "job-xb", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "import_unsealed", "activate over a broken lineage seal")
	s.ok(s.post("director-1", "job-xb", "end", map[string]any{"end_type": "revoked", "reason_code": "broken_seal"}), "end stays allowed")
}
