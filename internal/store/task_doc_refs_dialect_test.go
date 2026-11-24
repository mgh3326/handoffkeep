package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// Dialect-neutral cases for task_doc_refs.go (registry: tasks_area_cases_test.go).
// They run on PostgreSQL and MySQL. A reference is a link the task cites; the
// document's binding (documents.task_id) is a separate owner link.

func init() {
	registerTaskAreaCase("doc_refs/add_list_remove_are_idempotent", docRefsCaseAddListRemove)
	registerTaskAreaCase("doc_refs/refuse_missing_document_and_task", docRefsCaseRefusals)
	registerTaskAreaCase("doc_refs/separate_from_the_binding", docRefsCaseSeparateFromBinding)
	registerTaskAreaCase("doc_refs/dots_request_records_cited_documents", docRefsCaseDotsRequest)
	registerTaskAreaCase("doc_refs/dots_cited_set_ignores_order_and_repeats", docRefsCaseDotsOrderAndRepeats)
}

// docRefCount counts the reference rows naming key, whatever task holds them.
// A reference left behind by a deleted document has no document to join to, so
// ListTaskDocRefs cannot see it; the row count can.
func docRefCount(t *testing.T, s *Store, key string) int64 {
	t.Helper()
	var n int64
	if err := s.db().QueryRow(t.Context(), s.q(`SELECT COUNT(*) FROM task_doc_refs WHERE doc_key=$1`, "SELECT COUNT(*) FROM task_doc_refs WHERE doc_key=?"), key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// docRefsProject registers a fresh project; new tasks must name one.
func docRefsProject(t *testing.T, s *Store) string {
	t.Helper()
	project := fmt.Sprintf("docref-%d", srcEvUnique())
	if _, err := s.AddTaskProject(t.Context(), project, "tester"); err != nil {
		t.Fatal(err)
	}
	return project
}

// docRefsTask files a backlog task in a fresh project with a unique lane and title.
func docRefsTask(t *testing.T, s *Store, title string) Task {
	t.Helper()
	n := srcEvUnique()
	project := docRefsProject(t, s)
	x, err := s.CreateTask(t.Context(), Task{Lane: fmt.Sprintf("docref-lane-%d", n), Title: title, Kind: "implement", Priority: 5, CreatedBy: "tester", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// docRefsDoc stores a design document under a unique key, bound to bindTask
// when it is non-nil.
func docRefsDoc(t *testing.T, s *Store, bindTask *int64) string {
	t.Helper()
	key := fmt.Sprintf("tbls/docref-%d/doc", srcEvUnique())
	if _, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "design", Body: "# schema", CreatedBy: "tester", TaskID: bindTask}); err != nil {
		t.Fatal(err)
	}
	return key
}

func docRefsCaseAddListRemove(t *testing.T, s *Store) {
	ctx := t.Context()
	task := docRefsTask(t, s, "verify schema")
	key := docRefsDoc(t, s, nil)

	changed, err := s.AddTaskDocRef(ctx, task.ID, key, "tester")
	if err != nil || !changed {
		t.Fatalf("first add: changed=%v err=%v, want changed", changed, err)
	}
	again, err := s.AddTaskDocRef(ctx, task.ID, key, "tester")
	if err != nil || again {
		t.Fatalf("duplicate add: changed=%v err=%v, want no change", again, err)
	}
	refs, err := s.ListTaskDocRefs(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Key != key || refs[0].Kind != "design" || refs[0].CreatedBy != "tester" || refs[0].TaskID != task.ID {
		t.Fatalf("list = %+v, want the one reference to %s", refs, key)
	}
	removed, err := s.RemoveTaskDocRef(ctx, task.ID, key)
	if err != nil || !removed {
		t.Fatalf("remove: changed=%v err=%v, want changed", removed, err)
	}
	gone, err := s.RemoveTaskDocRef(ctx, task.ID, key)
	if err != nil || gone {
		t.Fatalf("remove again: changed=%v err=%v, want no change", gone, err)
	}
	refs, err = s.ListTaskDocRefs(ctx, task.ID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("list after remove = %+v err=%v, want empty", refs, err)
	}
}

func docRefsCaseRefusals(t *testing.T, s *Store) {
	ctx := t.Context()
	task := docRefsTask(t, s, "refusals")
	if _, err := s.AddTaskDocRef(ctx, task.ID, fmt.Sprintf("tbls/absent-%d/doc", srcEvUnique()), "tester"); !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("add a missing document: err=%v, want ErrDocNotFound", err)
	}
	key := docRefsDoc(t, s, nil)
	if _, err := s.AddTaskDocRef(ctx, task.ID+1_000_000, key, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("add to an unknown task: err=%v, want ErrTaskNotFound", err)
	}
	if _, err := s.ListTaskDocRefs(ctx, task.ID+1_000_000); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("list an unknown task: err=%v, want ErrTaskNotFound", err)
	}
	if _, err := s.RemoveTaskDocRef(ctx, task.ID+1_000_000, key); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("remove from an unknown task: err=%v, want ErrTaskNotFound", err)
	}
	refs, err := s.ListTaskDocRefs(ctx, task.ID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("a refused add left refs %+v err=%v", refs, err)
	}
}

func docRefsCaseSeparateFromBinding(t *testing.T, s *Store) {
	ctx := t.Context()
	owner := docRefsTask(t, s, "owner")
	citer := docRefsTask(t, s, "citer")
	key := docRefsDoc(t, s, &owner.ID)
	if _, err := s.AddTaskDocRef(ctx, citer.ID, key, "tester"); err != nil {
		t.Fatal(err)
	}
	refs, err := s.ListTaskDocRefs(ctx, citer.ID)
	if err != nil || len(refs) != 1 || refs[0].Key != key {
		t.Fatalf("citer refs = %+v err=%v, want the bound document", refs, err)
	}
	ownerRefs, err := s.ListTaskDocRefs(ctx, owner.ID)
	if err != nil || len(ownerRefs) != 0 {
		t.Fatalf("owner refs = %+v err=%v, a binding is not a reference", ownerRefs, err)
	}
	doc, found, err := s.GetDocument(ctx, key)
	if err != nil || !found || doc.TaskID == nil || *doc.TaskID != owner.ID {
		t.Fatalf("binding changed: doc=%+v found=%v err=%v", doc, found, err)
	}
}

// docRefsCaseDotsOrderAndRepeats: the cited set is one set. The same keys in
// another order, with repeats, make the same digest, so an idempotent retry
// succeeds and the task keeps one reference per key (hk 6138).
func docRefsCaseDotsOrderAndRepeats(t *testing.T, s *Store) {
	ctx := t.Context()
	a := docRefsDoc(t, s, nil)
	b := docRefsDoc(t, s, nil)
	project := docRefsProject(t, s)
	idem := fmt.Sprintf("docref-order-%d", srcEvUnique())
	in := DotsRequestInput{Title: "verify schema", Body: "check the schema docs", RequesterLane: "docref-requester", Project: project, IdempotencyKey: idem}
	first := in
	first.DocKeys = []string{b, a, a}
	task, err := s.CreateDotsRequest(ctx, "tester", first)
	if err != nil {
		t.Fatal(err)
	}
	retry := in
	retry.DocKeys = []string{a, b}
	again, err := s.CreateDotsRequest(ctx, "tester", retry)
	if err != nil {
		t.Fatalf("retry with the same keys in another order: %v", err)
	}
	if again.ID != task.ID {
		t.Fatalf("retry returned task %d, want the first request's task %d", again.ID, task.ID)
	}
	refs, err := s.ListTaskDocRefs(ctx, task.ID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("refs = %+v err=%v, want one reference per key", refs, err)
	}
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.Key] = true
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("refs = %+v, want %s and %s", refs, a, b)
	}
}

func docRefsCaseDotsRequest(t *testing.T, s *Store) {
	ctx := t.Context()
	key := docRefsDoc(t, s, nil)
	project := docRefsProject(t, s)
	in := DotsRequestInput{Title: "verify schema", Body: "check the schema doc", RequesterLane: "docref-requester", Project: project, DocKeys: []string{key, key}}
	task, err := s.CreateDotsRequest(ctx, "tester", in)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := s.ListTaskDocRefs(ctx, task.ID)
	if err != nil || len(refs) != 1 || refs[0].Key != key {
		t.Fatalf("request refs = %+v err=%v, want one reference (duplicate keys collapse)", refs, err)
	}

	before := srcEvUnique()
	_, err = s.CreateDotsRequest(ctx, "tester", DotsRequestInput{Title: "cites a missing document", Body: "x", RequesterLane: "docref-requester", Project: project, DocKeys: []string{fmt.Sprintf("tbls/absent-%d/doc", before)}})
	if !errors.Is(err, ErrDocNotFound) {
		t.Fatalf("request citing a missing document: err=%v, want ErrDocNotFound", err)
	}
}

// TestCanonicalDocKeysIsOneSet: any order and any repeats of one set give the
// same sorted, distinct list, and the caller's slice is not reordered.
func TestCanonicalDocKeysIsOneSet(t *testing.T) {
	in := []string{"tbls/b/doc", "tbls/a/doc", "tbls/a/doc"}
	got := canonicalDocKeys(in)
	if want := []string{"tbls/a/doc", "tbls/b/doc"}; !slices.Equal(got, want) {
		t.Fatalf("canonicalDocKeys = %v, want %v", got, want)
	}
	if in[0] != "tbls/b/doc" || in[1] != "tbls/a/doc" {
		t.Fatalf("caller's slice was reordered: %v", in)
	}
	if canonicalDocKeys(nil) != nil {
		t.Fatal("canonicalDocKeys(nil) must stay nil")
	}
}
