package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"strings"
	"sync"
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
	m := &Manager{Store: db, Objects: objs, Enabled: true, Config: Config{MaxBytes: 1000, StorageCap: 10000, PutCap: 100, GetCap: 100, Allow: allow, DirectUpload: true, StagingCap: 1 << 30, StagingClientCap: 1 << 30}}
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

// The client's PUT can land on the staging object at any time before it is
// deleted, so the test overwrites it in the window between hk's verify and its
// promotion. The stored object must still hold the verified bytes.
func TestOverwriteBetweenVerifyAndPromoteDoesNotChangeTheStoredObject(t *testing.T) {
	m, _, objs := newUploadManager(t)
	verified := []byte("verified bytes of the handoff\n")
	tk := begin(t, m, "node", "v.txt", "text/plain", verified, "")
	clientPUT(objs, tk, verified)
	staging := stagingPrefix + tk.UploadID
	forged := bytes.Repeat([]byte("x"), len(verified))
	overwrote := false
	objs.beforePut = func(k string) {
		if strings.HasPrefix(k, "hk/") && !overwrote {
			overwrote = true
			objs.mu.Lock()
			objs.objects[staging] = forged
			objs.mu.Unlock()
		}
	}
	x, created, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if err != nil || !created {
		t.Fatalf("complete created=%v err=%v", created, err)
	}
	if !overwrote {
		t.Fatal("the overwrite never happened between verify and promote; the race was not exercised")
	}
	objs.mu.Lock()
	stored := objs.objects[key(x.SHA256)]
	objs.mu.Unlock()
	if !bytes.Equal(stored, verified) || shaOf(stored) != x.SHA256 {
		t.Fatalf("promoted object holds %q, not the verified bytes", stored)
	}
}

// Concurrent completions of one upload id register exactly one attachment; the
// losers get attachment_upload_in_progress, and a later retry gets the stored one.
func TestConcurrentCompletesRegisterOneAttachment(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("concurrent completion\n")
	tk := begin(t, m, "node", "c.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	type result struct {
		x       store.Attachment
		created bool
		err     error
	}
	const n = 8
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, created, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
			results <- result{x, created, err}
		}()
	}
	wg.Wait()
	close(results)
	created := 0
	for r := range results {
		if r.err != nil {
			if !errors.Is(r.err, ErrUploadInProgress) {
				t.Fatalf("concurrent complete err=%v", r.err)
			}
			continue
		}
		if r.x.SHA256 != shaOf(body) {
			t.Fatalf("concurrent complete returned sha %s", r.x.SHA256)
		}
		if r.created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d completions reported created; want exactly 1", created)
	}
	db.mu.Lock()
	rows := len(db.rows)
	db.mu.Unlock()
	if rows != 1 {
		t.Fatalf("attachment rows=%d; want 1", rows)
	}
	if _, again, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); err != nil || again {
		t.Fatalf("retry after the race created=%v err=%v", again, err)
	}
}

// A signed URL is a secret for one object. It must appear only in the begin
// response: not in logs, errors, the upload rows, or the completed attachment.
func TestSignedURLsStayInTheBeginResponse(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	m, db, objs := newUploadManager(t)
	body := []byte("secret-free handoff\n")
	tk := begin(t, m, "node", "s.txt", "text/plain", body, "")
	secret := tk.URL
	if !strings.Contains(secret, "sig=test") {
		t.Fatalf("begin response has no signed URL to protect: %q", secret)
	}

	var seen []string
	clientPUT(objs, tk, body)
	x, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	seen = append(seen, mustJSON(t, x))

	bad := begin(t, m, "node", "bad.txt", "text/plain", body, "")
	objs.mu.Lock()
	objs.objects[stagingPrefix+bad.UploadID] = bytes.Repeat([]byte("y"), len(body))
	objs.mu.Unlock()
	_, _, refusal := m.CompleteUpload(t.Context(), "node", bad.UploadID)
	if refusal == nil {
		t.Fatal("mismatched bytes were accepted")
	}
	seen = append(seen, refusal.Error())
	seen = append(seen, logs.String())
	db.mu.Lock()
	for _, u := range db.uploads {
		seen = append(seen, mustJSON(t, u))
	}
	for _, a := range db.rows {
		seen = append(seen, mustJSON(t, a))
	}
	db.mu.Unlock()
	for i, text := range seen {
		if strings.Contains(text, secret) || strings.Contains(text, "sig=test") {
			t.Fatalf("signed URL leaked into output %d: %q", i, text)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The staging delete after a refusal is best effort; a leftover refused object
// is removed by the sweep once it is old.
func TestSweepRemovesLeftoverStagingOfRefusedUpload(t *testing.T) {
	m, _, objs := newUploadManager(t)
	body := []byte("declared text\n")
	tk := begin(t, m, "node", "r.txt", "text/plain", body, "")
	clientPUT(objs, tk, bytes.Repeat([]byte("z"), len(body)))
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); !errors.Is(err, ErrUploadRefused) {
		t.Fatalf("mismatch err=%v", err)
	}
	staging := stagingPrefix + tk.UploadID
	objs.mu.Lock()
	objs.objects[staging] = bytes.Repeat([]byte("z"), len(body))
	objs.modified[staging] = time.Now().UTC().Add(-3 * time.Hour)
	objs.mu.Unlock()
	deleted, err := m.SweepStaging(t.Context())
	if err != nil || deleted != 1 {
		t.Fatalf("sweep deleted=%d err=%v", deleted, err)
	}
	objs.mu.Lock()
	_, left := objs.objects[staging]
	objs.mu.Unlock()
	if left {
		t.Fatal("refused staging object survived the sweep")
	}
}

// The server's cap is enforced at completion too, not only where begin checked
// the declaration: a cap lowered after begin refuses the staged object.
func TestCompleteEnforcesTheCapAtCompletion(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := bytes.Repeat([]byte("c"), 20)
	tk := begin(t, m, "node", "big.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	m.Config.MaxBytes = 10
	_, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_too_large") {
		t.Fatalf("over-cap completion err=%v", err)
	}
	if db.uploads[tk.UploadID].Status != store.AttachmentUploadRefused {
		t.Fatalf("status=%q", db.uploads[tk.UploadID].Status)
	}
}

// The presigned PUT signs the declared Content-Length, so the URL accepts one
// body of exactly that size where the store checks signed headers.
func TestPresignedPutPinsContentLength(t *testing.T) {
	obj, err := NewR2(Config{Endpoint: "https://example.r2.cloudflarestorage.com", Bucket: "bkt", AccessKey: "AK", SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := obj.PresignPut(t.Context(), "incoming/abc", time.Minute, 1234)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if signed := parsed.Query().Get("X-Amz-SignedHeaders"); !strings.Contains(signed, "content-length") {
		t.Fatalf("SignedHeaders=%q; the declared length is not pinned", signed)
	}
}

// Direct upload stays off unless HK_ATTACH_DIRECT_UPLOAD is set, so no URL and
// no upload row exist for it by default.
func TestDirectUploadIsOffByDefault(t *testing.T) {
	m, db, _ := newUploadManager(t)
	m.Config.DirectUpload = false
	body := []byte("off by default\n")
	_, err := m.BeginUpload(t.Context(), "node", "a.txt", "text/plain", int64(len(body)), shaOf(body), "")
	if !errors.Is(err, ErrDirectUploadDisabled) {
		t.Fatalf("begin err=%v; want direct upload disabled", err)
	}
	if len(db.uploads) != 0 {
		t.Fatalf("a disabled begin recorded %d upload rows", len(db.uploads))
	}
}

// The environment turns direct upload on and sets the staging budget; both
// default to off and 1 GiB.
func TestConfigFromEnvDirectUploadAndStagingBudget(t *testing.T) {
	t.Setenv("HK_S3_ENDPOINT", "https://example.invalid")
	t.Setenv("HK_S3_BUCKET", "bkt")
	t.Setenv("HK_S3_ACCESS_KEY_ID", "AK")
	t.Setenv("HK_S3_SECRET_ACCESS_KEY", "SK")
	t.Setenv("HK_ATTACH_DIRECT_UPLOAD", "")
	t.Setenv("HK_ATTACH_STAGING_CAP_BYTES", "")
	c, on, err := ConfigFromEnv()
	if err != nil || !on || c.DirectUpload || c.StagingCap != DefaultStagingCap {
		t.Fatalf("defaults: on=%v direct=%v staging=%d err=%v", on, c.DirectUpload, c.StagingCap, err)
	}
	t.Setenv("HK_ATTACH_DIRECT_UPLOAD", "1")
	t.Setenv("HK_ATTACH_STAGING_CAP_BYTES", "4096")
	c, _, err = ConfigFromEnv()
	if err != nil || !c.DirectUpload || c.StagingCap != 4096 {
		t.Fatalf("enabled: direct=%v staging=%d err=%v", c.DirectUpload, c.StagingCap, err)
	}
}

// Outstanding staged bytes are bounded: a begin that would exceed the budget
// is refused and creates no row, and completing an upload frees its share.
func TestStagingBudgetBoundsOutstandingBytes(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.StagingCap = 100
	body := bytes.Repeat([]byte("b"), 60)
	first := begin(t, m, "node", "one.txt", "text/plain", body, "")
	rows := len(db.uploads)
	_, err := m.BeginUpload(t.Context(), "node", "two.txt", "text/plain", int64(len(body)), shaOf(body), "")
	if !errors.Is(err, store.ErrAttachmentStagingCap) {
		t.Fatalf("over-budget begin err=%v", err)
	}
	if len(db.uploads) != rows {
		t.Fatalf("a refused begin recorded a row")
	}
	clientPUT(objs, first, body)
	if _, _, err := m.CompleteUpload(t.Context(), "node", first.UploadID); err != nil {
		t.Fatal(err)
	}
	// A completed upload stays charged until its staging object is confirmed gone
	// and its presigned URL is past its life, not when it completes.
	if _, err := m.BeginUpload(t.Context(), "node", "three.txt", "text/plain", int64(len(body)), shaOf(body), ""); !errors.Is(err, store.ErrAttachmentStagingCap) {
		t.Fatalf("completed upload released its charge before reconciliation: %v", err)
	}
	m.Now = func() time.Time { return time.Now().Add(UploadTTL + 2*releaseGrace) }
	if _, err := m.SweepStaging(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginUpload(t.Context(), "node", "three.txt", "text/plain", int64(len(body)), shaOf(body), ""); err != nil {
		t.Fatalf("begin after reconciliation: %v", err)
	}
}

// staleUploadRead answers GetAttachmentUpload from an earlier read, so a call
// sees the row as pending after another completion has already claimed it.
type staleUploadRead struct {
	*fakeDB
	stale store.AttachmentUpload
}

func (s *staleUploadRead) GetAttachmentUpload(context.Context, string) (store.AttachmentUpload, bool, error) {
	return s.stale, true, nil
}

// An expiry check that loses the race to a claim must not delete the bytes the
// claiming completion is using, and must report in-progress, not expired.
func TestExpiryThatLosesTheRaceLeavesTheStagingBytes(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("claimed by another completion\n")
	tk := begin(t, m, "node", "race.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	db.mu.Lock()
	stale := db.uploads[tk.UploadID]
	claimed := stale
	claimed.Status = store.AttachmentUploadCompleting
	db.uploads[tk.UploadID] = claimed
	db.mu.Unlock()
	m.Store = &staleUploadRead{fakeDB: db, stale: stale}
	m.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); !errors.Is(err, ErrUploadInProgress) {
		t.Fatalf("losing expiry err=%v; want in progress", err)
	}
	if _, ok := objs.objects[stagingPrefix+tk.UploadID]; !ok {
		t.Fatal("a losing expiry deleted the staging bytes of the claiming completion")
	}
	if db.uploads[tk.UploadID].Status != store.AttachmentUploadCompleting {
		t.Fatalf("losing expiry changed the claimed row to %q", db.uploads[tk.UploadID].Status)
	}
}

// A staging object whose best-effort delete failed keeps its charge until the
// sweep removes it, and the charge is released only then.
func TestBudgetHeldUntilStagingObjectIsGone(t *testing.T) {
	m, _, objs := newUploadManager(t)
	m.Config.StagingCap = 100
	m.Config.StagingClientCap = 100
	body := bytes.Repeat([]byte("h"), 60)
	first := begin(t, m, "node", "h.txt", "text/plain", body, "")
	clientPUT(objs, first, body)
	if _, _, err := m.CompleteUpload(t.Context(), "node", first.UploadID); err != nil {
		t.Fatal(err)
	}
	// The delete failed: the object is back, recently modified.
	staging := stagingPrefix + first.UploadID
	objs.mu.Lock()
	objs.objects[staging] = body
	objs.modified[staging] = time.Now().UTC()
	objs.mu.Unlock()
	m.Now = func() time.Time { return time.Now().Add(UploadTTL + 2*releaseGrace) }
	if _, err := m.SweepStaging(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginUpload(t.Context(), "node", "h2.txt", "text/plain", int64(len(body)), shaOf(body), ""); !errors.Is(err, store.ErrAttachmentStagingCap) {
		t.Fatalf("charge released while the staging object still existed: %v", err)
	}
	// Once the object is older than the sweep age, the sweep deletes it and the
	// charge is released on the same pass.
	objs.mu.Lock()
	objs.modified[staging] = time.Now().UTC().Add(-3 * time.Hour)
	objs.mu.Unlock()
	m.Now = func() time.Time { return time.Now().Add(UploadTTL + 2*releaseGrace) }
	if _, err := m.SweepStaging(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginUpload(t.Context(), "node", "h2.txt", "text/plain", int64(len(body)), shaOf(body), ""); err != nil {
		t.Fatalf("begin after the object was swept: %v", err)
	}
}

// One creator cannot hold the whole budget: a begin over its own share is
// refused, while another creator can still begin.
func TestClientBudgetCannotBeMonopolized(t *testing.T) {
	m, _, _ := newUploadManager(t)
	m.Config.StagingCap = 1000
	m.Config.StagingClientCap = 100
	body := bytes.Repeat([]byte("m"), 60)
	begin(t, m, "greedy", "g1.txt", "text/plain", body, "")
	if _, err := m.BeginUpload(t.Context(), "greedy", "g2.txt", "text/plain", int64(len(body)), shaOf(body), ""); !errors.Is(err, store.ErrAttachmentStagingCap) {
		t.Fatalf("greedy client over its share: %v", err)
	}
	begin(t, m, "polite", "p1.txt", "text/plain", body, "")
}

// failingFinalize fails the completing -> done transition a fixed number of
// times, the transient database error after the attachment was stored.
type failingFinalize struct {
	*fakeDB
	fail int
}

func (f *failingFinalize) TransitionAttachmentUpload(ctx context.Context, id, from, to, reason string, now time.Time) (bool, error) {
	if from == store.AttachmentUploadCompleting && to == store.AttachmentUploadDone && f.fail > 0 {
		f.fail--
		return false, errors.New("database unavailable")
	}
	return f.fakeDB.TransitionAttachmentUpload(ctx, id, from, to, reason, now)
}

// If the final transition fails after the attachment is stored, the row stays
// completing. Retries inside the lease are refused; after the lease the claim
// is reclaimed and the completion is redone, and the client gets the result.
func TestRetryAfterFinalizeFailureRecovers(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("finalize may fail once\n")
	tk := begin(t, m, "node", "f.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	m.Store = &failingFinalize{fakeDB: db, fail: 1}
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); err == nil {
		t.Fatal("the failed finalize was reported as success")
	}
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); !errors.Is(err, ErrUploadInProgress) {
		t.Fatalf("retry inside the lease err=%v; want in progress", err)
	}
	m.Now = func() time.Time { return time.Now().Add(uploadLease + time.Minute) }
	x, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if err != nil {
		t.Fatalf("retry after the lease: %v", err)
	}
	if x.SHA256 != shaOf(body) || db.uploads[tk.UploadID].Status != store.AttachmentUploadDone {
		t.Fatalf("recovered sha=%s status=%q", x.SHA256, db.uploads[tk.UploadID].Status)
	}
	if _, ok := objs.objects[stagingPrefix+tk.UploadID]; ok {
		t.Fatal("staging object survived the recovered completion")
	}
}
