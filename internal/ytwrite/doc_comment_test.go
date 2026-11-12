package ytwrite

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// docFixtureBody is the fixture of the golden test, generated the same way
// by the import's JS (documentIssueComments) for the same text.
func docFixtureBody() string {
	var b strings.Builder
	line := func(n int) {
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "line %06d 한글 텍스트 %s\n", i, strings.Repeat("x", i%37))
		}
	}
	line(1500)
	b.WriteString(strings.Repeat("가", 90000) + "\n")
	line(300)
	return b.String()
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The split report's parts as the import posted them: byte length and SHA-256
// of each comment text (node documentIssueComments on docFixtureBody).
var importedSplitParts = []struct {
	length int
	sha    string
}{
	{71868, "a500f5de074db9e7e932b5f6e3c3ded24828f299f1013616fe55c7d1c6526d1d"},
	{244772, "dd699f7e836c3a4dbfb5de057b2419b4bb9077c5e9db59df23e1b67147a37c69"},
	{25305, "fb2a35ef34f6843d64ebd2895d5b764775a58b329c049006af982b6a0d915633"},
	{14372, "b1c2d56048dfbe534794472e9deadc3aa81718a7f66ad18da281d1313751a91f"},
}

func TestDocCommentsSplitReportMatchesImport(t *testing.T) {
	bodies, ok, err := DocComments("reports/job-b/report.md", "report", docFixtureBody())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if len(bodies) != len(importedSplitParts) {
		t.Fatalf("parts = %d, want %d", len(bodies), len(importedSplitParts))
	}
	for i, want := range importedSplitParts {
		if got := len(bodies[i]); got != want.length {
			t.Errorf("part %d: %d bytes, want %d", i+1, got, want.length)
		}
		if got := sha256Hex(bodies[i]); got != want.sha {
			t.Errorf("part %d: sha %s, want %s", i+1, got, want.sha)
		}
	}
	if !strings.HasPrefix(bodies[0], "# reports/job-b/report.md (part 1/4)\n\n") {
		t.Errorf("heading of part 1: %q", bodies[0][:60])
	}
	if !strings.HasPrefix(bodies[3], "# reports/job-b/report.md (part 4/4)\n\n") {
		t.Errorf("heading of part 4: %q", bodies[3][:60])
	}
}

func TestDocCommentsSmallReportAndBriefText(t *testing.T) {
	small, ok, err := DocComments("reports/job-a/report.md", "report", "hello\n")
	if err != nil || !ok || len(small) != 1 {
		t.Fatalf("small: %v %v %d", ok, err, len(small))
	}
	if want := "# reports/job-a/report.md\n\nhello\n"; small[0] != want {
		t.Errorf("small = %q, want %q", small[0], want)
	}

	brief, ok, err := DocComments("brief/2026-11-09/job-a", "brief", "ignored body")
	if err != nil || !ok || len(brief) != 1 {
		t.Fatalf("brief: %v %v %d", ok, err, len(brief))
	}
	want := "Article: [brief] brief/2026-11-09/job-a [brief] (hk document key: brief/2026-11-09/job-a)"
	if brief[0] != want {
		t.Errorf("brief = %q, want %q", brief[0], want)
	}
}

func TestDocCommentsBriefTitleIsCutAtRunes(t *testing.T) {
	key := "brief/2026-11-09/" + strings.Repeat("가", 300)
	bodies, _, _ := DocComments(key, "brief", "")
	title := "[brief] " + key + " [brief]"
	wantTitle := []rune(title)[:250]
	if !strings.HasPrefix(bodies[0], "Article: "+string(wantTitle)+" (hk document key: ") {
		t.Errorf("title not cut to 250 runes: %q", bodies[0][:40])
	}
}

func TestDocCommentsKeysThatAreNotTaskTied(t *testing.T) {
	for _, key := range []string{"decision/x", "reports/job/other.md", "reports/job/a/report.md", "brief/a", "brief/a/b/c", "notes/brief/a"} {
		if _, ok, err := DocComments(key, "note", "body"); ok || err != nil {
			t.Errorf("%q: ok=%v err=%v, want not task-tied", key, ok, err)
		}
	}
	for _, key := range []string{"reports/mwr-1/anything/deeper", "reports/job/report.md"} {
		if _, ok, _ := DocComments(key, "report", "body"); !ok {
			t.Errorf("%q: not task-tied, want report", key)
		}
	}
}

func TestDocCommentPartCapAndHKCommentCap(t *testing.T) {
	bodies, _, _ := DocComments("reports/job-b/report.md", "report", docFixtureBody())
	for _, b := range bodies {
		if err := CheckDocComment(CommentRequest{Principal: "p", Actor: "w", TaskID: 1, YTIssueID: "2-1", Tenant: "t", Body: b, Key: "k"}); err != nil {
			t.Fatalf("document part refused: %v", err)
		}
	}
	// hk's own comments keep the smaller cap.
	big := CommentRequest{Principal: "p", Actor: "w", TaskID: 1, YTIssueID: "2-1", Tenant: "t", Body: bodies[1], Key: "k"}
	if err := CheckComment(big); !errors.Is(err, ErrInvalid) {
		t.Fatalf("hk comment over %d bytes accepted: %v", MaxBodyBytes, err)
	}
	// The splitter keeps every part under the cap, so only a heading longer
	// than the reserved room can push a part over it: the import refused
	// that too.
	longKey := "reports/mwr-" + strings.Repeat("k", DocCommentPartBytes)
	if _, _, err := DocComments(longKey, "report", strings.Repeat("a", 2000)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a part over the cap was not refused: %v", err)
	}
}

func TestDocCommentRenderedFormIsOwnedByItsOp(t *testing.T) {
	bodies, _, _ := DocComments("reports/job-b/report.md", "report", docFixtureBody())
	payload := []byte(`{"body":` + jsonString(bodies[1]) + `}`)
	op := Op{ID: 4242, Actor: "worker-b", CreatedAt: time.Date(2026, 11, 12, 1, 2, 3, 0, time.UTC), Payload: payload}
	text, err := RenderComment(op)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "작성: worker-b · 2026-11-12T01:02:03Z · hk op 4242\n\n# reports/job-b/report.md (part 2/4)\n\n") {
		t.Errorf("rendered head: %q", text[:90])
	}
	if id, ok := RenderedOpID(text); !ok || id != 4242 {
		t.Errorf("RenderedOpID = %d, %v; want 4242", id, ok)
	}
}

// jsonString quotes s as a JSON string (the payload's body field).
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&15])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
