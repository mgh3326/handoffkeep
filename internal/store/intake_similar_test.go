package store

import (
	"math"
	"sort"
	"testing"
)

// The MySQL duplicate check scores titles in Go like pg_trgm does; no database.
func TestIntakeTrigramSimilarityMatchesPgTrgm(t *testing.T) {
	trigrams := func(s string) []string {
		var out []string
		for k := range intakeTrigrams(s) {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := trigrams("cat"); len(got) != 4 || got[0] != "  c" || got[1] != " ca" || got[2] != "at " || got[3] != "cat" {
		t.Fatalf("trigrams of cat = %q, want pg_trgm's show_trgm('cat')", got)
	}
	// Case and punctuation do not matter; words split on anything but letters and digits.
	if a, b := trigrams("Fix-The  LOGIN"), trigrams("fix the login"); len(a) != len(b) || len(a) == 0 {
		t.Fatalf("trigrams differ by case or punctuation: %q vs %q", a, b)
	}
	for _, tc := range []struct {
		a, b string
		want float64
	}{
		{"word", "word", 1},
		{"word", "two words", 4.0 / 11}, // the pg_trgm documentation's 0.363636
		{"cat", "cart", 2.0 / 7},
		{"abc", "xyz", 0},
		{"", "abc", 0},
		{"!!!", "???", 0},
		{"로그인 오류", "로그인 오류", 1},
	} {
		got := float64(intakeSimilarity(intakeTrigrams(tc.a), intakeTrigrams(tc.b)))
		if math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("similarity(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if back := float64(intakeSimilarity(intakeTrigrams(tc.b), intakeTrigrams(tc.a))); back != got {
			t.Errorf("similarity is not symmetric for %q, %q: %v vs %v", tc.a, tc.b, got, back)
		}
	}
	near := float64(intakeSimilarity(intakeTrigrams("Rewrite the intake duplicate detection for similar titles"), intakeTrigrams("Rewrite the intake duplicate detection for similar title")))
	if near < SimilarTaskThreshold {
		t.Fatalf("near-duplicate titles score %v, below the %v threshold", near, SimilarTaskThreshold)
	}
	far := float64(intakeSimilarity(intakeTrigrams("Rewrite the intake duplicate detection for similar titles"), intakeTrigrams("Zebra crossing paint schedule")))
	if far >= SimilarTaskThreshold {
		t.Fatalf("unrelated titles score %v, at or above the %v threshold", far, SimilarTaskThreshold)
	}
}
