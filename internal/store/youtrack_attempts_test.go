package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// The attempt rows decide what the YouTrack queue scans return: a failed key
// waits out its backoff, a parked key waits for a mapping, and a mapping write
// makes parked keys due again.

func TestYouTrackPushScanHonoursAttemptState(t *testing.T) {
	st := schemaStore(t, "youtrack_attempts_push")
	ctx := context.Background()
	if err := st.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	failing := youtrackTask(t, st, "handoffkeep", "failing")
	parked := youtrackTask(t, st, "handoffkeep", "parked")
	passing := youtrackTask(t, st, "handoffkeep", "passing")
	if err := st.RecordYouTrackAttempt(ctx, YouTrackQueueTask, strconv.FormatInt(failing.ID, 10), "boom", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordYouTrackAttempt(ctx, YouTrackQueueTask, strconv.FormatInt(parked.ID, 10), "", YouTrackSkipProjectMissing); err != nil {
		t.Fatal(err)
	}
	due := dueTaskIDs(t, st)
	if due[failing.ID] || due[parked.ID] || !due[passing.ID] {
		t.Fatalf("due after failure and skip = %v (failing=%d parked=%d passing=%d)", due, failing.ID, parked.ID, passing.ID)
	}

	if err := st.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	due = dueTaskIDs(t, st)
	if !due[parked.ID] {
		t.Fatalf("a mapping write did not make the parked task due again: %v", due)
	}
	if due[failing.ID] {
		t.Fatal("a mapping write must not clear a backoff")
	}

	if err := st.RecordYouTrackAttempt(ctx, YouTrackQueueTask, strconv.FormatInt(passing.ID, 10), "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestYouTrackDocsDueFollowsDefaultProjectAndAttemptState(t *testing.T) {
	st := schemaStore(t, "youtrack_attempts_docs")
	ctx := context.Background()
	for _, project := range []string{"handoffkeep", "panewire"} {
		if err := st.PutYouTrackProject(ctx, YouTrackProject{HKProject: project, ShortName: project}); err != nil {
			t.Fatal(err)
		}
	}
	doc, _, err := st.PutDocument(ctx, Document{Key: "design/attempts", Kind: "design", Body: "# attempts\n\nbody", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	patterns := []string{"design/%"}
	if err := st.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "design/attempts", HKProject: "handoffkeep", ArticleID: "A-1", SourceSHA: doc.SHA256, Bytes: 1, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// The article is in handoffkeep, the document's default, but the
	// default is now panewire: the document is due.
	due, err := st.DueYouTrackDocs(ctx, patterns, "panewire", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Key != "design/attempts" {
		t.Fatalf("a document in another default project must be due, got %v", keysOf(due))
	}

	if err := st.RecordYouTrackAttempt(ctx, YouTrackQueueDoc, "design/attempts", "boom", ""); err != nil {
		t.Fatal(err)
	}
	due, err = st.DueYouTrackDocs(ctx, patterns, "panewire", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("a failed document must wait out its backoff, got %v", keysOf(due))
	}
	if err := st.RecordYouTrackAttempt(ctx, YouTrackQueueDoc, "design/attempts", "", ""); err != nil {
		t.Fatal(err)
	}
}

func dueTaskIDs(t *testing.T, st *Store) map[int64]bool {
	t.Helper()
	tasks, err := st.DueYouTrackPushTasks(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]bool{}
	for _, task := range tasks {
		out[task.ID] = true
	}
	return out
}
