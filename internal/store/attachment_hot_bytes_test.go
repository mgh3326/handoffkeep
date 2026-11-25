package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hk 6570: the storage fuse and attach usage count only bytes still in the
// object store. A blob that was archived and then hot-deleted frees its bytes
// for the fuse and shows as archived; one archived but not yet hot-deleted
// still counts. It runs on PostgreSQL and on MySQL, each in its own schema.
func init() {
	registerTaskAreaCase("attachments/hot-deleted-frees-fuse", attachmentHotDeletedFreesFuseCase)
}

func attachmentHotDeletedFreesFuseCase(t *testing.T, s *Store) {
	ctx := t.Context()
	task := flowCaseTask(t, s, "implement")
	if _, err := s.CloseTaskCompleted(ctx, task.ID, "hk:doc research/hot-bytes-case", "tester", "done", "hot-bytes-case-close"); err != nil {
		t.Fatalf("close task: %v", err)
	}
	const fuse = int64(100)
	att := func(c byte, size int64) Attachment {
		return Attachment{SHA256: strings.Repeat(string(c), 64), SizeBytes: size, MIME: "text/plain", OriginalName: "a.txt", CreatedBy: "node", RefKind: "task", RefID: strconv.FormatInt(task.ID, 10)}
	}
	if _, _, err := s.PutAttachment(ctx, att('a', 60), fuse, 1<<40); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, _, err := s.PutAttachment(ctx, att('b', 30), fuse, 1<<40); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if _, _, err := s.PutAttachment(ctx, att('c', 20), fuse, 1<<40); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("put c over a full fuse: err=%v", err)
	}
	now := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
	sha := strings.Repeat("a", 64)
	if _, err := s.RecordAttachmentArchive(ctx, sha, "archive/tasks/"+strconv.FormatInt(task.ID, 10)+"/"+sha+".txt", 60, now); err != nil {
		t.Fatalf("record archive: %v", err)
	}
	// Archived but still in the object store: it still counts.
	if err := s.CheckAttachmentCapacity(ctx, 20, fuse, 1<<40, ""); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("archived blob not yet hot-deleted left the fuse: err=%v", err)
	}
	if err := s.MarkAttachmentHotDeleted(ctx, sha, now.Add(time.Minute)); err != nil {
		t.Fatalf("mark hot deleted: %v", err)
	}
	u, err := s.AttachmentUsage(ctx)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.TotalBytes != 30 || u.ArchivedBytes != 60 {
		t.Fatalf("usage bytes_total=%d bytes_archived=%d, want 30 and 60", u.TotalBytes, u.ArchivedBytes)
	}
	if n, err := s.AttachmentObjectCount(ctx); err != nil || n != 1 {
		t.Fatalf("object count = %d err=%v, want 1", n, err)
	}
	if err := s.CheckAttachmentCapacity(ctx, 20, fuse, 1<<40, ""); err != nil {
		t.Fatalf("preflight after hot delete: %v", err)
	}
	if _, created, err := s.PutAttachment(ctx, att('c', 20), fuse, 1<<40); err != nil || !created {
		t.Fatalf("put c after hot delete: created=%v err=%v", created, err)
	}
	// 30 + 20 in the store, 50 left: the reservation path counts the same way.
	now2 := time.Now().UTC().Truncate(time.Microsecond)
	up := AttachmentUpload{ID: strings.Repeat("e", 32), StagingKey: "incoming/" + strings.Repeat("e", 32), Name: "e.txt", MIME: "text/plain", SizeBytes: 50, SHA256: strings.Repeat("e", 64), RefKind: "none", CreatedBy: "node", CreatedAt: now2, ExpiresAt: now2.Add(time.Hour)}
	if err := s.ReserveAttachmentUpload(ctx, up, 1<<40, 1<<40, fuse); err != nil {
		t.Fatalf("reserve inside the fuse: %v", err)
	}
	if _, _, err := s.PutAttachment(ctx, att('d', 1), fuse, 1<<40); !errors.Is(err, ErrAttachmentStorageCap) {
		t.Fatalf("put past the fuse after the reservation: err=%v", err)
	}
}
