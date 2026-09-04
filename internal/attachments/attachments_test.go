package attachments

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type fakeObjects struct {
	puts, stats int
	fail        bool
	objects     map[string][]byte
}

func (f *fakeObjects) Stat(_ context.Context, k string) error {
	f.stats++
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
	rows    map[string]store.Attachment
	puts    int
	capErr  error
	hideGet bool
}

func (f *fakeDB) GetAttachment(_ context.Context, sha string) (store.Attachment, bool, error) {
	if f.hideGet {
		return store.Attachment{}, false, nil
	}
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
	secret := "sk-" + strings.Repeat("a", 48)
	if _, err := validate(c, "x.txt", "text/plain", []byte(secret)); err == nil || guard.Match(secret) == "" {
		t.Fatalf("secret guard=%v", err)
	}
}

func TestDefaultAllowlistAcceptsAllDeclaredTypesAndRejectsDisguises(t *testing.T) {
	allow := map[string]bool{}
	for _, x := range strings.Split("image/png,image/jpeg,image/gif,text/html,text/plain,text/markdown,application/json,application/pdf,text/csv,application/x-ndjson,text/x-log", ",") {
		allow[x] = true
	}
	m := &Manager{Store: &fakeDB{rows: map[string]store.Attachment{}}, Objects: &fakeObjects{objects: map[string][]byte{}}, Enabled: true, Config: Config{MaxBytes: 1000, StorageCap: 10000, PutCap: 100, GetCap: 100, Allow: allow}}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	for _, x := range []struct {
		name, mime string
		body       []byte
	}{
		{"x.png", "image/png", png}, {"x.jpg", "image/jpeg", []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0, "J"[0], "F"[0], "I"[0], "F"[0]}}, {"x.gif", "image/gif", []byte("GIF89a")},
		{"x.html", "text/html", []byte("<html>ok</html>")}, {"x.txt", "text/plain", []byte("plain")}, {"x.md", "text/markdown", []byte("# heading")}, {"x.json", "application/json", []byte(`{"ok":true}`)}, {"x.pdf", "application/pdf", []byte("%PDF-1.4")}, {"x.csv", "text/csv", []byte("a,b\n1,2")}, {"x.ndjson", "application/x-ndjson", []byte("{\"a\":1}\n")}, {"x.log", "text/x-log", []byte("INFO ready")},
	} {
		if _, created, err := m.Put(t.Context(), "node", x.name, x.mime, "", x.body); err != nil || !created {
			t.Fatalf("%s: created=%v err=%v", x.mime, created, err)
		}
	}
	if _, _, err := m.Put(t.Context(), "node", "fake.png", "image/png", "", []byte("not an image")); !errors.Is(err, ErrMIME) {
		t.Fatalf("png disguise=%v", err)
	}
	if _, _, err := m.Put(t.Context(), "node", "fake.json", "application/json", "", png); !errors.Is(err, ErrMIME) {
		t.Fatalf("json disguise=%v", err)
	}
}

func TestTextDeclarationsRejectBinarySniffsAndNUL(t *testing.T) {
	allow := map[string]bool{}
	for _, x := range strings.Split("image/png,image/jpeg,image/gif,text/html,text/plain,text/markdown,application/json,application/pdf,text/csv,application/x-ndjson,text/x-log", ",") {
		allow[x] = true
	}
	m := &Manager{Store: &fakeDB{rows: map[string]store.Attachment{}}, Objects: &fakeObjects{objects: map[string][]byte{}}, Enabled: true, Config: Config{MaxBytes: 1000, StorageCap: 10000, PutCap: 100, GetCap: 100, Allow: allow}}
	binaries := []struct {
		name string
		body []byte
	}{
		{"zip", []byte("PK\x03\x04\x14\x00")}, {"wasm", []byte("\x00asm\x01\x00\x00\x00")}, {"ogg", []byte("OggS\x00\x02")}, {"postscript", []byte("%!PS-Adobe-3.0")}, {"wave", []byte("RIFF\x24\x00\x00\x00WAVEfmt ")}, {"mpeg", []byte("ID3\x04\x00\x00")}, {"mp4", []byte("\x00\x00\x00\x18ftypmp42")}, {"avi", []byte("RIFF\x24\x00\x00\x00AVI ")}, {"ttf", []byte("\x00\x01\x00\x00\x00\x10")}, {"woff", []byte("wOFF\x00\x01\x00\x00")},
	}
	for _, binary := range binaries {
		for _, declared := range []struct{ name, mime string }{{"x.txt", "text/plain"}, {"x.md", "text/markdown"}, {"x.json", "application/json"}} {
			if _, _, err := m.Put(t.Context(), "node", binary.name+declared.name, declared.mime, "", binary.body); !errors.Is(err, ErrMIME) {
				t.Fatalf("%s as %s: %v", binary.name, declared.mime, err)
			}
		}
	}
	if _, _, err := m.Put(t.Context(), "node", "nul.txt", "text/plain", "", []byte("safe\x00binary")); !errors.Is(err, ErrMIME) {
		t.Fatalf("NUL text=%v", err)
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
func TestDedupeDBHitSkipsObjectLookup(t *testing.T) {
	db := &fakeDB{rows: map[string]store.Attachment{}}
	obj := &fakeObjects{objects: map[string][]byte{}}
	m := &Manager{Store: db, Objects: obj, Enabled: true, Config: Config{MaxBytes: 100, StorageCap: 100, PutCap: 10, GetCap: 10, Allow: map[string]bool{"text/plain": true}}}
	if _, _, e := m.Put(t.Context(), "node", "note.txt", "text/plain", "", []byte("same")); e != nil {
		t.Fatal(e)
	}
	obj.stats = 0
	if _, _, e := m.Put(t.Context(), "node", "note.txt", "text/plain", "", []byte("same")); e != nil {
		t.Fatal(e)
	}
	if obj.stats != 0 {
		t.Fatalf("DB dedupe still consulted object store: stats=%d", obj.stats)
	}
}
func TestDedupeObjectHitSkipsSecondObjectPut(t *testing.T) {
	db := &fakeDB{rows: map[string]store.Attachment{}}
	obj := &fakeObjects{objects: map[string][]byte{}}
	m := &Manager{Store: db, Objects: obj, Enabled: true, Config: Config{MaxBytes: 100, StorageCap: 100, PutCap: 10, GetCap: 10, Allow: map[string]bool{"text/plain": true}}}
	if _, _, e := m.Put(t.Context(), "node", "note.txt", "text/plain", "", []byte("same")); e != nil {
		t.Fatal(e)
	}
	db.hideGet = true
	if _, _, e := m.Put(t.Context(), "node", "other.txt", "text/plain", "", []byte("same")); e != nil {
		t.Fatal(e)
	}
	if obj.puts != 1 {
		t.Fatalf("object-exists dedupe failed: puts=%d", obj.puts)
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
