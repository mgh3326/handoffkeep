package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// testRuntimeRoleAppendOnly: the runtime role really lacks UPDATE, DELETE
// and TRUNCATE on the journal, and even the owner meets the append-only
// triggers (design §2 L45).
func testRuntimeRoleAppendOnly(t *testing.T, fx *pgFixture) {
	s := pgScenario(t, fx)
	s.activeBuilder("job-ro", "b2400-ro", 2400, "T1")
	s.ok(s.post("desktop", "job-ro", "completed", map[string]any{"report_b64": reportPayload(head('1'), nil)["report_b64"], "report_sha256": wrk.ContentSHA256(packReportBytes)}), "completed")
	ctx := context.Background()
	rt, err := pgx.Connect(ctx, fx.runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(ctx)
	for _, q := range []string{
		`UPDATE wrk_records SET kind=kind`, `DELETE FROM wrk_records`, `TRUNCATE wrk_records CASCADE`,
		`UPDATE wrk_contents SET size_bytes=size_bytes`, `DELETE FROM wrk_contents`, `TRUNCATE wrk_contents`,
		`UPDATE wrk_attempts SET kind=kind`, `DELETE FROM wrk_attempts`, `TRUNCATE wrk_attempts`,
	} {
		_, err := rt.Exec(ctx, q)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("runtime role %q: %v, want insufficient_privilege", q, err)
		}
	}
	if err := fx.st.CheckWrkRuntimeRole(ctx); err != nil {
		t.Fatalf("runtime role check refused the fixture role: %v", err)
	}
	if _, err := fx.admin.Exec(ctx, `UPDATE wrk_records SET kind=kind`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("owner update: %v, want the append-only trigger", err)
	}
	owner, err := store.Open(ctx, fx.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.CheckWrkRuntimeRole(ctx); err == nil {
		t.Fatal("the owner (superuser) role passed the runtime role check")
	}
}

// testLegacyFenceRows walks every route of the §5 fence table and the §6a
// handoffkeep rows: refused for a protected task, unchanged for an
// unrelated one, through HTTP, the assistant route and the store directly.
func testLegacyFenceRows(t *testing.T, fx *pgFixture) {
	s := pgScenario(t, fx)
	s.activeBuilder("job-fp", "b2400-fp", 2400, "T1")
	ctx := context.Background()
	refused := func(r packResponse, what string) {
		t.Helper()
		if r.body["error"] != "wrk_receipt_required" {
			t.Errorf("%s: %d %s, want wrk_receipt_required", what, r.status, r.raw)
		}
	}
	allowed := func(r packResponse, what string) {
		t.Helper()
		if r.status >= 300 || r.body["error"] == "wrk_receipt_required" || r.body["error"] == "scope_denied" {
			t.Errorf("unrelated %s: %d %s", what, r.status, r.raw)
		}
	}
	notFenced := func(r packResponse, what string) {
		t.Helper()
		if r.body["error"] == "wrk_receipt_required" || r.body["error"] == "scope_denied" {
			t.Errorf("unrelated %s was fenced: %s", what, r.raw)
		}
	}
	create := func(refs map[string]any) packResponse {
		return s.legacyPost("desktop", "/v1/tasks", map[string]any{"lane": "o3-lane", "title": "x", "kind": "implement", "project": "handoffkeep", "refs": refs})
	}
	refused(create(map[string]any{"job_id": "job-fp"}), "create naming an O3 job")
	allowed(create(map[string]any{"job_id": "legacy-job"}), "create")

	refused(s.legacyPost("desktop", "/v1/tasks/2400/claim", map[string]any{"claimed_by": "pane", "job_id": "job-fp"}), "claim")
	refused(s.legacyPost("director-1", "/v1/tasks/2400/claim", map[string]any{"claimed_by": "director-1", "no_job": "central"}), "central no_job claim")
	allowed(s.legacyPost("desktop", "/v1/tasks/2401/claim", map[string]any{"claimed_by": "pane", "job_id": "legacy-1"}), "claim")
	refused(s.legacyPost("desktop", "/v1/tasks/2402/claim", map[string]any{"claimed_by": "pane", "job_id": "job-fp"}), "claim binding an O3 job")

	refused(s.legacyPost("desktop", "/v1/tasks/2400/rebind", map[string]any{"from_job": "job-fp", "to_job": "job-z", "by": "pane", "reason": "takeover"}), "rebind")
	allowed(s.legacyPost("desktop", "/v1/tasks/2401/rebind", map[string]any{"from_job": "legacy-1", "to_job": "legacy-2", "by": "pane", "reason": "takeover"}), "rebind")

	// 2400 is protected and first in the lane; next skips it.
	next := s.legacyPost("desktop", "/v1/tasks/next", map[string]any{"lane": "o3-lane", "claimed_by": "pane", "job_id": "legacy-3"})
	if next.status != 200 || num(next.body["id"]) == 2400 {
		t.Errorf("next handed out the protected task: %d %s", next.status, next.raw)
	}
	refused(s.legacyPost("desktop", "/v1/tasks/next", map[string]any{"lane": "o3-lane", "claimed_by": "pane", "job_id": "job-fp"}), "next naming an O3 job")

	refused(s.legacyPost("desktop", "/v1/tasks/2400/transition", map[string]any{"to": "claimed", "refs": map[string]any{"pr": "https://pr.invalid/x"}, "no_job": "x"}), "transition with refs")
	refused(s.legacyPost("director-1", "/v1/tasks/2400/transition", map[string]any{"to": "hold"}), "central state-only without receipt")
	allowed(s.legacyPost("desktop", "/v1/tasks/2401/transition", map[string]any{"to": "in_progress"}), "transition")

	if item := batchItem(s.legacyPost("desktop", "/v1/tasks/relane", map[string]any{"ids": []int64{2400}, "to": "other", "note": "move", "allow_new_lane": true})); item.body["error"] != "wrk_receipt_required" {
		t.Errorf("relane: %v", item.body)
	}
	allowed(batchItem(s.legacyPost("desktop", "/v1/tasks/relane", map[string]any{"ids": []int64{2401}, "to": "other", "note": "move", "allow_new_lane": true})), "relane")

	refused(s.legacyPost("desktop", "/v1/tasks/2400/project", map[string]any{"project": "wrk", "note": "n"}), "project")
	allowed(s.legacyPost("desktop", "/v1/tasks/2401/project", map[string]any{"project": "wrk", "note": "n"}), "project")

	refused(s.legacyPost("desktop", "/v1/tasks/2400/decision-request/resolve", map[string]any{"request_id": "dr-2400-1", "kind": "answered", "option": "A"}), "decision-request resolve")
	notFenced(s.legacyPost("desktop", "/v1/tasks/2401/decision-request/resolve", map[string]any{"request_id": "dr-2401-1", "kind": "answered", "option": "A"}), "decision-request resolve")
	refused(s.legacyPost("desktop", "/v1/assistant/decisions/resolve", map[string]any{"request_id": "dr-2400-1", "option": "A"}), "assistant resolve")
	notFenced(s.legacyPost("desktop", "/v1/assistant/decisions/resolve", map[string]any{"request_id": "dr-2401-1", "option": "A"}), "assistant resolve")

	refused(s.legacyPost("desktop", "/v1/tasks/dispositions/2400/apply", map[string]any{"note": "apply"}), "disposition apply")
	notFenced(s.legacyPost("desktop", "/v1/tasks/dispositions/2401/apply", map[string]any{"note": "apply"}), "disposition apply")

	fx.setTaskState(2400, "needs_decision")
	fx.setTaskState(2403, "needs_decision")
	before := relayCount(t, fx)
	refused(s.legacyPost("desktop", "/v1/decisions/resolve", map[string]any{"type": "task", "id": 2400, "by": "pane", "answer": "a", "no_job": "x"}), "decisions resolve")
	if relayCount(t, fx) != before {
		t.Error("decisions resolve wrote a lane event for a protected task")
	}
	allowed(s.legacyPost("desktop", "/v1/decisions/resolve", map[string]any{"type": "task", "id": 2403, "by": "pane", "answer": "a", "no_job": "x"}), "decisions resolve")

	// The store itself, with no bearer principal (operator UI, MCP or any
	// direct caller): protected writes are scope_denied or receipt-refused.
	if _, err := fx.st.TransitionTask(ctx, 2400, "hold", "operator:x", "n", nil, ""); !errors.Is(err, store.ErrWrkScopeDenied) {
		t.Errorf("principal-less store transition: %v", err)
	}
	if _, err := fx.st.ClaimTask(ctx, 2400, "operator:x", "", "no job"); !errors.Is(err, store.ErrWrkReceiptRequired) {
		t.Errorf("store claim: %v", err)
	}
	if protected, err := fx.st.WrkTaskProtected(ctx, 2400); err != nil || !protected {
		t.Errorf("WrkTaskProtected(2400) = %v %v", protected, err)
	}
	if protected, _ := fx.st.WrkTaskProtected(ctx, 2401); protected {
		t.Error("unrelated task reported protected")
	}
	// Task protection starts at activate (dr-1925-1): a shadow subject's
	// task keeps legacy behaviour.
	s.ok(s.post("director-1", "job-sh", "claim", claimPayload("job-sh", "b2404-sh", "director-1", 2404, "desktop", "builder", "T1")), "shadow claim")
	if protected, _ := fx.st.WrkTaskProtected(ctx, 2404); protected != !store.WrkProtectAtActivate {
		t.Errorf("shadow task protected=%v", protected)
	}
}

func relayCount(t *testing.T, fx *pgFixture) int {
	var n int
	if err := fx.admin.QueryRow(context.Background(), `SELECT count(*) FROM relay_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// testConcurrentAppends: two appends racing on one job lock with the same
// CAS commit exactly one; two grant consumers pick one winner.
func testConcurrentAppends(t *testing.T, fx *pgFixture) {
	s := pgScenario(t, fx)
	s.activeBuilder("job-cc", "b2400-cc", 2400, "T1")
	claim, tail := s.cas("job-cc")
	race := func(payloads ...map[string]any) []packResponse {
		out := make([]packResponse, len(payloads))
		var wg sync.WaitGroup
		for i, p := range payloads {
			wg.Add(1)
			go func(i int, p map[string]any) {
				defer wg.Done()
				out[i] = s.postBody("desktop", "job-cc", fmt.Sprintf("00000000-0000-4000-a000-%012d", s.keys*10+i+1), map[string]any{"kind": "round_open", "claim_seq": claim, "expected_seq": tail, "payload": p})
			}(i, p)
		}
		wg.Wait()
		s.keys++
		return out
	}
	rs := race(openPayload(head('1'), nil, nil, nil), openPayload(head('2'), nil, nil, nil))
	if wins(rs) != 1 {
		t.Fatalf("two racing opens: %v", statuses(rs))
	}
	for _, h := range []byte{'3', '4'} {
		s.ok(s.post("desktop", "job-cc", "round_open", openPayload(head(h), nil, nil, nil)), "open")
	}
	g := s.ok(s.post("director-1", "job-cc", "grant", grantPayload("extension", 4, head('5'), 1, nil, nil)), "extension")
	gseq := num(g["record"].(map[string]any)["seq"])
	claim, tail = s.cas("job-cc")
	rs = race(openPayload(head('5'), nil, gseq, nil), openPayload(head('5'), nil, gseq, nil))
	if wins(rs) != 1 {
		t.Fatalf("two grant consumers: %v", statuses(rs))
	}
	s.refused(s.post("desktop", "job-cc", "round_open", openPayload(head('6'), nil, gseq, nil)), "grant_consumed", "refreshed loser")
}

func wins(rs []packResponse) int {
	n := 0
	for _, r := range rs {
		if r.status == 201 {
			n++
		} else if r.body["error"] != "sequence_conflict" {
			return -1
		}
	}
	return n
}

func statuses(rs []packResponse) []string {
	var out []string
	for _, r := range rs {
		out = append(out, fmt.Sprintf("%d %s", r.status, r.raw))
	}
	return out
}

// testEndRacesVerdict: an end racing a verdict serialises on the job lock;
// whichever commits second sees the first.
func testEndRacesVerdict(t *testing.T, fx *pgFixture) {
	for i := 0; i < 5; i++ {
		s := pgScenario(t, fx)
		job := fmt.Sprintf("job-ev%d", i)
		s.activeBuilder(job, "b2400-ev", 2400, "T1")
		s.ok(s.post("desktop", job, "round_open", openPayload(head('1'), nil, nil, nil)), "open")
		claim, tail := s.cas(job)
		var wg sync.WaitGroup
		var verdict, end packResponse
		wg.Add(2)
		go func() {
			defer wg.Done()
			verdict = s.postBody("desktop", job, wrk.NewUUID(), map[string]any{"kind": "verdict", "claim_seq": claim, "expected_seq": tail, "payload": verdictPayload(1, "PASS", head('1'), "/jobs/ev/v.md", nil, verdictBytes("PASS", head('1'), "ev"))})
		}()
		go func() {
			defer wg.Done()
			end = s.postBody("director-1", job, wrk.NewUUID(), map[string]any{"kind": "end", "claim_seq": claim, "expected_seq": tail, "payload": map[string]any{"end_type": "revoked", "reason_code": "race"}})
		}()
		wg.Wait()
		if wins([]packResponse{verdict, end}) != 1 {
			t.Fatalf("end vs verdict: %v", statuses([]packResponse{verdict, end}))
		}
		recs := s.records(job)
		if last := recs[len(recs)-1]; verdict.status == 201 && last.Kind != "verdict" || end.status == 201 && last.Kind != "end" {
			t.Fatalf("journal tail %s does not match the winner", last.Kind)
		}
	}
}

// testCrossJobLockOrder: a reviewer end and a target verdict naming that
// reviewer lock both jobs in lexicographic order; repeated races neither
// deadlock nor accept a verdict from an ended assignment.
func testCrossJobLockOrder(t *testing.T, fx *pgFixture) {
	for i := 0; i < 5; i++ {
		s := pgScenario(t, fx)
		job := fmt.Sprintf("job-lk%d", i)
		tester := "a-" + job + "-t"
		s.activeBuilder(job, "b2400-lk", 2400, "T2")
		s.ok(s.post("desktop", tester, "claim", reviewPayload(tester, "b2400-lk", 2400, "desktop", "tester", "T2", job, 1)), "mint")
		s.ok(s.post("desktop", job, "round_open", openPayload(head('1'), tester, nil, nil)), "open")
		var wg sync.WaitGroup
		var verdict, end packResponse
		wg.Add(2)
		go func() {
			defer wg.Done()
			verdict = s.post("desktop", job, "verdict", verdictPayload(1, "PASS", head('1'), "/jobs/lk/v.md", tester, verdictBytes("PASS", head('1'), "lk")))
		}()
		go func() {
			defer wg.Done()
			end = s.post("desktop", tester, "end", map[string]any{"end_type": "reaped", "reason_code": "race"})
		}()
		wg.Wait()
		if end.status != 201 {
			t.Fatalf("reviewer end: %s", end.raw)
		}
		if verdict.status != 201 && verdict.body["error"] != "round_not_open" {
			t.Fatalf("verdict: %s", verdict.raw)
		}
		if verdict.status == 201 {
			// It committed first; the reviewer's later end does not undo it.
			s.ok(s.post("desktop", job, "joined", reportPayload(head('1'), nil)), "joined after reviewer end")
		}
	}
}

// testLostReplyReplayPG: a crash between commit and reply is repaired by
// the exact-key replay returning the original committed record.
func testLostReplyReplayPG(t *testing.T, fx *pgFixture) {
	s := pgScenario(t, fx)
	s.activeBuilder("job-lr", "b2400-lr", 2400, "T1")
	claim, tail := s.cas("job-lr")
	key := wrk.NewUUID()
	body := map[string]any{"kind": "round_open", "claim_seq": claim, "expected_seq": tail, "payload": openPayload(head('1'), nil, nil, nil)}
	first := s.ok(s.postBody("desktop", "job-lr", key, body), "commit")
	s.ok(s.post("desktop", "job-lr", "round_open", openPayload(head('2'), nil, nil, nil)), "later write")
	again := s.postBody("desktop", "job-lr", key, body)
	if again.status != 200 || again.body["record"].(map[string]any)["record_id"] != first["record"].(map[string]any)["record_id"] {
		t.Fatalf("replay: %d %s", again.status, again.raw)
	}
	if n := fx.recordCount("job-lr"); n != 4 {
		t.Fatalf("journal has %d records, want 4 (no duplicate charge)", n)
	}
}

// testShadowProtectionStartsAtActivate (director-1 ruling on the legacy
// fence JobID branch, dr-1925-1): the fence predicate is receipt_required.
// A shadow builder claim leaves its task legacy-compatible; from activate
// the same legacy calls are refused; a review-role claim is fenced at once.
func testShadowProtectionStartsAtActivate(t *testing.T, fx *pgFixture) {
	s := pgScenario(t, fx)
	s.ok(s.post("director-1", "job-shp", "claim", claimPayload("job-shp", "b2405-shp", "director-1", 2405, "desktop", "builder", "T1")), "shadow claim")
	allowed := func(r packResponse, what string) {
		t.Helper()
		if r.status >= 300 {
			t.Fatalf("shadow %s: %d %s", what, r.status, r.raw)
		}
	}
	refused := func(r packResponse, what string) {
		t.Helper()
		if r.body["error"] != "wrk_receipt_required" {
			t.Fatalf("activated %s: %d %s, want wrk_receipt_required", what, r.status, r.raw)
		}
	}
	allowed(s.legacyPost("desktop", "/v1/tasks/2405/claim", map[string]any{"claimed_by": "pane", "job_id": "job-shp"}), "claim with the shadow job id")
	allowed(s.legacyPost("desktop", "/v1/tasks/2405/transition", map[string]any{"to": "in_progress"}), "transition")
	allowed(s.legacyPost("desktop", "/v1/tasks/next", map[string]any{"lane": "o3-lane", "claimed_by": "pane", "job_id": "job-shp"}), "next naming the shadow job")
	s.ok(s.post("director-1", "job-shp", "activate", map[string]any{"reason_code": "activation", "seal_seq": nil}), "activate")
	refused(s.legacyPost("desktop", "/v1/tasks/2405/transition", map[string]any{"to": "verifying"}), "transition")
	refused(s.legacyPost("desktop", "/v1/tasks/2405/rebind", map[string]any{"from_job": "job-shp", "to_job": "job-z", "by": "pane", "reason": "takeover"}), "rebind")
	refused(s.legacyPost("desktop", "/v1/tasks/2406/claim", map[string]any{"claimed_by": "pane", "job_id": "job-shp"}), "claim of another task naming the job")
	refused(s.legacyPost("desktop", "/v1/tasks/next", map[string]any{"lane": "o3-lane", "claimed_by": "pane", "job_id": "job-shp"}), "next naming the job")
	// A review-role claim is receipt_required from its claim.
	s.ok(s.post("desktop", "job-shp-t1", "claim", reviewPayload("job-shp-t1", "b2405-shp", 2405, "desktop", "tester", "T1", "job-shp", 1)), "reviewer mint")
	refused(s.legacyPost("desktop", "/v1/tasks/2407/claim", map[string]any{"claimed_by": "pane", "job_id": "job-shp-t1"}), "claim naming a reviewer job")
	refused(s.legacyPost("desktop", "/v1/relay/events", map[string]any{"kind": "job.completed", "job_id": "job-shp-t1", "owner_lane": "b2405-shp"}), "reviewer terminal relay")
}
