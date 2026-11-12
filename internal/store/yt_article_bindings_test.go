package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// ytArticleDoc reads document id's candidate as YTArticleDocs lists it.
func ytArticleDoc(t *testing.T, s *Store, id int64) YTArticleDoc {
	t.Helper()
	docs, err := s.YTArticleDocs(t.Context(), "", id-1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].DocumentID != id {
		t.Fatalf("YTArticleDocs after %d = %+v, want document %d", id-1, docs, id)
	}
	return docs[0]
}

// AddYTArticleBinding binds once, repeats as a no-op, and refuses a second
// different binding for a document, another document's article, a document
// changed since verification and an excluded document, without writing.
func TestAddYTArticleBinding(t *testing.T) {
	s := schemaStore(t, "ytab")
	ctx := t.Context()
	const tenant = "yt.example"
	if _, err := s.AddTaskProject(ctx, "ytab", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytab", ProjectID: "0-7", ShortName: "YTAB"}); err != nil {
		t.Fatal(err)
	}
	project := "ytab"
	task, err := s.CreateTask(ctx, Task{Lane: "ytab-lane", Title: "articles", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	put := func(key, body string, taskID *int64) int64 {
		t.Helper()
		d, _, err := s.PutDocument(ctx, Document{Key: key, Kind: "note", Body: body, CreatedBy: "test", TaskID: taskID})
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	shared := put("note/ytab/shared", "shared body", nil)
	owned := put("note/ytab/owned", "owned body", &task.ID)
	report := put("reports/ytab-job/report.md", "report body", &task.ID)

	sd := ytArticleDoc(t, s, shared)
	if sd.Bucket != YTArticleBucketShared || sd.BoundTenant != "" || sd.BodyBytes != len("shared body") {
		t.Fatalf("shared candidate = %+v", sd)
	}
	od := ytArticleDoc(t, s, owned)
	if od.Bucket != "ytab" || od.ShortName != "YTAB" || od.YTProjectID != "0-7" || od.TaskID != task.ID {
		t.Fatalf("owned candidate = %+v", od)
	}
	if got, err := s.YTArticleDocs(ctx, "ytab", 0, 10); err != nil || len(got) != 2 || got[0].DocumentID != owned || got[1].DocumentID != report {
		t.Fatalf("YTArticleDocs(ytab) = %+v, %v", got, err)
	}

	b := YTArticleBinding{DocumentID: shared, Tenant: tenant, ArticleID: "167-1", ArticleReadable: "SHRDOC-A-1", ProjectID: "0-9", ContentSHA: sd.SHA256, BoundBy: "test"}
	got, err := s.AddYTArticleBinding(ctx, b, sd.YTArticleCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != YTBindingImportVerified || got.ArticleID != "167-1" || got.ContentSHA != sd.SHA256 || got.NeedsUpdate || got.Part != 0 || got.Generation != 0 || got.BoundAt.IsZero() {
		t.Fatalf("binding = %+v", got)
	}
	again, err := s.AddYTArticleBinding(ctx, b, sd.YTArticleCandidate)
	if err != nil || again != got {
		t.Fatalf("same binding again = %+v, %v; want the stored %+v", again, err, got)
	}
	if d := ytArticleDoc(t, s, shared); d.BoundTenant != tenant {
		t.Fatalf("bound tenant = %q", d.BoundTenant)
	}

	// A second, different binding for the document is refused and the
	// stored one stays.
	other := b
	other.ArticleID, other.NeedsUpdate = "167-2", true
	if _, err := s.AddYTArticleBinding(ctx, other, sd.YTArticleCandidate); !errors.Is(err, ErrYTArticleBindingConflict) {
		t.Fatalf("different binding: err = %v, want ErrYTArticleBindingConflict", err)
	}
	if have, found, err := s.YTArticleBinding(ctx, shared); err != nil || !found || have != got {
		t.Fatalf("after the refusal the binding is %+v (found %v, %v), want %+v", have, found, err, got)
	}
	// Another document cannot take the same article.
	taken := YTArticleBinding{DocumentID: owned, Tenant: tenant, ArticleID: "167-1", ProjectID: "0-7", BoundBy: "test", NeedsUpdate: true}
	if _, err := s.AddYTArticleBinding(ctx, taken, od.YTArticleCandidate); !errors.Is(err, ErrYTArticleBindingConflict) {
		t.Fatalf("article of another document: err = %v, want ErrYTArticleBindingConflict", err)
	}
	// A document changed since its verification is refused.
	put("note/ytab/owned", "owned body, edited", &task.ID)
	fresh := taken
	fresh.ArticleID = "167-3"
	if _, err := s.AddYTArticleBinding(ctx, fresh, od.YTArticleCandidate); !errors.Is(err, ErrYTArticleBindingConflict) || !strings.Contains(err.Error(), "changed after verification") {
		t.Fatalf("changed document: err = %v", err)
	}
	if _, found, err := s.YTArticleBinding(ctx, owned); err != nil || found {
		t.Fatalf("changed document got a binding (found %v, %v)", found, err)
	}
	// An excluded document (a task's report) is refused.
	rd := ytArticleDoc(t, s, report)
	if !YTArticleExcluded(rd.Key, rd.TaskID) {
		t.Fatalf("%s should be excluded", rd.Key)
	}
	excl := YTArticleBinding{DocumentID: report, Tenant: tenant, ArticleID: "167-4", ProjectID: "0-7", BoundBy: "test"}
	if _, err := s.AddYTArticleBinding(ctx, excl, rd.YTArticleCandidate); err == nil {
		t.Fatal("excluded document was bound")
	}
	// The verified copy is fresh now: the edited document binds.
	od = ytArticleDoc(t, s, owned)
	if _, err := s.AddYTArticleBinding(ctx, fresh, od.YTArticleCandidate); err != nil {
		t.Fatal(err)
	}
}

// AddYTArticleBinding holds the document, its task and the project mapping
// from its comparison to its commit (finding r4544954725): a body edit or a
// project move in flight makes it wait, and once that commits it refuses
// instead of publishing the old verification.
func TestAddYTArticleBindingWaitsForSourceChanges(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytab_lock")
	ctx := t.Context()
	s, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	for _, p := range []string{"ytabl", "ytabl-other"} {
		if _, err := s.AddTaskProject(ctx, p, "test"); err != nil {
			t.Fatal(err)
		}
		if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: p, ProjectID: "0-" + p, ShortName: "YTABL"}); err != nil {
			t.Fatal(err)
		}
	}
	project := "ytabl"
	task, err := s.CreateTask(ctx, Task{Lane: "ytabl-lane", Title: "articles", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	for i, change := range []string{
		`UPDATE documents SET body='edited', sha256=repeat('e', 64) WHERE id=$1`,
		`UPDATE tasks SET project='ytabl-other' WHERE id=(SELECT task_id FROM documents WHERE id=$1)`,
		`UPDATE youtrack_projects SET yt_project_id='0-moved' WHERE hk_project=(SELECT t.project FROM documents d JOIN tasks t ON t.id=d.task_id WHERE d.id=$1)`,
	} {
		d, _, err := s.PutDocument(ctx, Document{Key: "note/ytabl/" + strconv.Itoa(i), Kind: "note", Body: "body", CreatedBy: "test", TaskID: &task.ID})
		if err != nil {
			t.Fatal(err)
		}
		verified := ytArticleDoc(t, s, d.ID).YTArticleCandidate
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, change, d.ID); err != nil {
			t.Fatal(err)
		}
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.AddYTArticleBinding(ctx, YTArticleBinding{DocumentID: d.ID, Tenant: "yt.example", ArticleID: "a-" + strconv.Itoa(i), ProjectID: verified.YTProjectID, BoundBy: "test"}, verified)
			done <- err
		}()
		// Poll until the binding waits on the change's transaction; a
		// binding that finishes first did not lock what it compared.
		deadline := time.Now().Add(20 * time.Second)
		for waiting := false; !waiting; {
			select {
			case err := <-done:
				t.Fatalf("change %d: the binding finished (%v) while the change was uncommitted", i, err)
			default:
			}
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("change %d: the binding never waited on the change", i)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrYTArticleBindingConflict) {
			t.Fatalf("change %d: err = %v, want ErrYTArticleBindingConflict", i, err)
		}
		if _, found, err := s.YTArticleBinding(ctx, d.ID); err != nil || found {
			t.Fatalf("change %d: a binding was published (found %v, %v)", i, found, err)
		}
	}
}
