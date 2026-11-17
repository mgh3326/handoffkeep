package store

import (
	"testing"
)

func TestBenchKeysKeepTrailingSpaces(t *testing.T) {
	testBenchKeysKeepTrailingSpaces(t, schemaStore(t, "bench_spaces"))
}

func testBenchKeysKeepTrailingSpaces(t *testing.T, s *Store) {
	t.Helper()
	rows := []BenchGrade{
		{Profile: "a", Grade: "A", DeviationRef: "test", DecidedBy: "test"},
		{Profile: "a ", Grade: "B", DeviationRef: "test", DecidedBy: "test"},
	}
	if _, err := s.UpsertBenchGrades(t.Context(), rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListBenchGrades(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, row := range got {
		found[row.Profile] = row.Grade
	}
	if found["a"] != "A" || found["a "] != "B" {
		t.Fatalf("distinct keys not retained: %v", found)
	}
}
