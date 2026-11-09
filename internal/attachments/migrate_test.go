package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type migrationDBFake struct {
	rows      []store.Attachment
	locations map[string]string
}

func (f *migrationDBFake) ListAttachmentObjects(context.Context) ([]store.Attachment, error) {
	return f.rows, nil
}
func (f *migrationDBFake) SetAttachmentObjectLocation(_ context.Context, sha, location string) error {
	if f.locations == nil {
		f.locations = map[string]string{}
	}
	f.locations[sha] = location
	return nil
}

func TestMigrateCopiesVerifiesAndIsIdempotent(t *testing.T) {
	body := []byte("archive bytes")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	key := key(sha)
	db := &migrationDBFake{rows: []store.Attachment{{SHA256: sha, SizeBytes: int64(len(body)), MIME: "text/plain"}}}
	source := &fakeObjects{objects: map[string][]byte{key: body}}
	target := &fakeObjects{objects: map[string][]byte{}}
	want := MigrationResult{Objects: 1, Bytes: int64(len(body))}
	got, err := Migrate(t.Context(), db, source, target, "https://oci.example/bucket", true)
	if err != nil || got != want {
		t.Fatalf("first migrate = %+v, %v; want %+v", got, err, want)
	}
	if db.locations[sha] != "https://oci.example/bucket" {
		t.Fatalf("location = %q", db.locations[sha])
	}
	if string(source.objects[key]) != string(body) {
		t.Fatal("source object was removed or changed")
	}
	if target.puts != 1 {
		t.Fatalf("target puts = %d, want 1", target.puts)
	}
	if _, err = Migrate(t.Context(), db, source, target, "https://oci.example/bucket", true); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if target.puts != 1 {
		t.Fatalf("rerun uploaded existing object: puts=%d", target.puts)
	}
}

func TestMigrateRefusesTargetSHAMismatch(t *testing.T) {
	body := []byte("source")
	bad := []byte("corrupt")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	k := key(sha)
	db := &migrationDBFake{rows: []store.Attachment{{SHA256: sha, SizeBytes: int64(len(body)), MIME: "text/plain"}}}
	source := &fakeObjects{objects: map[string][]byte{k: body}}
	target := &fakeObjects{objects: map[string][]byte{k: bad}}
	_, err := Migrate(t.Context(), db, source, target, "target", true)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	if db.locations[sha] != "" {
		t.Fatalf("recorded location despite mismatch: %q", db.locations[sha])
	}
	if string(target.objects[k]) != string(bad) {
		t.Fatal("mismatched target object was overwritten")
	}
}

func TestMigrateDryRunDoesNotReadOrWriteObjects(t *testing.T) {
	body := []byte("count only")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	db := &migrationDBFake{rows: []store.Attachment{{SHA256: sha, SizeBytes: int64(len(body)), MIME: "text/plain"}}}
	source := &fakeObjects{objects: map[string][]byte{key(sha): body}}
	target := &fakeObjects{objects: map[string][]byte{}}
	got, err := Migrate(t.Context(), db, source, target, "target", false)
	if err != nil || got != (MigrationResult{Objects: 1, Bytes: int64(len(body))}) {
		t.Fatalf("dry run = %+v, %v", got, err)
	}
	if source.stats != 0 || target.stats != 0 || target.puts != 0 || len(db.locations) != 0 {
		t.Fatalf("dry run touched stores: source=%+v target=%+v locations=%v", source, target, db.locations)
	}
}

func TestMigrateRefusesCorruptSource(t *testing.T) {
	body := []byte("expected")
	corrupt := []byte("corrupt")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	db := &migrationDBFake{rows: []store.Attachment{{SHA256: sha, SizeBytes: int64(len(body)), MIME: "text/plain"}}}
	source := &fakeObjects{objects: map[string][]byte{key(sha): corrupt}}
	target := &fakeObjects{objects: map[string][]byte{}}
	_, err := Migrate(t.Context(), db, source, target, "target", true)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	if target.puts != 0 || len(db.locations) != 0 {
		t.Fatalf("continued after source mismatch: puts=%d locations=%v", target.puts, db.locations)
	}
}

// A rerun after the cutover: a row added after the first run exists only at the
// target (its location already recorded), and a row the previous colour wrote
// after the snapshot exists only at the source. The rerun copies the second and
// does not read the first from the source.
func TestMigrateRerunAfterCutover(t *testing.T) {
	sha := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	late, onlyTarget := []byte("written to R2 after the snapshot"), []byte("uploaded to OCI after cutover")
	db := &migrationDBFake{rows: []store.Attachment{
		{SHA256: sha(onlyTarget), SizeBytes: int64(len(onlyTarget)), MIME: "text/plain", ObjectLocation: "target"},
		{SHA256: sha(late), SizeBytes: int64(len(late)), MIME: "text/plain"},
	}}
	source := &fakeObjects{objects: map[string][]byte{key(sha(late)): late}}
	target := &fakeObjects{objects: map[string][]byte{key(sha(onlyTarget)): onlyTarget}}
	if _, err := Migrate(t.Context(), db, source, target, "target", true); err != nil {
		t.Fatalf("rerun after cutover: %v", err)
	}
	if string(target.objects[key(sha(late))]) != string(late) || db.locations[sha(late)] != "target" {
		t.Fatalf("late source row not copied: locations=%v", db.locations)
	}
	if _, rewrote := db.locations[sha(onlyTarget)]; rewrote || target.puts != 1 {
		t.Fatalf("target-only row touched: locations=%v puts=%d", db.locations, target.puts)
	}
}
