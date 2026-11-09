package mcp

import (
	"strconv"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// task_comment enqueues the canary op in the comment's transaction; a replay
// with the same key returns the original comment and makes no second op. The
// tool's answer keeps its envelope.
func TestTaskCommentEnqueuesCanaryOp(t *testing.T) {
	st := newOperatorStore(t)
	st.SetYTCommentsMode(store.YTCommentsCanary)
	task := newOperatorTask(t, st, "yt canary over mcp")
	id := strconv.FormatInt(task.ID, 10)
	verified, err := st.YTCanaryCandidate(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddYTCommentCanary(t.Context(), store.YTCommentCanary{TaskID: task.ID, Tenant: "mcp.youtrack.cloud", IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", AddedBy: "test"}, verified); err != nil {
		t.Fatal(err)
	}
	session := writesSession(t, st)
	args := map[string]any{"id": task.ID, "body": "dots note for youtrack", "idempotency_key": "yt-cmt-1"}
	first := callTool[ytCommentOut](t, session, "task_comment", args)
	again := callTool[ytCommentOut](t, session, "task_comment", args)
	if first.Comment.ID == 0 || again.Comment.ID != first.Comment.ID || first.Comment.Author != "dots" || first.Actor == "" || first.ConsoleURL == "" || first.Replayed || !again.Replayed {
		t.Fatalf("comment envelope %+v / %+v", first, again)
	}
	ops, err := st.ListYTWriteOps(t.Context(), store.YTOpFilter{State: "all", TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Key != store.YTCommentKey(first.Comment.ID) || ops[0].Actor != "dots" || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("ops = %+v", ops)
	}
	// The envelope adds the projection; a replay names the same op.
	for name, out := range map[string]ytCommentOut{"first": first, "replay": again} {
		if p := out.YouTrackProjection; p == nil || p.Status != "pending" || p.OpID != ops[0].ID {
			t.Fatalf("%s: projection %+v, want pending with op %d", name, p, ops[0].ID)
		}
	}
}

// ytCommentOut is the task_comment envelope with its YouTrack projection.
type ytCommentOut struct {
	commentOut
	Replayed           bool                `json:"replayed"`
	YouTrackProjection *store.YTProjection `json:"youtrack_projection"`
}
