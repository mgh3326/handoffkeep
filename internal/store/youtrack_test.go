package store

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func youtrackTask(t *testing.T, st *Store, lane, title string) Task {
	t.Helper()
	project := "handoffkeep"
	x, err := st.CreateTask(context.Background(), Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestYouTrackMigrationAppliesOnce(t *testing.T) {
	_, url := testdb.Schema(t, "youtrack_migrate")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		st, err := Open(ctx, url)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		var recorded int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE id=$1`, youtrackMigrationID).Scan(&recorded); err != nil {
			t.Fatalf("open %d: migration row: %v", i, err)
		}
		if recorded != 1 {
			t.Fatalf("open %d: migration recorded %d times", i, recorded)
		}
		for _, table := range []string{"youtrack_links", "youtrack_cursor", "youtrack_comment_links", "youtrack_field_cache", "youtrack_processed_activities"} {
			var exists bool
			if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
				t.Fatalf("open %d: table %s exists=%v err=%v", i, table, exists, err)
			}
		}
		st.Close()
	}
}

func TestYouTrackLinksAreIdempotentAndConflict(t *testing.T) {
	st := schemaStore(t, "youtrack_links")
	ctx := context.Background()
	a := youtrackTask(t, st, "handoffkeep", "first")
	b := youtrackTask(t, st, "handoffkeep", "second")
	now := time.Now().UTC()
	link, err := st.LinkYouTrackIssue(ctx, a.ID, "1-1", "HK-1", now)
	if err != nil || link.TaskID != a.ID || link.ReadableID != "HK-1" {
		t.Fatalf("link=%+v err=%v", link, err)
	}
	if _, err := st.LinkYouTrackIssue(ctx, a.ID, "1-1", "HK-1", now.Add(time.Second)); err != nil {
		t.Fatalf("idempotent relink: %v", err)
	}
	if _, err := st.LinkYouTrackIssue(ctx, a.ID, "1-2", "HK-2", now); err != ErrYouTrackLinkConflict {
		t.Fatalf("changing a link: err=%v", err)
	}
	if _, err := st.LinkYouTrackIssue(ctx, b.ID, "1-1", "HK-1", now); err != ErrYouTrackLinkConflict {
		t.Fatalf("reusing an issue: err=%v", err)
	}
	found, ok, err := st.FindYouTrackLinkByReadableID(ctx, "HK-1")
	if err != nil || !ok || found.TaskID != a.ID {
		t.Fatalf("find readable: %+v ok=%v err=%v", found, ok, err)
	}
	if _, err := st.LinkYouTrackIssue(ctx, a.ID, "1-1", "HK-9", now); err != ErrYouTrackLinkConflict {
		t.Fatalf("changing a readable id: err=%v", err)
	}
}

func TestYouTrackCursorAndCommentLinksAreIdempotent(t *testing.T) {
	st := schemaStore(t, "youtrack_cursor")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "comment target")
	comment, err := st.CreateTaskComment(ctx, x.ID, "test", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkYouTrackComment(ctx, x.ID, comment.ID, "c-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkYouTrackComment(ctx, x.ID, comment.ID, "c-1", time.Now().UTC()); err != nil {
		t.Fatalf("idempotent comment link: %v", err)
	}
	found, ok, err := st.FindYouTrackCommentLinkByYouTrackID(ctx, "c-1")
	if err != nil || !ok || found.HKCommentID != comment.ID {
		t.Fatalf("find comment link: %+v ok=%v err=%v", found, ok, err)
	}
	if err := st.AdvanceYouTrackCursor(ctx, "cursor-1"); err != nil {
		t.Fatal(err)
	}
	cursor, err := st.GetYouTrackCursor(ctx)
	if err != nil || cursor.Cursor != "cursor-1" {
		t.Fatalf("cursor=%+v err=%v", cursor, err)
	}
	if err := st.AdvanceYouTrackCursor(ctx, "cursor-1"); err != nil {
		t.Fatalf("replayed cursor: %v", err)
	}
}

func TestYouTrackFieldCacheTTLAndStale(t *testing.T) {
	st := schemaStore(t, "youtrack_cache")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "cache target")
	if _, err := st.LinkYouTrackIssue(ctx, x.ID, "2-1", "HK-2", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.PutYouTrackFieldCache(ctx, YouTrackFieldCache{TaskID: x.ID, Priority: 3, Lane: "handoffkeep", FetchedAt: now}); err != nil {
		t.Fatal(err)
	}
	due, err := st.RefreshYouTrackFields(ctx, time.Hour, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("fresh cache should not be due: %v %v", due, err)
	}
	if err := st.PutYouTrackFieldCache(ctx, YouTrackFieldCache{TaskID: x.ID, Priority: 3, Lane: "handoffkeep", FetchedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	due, err = st.RefreshYouTrackFields(ctx, time.Hour, 10)
	if err != nil || len(due) != 1 || due[0].TaskID != x.ID {
		t.Fatalf("old cache should be due: %+v %v", due, err)
	}
	if err := st.MarkYouTrackFieldCacheStale(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	due, err = st.RefreshYouTrackFields(ctx, time.Hour, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("stale cache should be due: %+v %v", due, err)
	}
	caches, err := st.GetYouTrackFieldCaches(ctx, []int64{x.ID})
	if err != nil || !caches[x.ID].Stale || caches[x.ID].Priority != 3 {
		t.Fatalf("caches=%+v err=%v", caches, err)
	}
}

func TestApplyYouTrackActivityTwiceAppliesOnce(t *testing.T) {
	st := schemaStore(t, "youtrack_apply")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "apply target")
	priority := 4
	in := YouTrackActivity{ID: "a-1", TaskID: x.ID, IssueID: "3-1", Priority: &priority}
	result, err := st.ApplyYouTrackActivity(ctx, in)
	if err != nil || result.Outcome != YouTrackOutcomeApplied {
		t.Fatalf("first apply: %+v %v", result, err)
	}
	got, found, err := st.GetTask(ctx, x.ID)
	if err != nil || !found || got.Priority != 4 {
		t.Fatalf("task=%+v found=%v err=%v", got, found, err)
	}
	replay, err := st.ApplyYouTrackActivity(ctx, in)
	if err != nil || replay.Outcome != YouTrackOutcomeDuplicate || replay.Applied {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	var events int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND source='youtrack.activity'`, x.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events=%d want 1", events)
	}
}

func TestApplyYouTrackStateRefusalRecordsReasonAndRevertsLater(t *testing.T) {
	st := schemaStore(t, "youtrack_refuse")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "refusal target")
	if _, err := st.LinkYouTrackIssue(ctx, x.ID, "4-1", "HK-4", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyYouTrackActivity(ctx, YouTrackActivity{ID: "a-refuse", TaskID: x.ID, IssueID: "4-1", State: StateDeveloping})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != YouTrackOutcomeRejected || result.RevertField != "state" || result.RevertValue != StateBacklog {
		t.Fatalf("result=%+v", result)
	}
	got, _, _ := st.GetTask(ctx, x.ID)
	if got.State != StateBacklog {
		t.Fatalf("state=%s want backlog", got.State)
	}
	pending, err := st.PendingYouTrackReverts(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].IssueID != "4-1" {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := st.MarkYouTrackRevertDone(ctx, "a-refuse"); err != nil {
		t.Fatal(err)
	}
	pending, err = st.PendingYouTrackReverts(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after done=%+v err=%v", pending, err)
	}
}

func TestApplyYouTrackMergedUsesRefsPRAsEvidence(t *testing.T) {
	st := schemaStore(t, "youtrack_merged")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "merge target")
	for _, step := range []TransitionInput{
		{To: StateDeveloping, By: "test", Note: "start", NoJob: "test"},
		{To: StateReviewing, By: "test", Note: "review"},
	} {
		if _, err := st.TransitionTaskWith(ctx, x.ID, step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.TransitionTaskWith(ctx, x.ID, TransitionInput{To: StateReviewing, By: "test", Note: "pr", Refs: &TaskRefs{PR: "https://example.test/pr/1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LinkYouTrackIssue(ctx, x.ID, "5-1", "HK-5", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyYouTrackActivity(ctx, YouTrackActivity{ID: "a-merged", TaskID: x.ID, IssueID: "5-1", State: StateMerged})
	if err != nil || result.Outcome != YouTrackOutcomeApplied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, _, _ := st.GetTask(ctx, x.ID)
	if got.State != StateMerged || got.Refs.Closure == nil || got.Refs.Closure.Evidence != "https://example.test/pr/1" {
		t.Fatalf("task=%+v closure=%+v", got, got.Refs.Closure)
	}
}

func TestDuePushTasksSkipsUnchangedLinkedAndSettledUnlinked(t *testing.T) {
	st := schemaStore(t, "youtrack_push")
	ctx := context.Background()
	linked := youtrackTask(t, st, "handoffkeep", "linked")
	settled := youtrackTask(t, st, "handoffkeep", "settled")
	if _, err := st.LinkYouTrackIssue(ctx, linked.ID, "6-1", "HK-6", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkYouTrackTaskPushed(ctx, linked.ID, linked.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTaskWith(ctx, settled.ID, TransitionInput{To: StateDropped, By: "test", Note: "cancel"}); err != nil {
		t.Fatal(err)
	}
	dirty := youtrackTask(t, st, "handoffkeep", "dirty")
	tasks, err := st.DueYouTrackPushTasks(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, task := range tasks {
		seen[task.ID] = true
	}
	if seen[linked.ID] || seen[settled.ID] || !seen[dirty.ID] {
		t.Fatalf("due tasks=%+v (linked=%d settled=%d dirty=%d)", tasks, linked.ID, settled.ID, dirty.ID)
	}
}
