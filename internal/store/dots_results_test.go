package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestDotsRequesterLane(t *testing.T) {
	task := &Task{Lane: "x"}
	for _, tc := range []struct {
		name        string
		description string
		task        *Task
		want        string
	}{
		{"hk-lane line", "Please research.\nhk-lane: hk-design\nmore", nil, "hk-design"},
		{"hk-lane line beats task", "hk-lane: hk-design", task, "hk-design"},
		{"invalid name falls to task", "hk-lane: not a lane\n", task, "x"},
		{"invalid characters fall to task", "hk-lane: bad/name", task, "x"},
		{"no line, linked task", "Please research.", task, "x"},
		{"nothing", "", nil, DotsLane},
		{"second line ignored", "hk-lane: first\nhk-lane: second", nil, "first"},
		{"line must start the line", "see hk-lane: other", nil, DotsLane},
		{"CRLF description", "intro\r\nhk-lane: crlf-lane\r\nend", nil, "crlf-lane"},
	} {
		if got := dotsRequesterLane(tc.description, tc.task); got != tc.want {
			t.Errorf("%s: lane=%q want %q", tc.name, got, tc.want)
		}
	}
}

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

func TestRecordDotsResultLinksAndRoutes(t *testing.T) {
	s := dotsStore(t)
	ctx := t.Context()

	// One task with the issue as its Linear origin: linked, routed to its lane.
	linkedUUID := "0a1b2c3d-0000-4000-8000-0000000000b1"
	taskID := dotsSeedTask(t, s, "requester-lane", linkedUUID)
	issue := DotsIssue{Identifier: "ZZZ-21", Title: "Linked", URL: "https://linear.example/zzz/21", IssueUUID: linkedUUID}
	c := DotsComment{ID: "0a1b2c3d-0000-4000-8000-000000000021", CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] RESULT: use the ledger\ndetails"}
	key, created, err := s.RecordDotsResult(ctx, "RESULT", issue, c)
	if err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	doc, _, err := s.GetDocument(ctx, key)
	if err != nil || doc.TaskID == nil || *doc.TaskID != taskID || doc.Session != DotsLane || doc.Body != dotsResultBody("RESULT", issue, c) {
		t.Fatalf("doc task_id=%v session=%q err=%v", doc.TaskID, doc.Session, err)
	}
	row := dotsResultRow(t, s, c.ID)
	sum := sha256.Sum256([]byte(c.Body))
	if row.TaskID == nil || *row.TaskID != taskID || row.RoutedLane != "requester-lane" || row.AdoptionState != DotsAdoptionPending ||
		row.ArtefactSHA256 != hex.EncodeToString(sum[:]) || row.ArtefactSHA256 == doc.SHA256 || row.Verdict != "use the ledger" ||
		row.DocumentKey != key || row.IssueUUID != linkedUUID || row.IssueIdentifier != "ZZZ-21" || row.Kind != "RESULT" {
		t.Fatalf("row=%+v", row)
	}
	rows, err := s.ListUnsentNotifications(ctx, 100)
	if err != nil || len(rows) != 1 || rows[0].TargetLane != "requester-lane" || rows[0].EventID != "dots-"+c.ID || rows[0].Text != dotsResultText("RESULT", key, issue, c) {
		t.Fatalf("outbox=%+v err=%v", rows, err)
	}

	// The description's hk-lane line wins over the task lane, for a follow-up too.
	described := issue
	described.Description = "Research this.\nhk-lane: hk-design\n"
	followupID := "0f0f0f0f-0000-4000-8000-000000000021"
	if created, err := s.RecordDotsFollowup(ctx, described, followupID); err != nil || !created {
		t.Fatalf("followup created=%t err=%v", created, err)
	}

	// Two tasks with the same origin: unknown, both links stay NULL.
	sharedUUID := "0a1b2c3d-0000-4000-8000-0000000000b2"
	dotsSeedTask(t, s, "lane-a", sharedUUID)
	dotsSeedTask(t, s, "lane-b", sharedUUID)
	shared := DotsIssue{Identifier: "ZZZ-22", Title: "Shared", URL: "https://linear.example/zzz/22", IssueUUID: sharedUUID}
	sc := DotsComment{ID: "0a1b2c3d-0000-4000-8000-000000000022", CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] BLOCKED need access"}
	skey, _, err := s.RecordDotsResult(ctx, "BLOCKED", shared, sc)
	if err != nil {
		t.Fatal(err)
	}
	if sdoc, _, err := s.GetDocument(ctx, skey); err != nil || sdoc.TaskID != nil {
		t.Fatalf("shared doc task_id=%v err=%v", sdoc.TaskID, err)
	}
	if srow := dotsResultRow(t, s, sc.ID); srow.TaskID != nil || srow.RoutedLane != DotsLane {
		t.Fatalf("shared row=%+v", srow)
	}

	// The poller wrote the document first: the row is still written and the
	// link set, and no notification is queued.
	pollerUUID := "0a1b2c3d-0000-4000-8000-0000000000b3"
	pollerTask := dotsSeedTask(t, s, "poller-lane", pollerUUID)
	pissue := DotsIssue{Identifier: "ZZZ-23", Title: "Poller", URL: "https://linear.example/zzz/23", IssueUUID: pollerUUID}
	pc := DotsComment{ID: "0a1b2c3d-0000-4000-8000-000000000023", CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] QUESTION which one?"}
	pkey := dotsResultKey(pissue, pc)
	if _, _, err := s.PutDocument(ctx, Document{Key: pkey, Kind: "note", Session: DotsLane, Body: dotsResultBody("QUESTION", pissue, pc), CreatedBy: "poller"}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.RecordDotsResult(ctx, "QUESTION", pissue, pc); err != nil || created {
		t.Fatalf("poller-first created=%t err=%v", created, err)
	}
	if pdoc, _, err := s.GetDocument(ctx, pkey); err != nil || pdoc.TaskID == nil || *pdoc.TaskID != pollerTask || pdoc.CreatedBy != "poller" {
		t.Fatalf("poller doc=%+v err=%v", pdoc, err)
	}
	if prow := dotsResultRow(t, s, pc.ID); prow.TaskID == nil || *prow.TaskID != pollerTask || prow.AdoptionState != DotsAdoptionPending {
		t.Fatalf("poller row=%+v", prow)
	}

	rows, err = s.ListUnsentNotifications(ctx, 100)
	if err != nil || len(rows) != 3 {
		t.Fatalf("outbox=%+v err=%v", rows, err)
	}
	if rows[1].Kind != OutboxKindDotsFollowup || rows[1].TargetLane != "hk-design" || rows[2].Kind != OutboxKindDotsDecision || rows[2].TargetLane != DotsLane || rows[2].EventID != "dots-"+sc.ID {
		t.Fatalf("outbox=%+v", rows)
	}
}

func TestSetDotsAdoption(t *testing.T) {
	s := dotsStore(t)
	ctx := t.Context()
	issue := DotsIssue{Identifier: "ZZZ-31", Title: "Adopt", URL: "https://linear.example/zzz/31", IssueUUID: dotsTestIssueUUID}
	ids := []int64{}
	for _, cid := range []string{"0a1b2c3d-0000-4000-8000-000000000031", "0a1b2c3e-0000-4000-8000-000000000032"} {
		if _, _, err := s.RecordDotsResult(ctx, "RESULT", issue, DotsComment{ID: cid, CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] RESULT ok"}); err != nil {
			t.Fatal(err)
		}
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
