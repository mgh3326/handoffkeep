package store

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// YTArticleSummary, YTArticleIndexSummary and YTArticleExcluded equal the
// import's own rendering (youtrack/import/client.js): the golden is the JS
// output (testdata/gen_yt_article_summaries.js) for ASCII, Korean, a key over
// 250 runes and an astral character at the cut.
func TestYTArticleSummaryMatchesImport(t *testing.T) {
	f, err := os.Open("testdata/yt_article_summaries.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n, long, korean, astral := 0, false, false, false
	for sc.Scan() {
		var g struct {
			Key      string `json:"key"`
			Kind     string `json:"kind"`
			TaskID   *int64 `json:"task_id"`
			Excluded bool   `json:"excluded"`
			Single   string `json:"single"`
			Index    string `json:"index"`
		}
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		n++
		var task int64
		if g.TaskID != nil {
			task = *g.TaskID
		}
		if got := YTArticleSummary(g.Key, g.Kind); got != g.Single {
			t.Errorf("YTArticleSummary(%q, %q) = %q, want %q", g.Key, g.Kind, got, g.Single)
		}
		if got := YTArticleIndexSummary(g.Key, g.Kind); got != g.Index {
			t.Errorf("YTArticleIndexSummary(%q, %q) = %q, want %q", g.Key, g.Kind, got, g.Index)
		}
		if got := YTArticleExcluded(g.Key, task); got != g.Excluded {
			t.Errorf("YTArticleExcluded(%q, %d) = %v, want %v", g.Key, task, got, g.Excluded)
		}
		long = long || utf8.RuneCountInString(g.Key) > 250
		korean = korean || strings.ContainsRune(g.Key, '가')
		astral = astral || strings.ContainsRune(g.Single, '😀')
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 10 || !long || !korean || !astral {
		t.Fatalf("golden has %d rows (long %v, korean %v, astral at the cut %v)", n, long, korean, astral)
	}
}

// A body is split exactly when it is longer than the import's
// SAFE_PART_BYTES (240 KiB) in UTF-8.
func TestYTArticleSplit(t *testing.T) {
	if YTArticleSplit(240*1024) || !YTArticleSplit(240*1024+1) {
		t.Fatal("split threshold is not SAFE_PART_BYTES")
	}
}

// The summary_cut threshold (decision-6): a document is refused exactly when
// its full rendering is 242 code points or longer. Below it, both strings it
// can match are shorter than 250; every cut rendering of any key is 250 or
// longer, so the two sets never meet.
func TestYTArticleSummaryCutThreshold(t *testing.T) {
	const kind = "note"
	// keyFor builds a key "note/xxx…" whose full rendering
	// "[note] <key> [note]" is n code points long (prefix and kind add 14).
	keyFor := func(n int) string { return "note/" + strings.Repeat("가", n-14-5) }
	render := func(key string) int { return utf8.RuneCountInString("[note] " + key + " [" + kind + "]") }
	for _, tc := range []struct {
		rendering int
		cut       bool
	}{{241, false}, {242, true}, {249, true}, {250, true}, {251, true}} {
		key := keyFor(tc.rendering)
		if got := render(key); got != tc.rendering {
			t.Fatalf("test key renders to %d, want %d", got, tc.rendering)
		}
		single, index := utf8.RuneCountInString(YTArticleSummary(key, kind)), utf8.RuneCountInString(YTArticleIndexSummary(key, kind))
		if got := YTArticleSummaryCut(key, kind); got != tc.cut {
			t.Errorf("rendering %d: YTArticleSummaryCut = %v, want %v (summary %d, index %d code points)", tc.rendering, got, tc.cut, single, index)
		}
		// Every matchable string of a bindable document is under 250.
		if !tc.cut && (single >= 250 || index >= 250) {
			t.Errorf("rendering %d binds, but its summary is %d and index %d code points", tc.rendering, single, index)
		}
		// The summary is cut to exactly 250 from 250 on, and the index
		// (not cut again) is then 258.
		if tc.rendering >= 250 && (single != 250 || index != 258) {
			t.Errorf("rendering %d: summary %d, index %d; want 250 and 258", tc.rendering, single, index)
		}
		// From 242 on, the index alone reaches 250 (a cut summary's length).
		if tc.rendering >= 242 && index < 250 {
			t.Errorf("rendering %d: index %d code points, want at least 250", tc.rendering, index)
		}
	}
	// The finding's pair: A renders to exactly 250 and B's longer key cuts
	// to the same 250 code points. Both are refused.
	a := keyFor(250)
	b := a + " [note]x"
	if YTArticleSummary(a, kind) != YTArticleSummary(b, kind) || !YTArticleSummaryCut(a, kind) || !YTArticleSummaryCut(b, kind) {
		t.Fatalf("r4545414568 pair: equal summaries %v, cut %v/%v", YTArticleSummary(a, kind) == YTArticleSummary(b, kind), YTArticleSummaryCut(a, kind), YTArticleSummaryCut(b, kind))
	}
	// The index case: c renders to 242, so its index is exactly 250 code
	// points, and the longer key e's cut summary spells it out. Both are
	// refused; at 241 the index (249) is shorter than any cut summary.
	c := keyFor(242)
	e := c + " [" + kind + "]" + ytArticleIndexSuffix + "zz"
	if YTArticleIndexSummary(c, kind) != YTArticleSummary(e, kind) || !YTArticleSummaryCut(c, kind) || !YTArticleSummaryCut(e, kind) {
		t.Fatalf("index pair: equal %v, cut %v/%v", YTArticleIndexSummary(c, kind) == YTArticleSummary(e, kind), YTArticleSummaryCut(c, kind), YTArticleSummaryCut(e, kind))
	}
	if utf8.RuneCountInString(YTArticleIndexSummary(keyFor(241), kind)) != 249 {
		t.Fatal("at 241 the index should be 249 code points")
	}
}
