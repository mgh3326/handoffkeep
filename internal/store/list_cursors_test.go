package store

import (
	"fmt"
	"testing"
	"time"
)

// pageIDs pages a list with afterID until a short page ends it and returns the
// ids the pages delivered, in order.
func pageIDs[T any](t *testing.T, limit int, id func(T) int64, page func(*int64) ([]T, error)) []int64 {
	t.Helper()
	after := int64(0)
	out := []int64{}
	for {
		rows, err := page(&after)
		if err != nil {
			t.Fatalf("page after %d: %v", after, err)
		}
		for _, r := range rows {
			out = append(out, id(r))
		}
		if len(rows) < limit {
			return out
		}
		after = id(rows[len(rows)-1])
		if len(out) > 10000 {
			t.Fatalf("paging did not end after %d rows", len(out))
		}
	}
}

// wantEachOnce fails unless got holds every seeded id exactly once.
func wantEachOnce(t *testing.T, got, seeded []int64) {
	t.Helper()
	seen := map[int64]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("id %d returned twice: %v", id, got)
		}
		seen[id] = true
	}
	if len(got) != len(seeded) {
		t.Fatalf("paged %d rows, seeded %d: %v", len(got), len(seeded), got)
	}
	for _, id := range seeded {
		if !seen[id] {
			t.Fatalf("id %d missing from the pages: %v", id, got)
		}
	}
}

// TestListPlacementsPagesByID pages rows that all share one decided_at: the
// newest-first answer is unchanged, and the cursor visits each row once.
func TestListPlacementsPagesByID(t *testing.T) {
	s := schemaStore(t, "list_cursors_placements")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	since := time.Now().UTC().Add(-2 * time.Hour)
	var seeded []int64
	for i := range 5 {
		d := placementWithCandidate(PlacementCandidate{Host: "node-a", Outcome: "ok"})
		d.JobID, d.DecidedAt = fmt.Sprintf("cursor-job-%d", i), at
		stored, _, err := s.RecordPlacement(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, stored.ID)
	}

	newest, err := s.ListPlacements(t.Context(), "", "", since, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) != 2 || newest[0].ID != seeded[4] || newest[1].ID != seeded[3] {
		t.Fatalf("newest-first page = %d rows, first ids %v, want %d,%d", len(newest), idsOfPlacements(newest), seeded[4], seeded[3])
	}

	got := pageIDs(t, 2, func(d PlacementDecision) int64 { return d.ID }, func(after *int64) ([]PlacementDecision, error) {
		return s.ListPlacements(t.Context(), "", "", since, after, 2)
	})
	wantEachOnce(t, got, seeded)
}

func idsOfPlacements(xs []PlacementDecision) []int64 {
	out := make([]int64, len(xs))
	for i, x := range xs {
		out[i] = x.ID
	}
	return out
}

// TestListBenchRepsPagesByID pages rows that share recorded_at: the
// newest-first answer is unchanged, and the cursor visits each row once.
func TestListBenchRepsPagesByID(t *testing.T) {
	s := schemaStore(t, "list_cursors_bench")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var seeded []int64
	for i := range 5 {
		ids, err := s.UpsertBenchReps(t.Context(), []BenchRep{{OriginID: int64(i + 1), Profile: "cursor", RecordedAt: at, CreatedBy: "list-cursors"}})
		if err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, ids[0])
	}

	newest, err := s.ListBenchReps(t.Context(), "", "", "", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) != 2 || newest[0].ID != seeded[4] || newest[1].ID != seeded[3] {
		t.Fatalf("newest-first page = %d rows, want %d,%d first", len(newest), seeded[4], seeded[3])
	}

	got := pageIDs(t, 2, func(x BenchRep) int64 { return x.ID }, func(after *int64) ([]BenchRep, error) {
		return s.ListBenchReps(t.Context(), "", "", "", after, 2)
	})
	wantEachOnce(t, got, seeded)
}

// TestListDocumentsPagesByID pages documents: the key-ordered answer is
// unchanged, and the cursor visits each row once in id order.
func TestListDocumentsPagesByID(t *testing.T) {
	s := schemaStore(t, "list_cursors_documents")
	var seeded []int64
	for i := range 5 {
		d, _, err := s.PutDocument(t.Context(), Document{Key: fmt.Sprintf("cursor-doc/%d", i), Kind: "note", Body: "x", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, d.ID)
	}

	byKey, err := s.ListDocuments(t.Context(), "cursor-doc/", "", "", 0, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(byKey) != 5 || byKey[0].Key != "cursor-doc/0" || byKey[4].Key != "cursor-doc/4" {
		t.Fatalf("key order changed: %v", byKey)
	}

	got := pageIDs(t, 2, func(d Document) int64 { return d.ID }, func(after *int64) ([]Document, error) {
		return s.ListDocuments(t.Context(), "cursor-doc/", "", "", 0, after, 2)
	})
	wantEachOnce(t, got, seeded)
}

// TestListChangesByTaskPagesByID pages the changes linked to one task: the id
// order is unchanged, and the cursor visits each row once.
func TestListChangesByTaskPagesByID(t *testing.T) {
	s := schemaStore(t, "list_cursors_changes")
	x, err := s.CreateTask(t.Context(), Task{Lane: "list-cursors", Title: "cursor target", Kind: "implement", CreatedBy: "list-cursors"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour)
	var seeded []int64
	for i := range 5 {
		_, err := s.ApplyTaskSourceEvent(t.Context(), TaskSourceEvent{Source: SourceMergewakePR, SourceRef: fmt.Sprintf("https://github.com/example/cursor/pull/%d", i+1), TaskID: x.ID, At: &at})
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListChangesByTask(t.Context(), x.ID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		seeded = append(seeded, c.ID)
	}
	if len(seeded) != 5 {
		t.Fatalf("seeded %d changes, want 5", len(seeded))
	}

	got := pageIDs(t, 2, func(c Change) int64 { return c.ID }, func(after *int64) ([]Change, error) {
		return s.ListChangesByTask(t.Context(), x.ID, after, 2)
	})
	wantEachOnce(t, got, seeded)
}
