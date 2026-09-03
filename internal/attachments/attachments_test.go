package attachments

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type fakeObjects struct {
	puts    int
	fail    bool
	objects map[string][]byte
}

func (f *fakeObjects) Stat(_ context.Context, k string) error {
	if _, ok := f.objects[k]; ok {
		return nil
	}
	return errors.New("missing")
}
func (f *fakeObjects) Put(_ context.Context, k string, r io.Reader, _ int64, _ string) error {
	f.puts++
	if f.fail {
		return errors.New("down")
	}
	b, _ := io.ReadAll(r)
	f.objects[k] = b
	return nil
}
func (f *fakeObjects) Get(_ context.Context, k string) (io.ReadCloser, error) {
	b, ok := f.objects[k]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeObjects) Presign(context.Context, string, time.Duration) (string, error) {
	return "https://fake.invalid", nil
}

type fakeDB struct {
	rows   map[string]store.Attachment
	puts   int
	capErr error
}

func (f *fakeDB) GetAttachment(_ context.Context, sha string) (store.Attachment, bool, error) {
	x, ok := f.rows[sha]
	return x, ok, nil
}
func (f *fakeDB) PutAttachment(_ context.Context, x store.Attachment, _, _ int64) (store.Attachment, bool, error) {
	f.puts++
	if _, ok := f.rows[x.SHA256]; ok {
		return f.rows[x.SHA256], false, nil
	}
	f.rows[x.SHA256] = x
	return x, true, nil
}
func (f *fakeDB) CheckAttachmentCapacity(context.Context, int64, int64, int64) error { return f.capErr }
func (*fakeDB) RecordAttachmentGet(context.Context, string, int64) error             { return nil }
func (*fakeDB) ListAttachments(context.Context, string, string, int) ([]store.Attachment, error) {
	return nil, nil
}
func (*fakeDB) AttachmentUsage(context.Context) (store.AttachmentUsage, error) {
	return store.AttachmentUsage{}, nil
}

func TestDisabledWhenS3SettingsAbsentOrPartial(t *testing.T) {
	t.Setenv("HK_S3_ENDPOINT", "")
	t.Setenv("HK_S3_BUCKET", "")
	t.Setenv("HK_S3_ACCESS_KEY_ID", "")
	t.Setenv("HK_S3_SECRET_ACCESS_KEY", "")
	_, enabled, err := ConfigFromEnv()
	if err != nil || enabled {
		t.Fatalf("absent enabled=%v err=%v", enabled, err)
	}
	t.Setenv("HK_S3_ENDPOINT", "fake.local")
	_, enabled, err = ConfigFromEnv()
	if err != nil || enabled {
		t.Fatalf("partial enabled=%v err=%v", enabled, err)
	}
}

func TestPositiveCapsRequired(t *testing.T) {
	for _, k := range []string{"HK_S3_ENDPOINT", "HK_S3_BUCKET", "HK_S3_ACCESS_KEY_ID", "HK_S3_SECRET_ACCESS_KEY"} {
		t.Setenv(k, "fake")
	}
	t.Setenv("HK_ATTACH_STORAGE_CAP_BYTES", "0")
	_, _, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("zero storage cap accepted")
	}
}

func TestContentGuards(t *testing.T) {
	c := Config{MaxBytes: 3, Allow: map[string]bool{"text/plain": true, "image/png": true}}
	if _, err := validate(c, "large.txt", "text/plain", []byte("four")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("max guard=%v", err)
	}
	c.MaxBytes = 100
	if _, err := validate(c, "backup.sql", "text/plain", []byte("ok")); !errors.Is(err, ErrExtension) {
		t.Fatalf("extension guard=%v", err)
	}
	if _, err := validate(c, "x.txt", "image/png", []byte("plain text")); !errors.Is(err, ErrMIME) {
		t.Fatalf("mime guard=%v", err)
	}
	if _, err := validate(c, "x.txt", "text/plain", []byte("sk-abcdefghijk")); err == nil || guard.Match("sk-abcdefghijk") == "" {
		t.Fatalf("secret guard=%v", err)
	}
}

func TestParseRef(t *testing.T) {
	for _, ref := range []string{"checkpoint:12", "document:jobs/a.md", "memory:agent/name", ""} {
		if _, _, err := ParseRef(ref); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	if _, _, err := ParseRef("bad:x"); err == nil {
		t.Fatal("bad ref accepted")
	}
}

func TestDedupeSkipsSecondObjectPut(t *testing.T) {
	db := &fakeDB{rows: map[string]store.Attachment{}}
	obj := &fakeObjects{objects: map[string][]byte{}}
	m := &Manager{Store: db, Objects: obj, Enabled: true, Config: Config{MaxBytes: 100, StorageCap: 100, PutCap: 10, GetCap: 10, Allow: map[string]bool{"text/plain": true}}}
	for range 2 {
		if _, _, err := m.Put(t.Context(), "node", "note.txt", "text/plain", "", []byte("same")); err != nil {
			t.Fatal(err)
		}
	}
	if obj.puts != 1 || db.puts != 2 {
		t.Fatalf("puts object=%d db=%d", obj.puts, db.puts)
	}
}
func TestObjectFailureWritesNoMetadata(t *testing.T) {
	db := &fakeDB{rows: map[string]store.Attachment{}}
	obj := &fakeObjects{objects: map[string][]byte{}, fail: true}
	m := &Manager{Store: db, Objects: obj, Enabled: true, Config: Config{MaxBytes: 100, StorageCap: 100, PutCap: 10, GetCap: 10, Allow: map[string]bool{"text/plain": true}}}
	if _, _, err := m.Put(t.Context(), "node", "note.txt", "text/plain", "", []byte("safe")); !errors.Is(err, ErrR2) {
		t.Fatalf("err=%v", err)
	}
	if db.puts != 0 {
		t.Fatalf("metadata writes=%d", db.puts)
	}
}
func TestStorageFuseBlocksObjectPut(t *testing.T) {
	db := &fakeDB{rows: map[string]store.Attachment{}, capErr: store.ErrAttachmentStorageCap}
	obj := &fakeObjects{objects: map[string][]byte{}}
	m := &Manager{Store: db, Objects: obj, Enabled: true, Config: Config{MaxBytes: 100, StorageCap: 100, PutCap: 10, GetCap: 10, Allow: map[string]bool{"text/plain": true}}}
	if _, _, err := m.Put(t.Context(), "node", "note.txt", "text/plain", "", []byte("safe")); !errors.Is(err, store.ErrAttachmentStorageCap) {
		t.Fatalf("err=%v", err)
	}
	if obj.puts != 0 {
		t.Fatalf("R2 puts=%d", obj.puts)
	}
}
