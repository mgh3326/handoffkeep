package store

import (
	"slices"
	"testing"
)

// TestYTCapByIDMergesArms checks the merge of a class's per-state arms
// (ytCapByID): each arm holds its first limit ids, and the first limit of the
// union is the first limit of the class, whatever the arm split.
func TestYTCapByIDMergesArms(t *testing.T) {
	id := func(v int64) int64 { return v }
	cases := []struct {
		name  string
		arms  [][]int64
		limit int
		want  []int64
	}{
		{"interleaved arms keep the lowest ids", [][]int64{{3, 5, 9}, {1, 7, 8}}, 3, []int64{1, 3, 5}},
		{"one arm empty", [][]int64{{}, {4, 6}}, 3, []int64{4, 6}},
		{"fewer than the limit keeps all, sorted", [][]int64{{9}, {2}}, 5, []int64{2, 9}},
		{"no rows", [][]int64{{}, {}}, 3, []int64{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var all []int64
			for _, arm := range tc.arms {
				all = append(all, arm...)
			}
			got := ytCapByID(all, id, tc.limit)
			if !slices.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
