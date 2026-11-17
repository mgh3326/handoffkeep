package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Dialect-neutral cases for task_origins.go (registry: tasks_area_cases_test.go).

func init() {
	registerTaskAreaCase("origins/add_refresh_and_unchanged", originsCaseAddRefresh)
	registerTaskAreaCase("origins/distinct_identities", originsCaseIdentities)
	registerTaskAreaCase("origins/refusals", originsCaseRefusals)
	registerTaskAreaCase("origins/list_and_find", originsCaseListFind)
	registerTaskAreaCase("origins/concurrent_add_creates_one_row", originsCaseConcurrent)
}

// originsLinearID is a fresh lowercase UUID, which is what a Linear origin's
// external_id must be.
func originsLinearID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func originsInput(t *testing.T) TaskOriginInput {
	return TaskOriginInput{Provider: OriginLinear, Namespace: fmt.Sprintf("ws-%d", srcEvUnique()), ExternalID: originsLinearID(t), DisplayRef: "MGH-151", URL: "https://linear.app/example/issue/MGH-151", Title: "an issue"}
}

func originsCaseAddRefresh(t *testing.T, s *Store) {
	ctx := t.Context()
	task := srcEvTask(t, s, "origin holder")
	in := originsInput(t)
	first, created, err := s.AddTaskOrigin(ctx, task.ID, "tester", in)
	if err != nil || !created {
		t.Fatalf("first add: created=%t err=%v", created, err)
	}
	if first.TaskID != task.ID || first.Provider != in.Provider || first.Namespace != in.Namespace || first.ExternalID != in.ExternalID ||
		first.DisplayRef != in.DisplayRef || first.URL != in.URL || first.TitleSnapshot != in.Title || first.CreatedBy != "tester" || first.ID < 1 {
		t.Fatalf("stored origin: %+v", first)
	}
	// The same values again: no new row, nothing refreshed.
	same, created, err := s.AddTaskOrigin(ctx, task.ID, "other", in)
	if err != nil || created || same.ID != first.ID || !same.SnapshotAt.Equal(first.SnapshotAt) || same.CreatedBy != "tester" {
		t.Fatalf("same add: created=%t err=%v origin=%+v", created, err, same)
	}
	// A given value that differs refreshes display_ref, url, the title and
	// snapshot_at, and keeps the row and its creator.
	moved := in
	moved.DisplayRef, moved.Title = "ROB-1514", "the issue, renamed"
	refreshed, created, err := s.AddTaskOrigin(ctx, task.ID, "other", moved)
	if err != nil || created || refreshed.ID != first.ID || refreshed.DisplayRef != "ROB-1514" || refreshed.TitleSnapshot != "the issue, renamed" ||
		refreshed.URL != in.URL || refreshed.CreatedBy != "tester" || !refreshed.CreatedAt.Equal(first.CreatedAt) || !refreshed.SnapshotAt.After(first.SnapshotAt) {
		t.Fatalf("refresh: created=%t err=%v origin=%+v first=%+v", created, err, refreshed, first)
	}
	// A value left out keeps the stored one, and an unchanged row answers as is.
	sparse := TaskOriginInput{Provider: in.Provider, Namespace: in.Namespace, ExternalID: in.ExternalID}
	kept, created, err := s.AddTaskOrigin(ctx, task.ID, "tester", sparse)
	if err != nil || created || kept.ID != first.ID || kept.DisplayRef != "ROB-1514" || kept.TitleSnapshot != "the issue, renamed" || kept.URL != in.URL ||
		!kept.SnapshotAt.Equal(refreshed.SnapshotAt) {
		t.Fatalf("sparse add: created=%t err=%v origin=%+v", created, err, kept)
	}
	got, err := s.ListTaskOrigins(ctx, task.ID)
	if err != nil || len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("list after refreshes: %+v err=%v", got, err)
	}
	// A first add with only the key stores NULLs, read back as empty.
	bare := TaskOriginInput{Provider: OriginGitHub, Namespace: "example/app", ExternalID: fmt.Sprintf("I_%d", srcEvUnique())}
	b, created, err := s.AddTaskOrigin(ctx, task.ID, "tester", bare)
	if err != nil || !created || b.DisplayRef != "" || b.URL != "" || b.TitleSnapshot != "" {
		t.Fatalf("bare add: created=%t err=%v origin=%+v", created, err, b)
	}
}

func originsCaseIdentities(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b := srcEvTask(t, s, "origin a"), srcEvTask(t, s, "origin b")
	in := originsInput(t)
	one, created, err := s.AddTaskOrigin(ctx, a.ID, "tester", in)
	if err != nil || !created {
		t.Fatalf("a: created=%t err=%v", created, err)
	}
	// The key is (provider, namespace, external_id, task): another task, or
	// another namespace, is another row.
	other, created, err := s.AddTaskOrigin(ctx, b.ID, "tester", in)
	if err != nil || !created || other.ID == one.ID || other.TaskID != b.ID {
		t.Fatalf("b: created=%t err=%v origin=%+v", created, err, other)
	}
	elsewhere := in
	elsewhere.Namespace = in.Namespace + "-2"
	third, created, err := s.AddTaskOrigin(ctx, a.ID, "tester", elsewhere)
	if err != nil || !created || third.ID == one.ID {
		t.Fatalf("other namespace: created=%t err=%v origin=%+v", created, err, third)
	}
	got, err := s.ListTaskOrigins(ctx, a.ID)
	if err != nil || len(got) != 2 || got[0].ID != one.ID || got[1].ID != third.ID {
		t.Fatalf("a's origins: %+v err=%v", got, err)
	}
}

func originsCaseRefusals(t *testing.T, s *Store) {
	ctx := t.Context()
	task := srcEvTask(t, s, "origin refusals")
	in := originsInput(t)
	if _, _, err := s.AddTaskOrigin(ctx, 1<<40, "tester", in); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: err=%v, want ErrTaskNotFound", err)
	}
	// A refused add stores nothing, so the missing-task add left no row behind.
	if _, _, err := s.AddTaskOrigin(ctx, task.ID, "", in); err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("empty actor: err=%v, want a validation error", err)
	}
	bad := in
	bad.Provider = "jira"
	if _, _, err := s.AddTaskOrigin(ctx, task.ID, "tester", bad); err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown provider: err=%v, want a validation error", err)
	}
	bad = in
	bad.ExternalID = "MGH-151"
	if _, _, err := s.AddTaskOrigin(ctx, task.ID, "tester", bad); err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("linear external_id that is no UUID: err=%v, want a validation error", err)
	}
	if got, err := s.ListTaskOrigins(ctx, task.ID); err != nil || len(got) != 0 {
		t.Fatalf("refused adds left origins: %+v err=%v", got, err)
	}
	if _, err := s.ListTaskOrigins(ctx, 1<<40); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("list of a missing task: err=%v, want ErrTaskNotFound", err)
	}
	if _, err := s.ListTaskOriginsPage(ctx, task.ID, 0); err == nil {
		t.Fatal("a page limit of 0 was accepted")
	}
	if _, err := s.FindTaskOrigins(ctx, "jira", "x", 10); err == nil {
		t.Fatal("find with an unknown provider was accepted")
	}
	if _, err := s.FindTaskOrigins(ctx, OriginLinear, "MGH-151", 10); err == nil {
		t.Fatal("find with a linear id that is no UUID was accepted")
	}
}

func originsCaseListFind(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b := srcEvTask(t, s, "find a"), srcEvTask(t, s, "find b")
	shared := originsInput(t)
	for _, id := range []int64{b.ID, a.ID} {
		if _, created, err := s.AddTaskOrigin(ctx, id, "tester", shared); err != nil || !created {
			t.Fatalf("add to %d: created=%t err=%v", id, created, err)
		}
	}
	moved := shared
	moved.Namespace += "-moved"
	if _, created, err := s.AddTaskOrigin(ctx, a.ID, "tester", moved); err != nil || !created {
		t.Fatalf("add moved: created=%t err=%v", created, err)
	}
	// The durable id finds every task and namespace, in (task_id, id) order.
	found, err := s.FindTaskOrigins(ctx, OriginLinear, shared.ExternalID, 0)
	if err != nil || len(found) != 3 {
		t.Fatalf("find: %+v err=%v", found, err)
	}
	for i := 1; i < len(found); i++ {
		prev, cur := found[i-1], found[i]
		if prev.TaskID > cur.TaskID || (prev.TaskID == cur.TaskID && prev.ID >= cur.ID) {
			t.Fatalf("find order: %+v", found)
		}
	}
	if limited, err := s.FindTaskOrigins(ctx, OriginLinear, shared.ExternalID, 2); err != nil || len(limited) != 2 || limited[0].ID != found[0].ID || limited[1].ID != found[1].ID {
		t.Fatalf("find limit 2: %+v err=%v", limited, err)
	}
	if none, err := s.FindTaskOrigins(ctx, OriginLinear, originsLinearID(t), 10); err != nil || len(none) != 0 {
		t.Fatalf("find of an unknown id: %+v err=%v", none, err)
	}
	if none, err := s.FindTaskOrigins(ctx, OriginGitHub, shared.ExternalID, 10); err != nil || len(none) != 0 {
		t.Fatalf("find under another provider: %+v err=%v", none, err)
	}
	// A task's origins come back in id order, and a page bounds them.
	all, err := s.ListTaskOrigins(ctx, a.ID)
	if err != nil || len(all) != 2 || all[0].ID >= all[1].ID {
		t.Fatalf("list of a: %+v err=%v", all, err)
	}
	page, err := s.ListTaskOriginsPage(ctx, a.ID, 1)
	if err != nil || len(page) != 1 || page[0].ID != all[0].ID {
		t.Fatalf("page of a: %+v err=%v", page, err)
	}
	if empty, err := s.ListTaskOrigins(ctx, srcEvTask(t, s, "no origins").ID); err != nil || len(empty) != 0 || empty == nil {
		t.Fatalf("list of a task without origins: %+v err=%v", empty, err)
	}
}

func originsCaseConcurrent(t *testing.T, s *Store) {
	ctx := t.Context()
	task := srcEvTask(t, s, "concurrent origin")
	in := originsInput(t)
	const workers = 6
	created := make([]bool, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created[i], errs[i] = s.AddTaskOrigin(ctx, task.ID, "tester", in)
		}()
	}
	wg.Wait()
	creators := 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if created[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("%d of %d concurrent adds reported creating the row, want 1", creators, workers)
	}
	if got, err := s.ListTaskOrigins(ctx, task.ID); err != nil || len(got) != 1 {
		t.Fatalf("origins after %d concurrent adds: %+v err=%v", workers, got, err)
	}
}
