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
