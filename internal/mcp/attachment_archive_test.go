package mcp

import (
	"testing"
	"time"
)

// TestAttachmentArchivedAnswers: an archived blob answers where it is, for
// both reads, instead of content or a URL.
func TestAttachmentArchivedAnswers(t *testing.T) {
	cs, db, objs, _ := newAttachmentTestServer(t)
	sha := storeRow(db, objs, "a.txt", "text/plain", []byte("hello"))
	const location = "fleet-artifacts/archive/tasks/7/attachments/x-a.txt"
	at := time.Now().UTC()
	row := db.rows[sha]
	row.ArchivedAt, row.ArchiveLocation = &at, location
	db.rows[sha] = row

	read, err := attachmentCall(t, cs, "attachment_read", map[string]any{"sha256": sha})
	if err != nil {
		t.Fatalf("attachment_read refused an archived blob: %v", err)
	}
	if read["archived"] != true || read["location"] != location || read["sha256"] != sha {
		t.Fatalf("attachment_read answer %v", read)
	}
	if note, _ := read["note"].(string); note == "" {
		t.Fatalf("attachment_read note missing: %v", read)
	}

	url, err := attachmentCall(t, cs, "attachment_get_url", map[string]any{"sha256": sha})
	if err != nil {
		t.Fatalf("attachment_get_url refused an archived blob: %v", err)
	}
	if url["archived"] != true || url["location"] != location {
		t.Fatalf("attachment_get_url answer %v", url)
	}
	if _, signed := url["url"]; signed {
		t.Fatalf("archived blob was signed: %v", url)
	}
}
