package store

import (
	"errors"
	"testing"
)

func TestDocumentTaskAndOrigin(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	taskA := seedTaskRow(t, pool, "doc-origin", "task a", "backlog")
	taskB := seedTaskRow(t, pool, "doc-origin", "task b", "backlog")
	put := func(key, body string, task *int64, class string) (Document, bool) {
		t.Helper()
		d, changed, err := s.PutDocument(ctx, Document{Key: key, Kind: "design", Body: body, CreatedBy: "tester", TaskID: task, OriginClass: class})
		if err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		return d, changed
	}

	// An old-shape put leaves both new fields unset.
	old, changed := put("doc-origin/old", "body", nil, "")
	got, found, err := s.GetDocument(ctx, old.Key)
	if err != nil || !found || !changed || got.TaskID != nil || got.OriginClass != "" {
		t.Fatalf("old-shape put: %+v changed=%t err=%v", got, changed, err)
	}
	var nullClass bool
	if err := pool.QueryRow(ctx, `SELECT origin_class IS NULL AND task_id IS NULL FROM documents WHERE key=$1`, old.Key).Scan(&nullClass); err != nil || !nullClass {
		t.Fatalf("old-shape row stores NULLs: %t err=%v", nullClass, err)
	}

	// task_id and origin_class are stored and read back by key and by id.
	d, changed := put("doc-origin/a", "body a", &taskA, "job")
	if !changed || d.TaskID == nil || *d.TaskID != taskA || d.OriginClass != "job" {
		t.Fatalf("put: %+v changed=%t", d, changed)
	}
	byKey, _, _ := s.GetDocument(ctx, d.Key)
	byID, _, _ := s.GetDocumentByID(ctx, d.ID)
	for name, x := range map[string]Document{"key": byKey, "id": byID} {
		if x.TaskID == nil || *x.TaskID != taskA || x.OriginClass != "job" || x.Kind != "design" {
			t.Errorf("get by %s: %+v", name, x)
		}
	}

	// A re-put of the same body that omits them keeps them (unchanged).
	again, changed := put(d.Key, "body a", nil, "")
	if changed {
		t.Fatalf("identical re-put without the new fields changed the row: %+v", again)
	}
	if again.TaskID == nil || *again.TaskID != taskA || again.OriginClass != "job" {
		t.Fatalf("unchanged re-put answered task_id=%v origin_class=%q, want the stored %d and job", again.TaskID, again.OriginClass, taskA)
	}
	// A re-put with a different body that omits them keeps them too.
	edited, changed := put(d.Key, "body a v2", nil, "")
	if !changed || edited.TaskID == nil || *edited.TaskID != taskA || edited.OriginClass != "job" {
		t.Fatalf("edited re-put: %+v changed=%t", edited, changed)
	}
	// A re-put with the same values is unchanged.
	if _, changed = put(d.Key, "body a v2", &taskA, "job"); changed {
		t.Fatal("re-put with matching task and class changed the row")
	}

	// A re-put of the same body that only adds task_id changes the row.
	_, _ = put("doc-origin/b", "body b", nil, "")
	added, changed := put("doc-origin/b", "body b", &taskB, "")
	if !changed || added.TaskID == nil || *added.TaskID != taskB {
		t.Fatalf("adding task_id: %+v changed=%t", added, changed)
	}
	// Likewise origin_class alone.
	classed, changed := put("doc-origin/b", "body b", nil, "authored")
	if !changed || classed.OriginClass != "authored" || classed.TaskID == nil || *classed.TaskID != taskB {
		t.Fatalf("adding origin_class: %+v changed=%t", classed, changed)
	}

	// Refusals: an unknown class, a missing task, a non-positive task.
	if _, _, err := s.PutDocument(ctx, Document{Key: "doc-origin/bad", Kind: "note", Body: "x", CreatedBy: "tester", OriginClass: "robot"}); err == nil {
		t.Error("unknown origin_class accepted")
	}
	missing := int64(999999999)
	if _, _, err := s.PutDocument(ctx, Document{Key: "doc-origin/bad", Kind: "note", Body: "x", CreatedBy: "tester", TaskID: &missing}); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("missing task: err=%v, want ErrTaskNotFound", err)
	}
	zero := int64(0)
	if _, _, err := s.PutDocument(ctx, Document{Key: "doc-origin/bad", Kind: "note", Body: "x", CreatedBy: "tester", TaskID: &zero}); err == nil {
		t.Error("task_id 0 accepted")
	}
	if _, found, _ := s.GetDocument(ctx, "doc-origin/bad"); found {
		t.Error("a refused put left a document behind")
	}
	for _, kind := range []string{"decision", "research", "design", "policy", "evidence"} {
		if _, _, err := s.PutDocument(ctx, Document{Key: "doc-origin/kind-" + kind, Kind: kind, Body: "x", CreatedBy: "tester"}); err != nil {
			t.Errorf("kind %s: %v", kind, err)
		}
	}
	if _, _, err := s.PutDocument(ctx, Document{Key: "doc-origin/kind-bogus", Kind: "bogus", Body: "x", CreatedBy: "tester"}); err == nil {
		t.Error("kind bogus accepted")
	}

	// The list filter returns only that task's documents, with the new fields.
	listed, err := s.ListDocuments(ctx, "doc-origin/", "", "", taskA, 100)
	if err != nil || len(listed) != 1 || listed[0].Key != "doc-origin/a" || listed[0].TaskID == nil || *listed[0].TaskID != taskA || listed[0].OriginClass != "job" {
		t.Fatalf("list task a: %+v err=%v", listed, err)
	}
	listed, err = s.ListDocuments(ctx, "doc-origin/", "", "", taskB, 100)
	if err != nil || len(listed) != 1 || listed[0].Key != "doc-origin/b" {
		t.Fatalf("list task b: %+v err=%v", listed, err)
	}
	listed, err = s.ListDocuments(ctx, "doc-origin/", "design", "", 0, 1000)
	if err != nil || len(listed) < 4 {
		t.Fatalf("list without a task filter: %d docs err=%v", len(listed), err)
	}
	if _, err := s.ListDocuments(ctx, "", "", "", -1, 10); err == nil {
		t.Error("negative task filter accepted")
	}
}
