//go:build mysql

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLBench(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	ref := "hk:task/1"
	rep := BenchRep{OriginID: 1, Profile: "profile", TaskRef: &ref, RecordedAt: time.Now().UTC().Truncate(time.Microsecond), CreatedBy: "worker"}
	ids, err := s.UpsertBenchReps(ctx, []BenchRep{rep})
	if err != nil || len(ids) != 1 {
		t.Fatalf("insert: %v %v", ids, err)
	}
	again, err := s.UpsertBenchReps(ctx, []BenchRep{rep})
	if err != nil || len(again) != 1 || again[0] != ids[0] {
		t.Fatalf("resend: %v %v", again, err)
	}
	conflict := rep
	conflict.Profile = "different"
	if _, err := s.UpsertBenchReps(ctx, []BenchRep{conflict}); err == nil {
		t.Fatal("conflicting resend accepted")
	} else {
		var e *BenchRepConflictError
		if !errors.As(err, &e) {
			t.Fatalf("wrong conflict: %v", err)
		}
	}
	tokens := int64(42)
	patched, err := s.PatchBenchRepOutcome(ctx, ids[0], BenchRepOutcomePatch{InputTokens: &tokens})
	if err != nil || patched.InputTokens == nil || *patched.InputTokens != tokens {
		t.Fatalf("patch: %+v %v", patched, err)
	}
	listed, err := s.ListBenchRepsByTaskRef(ctx, ref, 10)
	if err != nil || len(listed) != 1 || listed[0].ID != ids[0] {
		t.Fatalf("list: %+v %v", listed, err)
	}
	rows := []BenchCatalogEntry{{Profile: "profile", Effort: "low", ModelID: "model", Pool: "pool", Grade: "B", DeviationRef: "test", DecidedBy: "test"}, {Profile: "profile", Effort: "high", ModelID: "model", Pool: "pool", Grade: "A", DeviationRef: "test", DecidedBy: "test"}}
	if _, err := s.UpsertBenchCatalog(ctx, rows); err != nil {
		t.Fatal(err)
	}
	listedCatalog, err := s.ListBenchCatalog(ctx, "pool", false)
	if err != nil || len(listedCatalog) != 2 {
		t.Fatalf("catalog: %+v %v", listedCatalog, err)
	}
	rows[1].Grade = "C"
	if _, err := s.UpsertBenchCatalog(ctx, rows); !errors.Is(err, ErrBenchCatalogMonotonicity) {
		t.Fatalf("monotonicity: %v", err)
	}
}

func TestMySQLBenchSharedBehaviors(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Store)
	}{
		{"ladder", testBenchSolGradeLadder},
		{"unrated", testBenchUnratedStore},
		{"cursor", testListBenchRepsPagesByID},
		{"scored-sol", func(t *testing.T, s *Store) { testBenchUnratedSolGradeMirrorPreservesScore(t, s, "codex-sol") }},
		{"scored-kiro", func(t *testing.T, s *Store) { testBenchUnratedSolGradeMirrorPreservesScore(t, s, "kiro-sol") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.Context(), testdb.MySQLDatabase(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			tc.run(t, s)
		})
	}
}
