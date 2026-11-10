package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func youtrackMirrorInput(taskID int64, issueID, display, tenant, role string, at time.Time) YouTrackMirrorInput {
	return YouTrackMirrorInput{TaskID: taskID, IssueID: issueID, DisplayRef: display, Tenant: tenant, Role: role, SyncedAt: at}
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
		for _, table := range []string{"youtrack_projects", "youtrack_outbox", "youtrack_cursor", "youtrack_comment_links", "youtrack_field_cache", "youtrack_processed_activities"} {
			var exists bool
			if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
				t.Fatalf("open %d: table %s exists=%v err=%v", i, table, exists, err)
			}
		}
		project := "handoffkeep"
		task, err := st.CreateTask(ctx, Task{Lane: "handoffkeep", Title: "origin probe", Kind: "implement", CreatedBy: "test", Project: &project})
		if err != nil {
			t.Fatalf("open %d: create: %v", i, err)
		}
		if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(task.ID, fmt.Sprintf("%d-1", i+1), fmt.Sprintf("HK-%d", i+1), "tenant", "mirror", time.Now().UTC())); err != nil {
			t.Fatalf("open %d: mirror: %v", i, err)
		}
		st.Close()
	}
}

func TestYouTrackMirrorIsIdempotentAndConflicts(t *testing.T) {
	st := schemaStore(t, "youtrack_mirror")
	ctx := context.Background()
	a := youtrackTask(t, st, "handoffkeep", "first")
	b := youtrackTask(t, st, "handoffkeep", "second")
	now := time.Now().UTC()
	mirror, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(a.ID, "1-1", "HK-1", "tenant", "mirror", now))
	if err != nil || mirror.TaskID != a.ID || mirror.IssueID != "1-1" || mirror.DisplayRef != "HK-1" || mirror.Role != "mirror" {
		t.Fatalf("mirror=%+v err=%v", mirror, err)
	}
	if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(a.ID, "1-1", "HK-1", "tenant", "mirror", now.Add(time.Second))); err != nil {
		t.Fatalf("idempotent relink: %v", err)
	}
	if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(b.ID, "1-1", "HK-1", "tenant", "mirror", now)); err != ErrYouTrackMirrorConflict {
		t.Fatalf("reusing an issue: err=%v", err)
	}
	found, ok, err := st.FindYouTrackMirrorByIssueID(ctx, "1-1")
	if err != nil || !ok || found.TaskID != a.ID {
		t.Fatalf("find by issue: %+v ok=%v err=%v", found, ok, err)
	}
	// One issue per task: a new issue id moves the same row.
	moved, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(a.ID, "1-2", "HK-2", "tenant", "mirror", now))
	if err != nil || moved.IssueID != "1-2" || moved.DisplayRef != "HK-2" {
		t.Fatalf("moved=%+v err=%v", moved, err)
	}
	if _, ok, _ := st.FindYouTrackMirrorByIssueID(ctx, "1-1"); ok {
		t.Fatal("the old issue id still resolves after a move")
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

func TestYouTrackFieldCacheSourceAndStale(t *testing.T) {
	st := schemaStore(t, "youtrack_cache")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "cache target")
	if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(x.ID, "2-1", "HK-2", "tenant", "mirror", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.PutYouTrackFieldCache(ctx, YouTrackFieldCache{TaskID: x.ID, Priority: 3, Lane: "handoffkeep", Source: YouTrackSourceYouTrack, FetchedAt: now}); err != nil {
		t.Fatal(err)
	}
	caches, err := st.GetYouTrackFieldCaches(ctx, []int64{x.ID})
	if err != nil || caches[x.ID].Source != YouTrackSourceYouTrack || caches[x.ID].Priority != 3 {
		t.Fatalf("caches=%+v err=%v", caches, err)
	}
	// An hk write keeps the value but records hk as the source.
	if err := st.SetYouTrackCachedPriority(ctx, x.ID, 4, now); err != nil {
		t.Fatal(err)
	}
	cache, _, _ := st.GetYouTrackFieldCache(ctx, x.ID)
	if cache.Priority != 4 || cache.Source != YouTrackSourceHK {
		t.Fatalf("cache after hk write=%+v", cache)
	}
	if err := st.MarkYouTrackFieldCacheStale(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	cache, _, _ = st.GetYouTrackFieldCache(ctx, x.ID)
	if !cache.Stale {
		t.Fatalf("cache not stale: %+v", cache)
	}
	// An hk write updates its own column but must not clear the stale flag.
	if err := st.SetYouTrackCachedPriority(ctx, x.ID, 6, now); err != nil {
		t.Fatal(err)
	}
	cache, _, _ = st.GetYouTrackFieldCache(ctx, x.ID)
	if !cache.Stale {
		t.Fatalf("an hk write cleared stale: %+v", cache)
	}
	refreshed, err := st.RefreshYouTrackFields(ctx, 10)
	if err != nil || len(refreshed) != 1 || refreshed[0].TaskID != x.ID {
		t.Fatalf("refresh=%+v err=%v", refreshed, err)
	}
}

func TestYouTrackOutboxSupersedesAndReschedules(t *testing.T) {
	st := schemaStore(t, "youtrack_outbox")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "outbox target")
	if err := st.EnqueueYouTrack(ctx, x.ID, YouTrackOpPriority, "3"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueYouTrack(ctx, x.ID, YouTrackOpPriority, "5"); err != nil {
		t.Fatal(err)
	}
	items, err := st.DueYouTrackOutbox(ctx, 10)
	if err != nil || len(items) != 1 || items[0].Value != "5" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if err := st.RescheduleYouTrackOutbox(ctx, items[0].ID, items[0].Value, 1, time.Now().UTC().Add(time.Hour), "boom"); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.DueYouTrackOutbox(ctx, 10); len(due) != 0 {
		t.Fatalf("a future item is due: %+v", due)
	}
	if err := st.RescheduleYouTrackOutbox(ctx, items[0].ID, items[0].Value, 2, time.Now().UTC().Add(-time.Second), "boom"); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.DueYouTrackOutbox(ctx, 10); len(due) != 1 || due[0].Attempts != 2 {
		t.Fatalf("rescheduled=%+v", due)
	}
	// A newer write updates the same row in place. A drain that sent the old
	// value must not close or back off the newer one.
	if err := st.EnqueueYouTrack(ctx, x.ID, YouTrackOpPriority, "7"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkYouTrackOutboxDone(ctx, items[0].ID, "5"); err != nil {
		t.Fatal(err)
	}
	due, err := st.DueYouTrackOutbox(ctx, 10)
	if err != nil || len(due) != 1 || due[0].Value != "7" {
		t.Fatalf("a superseded row was closed: %+v err=%v", due, err)
	}
	if err := st.RescheduleYouTrackOutbox(ctx, items[0].ID, "5", 3, time.Now().UTC().Add(time.Hour), "boom"); err != nil {
		t.Fatal(err)
	}
	due, err = st.DueYouTrackOutbox(ctx, 10)
	if err != nil || len(due) != 1 || due[0].Value != "7" || due[0].Attempts != 0 {
		t.Fatalf("a superseded row was rescheduled: %+v err=%v", due, err)
	}
	if err := st.MarkYouTrackOutboxDone(ctx, items[0].ID, "7"); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.DueYouTrackOutbox(ctx, 10); len(due) != 0 {
		t.Fatalf("done item is due: %+v", due)
	}
	// A new write after the done row is a new pending row.
	if err := st.EnqueueYouTrack(ctx, x.ID, YouTrackOpLane, "rstack"); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.DueYouTrackOutbox(ctx, 10); len(due) != 1 || due[0].Op != YouTrackOpLane {
		t.Fatalf("second item=%+v", due)
	}
}

func TestYouTrackProjectMapping(t *testing.T) {
	st := schemaStore(t, "youtrack_projects")
	ctx := context.Background()
	projects, err := st.ListYouTrackProjects(ctx)
	if err != nil || len(projects) < 2 {
		t.Fatalf("projects=%+v err=%v", projects, err)
	}
	if p, ok, err := st.YouTrackProjectFor(ctx, "handoffkeep"); err != nil || !ok || p.ShortName != "HK" {
		t.Fatalf("handoffkeep=%+v ok=%v err=%v", p, ok, err)
	}
	if p, ok, err := st.YouTrackProjectFor(ctx, "unmapped-project"); err != nil || !ok || p.ShortName != "OTHER" {
		t.Fatalf("fallback=%+v ok=%v err=%v", p, ok, err)
	}
	if p, ok, err := st.YouTrackProjectByShortName(ctx, "HK"); err != nil || !ok || p.HKProject != "handoffkeep" {
		t.Fatalf("by short name=%+v ok=%v err=%v", p, ok, err)
	}
	if err := st.PutYouTrackProject(ctx, YouTrackProject{HKProject: "new-project", ProjectID: "0-9", ShortName: "NP"}); err != nil {
		t.Fatal(err)
	}
	if p, ok, _ := st.YouTrackProjectFor(ctx, "new-project"); !ok || p.ProjectID != "0-9" {
		t.Fatalf("new mapping=%+v ok=%v", p, ok)
	}
	if err := st.SetYouTrackProjectID(ctx, "new-project", "0-10"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := st.YouTrackProjectFor(ctx, "new-project"); p.ProjectID != "0-10" {
		t.Fatalf("id update=%+v", p)
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
	if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(x.ID, "4-1", "HK-4", "tenant", "mirror", time.Now().UTC())); err != nil {
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
		{To: StateReviewing, By: "test", Note: "review", Refs: &TaskRefs{PR: "https://example.test/pr/1"}},
	} {
		if _, err := st.TransitionTaskWith(ctx, x.ID, step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(x.ID, "5-1", "HK-5", "tenant", "mirror", time.Now().UTC())); err != nil {
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

func TestDuePushTasksSkipsUnchangedMirroredAndSettledUnmirrored(t *testing.T) {
	st := schemaStore(t, "youtrack_push")
	ctx := context.Background()
	mirrored := youtrackTask(t, st, "handoffkeep", "mirrored")
	settled := youtrackTask(t, st, "handoffkeep", "settled")
	now := time.Now().UTC()
	if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(mirrored.ID, "6-1", "HK-6", "tenant", "mirror", now)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkYouTrackTaskSynced(ctx, mirrored.ID, mirrored.UpdatedAt); err != nil {
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
	if seen[mirrored.ID] || seen[settled.ID] || !seen[dirty.ID] {
		t.Fatalf("due tasks=%+v (mirrored=%d settled=%d dirty=%d)", tasks, mirrored.ID, settled.ID, dirty.ID)
	}
}

func TestApplyYouTrackActivityKeepsAnUnpushedTaskDue(t *testing.T) {
	st := schemaStore(t, "youtrack_watermark")
	ctx := context.Background()
	x := youtrackTask(t, st, "handoffkeep", "watermark target")
	pushed := x.UpdatedAt.UTC()
	if _, err := st.UpsertYouTrackMirror(ctx, youtrackMirrorInput(x.ID, "6-2", "HK-6", "tenant", "mirror", pushed)); err != nil {
		t.Fatal(err)
	}
	// An hk change lands after the push, so the task is due again.
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET priority=3,updated_at=updated_at + interval '1 minute' WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackFieldCache(ctx, YouTrackFieldCache{TaskID: x.ID, Priority: 1, Lane: "handoffkeep", Source: YouTrackSourceYouTrack, FetchedAt: pushed}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueYouTrack(ctx, x.ID, YouTrackOpPriority, "9"); err != nil {
		t.Fatal(err)
	}
	priority := 4
	if _, err := st.ApplyYouTrackActivity(ctx, YouTrackActivity{ID: "a-watermark", TaskID: x.ID, IssueID: "6-2", Priority: &priority}); err != nil {
		t.Fatal(err)
	}
	if cache, _, _ := st.GetYouTrackFieldCache(ctx, x.ID); cache.Priority != 4 {
		t.Fatalf("the pull did not keep the cache in step: %+v", cache)
	}
	if due, _ := st.DueYouTrackOutbox(ctx, 10); len(due) != 0 {
		t.Fatalf("a stale export survived the operator change: %+v", due)
	}
	mirror, _, err := st.GetYouTrackMirror(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.SyncedAt == nil || !mirror.SyncedAt.Equal(pushed) {
		t.Fatalf("an activity advanced the watermark past an unpushed change: synced_at=%v pushed=%v", mirror.SyncedAt, pushed)
	}
	due, err := st.DueYouTrackPushTasks(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ID != x.ID {
		t.Fatalf("an unpushed task is not due: %+v err=%v", due, err)
	}

	// Once the task has been pushed, an activity advances the watermark.
	task, _, _ := st.GetTask(ctx, x.ID)
	if err := st.MarkYouTrackTaskSynced(ctx, x.ID, task.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	next := 5
	if _, err := st.ApplyYouTrackActivity(ctx, YouTrackActivity{ID: "a-watermark-2", TaskID: x.ID, IssueID: "6-2", Priority: &next}); err != nil {
		t.Fatal(err)
	}
	mirror, _, _ = st.GetYouTrackMirror(ctx, x.ID)
	if mirror.SyncedAt == nil || mirror.SyncedAt.Equal(pushed) {
		t.Fatalf("a pushed task's watermark did not advance: %+v", mirror.SyncedAt)
	}
}

func TestCreateYouTrackIssueRefusesSecretLikeTitle(t *testing.T) {
	st := schemaStore(t, "youtrack_secret_title")
	ctx := context.Background()
	title := "leaked ghp_" + strings.Repeat("a", 36)
	in := YouTrackActivity{ID: "a-secret", IssueID: "6-9", NewIssue: &YouTrackNewIssue{
		IssueID: "6-9", ReadableID: "HK-9", Title: title, Lane: "operator-desk", Kind: "implement", Project: "handoffkeep", Tenant: "tenant",
	}}
	result, err := st.ApplyYouTrackActivity(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != YouTrackOutcomeSkipped {
		t.Fatalf("outcome=%+v", result)
	}
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title=$1`, title).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("a secret-shaped title was stored as a task")
	}
	var outcome, refusal string
	if err := st.pool.QueryRow(ctx, `SELECT outcome,refusal FROM youtrack_processed_activities WHERE activity_id=$1`, "a-secret").Scan(&outcome, &refusal); err != nil {
		t.Fatal(err)
	}
	if outcome != YouTrackOutcomeSkipped || refusal == "" {
		t.Fatalf("recorded outcome=%q refusal=%q", outcome, refusal)
	}
}

func TestTruncateTextKeepsRunesWhole(t *testing.T) {
	v := strings.Repeat("가", 300) // 900 bytes; 512 is not a rune boundary.
	got := truncateText(v, 512)
	if len(got) > 512 || !utf8.ValidString(got) || !strings.HasPrefix(v, got) {
		t.Fatalf("truncateText split a rune: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
	if got := truncateText("short", 512); got != "short" {
		t.Fatalf("short value changed: %q", got)
	}
}

func TestYouTrackSchemaVocabulary(t *testing.T) {
	st := schemaStore(t, "youtrack_schema_vocab")
	ctx := context.Background()
	youtrackTask(t, st, "lane-b", "one")
	youtrackTask(t, st, "lane-a", "two")
	youtrackTask(t, st, "lane-b", "three")
	if _, err := st.AddTaskProject(ctx, "mergewake", "test"); err != nil {
		t.Fatal(err)
	}
	other := "mergewake"
	if _, err := st.CreateTask(ctx, Task{Lane: "lane-m", Title: "four", Kind: "implement", CreatedBy: "test", Project: &other}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, Task{Lane: "lane-n", Title: "five", Kind: "implement", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	lanes, err := st.ListTaskLanes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"": {"lane-n"}, "handoffkeep": {"lane-a", "lane-b"}, "mergewake": {"lane-m"}}
	if !reflect.DeepEqual(lanes, want) {
		t.Fatalf("lanes=%v, want %v (each project's own lanes, no project under \"\")", lanes, want)
	}
	for i, name := range []string{"second", "first"} {
		if _, err := st.SaveMilestone(ctx, ProjectMilestone{Project: "handoffkeep", Name: name, Status: "planned", SortOrder: 2 - i}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListMilestoneNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names := got["handoffkeep"]; len(names) != 2 || names[0] != "first" || names[1] != "second" {
		t.Fatalf("milestones=%v, want [first second] by sort_order", got)
	}
}
