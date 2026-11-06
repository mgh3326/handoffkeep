package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// dotsVector is one row of testdata/dots_vectors.json: a fake comment and what
// the reconciling poller's own code produces for it. Both paths must agree byte
// for byte, or the second writer would rewrite the first one's document.
type dotsVector struct {
	ID         string  `json:"id"`
	CreatedAt  string  `json:"createdAt"`
	Body       string  `json:"body"`
	Identifier string  `json:"identifier"`
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	Kind       *string `json:"kind"`
	Summary    string  `json:"summary"`
	Follow     string  `json:"follow"`
	Key        string  `json:"key"`
	DocBody    string  `json:"docBody"`
	Text       string  `json:"text"`
}

func TestDotsFormatsMatchPoller(t *testing.T) {
	raw, err := os.ReadFile("testdata/dots_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []dotsVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range vectors {
		t.Run(v.Identifier, func(t *testing.T) {
			kind, dots := DotsKind(v.Body)
			if dots != (v.Kind != nil) || (dots && kind != *v.Kind) {
				t.Fatalf("kind=%q dots=%t want %v", kind, dots, v.Kind)
			}
			if got := dotsSummaryLine(v.Body); got != v.Summary {
				t.Fatalf("summary=%q want %q", got, v.Summary)
			}
			issue := DotsIssue{Identifier: v.Identifier, Title: v.Title, URL: v.URL}
			if got := dotsFollowupText(issue); got != v.Follow {
				t.Fatalf("followup=%q want %q", got, v.Follow)
			}
			if DotsKindRecorded(kind) != (v.Key != "") {
				t.Fatalf("recorded=%t key=%q", DotsKindRecorded(kind), v.Key)
			}
			if v.Key == "" {
				return
			}
			c := DotsComment{ID: v.ID, CreatedAt: v.CreatedAt, Body: v.Body}
			key := dotsResultKey(issue, c)
			if key != v.Key {
				t.Fatalf("key=%q want %q", key, v.Key)
			}
			if got := dotsResultBody(kind, issue, c); got != v.DocBody {
				t.Fatalf("body=%q want %q", got, v.DocBody)
			}
			if got := dotsResultText(kind, key, issue, c); got != v.Text {
				t.Fatalf("text=%q want %q", got, v.Text)
			}
		})
	}
}

// The poller would fail on these; the outbox row must stay drainable instead.
func TestDotsLaneTextIsDrainable(t *testing.T) {
	text := dotsLaneText("a\tb\x7fc\u0085d" + strings.Repeat("가", RelayLaneEventMaxBytes))
	if len(text) > RelayLaneEventMaxBytes {
		t.Fatalf("len=%d", len(text))
	}
	for _, r := range text {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			t.Fatalf("control character %q in %q", r, text[:16])
		}
	}
	if !strings.HasPrefix(text, "a b c d") {
		t.Fatalf("prefix %q", text[:16])
	}
}

func TestRecordDotsRejectsMalformedInput(t *testing.T) {
	s := &Store{} // validation runs before any database access
	issue := DotsIssue{Identifier: "ZZZ-1", Title: "t", URL: "u", IssueUUID: dotsTestIssueUUID}
	good := DotsComment{ID: "0a1b2c3d-0000", CreatedAt: "2026-11-02T00:00:00.000Z", Body: "b"}
	bad := []struct {
		name  string
		kind  string
		issue DotsIssue
		c     DotsComment
	}{
		{"kind", "START", issue, good},
		{"identifier", "RESULT", DotsIssue{Identifier: "../x"}, good},
		{"short id", "RESULT", issue, DotsComment{ID: "abc", CreatedAt: good.CreatedAt}},
		{"created at", "RESULT", issue, DotsComment{ID: good.ID, CreatedAt: "soon"}},
		{"issue uuid", "RESULT", DotsIssue{Identifier: "ZZZ-1", IssueUUID: "issue-1"}, good},
	}
	for _, tc := range bad {
		if _, _, err := s.RecordDotsResult(t.Context(), tc.kind, tc.issue, tc.c); err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
	if _, err := s.RecordDotsFollowup(t.Context(), DotsIssue{Identifier: "../x"}, good.ID); err == nil {
		t.Fatal("followup: accepted")
	}
}

func dotsStore(t *testing.T) *Store { return schemaStore(t, "hkdots") }

const dotsTestIssueUUID = "0a1b2c3d-0000-4000-8000-0000000000aa"

func TestRecordDotsResultWritesOnce(t *testing.T) {
	s := dotsStore(t)
	ctx := t.Context()
	issue := DotsIssue{Identifier: "ZZZ-7", Title: "Fake issue", URL: "https://linear.example/zzz/7", IssueUUID: dotsTestIssueUUID}
	c := DotsComment{ID: "0a1b2c3d-0000-4000-8000-000000000007", CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] RESULT done\nsummary"}
	key, created, err := s.RecordDotsResult(ctx, "RESULT", issue, c)
	if err != nil || !created || key != "dots/results/ZZZ-7/2026-11-02-0a1b2c3d" {
		t.Fatalf("key=%q created=%t err=%v", key, created, err)
	}
	doc, found, err := s.GetDocument(ctx, key)
	if err != nil || !found || doc.Kind != "note" || doc.Session != DotsLane || doc.Body != dotsResultBody("RESULT", issue, c) {
		t.Fatalf("doc=%+v found=%t err=%v", doc, found, err)
	}
	if doc.OriginClass != "import" || doc.TaskID != nil || doc.CreatedBy != dotsCreatedBy {
		t.Fatalf("dots result origin_class=%q task_id=%v created_by=%q, want import, none, %s", doc.OriginClass, doc.TaskID, doc.CreatedBy, dotsCreatedBy)
	}
	// A redelivery carrying an edited comment and a new title changes nothing.
	edited := c
	edited.Body = "[dots] RESULT rewritten\nother"
	if _, created, err = s.RecordDotsResult(ctx, "RESULT", DotsIssue{Identifier: "ZZZ-7", Title: "Renamed", URL: issue.URL, IssueUUID: dotsTestIssueUUID}, edited); err != nil || created {
		t.Fatalf("redelivery created=%t err=%v", created, err)
	}
	again, _, err := s.GetDocument(ctx, key)
	if err != nil || again.SHA256 != doc.SHA256 || !again.UpdatedAt.Equal(doc.UpdatedAt) {
		t.Fatalf("document changed: %+v err=%v", again, err)
	}
	rows, err := s.ListUnsentNotifications(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != OutboxKindDotsReport || rows[0].TargetLane != DotsLane || rows[0].EventID != "dots-"+c.ID || !strings.Contains(rows[0].Text, "hk:doc "+key) {
		t.Fatalf("outbox=%+v", rows)
	}
}

func TestRecordDotsResultLeavesPollerDocument(t *testing.T) {
	s := dotsStore(t)
	ctx := t.Context()
	key := "dots/results/ZZZ-8/2026-11-02-0a1b2c3d"
	if _, _, err := s.PutDocument(ctx, Document{Key: key, Kind: "note", Session: DotsLane, Body: "poller wrote this", CreatedBy: "poller"}); err != nil {
		t.Fatal(err)
	}
	_, created, err := s.RecordDotsResult(ctx, "QUESTION", DotsIssue{Identifier: "ZZZ-8", Title: "t", URL: "u", IssueUUID: dotsTestIssueUUID}, DotsComment{ID: "0a1b2c3d-0000-4000-8000-000000000008", CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] QUESTION hm"})
	if err != nil || created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	doc, _, err := s.GetDocument(ctx, key)
	if err != nil || doc.Body != "poller wrote this" {
		t.Fatalf("doc=%q err=%v", doc.Body, err)
	}
	rows, err := s.ListUnsentNotifications(ctx, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("outbox=%+v err=%v", rows, err)
	}
}

func TestRecordDotsResultKindsAndFollowupKinds(t *testing.T) {
	s := dotsStore(t)
	ctx := t.Context()
	issue := DotsIssue{Identifier: "ZZZ-9", Title: "t", URL: "https://linear.example/zzz/9", IssueUUID: dotsTestIssueUUID}
	for i, kind := range []string{"BLOCKED", "QUESTION"} {
		id := "0a1b2c3" + string(rune('0'+i)) + "-0000-4000-8000-000000000009"
		if _, created, err := s.RecordDotsResult(ctx, kind, issue, DotsComment{ID: id, CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] " + kind}); err != nil || !created {
			t.Fatalf("%s created=%t err=%v", kind, created, err)
		}
	}
	commentID := "0f0f0f0f-0000-4000-8000-000000000009"
	if created, err := s.RecordDotsFollowup(ctx, issue, commentID); err != nil || !created {
		t.Fatalf("followup created=%t err=%v", created, err)
	}
	if created, err := s.RecordDotsFollowup(ctx, issue, commentID); err != nil || created {
		t.Fatalf("repeat followup created=%t err=%v", created, err)
	}
	rows, err := s.ListUnsentNotifications(ctx, 100)
	if err != nil || len(rows) != 3 {
		t.Fatalf("outbox=%+v err=%v", rows, err)
	}
	if rows[0].Kind != OutboxKindDotsDecision || rows[1].Kind != OutboxKindDotsDecision || rows[2].Kind != OutboxKindDotsFollowup || rows[2].EventID != "dots-followup-"+commentID || rows[2].Text != dotsFollowupText(issue) {
		t.Fatalf("outbox=%+v", rows)
	}
}

// A recorded dots result is stored as an import; its key, body and creator
// are the poller's.
func TestDotsResultIsImport(t *testing.T) {
	s := dotsStore(t)
	ctx := t.Context()
	issue := DotsIssue{Identifier: "ZZZ-8", Title: "Fake issue", URL: "https://linear.example/zzz/8", IssueUUID: dotsTestIssueUUID}
	c := DotsComment{ID: "0a1b2c3d-0000-4000-8000-000000000008", CreatedAt: "2026-11-02T01:02:03.000Z", Body: "[dots] RESULT done\nsummary"}
	key, created, err := s.RecordDotsResult(ctx, "RESULT", issue, c)
	if err != nil || !created || key != dotsResultKey(issue, c) {
		t.Fatalf("key=%q created=%t err=%v", key, created, err)
	}
	doc, found, err := s.GetDocument(ctx, key)
	if err != nil || !found {
		t.Fatalf("doc=%+v found=%t err=%v", doc, found, err)
	}
	if doc.OriginClass != "import" || doc.TaskID != nil || doc.Body != dotsResultBody("RESULT", issue, c) || doc.CreatedBy != dotsCreatedBy {
		t.Fatalf("dots result: origin_class=%q task_id=%v created_by=%q", doc.OriginClass, doc.TaskID, doc.CreatedBy)
	}
}
