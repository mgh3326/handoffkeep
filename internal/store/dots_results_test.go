package store

import (
	"encoding/hex"
	"errors"
	"testing"
)

func TestDotsMigration(t *testing.T) {
	s := schemaStore(t, "dots_mig")
	ctx := t.Context()
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max(version)=%d err=%v, want %d", version, err, SchemaVersion)
	}
	insert := `INSERT INTO dots_results(document_key,comment_id,issue_uuid,issue_identifier,kind,artefact_sha256,routed_lane,created_at,updated_at,adoption_state,superseded_by) VALUES($1,$2,'u','ZZZ-1',$3,$4,'lane',now(),now(),$5,$6) RETURNING id, adoption_state`
	sum := hex.EncodeToString(make([]byte, 32))
	var id int64
	var state string
	if err := s.pool.QueryRow(ctx, `INSERT INTO dots_results(document_key,comment_id,issue_uuid,issue_identifier,kind,artefact_sha256,routed_lane,created_at,updated_at) VALUES('k1','c1','u','ZZZ-1','RESULT',$1,'lane',now(),now()) RETURNING id, adoption_state`, sum).Scan(&id, &state); err != nil || state != DotsAdoptionPending {
		t.Fatalf("default state=%q err=%v", state, err)
	}
	for name, args := range map[string][]any{
		"kind":                    {"k2", "c2", "START", sum, DotsAdoptionPending, nil},
		"sha":                     {"k2", "c2", "RESULT", "ABC", DotsAdoptionPending, nil},
		"state":                   {"k2", "c2", "RESULT", sum, "adopted", nil},
		"superseded without id":   {"k2", "c2", "RESULT", sum, DotsAdoptionSuperseded, nil},
		"superseded_by elsewhere": {"k2", "c2", "RESULT", sum, DotsAdoptionAccepted, id},
		"document_key unique":     {"k1", "c2", "RESULT", sum, DotsAdoptionPending, nil},
		"comment_id unique":       {"k2", "c1", "RESULT", sum, DotsAdoptionPending, nil},
	} {
		var got int64
		if err := s.pool.QueryRow(ctx, insert, args...).Scan(&got, &state); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func dotsSeedTask(t *testing.T, s *Store, lane, uuid string) int64 {
	t.Helper()
	id := seedTaskRow(t, s.pool, lane, "dots task", "backlog")
	in := validOrigin()
	in.ExternalID = uuid
	if _, _, err := s.AddTaskOrigin(t.Context(), id, "tester", in); err != nil {
		t.Fatal(err)
	}
	return id
}

func dotsResultRow(t *testing.T, s *Store, commentID string) DotsResult {
	t.Helper()
	var x DotsResult
	if err := scanDotsResult(s.pool.QueryRow(t.Context(), `SELECT `+dotsResultColumns+` FROM dots_results WHERE comment_id=$1`, commentID), &x); err != nil {
		t.Fatal(err)
	}
	return x
}

func TestSetDotsAdoption(t *testing.T) {
	s := dotsStore(t)
	ctx := t.Context()
	issue := DotsIssue{Identifier: "ZZZ-31", Title: "Adopt", URL: "https://linear.example/zzz/31", IssueUUID: dotsTestIssueUUID}
	ids := []int64{}
	for _, cid := range []string{"0a1b2c3d-0000-4000-8000-000000000031", "0a1b2c3e-0000-4000-8000-000000000032"} {
		seedDotsResult(t, s, "RESULT", issue, DotsComment{ID: cid, CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] RESULT ok"})
		ids = append(ids, dotsResultRow(t, s, cid).ID)
	}
	first, second := ids[0], ids[1]
	pr := "https://github.com/example/repo/pull/12"

	got, changed, err := s.SetDotsAdoption(ctx, first, "tester", DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: pr})
	if err != nil || !changed || got.AdoptionState != DotsAdoptionAccepted || got.AdoptedRef != pr || got.AdoptedBy != "tester" || got.AdoptedAt == nil {
		t.Fatalf("accept: %+v changed=%t err=%v", got, changed, err)
	}
	again, changed, err := s.SetDotsAdoption(ctx, first, "other", DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: pr})
	if err != nil || changed || again.AdoptedBy != "tester" || !again.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("replay: %+v changed=%t err=%v", again, changed, err)
	}
	if got, changed, err = s.SetDotsAdoption(ctx, first, "tester", DotsAdoptionInput{State: DotsAdoptionPartial, Ref: "hk:task/7"}); err != nil || !changed || got.AdoptionState != DotsAdoptionPartial {
		t.Fatalf("partial: %+v changed=%t err=%v", got, changed, err)
	}

	for name, in := range map[string]DotsAdoptionInput{
		"superseded without superseded_by": {State: DotsAdoptionSuperseded},
		"superseded_by without superseded": {State: DotsAdoptionRejected, SupersededBy: second},
		"accepted without ref":             {State: DotsAdoptionAccepted},
		"bad ref":                          {State: DotsAdoptionAccepted, Ref: "https://example.com/x"},
		"back to pending":                  {State: DotsAdoptionPending},
		"self supersede":                   {State: DotsAdoptionSuperseded, SupersededBy: first},
		"missing superseder":               {State: DotsAdoptionSuperseded, SupersededBy: 1 << 40},
	} {
		if _, _, err := s.SetDotsAdoption(ctx, first, "tester", in); err == nil || errors.Is(err, ErrDotsAdoptionFinal) {
			t.Errorf("%s: err=%v, want a validation error", name, err)
		}
	}
	if _, _, err := s.SetDotsAdoption(ctx, 1<<40, "tester", DotsAdoptionInput{State: DotsAdoptionRejected}); !errors.Is(err, ErrDotsResultNotFound) {
		t.Fatalf("missing id err=%v", err)
	}

	sup := DotsAdoptionInput{State: DotsAdoptionSuperseded, SupersededBy: second, Ref: "hk:doc dots/results/ZZZ-31/2026-11-02-0a1b2c3e"}
	if got, changed, err = s.SetDotsAdoption(ctx, first, "tester", sup); err != nil || !changed || got.SupersededBy == nil || *got.SupersededBy != second {
		t.Fatalf("supersede: %+v changed=%t err=%v", got, changed, err)
	}
	if _, changed, err = s.SetDotsAdoption(ctx, first, "tester", sup); err != nil || changed {
		t.Fatalf("supersede replay changed=%t err=%v", changed, err)
	}
	if _, _, err = s.SetDotsAdoption(ctx, first, "tester", DotsAdoptionInput{State: DotsAdoptionAccepted, Ref: pr}); !errors.Is(err, ErrDotsAdoptionFinal) {
		t.Fatalf("change after superseded err=%v", err)
	}

	list, err := s.ListDotsResults(ctx, 0, "ZZZ-31", 10)
	if err != nil || len(list) != 2 || list[0].ID != second || list[1].ID != first {
		t.Fatalf("list=%+v err=%v", list, err)
	}
}

// seedDotsResult records one dots result with no linked task, the shape the
// retired Linear webhook path stored (hk 4425 removed that writer; the rows
// it left are still adopted through SetDotsAdoption).
func seedDotsResult(t *testing.T, s *Store, kind string, issue DotsIssue, c DotsComment) {
	t.Helper()
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, _, err := recordDotsResultTx(t.Context(), tx, kind, issue, c, nil, DotsLane, "linear-webhook"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
