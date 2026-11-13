package ytwrite

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A 1 MB comment renders whole; a body that fits the bound alone but not
// with its provenance line and marker is refused by name, never cut.
func TestRenderCommentKeepsAOneMBBodyAndRefusesOverTheBound(t *testing.T) {
	op := func(body string) Op {
		payload, err := json.Marshal(CommentPayload{Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return Op{ID: 9, Principal: "p", Kind: KindCommentAppend, Actor: "worker", CreatedAt: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), Payload: payload}
	}
	oneMB := strings.Repeat("가", (1<<20)/3)
	text, err := RenderComment(op(oneMB))
	if err != nil || !strings.Contains(text, oneMB) {
		t.Fatalf("1 MB comment not rendered whole: err=%v", err)
	}
	if _, err := RenderComment(op(strings.Repeat("x", TextMaxChars))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("comment over the text bound rendered: %v", err)
	}
}

// A 1 MB task title is accepted by the comment check's bound and its issue
// description renders whole; the summary is cut to the summary rule.
func TestRenderIssueKeepsTheDescriptionAndCutsTheSummary(t *testing.T) {
	title := strings.Repeat("제", 300) + strings.Repeat("목", (1<<20)/3)
	summary, description := RenderIssue(IssueText{Actor: "worker", At: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), OpID: 4, TaskID: 12, Project: "handoffkeep", Title: title})
	if n := utf8.RuneCountInString(summary); n != SummaryMaxChars {
		t.Fatalf("summary is %d characters, want the %d-character rule", n, SummaryMaxChars)
	}
	if !strings.Contains(description, title) {
		t.Fatal("issue description cut the title")
	}
	if !WithinTextMax(description) {
		t.Fatal("rendered description is over the text bound")
	}
}
