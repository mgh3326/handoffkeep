package attachments

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// archiveManager builds a manager over one stored blob whose hot object is
// present, in the given mode.
func archiveManager(t *testing.T, mode ArchiveMode) (*Manager, *fakeDB, *fakeObjects, string) {
	t.Helper()
	db := &fakeDB{rows: map[string]store.Attachment{}}
	objs := &fakeObjects{objects: map[string][]byte{}}
	sha := strings.Repeat("a", 64)
	db.rows[sha] = store.Attachment{SHA256: sha, SizeBytes: 5, MIME: "text/plain", OriginalName: "a.txt"}
	objs.objects[key(sha)] = []byte("hello")
	m := &Manager{Store: db, Objects: objs, Enabled: true, ArchiveMode: mode}
	return m, db, objs, sha
}

func TestArchiveModeOffRefuses(t *testing.T) {
	m, db, objs, sha := archiveManager(t, ArchiveModeOff)
	_, err := m.Archive(t.Context(), sha, "fleet-artifacts/archive/tasks/1/attachments/x-a.txt", 5)
	if !errors.Is(err, ErrArchiveOff) {
		t.Fatalf("err=%v, want ErrArchiveOff", err)
	}
	if db.rows[sha].ArchivedAt != nil || db.rows[sha].ArchiveLocation != "" {
		t.Fatalf("mode off recorded %+v", db.rows[sha])
	}
	if _, ok := objs.objects[key(sha)]; !ok {
		t.Fatal("mode off deleted the hot object")
	}
}

func TestArchiveModeRecordKeepsHotCopy(t *testing.T) {
	m, db, objs, sha := archiveManager(t, ArchiveModeRecord)
	const location = "fleet-artifacts/archive/tasks/1/attachments/x-a.txt"
	rec, err := m.Archive(t.Context(), sha, location, 5)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ArchivedAt.IsZero() || rec.Location != location || rec.HotDeleted {
		t.Fatalf("record mode result %+v", rec)
	}
	row := db.rows[sha]
	if row.ArchivedAt == nil || row.ArchiveLocation != location || row.HotDeletedAt != nil {
		t.Fatalf("stored row %+v", row)
	}
	if _, ok := objs.objects[key(sha)]; !ok {
		t.Fatal("record mode deleted the hot object")
	}
	// A repeat with the same location answers the recorded result.
	again, err := m.Archive(t.Context(), sha, location, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ArchivedAt.Equal(rec.ArchivedAt) || again.Location != location || again.HotDeleted {
		t.Fatalf("repeat %+v, first %+v", again, rec)
	}
}

func TestArchiveModeOnDeletesHotCopy(t *testing.T) {
	m, db, objs, sha := archiveManager(t, ArchiveModeOn)
	const location = "fleet-artifacts/archive/tasks/1/attachments/x-a.txt"
	rec, err := m.Archive(t.Context(), sha, location, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.HotDeleted {
		t.Fatalf("mode on did not delete: %+v", rec)
	}
	if _, ok := objs.objects[key(sha)]; ok {
		t.Fatal("hot object still present")
	}
	if db.rows[sha].HotDeletedAt == nil {
		t.Fatal("hot delete not recorded")
	}
	again, err := m.Archive(t.Context(), sha, location, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !again.HotDeleted || !again.ArchivedAt.Equal(rec.ArchivedAt) {
		t.Fatalf("repeat %+v, first %+v", again, rec)
	}
}

// TestArchiveHotDeleteRetriedAfterFailure is the design's never-silent rule: a
// failed delete answers hot_deleted=false, logs the failure and leaves the row
// unmarked, so the next archive call for that sha deletes it.
func TestArchiveHotDeleteRetriedAfterFailure(t *testing.T) {
	m, db, objs, sha := archiveManager(t, ArchiveModeOn)
	objs.deleteErr = errors.New("object store down")
	const location = "fleet-artifacts/archive/tasks/1/attachments/x-a.txt"
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	rec, err := m.Archive(t.Context(), sha, location, 5)
	if err != nil {
		t.Fatalf("a failed delete is not an archive failure: %v", err)
	}
	if rec.HotDeleted {
		t.Fatal("failed delete reported hot_deleted")
	}
	if db.rows[sha].HotDeletedAt != nil {
		t.Fatal("failed delete marked the row")
	}
	if !strings.Contains(logged.String(), "attach.archive.hot_delete_failed") {
		t.Fatalf("failure not logged: %q", logged.String())
	}
	if _, ok := objs.objects[key(sha)]; !ok {
		t.Fatal("failed delete removed the object")
	}

	objs.deleteErr = nil
	rec, err = m.Archive(t.Context(), sha, location, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.HotDeleted {
		t.Fatal("retry did not delete")
	}
	if _, ok := objs.objects[key(sha)]; ok {
		t.Fatal("object still present after retry")
	}
	if db.rows[sha].HotDeletedAt == nil {
		t.Fatal("retry did not record the delete")
	}
}

func TestArchivedReadsReportLocation(t *testing.T) {
	m, db, _, sha := archiveManager(t, ArchiveModeRecord)
	const location = "fleet-artifacts/archive/tasks/1/attachments/x-a.txt"
	if rec, err := m.Archive(t.Context(), sha, location, 5); err != nil || rec.Location != location {
		t.Fatalf("archive %+v err=%v", rec, err)
	}
	// The archived record is checked before the object store is touched.
	ctx := context.Background()
	var archived *ArchivedError
	if _, _, err := m.Get(ctx, sha); !errors.As(err, &archived) || archived.Location != location || archived.SHA256 != sha {
		t.Fatalf("Get err=%v", err)
	}
	if _, err := m.Presign(ctx, sha); !errors.As(err, &archived) || archived.Location != location {
		t.Fatalf("Presign err=%v", err)
	}
	if _, err := m.PresignDownload(ctx, sha, "attachment"); !errors.As(err, &archived) || archived.Location != location {
		t.Fatalf("PresignDownload err=%v", err)
	}
	if row := db.rows[sha]; row.ArchivedAt == nil {
		t.Fatal("row lost its archive record")
	}
}
