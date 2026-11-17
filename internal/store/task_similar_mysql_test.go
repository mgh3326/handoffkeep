package store

import (
	"math"
	"testing"
)

// The Go-side duplicate check needs no database: these tests run everywhere.

func TestTrigramSimilarityMatchesPgTrgm(t *testing.T) {
	// pg_trgm documents similarity('word','two words') = 0.36363637.
	got := trigramSimilarity(titleTrigrams("word"), titleTrigrams("two words"))
	if math.Abs(got-0.36363637) > 1e-7 {
		t.Fatalf("similarity(word, two words) = %v, want 0.36363637", got)
	}
	for _, c := range []struct {
		a, b string
		want float64
	}{
		{"Fix the build", "fix the build", 1},        // case is folded
		{"Fix the build", "Fix, the   build!", 1},    // punctuation and spacing only separate words
		{"fix build", "rotate credentials", 0},       // nothing shared
		{"", "anything", 0},                          // no words, no trigrams
		{"", "", 0},                                  // two empty titles are not duplicates
		{"한글 제목", "한글 제목", 1},                        // letters of any script count
		{"alpha beta", "beta alpha", 1},              // word order is not part of a trigram set
		{"abc", "abcd", float64(float32(3.0 / 6.0))}, // 3 shared of 4 + 5 distinct
	} {
		if got := trigramSimilarity(titleTrigrams(c.a), titleTrigrams(c.b)); got != c.want {
			t.Errorf("similarity(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestTitleTrigramsPadWords(t *testing.T) {
	got := titleTrigrams("Ab")
	want := []string{"  a", " ab", "ab "}
	if len(got) != len(want) {
		t.Fatalf("trigrams = %v", got)
	}
	for _, g := range want {
		if _, ok := got[g]; !ok {
			t.Fatalf("trigrams = %v, missing %q", got, g)
		}
	}
	if got := titleTrigrams("  ,;  "); len(got) != 0 {
		t.Fatalf("separators only = %v", got)
	}
}

func TestRankSimilarTitles(t *testing.T) {
	open := []openTaskTitle{
		{ID: 9, Title: "Fix the flaky payment webhook retry", State: "backlog", Lane: "a"},
		{ID: 3, Title: "Fix the flaky payment webhook retry", State: "unstarted", Lane: "b"},
		{ID: 5, Title: "Fix the flaky payment webhook retries", State: "developing", Lane: "c"},
		{ID: 4, Title: "Rotate the staging database credentials", State: "backlog", Lane: "d"},
		{ID: 7, Title: "Payment webhook", State: "backlog", Lane: "e"},
	}
	got := rankSimilarTitles(open, []string{"Fix the flaky payment webhook retry"}, SimilarTaskThreshold, SimilarTaskLimit)
	if len(got) < 3 {
		t.Fatalf("ranked = %+v", got)
	}
	// Exact matches first, ties by id; then the near match.
	if got[0].ID != 3 || got[1].ID != 9 || got[2].ID != 5 {
		t.Fatalf("order = %d %d %d, want 3 9 5", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Similarity != 1 || got[1].Similarity != 1 || got[2].Similarity >= 1 || got[2].Similarity < SimilarTaskThreshold {
		t.Fatalf("scores = %v %v %v", got[0].Similarity, got[1].Similarity, got[2].Similarity)
	}
	if got[0].State != "unstarted" || got[0].Lane != "b" || got[0].Title != open[1].Title {
		t.Fatalf("fields = %+v", got[0])
	}
	for _, x := range got {
		if x.ID == 4 {
			t.Fatalf("unrelated title ranked: %+v", got)
		}
	}

	// The best of several new titles decides; the limit and a stable order hold.
	many := make([]openTaskTitle, 0, 12)
	for i := int64(12); i >= 1; i-- {
		many = append(many, openTaskTitle{ID: i, Title: "same title", State: "backlog", Lane: "x"})
	}
	top := rankSimilarTitles(many, []string{"unrelated words", "same title"}, SimilarTaskThreshold, SimilarTaskLimit)
	if len(top) != SimilarTaskLimit {
		t.Fatalf("limit: %d results", len(top))
	}
	for i, x := range top {
		if x.ID != int64(i+1) || x.Similarity != 1 {
			t.Fatalf("top[%d] = %+v", i, x)
		}
	}

	// Deterministic: the same input twice gives the same answer; a threshold of
	// 1 keeps only exact matches; no titles and no open tasks give none.
	again := rankSimilarTitles(many, []string{"unrelated words", "same title"}, SimilarTaskThreshold, SimilarTaskLimit)
	for i := range top {
		if top[i] != again[i] {
			t.Fatalf("not deterministic at %d: %+v vs %+v", i, top[i], again[i])
		}
	}
	if exact := rankSimilarTitles(open, []string{"Fix the flaky payment webhook retry"}, 1, 10); len(exact) != 2 {
		t.Fatalf("threshold 1 = %+v", exact)
	}
	if got := rankSimilarTitles(open, nil, SimilarTaskThreshold, SimilarTaskLimit); len(got) != 0 {
		t.Fatalf("no titles = %+v", got)
	}
	if got := rankSimilarTitles(nil, []string{"x"}, SimilarTaskThreshold, SimilarTaskLimit); got == nil || len(got) != 0 {
		t.Fatalf("no open tasks = %#v, want an empty non-nil list", got)
	}
}
