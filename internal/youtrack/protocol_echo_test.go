package youtrack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// echoActivity is one comment added to issue 9-17 in the activity feed.
func echoActivity(id, commentID, text string) Activity {
	body, _ := json.Marshal(text)
	return Activity{
		ID: id, Author: operator(), Target: &Issue{ID: "9-17", IDReadable: "HK-17"},
		Categories: []ActivityCategory{{Type: "CommentsCategory", Added: []json.RawMessage{json.RawMessage(`{"id":"` + commentID + `","text":` + string(body) + `}`)}}},
	}
}

// A comment the write protocol put on an issue comes back in the activity
// feed. Even when its author is not recognised as hk-bot (another token, a
// renamed bot), it is an echo, not a second PG comment, when it has the exact
// form hk renders and its marker names an op hk wrote to that issue. A comment
// that only quotes a marker, or names an op hk does not have or wrote
// elsewhere, is an ordinary comment and is pulled.
func TestProtocolActivityDoesNotEcho(t *testing.T) {
	fs := newFakeStore()
	fs.cursor.Cursor = "before"
	fs.ytWriteOps = map[int64]ytWriteOp{17: {tenant: "tenant", issueID: "9-17"}, 18: {tenant: "tenant", issueID: "9-99"}, 19: {tenant: "other-tenant", issueID: "9-17"}}
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "echo target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-17", "HK-17")
	quoted := "the marker reads <!-- hk-op:17 --> in the docs"
	unknownOp := "작성: worker · 2026-11-09T12:00:00Z · hk op 19\n\n남의 코멘트\n\n<!-- hk-op:19 -->"
	elsewhere := "작성: worker · 2026-11-09T12:00:00Z · hk op 18\n\n다른 이슈에 쓴 것\n\n<!-- hk-op:18 -->"
	yt.activities = []Activity{
		echoActivity("a-echo", "c-17", "작성: worker · 2026-11-09T12:00:00Z · hk op 17\n\n첫 코멘트\n\n<!-- hk-op:17 -->"),
		echoActivity("a-quoted", "c-q", quoted),
		echoActivity("a-unknown-op", "c-u", unknownOp),
		echoActivity("a-elsewhere", "c-e", elsewhere),
	}
	svc := newTestService(t, yt, fs)
	report, err := svc.Sync(context.Background(), SyncOptions{Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	got := map[string]bool{}
	for _, c := range fs.comments[task.ID] {
		got[c.Body] = true
	}
	if len(fs.comments[task.ID]) != 3 || !got[quoted] || !got[unknownOp] || !got[elsewhere] {
		t.Fatalf("pulled comments %+v; want the three that are not hk's echo, and not the echo", fs.comments[task.ID])
	}
}
