package ui

import (
	"strings"
	"testing"
)

func TestBoardDocumentFormatKeepsWholeStoredDocument(t *testing.T) {
	body := strings.Repeat("full document content\n", 20_000)
	format, reason := boardDocFormat(body)
	if format != "markdown" || reason != "" {
		t.Fatalf("format=%q reason=%q for %d-byte document", format, reason, len(body))
	}
}
