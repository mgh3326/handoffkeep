package store

import (
	"context"
	"testing"
	"time"
)

func TestDotsReplyAndYouTrackCommentLockOrder(t *testing.T) {
	dbURL, app := lockOrderSchema(t, "dots_lock")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	dots := openLockActorStore(t, dbURL, app("dots"))
	youtrack := openLockActorStore(t, dbURL, app("youtrack"))
	task, err := dots.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	blocker := lockBlocker(t, ctx, dbURL, app("blocker"))
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, taskCommentInsertLock); err != nil {
		t.Fatal(err)
	}
	reply := startLockActor(app("dots"), func() error {
		_, err := dots.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: "start", ExpectedUpdatedAt: &task.UpdatedAt})
		return err
	})
	if !waitBlockedBy(t, ctx, dots.pool, reply, app("blocker")) {
		t.Fatalf("reply finished before comment lock was released: %v", reply.err)
	}
	activity := startLockActor(app("youtrack"), func() error {
		_, err := youtrack.ApplyYouTrackActivity(ctx, YouTrackActivity{ID: "dots-lock-activity", TaskID: task.ID, IssueID: "issue-1", Comment: "A comment", CommentID: "comment-1"})
		return err
	})
	if !waitBlockedBy(t, ctx, dots.pool, activity, app("dots")) {
		t.Fatalf("YouTrack activity finished before the reply: %v", activity.err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	finishLockActors(t, ctx, reply, activity)
}
