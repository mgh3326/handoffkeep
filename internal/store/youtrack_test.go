package store

import (
	"context"
	"fmt"
	"reflect"
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
	// New tasks need a project; older rows may have none.
	legacy := youtrackTask(t, st, "lane-n", "five")
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET project=NULL WHERE id=$1`, legacy.ID); err != nil {
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
