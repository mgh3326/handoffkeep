package store

import (
	"errors"
	"fmt"
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
