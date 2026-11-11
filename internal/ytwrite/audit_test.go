package ytwrite_test

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

const (
	auditIssue = "2-17"
	auditBot   = "1-1"
	auditHuman = "1-7"
)

// auditRendered is the text hk writes for op id copying body.
func auditRendered(t *testing.T, id int64, body string) string {
	t.Helper()
	payload, err := json.Marshal(ytwrite.CommentPayload{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	text, err := ytwrite.RenderComment(ytwrite.Op{ID: id, Actor: "worker", CreatedAt: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// auditList lists the issue through the fake remote with a small page, so
// the comments come back through the stable enumeration.
func auditList(t *testing.T, f *ytwritetest.FakeYouTrack) []ytwrite.RemoteComment {
	t.Helper()
	got, err := f.ListComments(t.Context(), auditIssue, 2)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func copies(id, op int64, yt ...string) ytwrite.AuditCopies {
	return ytwrite.AuditCopies{CommentID: id, OpID: op, YTIDs: yt}
}

func TestAuditCommentsClasses(t *testing.T) {
	f := ytwritetest.NewFake()
	// 1: written by op 7, its rendered copy is on the issue (by the bot).
	byOp := f.InjectAs(auditIssue, auditRendered(t, 7, "copied by the protocol"), auditBot)
	// 2: imported before the binding, text is the body exactly.
	imported := f.InjectAs(auditIssue, "imported body\nsecond line", auditBot)
	// 3: a copy whose text differs by a trailing newline is not a copy.
	spaced := f.InjectAs(auditIssue, "spaced body\n", auditHuman)
	// 4: two equal copies of one body.
	twiceA := f.InjectAs(auditIssue, "twice", auditBot)
	twiceB := f.InjectAs(auditIssue, "twice", auditBot)
	// An op rendering for an op no hk comment here owns: the bot's, no match.
	strayOp := f.InjectAs(auditIssue, auditRendered(t, 99, "other task"), auditBot)
	// A person's comment, and a deleted copy that does not count.
	human := f.InjectAs(auditIssue, "said in YouTrack", auditHuman)
	deleted := f.InjectAs(auditIssue, "missing", auditBot)
	f.Delete(auditIssue, deleted)

	hk := []ytwrite.AuditComment{
		{ID: 4, Body: "twice"},
		{ID: 1, Body: "copied by the protocol", OpID: 7},
		{ID: 2, Body: "imported body\nsecond line"},
		{ID: 3, Body: "spaced body"},
		{ID: 5, Body: "missing"},
	}
	got := ytwrite.AuditComments(hk, auditList(t, f), auditBot, nil)
	want := ytwrite.CommentAudit{
		Present:     []ytwrite.AuditCopies{copies(1, 7, byOp), copies(2, 0, imported)},
		Missing:     []ytwrite.AuditCopies{copies(3, 0), copies(5, 0)},
		Duplicate:   []ytwrite.AuditCopies{copies(4, 0, twiceA, twiceB)},
		YTOnlyBot:   []string{strayOp},
		YTOnlyHuman: []string{spaced, human},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit\n got %+v\nwant %+v", got, want)
	}
}

// Equal bodies share their copies out in comment id order, a comment its op
// already copied last; a copy left over makes a duplicate.
func TestAuditCommentsEqualBodies(t *testing.T) {
	t.Run("one copy for two comments", func(t *testing.T) {
		f := ytwritetest.NewFake()
		only := f.InjectAs(auditIssue, "LGTM", auditBot)
		got := ytwrite.AuditComments([]ytwrite.AuditComment{{ID: 1, Body: "LGTM"}, {ID: 2, Body: "LGTM"}}, auditList(t, f), auditBot, nil)
		if !reflect.DeepEqual(got.Present, []ytwrite.AuditCopies{copies(1, 0, only)}) || !reflect.DeepEqual(got.Missing, []ytwrite.AuditCopies{copies(2, 0)}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("op copy and an imported copy of another comment", func(t *testing.T) {
		f := ytwritetest.NewFake()
		rendered := f.InjectAs(auditIssue, auditRendered(t, 3, "LGTM"), auditBot)
		imported := f.InjectAs(auditIssue, "LGTM", auditBot)
		got := ytwrite.AuditComments([]ytwrite.AuditComment{{ID: 1, Body: "LGTM", OpID: 3}, {ID: 2, Body: "LGTM"}}, auditList(t, f), auditBot, nil)
		want := []ytwrite.AuditCopies{copies(1, 3, rendered), copies(2, 0, imported)}
		if !reflect.DeepEqual(got.Present, want) || len(got.Missing)+len(got.Duplicate)+len(got.YTOnlyBot)+len(got.YTOnlyHuman) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("op copy and an imported copy of the same comment", func(t *testing.T) {
		f := ytwritetest.NewFake()
		rendered := f.InjectAs(auditIssue, auditRendered(t, 3, "LGTM"), auditBot)
		imported := f.InjectAs(auditIssue, "LGTM", auditHuman)
		got := ytwrite.AuditComments([]ytwrite.AuditComment{{ID: 1, Body: "LGTM", OpID: 3}}, auditList(t, f), auditBot, nil)
		if !reflect.DeepEqual(got.Duplicate, []ytwrite.AuditCopies{copies(1, 3, rendered, imported)}) || len(got.Present) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
}

// A legacy mirror copy is matched by its recorded id, whatever its text
// (the push wraps the body), and only the comment it was recorded for.
func TestAuditCommentsMirrorLink(t *testing.T) {
	f := ytwritetest.NewFake()
	mirrored := f.InjectAs(auditIssue, "작성: worker\n\npushed\n\n<!-- hk-comment:1 -->", auditBot)
	unlinked := f.InjectAs(auditIssue, "작성: worker\n\npushed\n\n<!-- hk-comment:2 -->", auditBot)
	got := ytwrite.AuditComments([]ytwrite.AuditComment{{ID: 1, Body: "pushed", LinkedYTIDs: []string{mirrored}}, {ID: 2, Body: "pushed"}}, auditList(t, f), auditBot, nil)
	want := ytwrite.CommentAudit{
		Present:   []ytwrite.AuditCopies{copies(1, 0, mirrored)},
		Missing:   []ytwrite.AuditCopies{copies(2, 0)},
		YTOnlyBot: []string{unlinked},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit\n got %+v\nwant %+v", got, want)
	}
}

// A legacy marker counts only after a recorded link and an op rendering: an
// op copy whose body quotes another comment's marker stays its op's.
func TestAuditCommentsEvidenceOrder(t *testing.T) {
	legacy := func(text string) (int64, bool) {
		rest, ok := strings.CutPrefix(text, "legacy:")
		if !ok {
			return 0, false
		}
		n, err := strconv.ParseInt(rest, 10, 64)
		return n, err == nil
	}
	f := ytwritetest.NewFake()
	// Comment 2's op copy: the legacy reader would name comment 1 in it.
	opCopy := f.InjectAs(auditIssue, auditRendered(t, 9, "B"), auditBot)
	byMarker := f.InjectAs(auditIssue, "legacy:3", auditBot)
	quoting := func(text string) (int64, bool) {
		if text == auditRendered(t, 9, "B") {
			return 1, true
		}
		return legacy(text)
	}
	got := ytwrite.AuditComments([]ytwrite.AuditComment{{ID: 1, Body: "A"}, {ID: 2, Body: "B", OpID: 9}, {ID: 3, Body: "C"}}, auditList(t, f), auditBot, quoting)
	want := ytwrite.CommentAudit{
		Present: []ytwrite.AuditCopies{copies(2, 9, opCopy), copies(3, 0, byMarker)},
		Missing: []ytwrite.AuditCopies{copies(1, 0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit\n got %+v\nwant %+v", got, want)
	}
}

// With no bot id known, nothing counts as the bot's.
func TestAuditCommentsUnknownBot(t *testing.T) {
	got := ytwrite.AuditComments(nil, []ytwrite.RemoteComment{{ID: "4-1", Text: "x"}}, "", nil)
	if !reflect.DeepEqual(got.YTOnlyHuman, []string{"4-1"}) || got.YTOnlyBot != nil {
		t.Fatalf("got %+v", got)
	}
}
