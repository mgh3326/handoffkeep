package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocumentTitle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		head      string
		truncated bool
		want      string
	}{
		{"first heading", "# Alpha\n\nbody", false, "Alpha"},
		{"deep heading", "intro\n\n### Third level\n", false, "Third level"},
		{"front matter skipped", "---\ntask: 1\n---\n# Beta\n", false, "Beta"},
		{"closing hashes", "# Title ###\n", false, "Title"},
		{"hash is part of the name", "# C#\n", false, "C#"},
		{"no heading", "plain text\n", false, ""},
		{"empty", "", false, ""},
		{"bom before heading", "\ufeff# BOM title\n", false, "BOM title"},
		{"unterminated front matter is scanned as content", "---\ntask: 1\n# Not hidden\n", false, "Not hidden"},
		{"heading without space is not a heading", "#nope\n# Yes\n", false, "Yes"},
		{"untruncated last line without newline is a heading", "# Real", false, "Real"},
		{"truncated partial heading is no title", "# Re", true, ""},
		{"truncated mid-body keeps earlier headings", "# Real\nmore", true, "Real"},
	} {
		if got := documentTitle(tc.head, tc.truncated); got != tc.want {
			t.Fatalf("%s: documentTitle(%q, %v)=%q, want %q", tc.name, tc.head, tc.truncated, got, tc.want)
		}
	}
}

func TestDocumentTitleTruncatesLongHeadings(t *testing.T) {
	title := documentTitle("# "+strings.Repeat("가", 300), false)
	if len([]rune(title)) != 201 || !strings.HasSuffix(title, "…") {
		t.Fatalf("truncated title has %d runes: %q", len([]rune(title)), title)
	}
}

func TestDocsListRejectsInvalidQuery(t *testing.T) {
	h := &Handler{}
	for _, target := range []string{
		"/ui/api/docs?prefix=%00",
		"/ui/api/docs?before=%00",
		"/ui/api/docs?limit=0",
		"/ui/api/docs?limit=201",
		"/ui/api/docs?limit=nope",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		h.docsList(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400", target, rec.Code)
		}
	}
}
