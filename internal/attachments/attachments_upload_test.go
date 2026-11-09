package attachments

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func newUploadManager(t *testing.T) (*Manager, *fakeDB, *fakeObjects) {
	t.Helper()
	db := &fakeDB{rows: map[string]store.Attachment{}}
	objs := &fakeObjects{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	allow := map[string]bool{}
	for _, x := range strings.Split(DefaultMIMEAllow, ",") {
		allow[x] = true
	}
	m := &Manager{Store: db, Objects: objs, Enabled: true, Config: Config{MaxBytes: 1000, StorageCap: 10000, PutCap: 100, GetCap: 100, Allow: allow}}
	return m, db, objs
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// begin declares an upload of body and returns the ticket; the test stands in
// for the client by writing the bytes to the staging key the ticket names.
func begin(t *testing.T, m *Manager, client, name, mime string, body []byte, ref string) UploadTicket {
	t.Helper()
	tk, err := m.BeginUpload(t.Context(), client, name, mime, int64(len(body)), shaOf(body), ref)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tk
}

func clientPUT(objs *fakeObjects, tk UploadTicket, body []byte) {
	objs.objects[stagingPrefix+tk.UploadID] = body
}

func TestDirectUploadStoresTheBytesLikePut(t *testing.T) {
	m, db, objs := newUploadManager(t)
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{7}, 40)...)
	tk := begin(t, m, "node", "shot.png", "image/png", png, "task:12")
	if tk.Method != "PUT" || tk.ExpiresInSeconds != 600 || !strings.HasPrefix(tk.URL, "https://") || !strings.Contains(tk.URL, stagingPrefix+tk.UploadID) {
		t.Fatalf("ticket=%+v", tk)
	}
	if len(tk.UploadID) != 32 {
		t.Fatalf("upload id %q is not 32 hex", tk.UploadID)
	}
	clientPUT(objs, tk, png)
	x, created, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if err != nil || !created {
		t.Fatalf("complete created=%v err=%v", created, err)
	}
	sha := shaOf(png)
	if x.SHA256 != sha || x.RefKind != "task" || x.RefID != "12" || x.MIME != "image/png" {
		t.Fatalf("stored attachment=%+v", x)
	}
	if _, ok := objs.objects[key(sha)]; !ok {
		t.Fatal("content-addressed object missing")
	}
	if _, ok := objs.objects[stagingPrefix+tk.UploadID]; ok {
		t.Fatal("staging object survived a successful completion")
	}
	if row := db.uploads[tk.UploadID]; row.Status != store.AttachmentUploadDone {
		t.Fatalf("row status=%q", row.Status)
	}
}

func TestCompleteIsIdempotentAndOnlyTheBeginnerCanComplete(t *testing.T) {
	m, _, objs := newUploadManager(t)
	body := []byte("notes for the handoff\n")
	tk := begin(t, m, "node", "notes.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	if _, _, err := m.CompleteUpload(t.Context(), "other-client", tk.UploadID); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("other client err=%v", err)
	}
	first, created, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if err != nil || !created {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	again, created, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if err != nil || created || again.SHA256 != first.SHA256 {
		t.Fatalf("second created=%v err=%v sha=%s", created, err, again.SHA256)
	}
}

func TestMismatchedBytesAreRefusedAndFinal(t *testing.T) {
	m, db, objs := newUploadManager(t)
	declared := []byte("the bytes the client declared\n")
	tk := begin(t, m, "node", "a.txt", "text/plain", declared, "")
	clientPUT(objs, tk, []byte(strings.ToUpper(string(declared)))) // same length, different content
	_, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_sha256_mismatch") {
		t.Fatalf("sha mismatch err=%v", err)
	}
	if _, ok := objs.objects[stagingPrefix+tk.UploadID]; ok {
		t.Fatal("refused staging object survived")
	}
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); !errors.Is(err, ErrUploadRefused) {
		t.Fatalf("refusal was not final: %v", err)
	}
	if db.uploads[tk.UploadID].Reason != "upload_sha256_mismatch" {
		t.Fatalf("reason=%q", db.uploads[tk.UploadID].Reason)
	}

	short := begin(t, m, "node", "b.txt", "text/plain", declared, "")
	clientPUT(objs, short, declared[:5])
	if _, _, err := m.CompleteUpload(t.Context(), "node", short.UploadID); !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_size_mismatch") {
		t.Fatalf("size mismatch err=%v", err)
	}
}

func TestSecretTextIsRefusedAtCompletion(t *testing.T) {
	m, _, objs := newUploadManager(t)
	secret := []byte("sk-" + strings.Repeat("a", 48))
	tk := begin(t, m, "node", "key.txt", "text/plain", secret, "")
	clientPUT(objs, tk, secret)
	_, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "secret_like_content") {
		t.Fatalf("secret err=%v", err)
	}
	if strings.Contains(err.Error(), "sk-") {
		t.Fatalf("refusal echoes the secret: %v", err)
	}
	if _, ok := objs.objects[stagingPrefix+tk.UploadID]; ok {
		t.Fatal("secret staging object survived")
	}
}

func TestCompleteBeforeTheBytesArriveIsRetryable(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("late bytes\n")
	tk := begin(t, m, "node", "late.txt", "text/plain", body, "")
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); !errors.Is(err, ErrUploadNotReceived) {
		t.Fatalf("early complete err=%v", err)
	}
	if db.uploads[tk.UploadID].Status != store.AttachmentUploadPending {
		t.Fatalf("retryable failure left status=%q", db.uploads[tk.UploadID].Status)
	}
	clientPUT(objs, tk, body)
	if _, created, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); err != nil || !created {
		t.Fatalf("retry created=%v err=%v", created, err)
	}
}

func TestExpiredUploadIsRefusedAndItsStagingDeleted(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("too late\n")
	tk := begin(t, m, "node", "late.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	m.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); !errors.Is(err, ErrUploadExpired) {
		t.Fatalf("expired err=%v", err)
	}
	if _, ok := objs.objects[stagingPrefix+tk.UploadID]; ok {
		t.Fatal("expired staging object survived")
	}
	if db.uploads[tk.UploadID].Status != store.AttachmentUploadRefused {
		t.Fatalf("expired status=%q", db.uploads[tk.UploadID].Status)
	}
}

func TestBeginRefusesBadDeclarationsBeforeAnyBytes(t *testing.T) {
	m, db, _ := newUploadManager(t)
	body := []byte("ok\n")
	sha := shaOf(body)
	for name, c := range map[string]struct {
		file, mime, sha, ref string
		size                 int64
		want                 error
	}{
		"mime":     {"a.exe", "application/x-msdownload", sha, "", 3, ErrMIME},
		"size":     {"a.txt", "text/plain", sha, "", 1001, ErrTooLarge},
		"sha":      {"a.txt", "text/plain", "ABC", "", 3, nil},
		"name":     {"a.zip", "text/plain", sha, "", 3, ErrExtension},
		"ref":      {"a.txt", "text/plain", sha, "task:012", 3, nil},
		"negative": {"a.txt", "text/plain", sha, "", -1, ErrTooLarge},
	} {
		_, err := m.BeginUpload(t.Context(), "node", c.file, c.mime, c.size, c.sha, c.ref)
		if err == nil {
			t.Fatalf("%s: declaration accepted", name)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Fatalf("%s: err=%v want %v", name, err, c.want)
		}
	}
	if len(db.uploads) != 0 {
		t.Fatalf("refused declarations recorded %d upload rows", len(db.uploads))
	}
}

func TestSweepDeletesOnlyOldStaging(t *testing.T) {
	m, _, objs := newUploadManager(t)
	now := time.Now().UTC()
	objs.objects[stagingPrefix+"young"] = []byte("a")
	objs.modified[stagingPrefix+"young"] = now
	objs.objects[stagingPrefix+"old"] = []byte("b")
	objs.modified[stagingPrefix+"old"] = now.Add(-3 * time.Hour)
	objs.objects[key(shaOf([]byte("kept")))] = []byte("kept")
	objs.modified[key(shaOf([]byte("kept")))] = now.Add(-30 * 24 * time.Hour)
	deleted, err := m.SweepStaging(t.Context())
	if err != nil || deleted != 1 {
		t.Fatalf("sweep deleted=%d err=%v", deleted, err)
	}
	if _, ok := objs.objects[stagingPrefix+"old"]; ok {
		t.Fatal("old staging object survived the sweep")
	}
	if _, ok := objs.objects[stagingPrefix+"young"]; !ok {
		t.Fatal("young staging object was swept")
	}
	if _, ok := objs.objects[key(shaOf([]byte("kept")))]; !ok {
		t.Fatal("content object was swept")
	}
}
