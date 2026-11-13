package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Task titles, task text and comments take YouTrack's text bound, counted in
// characters: a 1 MB text is accepted and 20,000,001 characters is refused.
// A document has no size refusal; it is split into article parts instead.
func TestTaskTextTakesTheYouTrackTextBound(t *testing.T) {
	oneMB := strings.Repeat("가", (1<<20)/3)
	if err := CheckTaskCommentBody(oneMB); err != nil {
		t.Fatalf("1 MB comment refused: %v", err)
	}
	if !validTextBound(oneMB) {
		t.Fatal("1 MB task text refused")
	}
	if err := checkTriageItem(triageItem{Title: "title", Body: oneMB}); err != nil {
		t.Fatalf("1 MB intake body refused: %v", err)
	}
	over := strings.Repeat("x", ytwrite.TextMaxChars+1)
	if err := CheckTaskCommentBody(over); !errors.Is(err, ErrTaskCommentTooLong) {
		t.Fatalf("comment over the bound: %v", err)
	}
	if validTextBound(over) {
		t.Fatal("task text over the bound accepted")
	}
	if !validDocumentText(strings.Repeat("x", 3<<20)) {
		t.Fatal("a document longer than one article part was refused")
	}
}
