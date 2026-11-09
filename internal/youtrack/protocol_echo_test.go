package youtrack

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// A comment the write protocol put on an issue comes back in the activity
// feed. Even when its author is not recognised as hk-bot (another token, a
// renamed bot), its op marker keeps it from becoming a second PG comment.
func TestProtocolActivityDoesNotEcho(t *testing.T) {
	fs := newFakeStore()
	fs.cursor.Cursor = "before"
	yt := newFakeYouTrack()
	task := fs.addTask("handoffkeep", "handoffkeep", "echo target", store.StateBacklog, 0)
	mirrorIssue(fs, yt, task.ID, "9-17", "HK-17")
	text, _ := json.Marshal("작성: worker · 2026-11-09T12:00:00Z · hk op 17\n\n첫 코멘트\n\n<!-- hk-op:17 -->")
	yt.activities = []Activity{{
		ID: "a-echo", Author: operator(), Target: &Issue{ID: "9-17", IDReadable: "HK-17"},
		Categories: []ActivityCategory{{Type: "CommentsCategory", Added: []json.RawMessage{json.RawMessage(`{"id":"c-17","text":` + string(text) + `}`)}}},
	}}
	svc := newTestService(t, yt, fs)
	report, err := svc.Sync(context.Background(), SyncOptions{Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors=%v", report.Errors)
	}
	if len(fs.comments[task.ID]) != 0 {
		t.Fatalf("protocol echo became a PG comment: %+v", fs.comments[task.ID])
	}
}
