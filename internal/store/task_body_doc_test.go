package store

import (
	"context"
	"strings"
	"testing"
)

// ValidBodyDoc checks shape only: a document key, optionally with a
// transitional "#section" scroll target. Anything that could carry body text
// (spaces, newlines) or escape the key space is refused.
func TestValidBodyDoc(t *testing.T) {
	for _, ok := range []string{
		"design/2026-09-21/task495-unblock-body-comments-search",
		"design/2026-09-21/task495-unblock-body-comments-search#3",
		"k/x#§3-태스크-가",
		"a",
		strings.Repeat("k", 512),
		"k#" + strings.Repeat("s", BodyDocSectionMax),
	} {
		if !ValidBodyDoc(ok) {
			t.Errorf("ValidBodyDoc(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",
		"../etc/passwd",
		"k/../x",
		"/abs/key",
		"key with space",
		"key\nsecond line",
		"본문 글자",
		"key\t",
		"k#",
		"k#a b",
		"k#a\nb",
		"k#a#b",
		"#section-only",
		strings.Repeat("k", 513),
		"k#" + strings.Repeat("s", BodyDocSectionMax+1),
		"k#\x00",
		"k\x00",
		"javascript:alert(1)",
	} {
		if ValidBodyDoc(bad) {
			t.Errorf("ValidBodyDoc(%q) = true, want false", bad)
		}
	}
}

func TestCreateTaskBodyDoc(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	// The document does not exist: shape alone decides, creation is not blocked.
	x, err := s.CreateTask(ctx, Task{Lane: "lane-a", Title: "one line", Kind: "implement", CreatedBy: "t", BodyDoc: "design/not-written-yet#3"})
	if err != nil {
		t.Fatal(err)
	}
	if x.BodyDoc != "design/not-written-yet#3" {
		t.Fatalf("create body_doc=%q", x.BodyDoc)
	}
	got, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found || got.BodyDoc != "design/not-written-yet#3" {
		t.Fatalf("get body_doc=%q found=%t err=%v", got.BodyDoc, found, err)
	}
	page, err := s.ListTasksPage(ctx, "lane-a", "", "", 0, 10)
	if err != nil || len(page) != 1 || page[0].BodyDoc != "design/not-written-yet#3" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	// A transition leaves the pointer alone.
	moved, err := s.TransitionTask(ctx, x.ID, "hold", "t", "", nil)
	if err != nil || moved.BodyDoc != "design/not-written-yet#3" {
		t.Fatalf("transition body_doc=%q err=%v", moved.BodyDoc, err)
	}
	// No body_doc stays the empty default.
	plain, err := s.CreateTask(ctx, Task{Lane: "lane-a", Title: "no body", Kind: "implement", CreatedBy: "t"})
	if err != nil || plain.BodyDoc != "" {
		t.Fatalf("plain body_doc=%q err=%v", plain.BodyDoc, err)
	}
	for _, bad := range []string{"body text with spaces", "k/../x", "k#a b"} {
		if _, err := s.CreateTask(ctx, Task{Lane: "lane-a", Title: "bad", Kind: "implement", CreatedBy: "t", BodyDoc: bad}); err == nil || !strings.Contains(err.Error(), "body_doc") {
			t.Fatalf("CreateTask(body_doc=%q) err=%v, want body_doc refusal", bad, err)
		}
	}
}
