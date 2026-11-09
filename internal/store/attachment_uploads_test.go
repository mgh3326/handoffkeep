package store

import (
	"testing"
	"time"
)

// Direct-upload rows: create, read, and a transition that only one caller can
// win. Runs in its own schema because it counts rows.
func TestAttachmentUploadRowsTransitionOnce(t *testing.T) {
	s := schemaStore(t, "attachment-uploads")
	now := time.Now().UTC().Truncate(time.Microsecond)
	const id = "0123456789abcdef0123456789abcdef"
	row := AttachmentUpload{ID: id, StagingKey: "incoming/" + id, Name: "a.txt", MIME: "text/plain", SizeBytes: 3, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RefKind: "task", RefID: "12", CreatedBy: "node", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateAttachmentUpload(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAttachmentUpload(t.Context(), id)
	if err != nil || !ok || got.Status != AttachmentUploadPending || got.RefKind != "task" || got.RefID != "12" || got.SizeBytes != 3 {
		t.Fatalf("get ok=%v err=%v row=%+v", ok, err, got)
	}
	if _, ok, err := s.GetAttachmentUpload(t.Context(), "ffffffffffffffffffffffffffffffff"); err != nil || ok {
		t.Fatalf("unknown id ok=%v err=%v", ok, err)
	}
	if _, ok, _ := s.GetAttachmentUpload(t.Context(), "not-an-id"); ok {
		t.Fatal("malformed id was read")
	}
	claimed, err := s.TransitionAttachmentUpload(t.Context(), id, AttachmentUploadPending, AttachmentUploadCompleting, "", now)
	if err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	again, err := s.TransitionAttachmentUpload(t.Context(), id, AttachmentUploadPending, AttachmentUploadCompleting, "", now)
	if err != nil || again {
		t.Fatalf("second claim won=%v err=%v", again, err)
	}
	if err := s.CreateAttachmentUpload(t.Context(), AttachmentUpload{ID: "bad"}); err == nil {
		t.Fatal("malformed upload id stored")
	}
}
