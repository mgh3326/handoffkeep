package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Dialect-neutral archive case (hk 5088): a blob of a completed task is
// archived and hot-deleted through the public calls, and the stored columns are
// read back. It runs on PostgreSQL and on MySQL. On MySQL the archive and the
// hot-delete updates bind their values in statement order, so a regression that
// swaps them fails here (archived_at would receive the sha, and the WHERE would
// match nothing).
func init() {
	registerTaskAreaCase("attachments/archive-record-hot-delete", attachmentArchiveRecordCase)
}

func attachmentArchiveRecordCase(t *testing.T, s *Store) {
	ctx := t.Context()
	task := flowCaseTask(t, s, "implement")
	if _, err := s.CloseTaskCompleted(ctx, task.ID, "hk:doc research/archive-case", "tester", "done", "archive-case-close"); err != nil {
		t.Fatalf("close task: %v", err)
	}
	sha := strings.Repeat("c", 64)
	if _, _, err := s.PutAttachment(ctx, Attachment{SHA256: sha, SizeBytes: 10, MIME: "text/plain", OriginalName: "archive.txt", CreatedBy: "node", RefKind: "task", RefID: strconv.FormatInt(task.ID, 10)}, 1<<40, 1<<40); err != nil {
		t.Fatalf("put attachment: %v", err)
	}
	// The grace period is measured from now, so a time past it makes the close
	// old enough to archive.
	now := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
	location := "fleet-artifacts/archive/tasks/" + strconv.FormatInt(task.ID, 10) + "/" + sha + ".txt"
	rec, err := s.RecordAttachmentArchive(ctx, sha, location, 10, now)
	if err != nil {
		t.Fatalf("record archive: %v", err)
	}
	if !rec.ArchivedAt.Equal(now) || rec.Location != location {
		t.Fatalf("record = %+v, want archived at %v in %s", rec, now, location)
	}
	row, found, err := s.GetAttachment(ctx, sha)
	if err != nil || !found {
		t.Fatalf("get after archive: found=%t err=%v", found, err)
	}
	if row.ArchivedAt == nil || !row.ArchivedAt.Equal(now) || row.ArchiveLocation != location {
		t.Fatalf("stored archive columns = archived_at %v location %q, want %v %q", row.ArchivedAt, row.ArchiveLocation, now, location)
	}
	hotAt := now.Add(2 * time.Hour)
	if err := s.MarkAttachmentHotDeleted(ctx, sha, hotAt); err != nil {
		t.Fatalf("mark hot deleted: %v", err)
	}
	row, _, err = s.GetAttachment(ctx, sha)
	if err != nil {
		t.Fatalf("get after hot delete: %v", err)
	}
	if row.HotDeletedAt == nil || !row.HotDeletedAt.Equal(hotAt) {
		t.Fatalf("hot_deleted_at = %v, want %v", row.HotDeletedAt, hotAt)
	}
}
