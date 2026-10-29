package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocumentTitle(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
		want string
	}{
		{"first heading", "# Alpha\n\nbody", "Alpha"},
		{"deep heading", "intro\n\n### Third level\n", "Third level"},
		{"front matter skipped", "---\ntask: 1\n---\n# Beta\n", "Beta"},
		{"closing hashes", "# Title ###\n", "Title"},
		{"hash is part of the name", "# C#\n", "C#"},
		{"no heading", "plain text\n", ""},
		{"empty", "", ""},
		{"bom before heading", "\ufeff# BOM title\n", "BOM title"},
		{"unterminated front matter is scanned as content", "---\ntask: 1\n# Not hidden\n", "Not hidden"},
		{"heading without space is not a heading", "#nope\n# Yes\n", "Yes"},
	} {
		if got := documentTitle(tc.head); got != tc.want {
			t.Fatalf("%s: documentTitle(%q)=%q, want %q", tc.name, tc.head, got, tc.want)
		}
	}
}

func TestDocumentTitleTruncatesLongHeadings(t *testing.T) {
	title := documentTitle("# " + strings.Repeat("가", 300))
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
