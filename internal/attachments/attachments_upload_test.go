package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestBeginRefusesAboveBufferedCompletionFuse(t *testing.T) {
	db := &fakeDB{rows: map[string]store.Attachment{}}
	objects := &fakeObjects{objects: map[string][]byte{}}
	m := &Manager{
		Store:   db,
		Objects: objects,
		Enabled: true,
		Config: Config{
			MaxBytes: DefaultMaxBytes, StorageCap: 100 << 30, PutCap: 10, GetCap: 10,
			Allow: map[string]bool{"text/plain": true}, DirectUpload: true,
			StagingCap: DefaultStagingCap, StagingClientCap: DefaultStagingClientCap,
		},
	}
	if DefaultMaxBytes != 50<<30 {
		t.Fatalf("OCI per-object ceiling=%d, want 50 GiB", DefaultMaxBytes)
	}
	ticket, err := m.BeginUpload(t.Context(), "node", "large.txt", "text/plain", BufferedCompletionMaxBytes+1, strings.Repeat("a", 64), "")
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_completion_buffer_limit_exceeded") {
		t.Fatalf("ticket=%+v err=%v, want buffered completion fuse refusal", ticket, err)
	}
	if ticket.UploadID != "" || len(db.uploads) != 0 || len(objects.objects) != 0 {
		t.Fatalf("oversized begin created ticket, row, or staging object: ticket=%+v rows=%d objects=%d", ticket, len(db.uploads), len(objects.objects))
	}
}

// Only text stays fuse-bound at begin: a binary upload above the buffered
// fuse gets a ticket and completes by streaming (hk 4466). Under the old
// completion this declaration was refused.
func TestBeginAllowsLargeBinaryUpload(t *testing.T) {
	db := &fakeDB{rows: map[string]store.Attachment{}}
	objects := &fakeObjects{objects: map[string][]byte{}}
	m := &Manager{
		Store:   db,
		Objects: objects,
		Enabled: true,
		Config: Config{
			MaxBytes: DefaultMaxBytes, StorageCap: 1 << 40, PutCap: 10, GetCap: 10,
			Allow: map[string]bool{"image/png": true, "text/plain": true}, DirectUpload: true,
			StagingCap: 1 << 40, StagingClientCap: 1 << 40,
		},
	}
	ticket, err := m.BeginUpload(t.Context(), "node", "big.png", "image/png", BufferedCompletionMaxBytes+1, strings.Repeat("a", 64), "")
	if err != nil {
		t.Fatalf("large binary begin err=%v", err)
	}
	if ticket.UploadID == "" || ticket.URL == "" {
		t.Fatalf("ticket=%+v", ticket)
	}
	row, ok := db.uploads[ticket.UploadID]
	if !ok || row.SizeBytes != BufferedCompletionMaxBytes+1 {
		t.Fatalf("row=%+v ok=%v", row, ok)
	}
}

func TestCompleteRefusesObjectsAboveBufferedCompletionFuse(t *testing.T) {
	m, db, objs := newUploadManager(t)
	if BufferedCompletionMaxBytes != 256<<20 {
		t.Fatalf("completion fuse=%d, want 256 MiB", BufferedCompletionMaxBytes)
	}
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 30
	size := BufferedCompletionMaxBytes + 1
	// Rows created before the begin-time fuse (or racing a rollout) remain
	// protected by the completion-time check.
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "large.txt", MIME: "text/plain", SizeBytes: size,
		SHA256: strings.Repeat("a", 64), CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	objs.objects[stagingKey] = []byte("fake staged object")
	objs.headSizes = map[string]int64{stagingKey: size}

	_, _, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_completion_buffer_limit_exceeded") {
		t.Fatalf("complete err=%v, want buffered completion fuse refusal", err)
	}
	if got := db.uploads[uploadID].Reason; got != "upload_completion_buffer_limit_exceeded" {
		t.Fatalf("refusal reason=%q", got)
	}
}

// A non-text upload above the buffered-completion fuse completes by
// streaming: the staged object is verified while it is copied to a private
// holding object, then promoted to the content key. Under the old
// whole-buffer completion this upload was refused outright.
func TestStreamedCompletionStoresALargeBinaryUpload(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	m.Config.StagingCap = 1 << 40
	m.Config.StagingClientCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	body := make([]byte, size)
	copy(body, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	sha := shaOf(body)
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: sha, CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	objs.objects[stagingKey] = body

	x, created, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if err != nil || !created {
		t.Fatalf("streamed complete created=%v err=%v", created, err)
	}
	if x.SHA256 != sha || x.MIME != "image/png" || x.SizeBytes != size {
		t.Fatalf("stored attachment=%+v", x)
	}
	objs.mu.Lock()
	stored, staged := objs.objects[key(sha)], objs.objects[stagingKey]
	objs.mu.Unlock()
	held := hasObjectWithPrefix(objs, holdingKeyPrefix(uploadID))
	if !bytes.Equal(stored, body) {
		t.Fatalf("promoted object holds %d bytes, want the verified %d", len(stored), len(body))
	}
	if staged != nil || held {
		t.Fatalf("staging or holding object survived: staged=%v held=%v", staged != nil, held)
	}
	if row := db.uploads[uploadID]; row.Status != store.AttachmentUploadDone {
		t.Fatalf("row status=%q", row.Status)
	}
}

// The streamed path keeps the same refusals as the buffered one: a staging
// object whose size or sha256 does not match the declaration is refused and
// its objects deleted.
func TestStreamedCompletionRefusesMismatchedBytes(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: strings.Repeat("a", 64), CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	png := func(n int64) []byte {
		b := make([]byte, n)
		copy(b, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
		return b
	}
	// Right size, wrong bytes: the sha check refuses after the stream.
	objs.objects[stagingKey] = png(size)
	_, _, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_sha256_mismatch") {
		t.Fatalf("sha mismatch err=%v", err)
	}
	if hasObjectWithPrefix(objs, holdingKeyPrefix(uploadID)) {
		t.Fatal("holding object survived a refusal")
	}
	// The object is one byte short of the declaration: Head agrees with the
	// row (the fake reports the declared size), so the stream itself must
	// catch the shortfall.
	db.uploads[uploadID] = store.AttachmentUpload{
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size + 1,
		SHA256: strings.Repeat("a", 64), CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}
	objs.objects[stagingKey] = png(size)
	objs.headSizes = map[string]int64{stagingKey: size + 1}
	_, _, err = m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_size_mismatch") {
		t.Fatalf("short stream err=%v", err)
	}
	// The object is one byte longer than the declaration: the extra byte is
	// never stored, and the count check refuses.
	db.uploads[uploadID] = store.AttachmentUpload{
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: strings.Repeat("a", 64), CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}
	objs.objects[stagingKey] = png(size + 1)
	objs.headSizes = map[string]int64{stagingKey: size}
	_, _, err = m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_size_mismatch") {
		t.Fatalf("long stream err=%v", err)
	}
}

// A staging overwrite after the streamed verify cannot reach the content
// key: promotion writes the verified holding object, never the staging one.
// The presigned PUT stays valid until it expires, so this is the race the
// buffered path already guarded against.
func TestStreamedCompletionOverwriteBetweenVerifyAndPromote(t *testing.T) {
	m, _, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	body := make([]byte, size)
	copy(body, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	sha := shaOf(body)
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db := m.Store.(*fakeDB)
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: sha, CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	objs.objects[stagingKey] = body
	overwrote := false
	objs.beforePut = func(k string) {
		// The first hk/ put is the promotion: the staging object is forged
		// in place, as a reused presigned URL would. The stored object must
		// still be the bytes hk checked.
		if strings.HasPrefix(k, "hk/") && !overwrote {
			overwrote = true
			objs.mu.Lock()
			objs.objects[stagingKey] = bytes.Repeat([]byte("x"), int(size))
			objs.mu.Unlock()
		}
	}
	x, created, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if err != nil {
		t.Fatalf("streamed complete err=%v", err)
	}
	if !created {
		t.Fatal("streamed complete was not created")
	}
	objs.mu.Lock()
	stored := objs.objects[key(x.SHA256)]
	objs.mu.Unlock()
	if !bytes.Equal(stored, body) {
		t.Fatal("promoted object does not hold the verified bytes")
	}
}

// holdingKeyPrefix is the per-claim holding object prefix for an upload; the
// claim nonce suffix keeps concurrent claims from sharing one object.
func holdingKeyPrefix(uploadID string) string { return stagingPrefix + "done-" + uploadID + "-" }

// hasObjectWithPrefix reports whether any stored object key starts with p.
func hasObjectWithPrefix(objs *fakeObjects, p string) bool {
	objs.mu.Lock()
	defer objs.mu.Unlock()
	for k := range objs.objects {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// corruptGetObjects forges the bytes of the first key under prefix the first
// time it is read, the way a corrupted holding object would look to the
// promotion.
type corruptGetObjects struct {
	*fakeObjects
	prefix string
	forged []byte
	done   bool
}

func (c *corruptGetObjects) Get(ctx context.Context, k string) (io.ReadCloser, error) {
	if strings.HasPrefix(k, c.prefix) && !c.done {
		c.done = true
		c.mu.Lock()
		c.objects[k] = c.forged
		c.mu.Unlock()
	}
	return c.fakeObjects.Get(ctx, k)
}

// The promotion re-verifies the holding object as it streams: bytes that do
// not match the declaration are never stored, the partial object is removed,
// and the failure is retryable rather than a refusal.
func TestStreamedCompletionRefusesCorruptedHoldingObject(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	body := make([]byte, size)
	copy(body, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	sha := shaOf(body)
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: sha, CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	objs.objects[stagingKey] = body
	forged := bytes.Repeat([]byte("x"), int(size))
	m.Objects = &corruptGetObjects{fakeObjects: objs, prefix: holdingKeyPrefix(uploadID), forged: forged}

	_, _, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadNotReceived) {
		t.Fatalf("corrupted holding err=%v", err)
	}
	objs.mu.Lock()
	stored, staged := objs.objects[key(sha)], objs.objects[stagingKey]
	row := db.uploads[uploadID]
	objs.mu.Unlock()
	held := hasObjectWithPrefix(objs, holdingKeyPrefix(uploadID))
	if stored != nil {
		t.Fatal("a corrupted holding object reached the content key")
	}
	if held {
		t.Fatal("holding object survived")
	}
	if staged == nil {
		t.Fatal("staging object deleted on a retryable failure")
	}
	if row.Status != store.AttachmentUploadPending {
		t.Fatalf("row status=%q, want pending for retry", row.Status)
	}
}

// A promotion that fails verification must not delete the shared content key:
// a concurrent completion for the same sha can Stat the key while the
// promotion is in flight, skip its own write and register the attachment, so
// deleting the object afterwards would leave metadata pointing at nothing.
// The promotion writes a private per-claim key and publishes it with a
// server-side copy only after verification passes.
func TestStreamedCompletionFailedPromotionKeepsPublishedContentKey(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	m.Config.StagingCap = 1 << 40
	m.Config.StagingClientCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	body := make([]byte, size)
	copy(body, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	sha := shaOf(body)
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: sha, CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	objs.objects[stagingKey] = body

	// While the promotion streams, a concurrent completion for the same sha
	// publishes the verified content key and registers the attachment, the way
	// a putVerified that Stat-hit the in-flight object would.
	registered := false
	objs.beforePut = func(k string) {
		if registered || (k != key(sha) && !strings.HasPrefix(k, stagingPrefix+"promo-")) {
			return
		}
		registered = true
		objs.objects[key(sha)] = body
		_, _, _ = db.PutAttachment(t.Context(), store.Attachment{SHA256: sha, SizeBytes: size, MIME: "image/png", OriginalName: "big.png", CreatedBy: "node"}, 1<<40, 100)
	}
	forged := bytes.Repeat([]byte("x"), int(size))
	m.Objects = &corruptGetObjects{fakeObjects: objs, prefix: holdingKeyPrefix(uploadID), forged: forged}

	_, _, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadNotReceived) {
		t.Fatalf("corrupted holding err=%v", err)
	}
	if !registered {
		t.Fatal("the promotion never reached its private publish key")
	}
	objs.mu.Lock()
	stored := objs.objects[key(sha)]
	objs.mu.Unlock()
	if !bytes.Equal(stored, body) {
		t.Fatal("a failed promotion deleted or overwrote the published content key")
	}
	if _, ok := db.rows[sha]; !ok {
		t.Fatal("the concurrently registered attachment is gone")
	}
}

// corruptCopyObjects lands corrupted bytes at the copy's destination: the
// private promo object verifies, but the published content key does not.
type corruptCopyObjects struct {
	*fakeObjects
}

func (o *corruptCopyObjects) Copy(ctx context.Context, src, dst string) error {
	if err := o.fakeObjects.Copy(ctx, src, dst); err != nil {
		return err
	}
	o.mu.Lock()
	b := append([]byte(nil), o.objects[dst]...)
	b[0] ^= 0xff
	o.objects[dst] = b
	o.mu.Unlock()
	return nil
}

// The published object is what dedup adopts, so completion re-hashes it after
// the copy: corrupted output fails the promotion, the bad object is removed,
// and the released claim retries cleanly.
func TestStreamedCompletionVerifiesPublishedObject(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	m.Config.StagingCap = 1 << 40
	m.Config.StagingClientCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	body := make([]byte, size)
	copy(body, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	sha := shaOf(body)
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: sha, CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	objs.objects[stagingKey] = body

	m.Objects = &corruptCopyObjects{fakeObjects: objs}
	_, _, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadNotReceived) {
		t.Fatalf("corrupt published object err=%v", err)
	}
	objs.mu.Lock()
	_, published := objs.objects[key(sha)]
	objs.mu.Unlock()
	if published {
		t.Fatal("the corrupted content key survived verification")
	}
	if db.uploads[uploadID].Status != store.AttachmentUploadPending {
		t.Fatalf("claim not released: status=%q", db.uploads[uploadID].Status)
	}

	m.Objects = objs
	x, created, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if err != nil {
		t.Fatalf("retry after corrupt copy: %v", err)
	}
	if x.SHA256 != sha || !created || db.uploads[uploadID].Status != store.AttachmentUploadDone {
		t.Fatalf("recovered sha=%s created=%v status=%q", x.SHA256, created, db.uploads[uploadID].Status)
	}
	objs.mu.Lock()
	stored := objs.objects[key(sha)]
	objs.mu.Unlock()
	if !bytes.Equal(stored, body) {
		t.Fatal("the retried publication does not hold the verified bytes")
	}
}

// failGetObjects fails Get for one key: the post-copy read of the published
// object errors after Copy already landed bytes there.
type failGetObjects struct {
	*fakeObjects
	key string
}

func (o *failGetObjects) Get(ctx context.Context, k string) (io.ReadCloser, error) {
	if k == o.key {
		return nil, errors.New("read failed")
	}
	return o.fakeObjects.Get(ctx, k)
}

// A failed post-copy read must not leave the unverified object at the shared
// key: the released claim's retry would Stat-adopt it without re-writing.
func TestStreamedCompletionFailedPublishedReadCleansUp(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	m.Config.StagingCap = 1 << 40
	m.Config.StagingClientCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	body := make([]byte, size)
	copy(body, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	sha := shaOf(body)
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: sha, CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	objs.objects[stagingKey] = body

	m.Objects = &failGetObjects{fakeObjects: objs, key: key(sha)}
	_, _, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadNotReceived) {
		t.Fatalf("failed published read err=%v", err)
	}
	objs.mu.Lock()
	_, published := objs.objects[key(sha)]
	objs.mu.Unlock()
	if published {
		t.Fatal("the unverified content key survived a failed read")
	}
	if db.uploads[uploadID].Status != store.AttachmentUploadPending {
		t.Fatalf("claim not released: status=%q", db.uploads[uploadID].Status)
	}

	m.Objects = objs
	x, created, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if err != nil {
		t.Fatalf("retry after failed read: %v", err)
	}
	if x.SHA256 != sha || !created || db.uploads[uploadID].Status != store.AttachmentUploadDone {
		t.Fatalf("recovered sha=%s created=%v status=%q", x.SHA256, created, db.uploads[uploadID].Status)
	}
}

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
	obj, err := NewS3(Config{Endpoint: "https://example.r2.cloudflarestorage.com", Bucket: "bkt", AccessKey: "AK", SecretKey: "SK"})
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

// A completion whose lease was reclaimed while it promoted cannot move the
// row: its claim-scoped transitions no longer match, so it reports the upload
// in progress and the new claim is untouched. Under the old unscoped
// transitions the stale Completing -> Done would have succeeded and stolen
// the row from the completion that owns it now.
func TestReclaimedCompletionCannotMoveTheRow(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("promoted while a second completion owns the claim\n")
	tk := begin(t, m, "node", "race.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	const otherNonce = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	objs.beforePut = func(k string) {
		if !strings.HasPrefix(k, "hk/") {
			return
		}
		// The first completion's lease expired and a second CompleteUpload
		// reclaimed the row while the first was still promoting.
		ctx := context.Background()
		now := m.now()
		if ok, err := db.TransitionAttachmentUpload(ctx, tk.UploadID, store.AttachmentUploadCompleting, store.AttachmentUploadPending, "", now); err != nil || !ok {
			t.Errorf("reclaim ok=%v err=%v", ok, err)
		}
		if ok, err := db.ClaimAttachmentUpload(ctx, tk.UploadID, otherNonce, now); err != nil || !ok {
			t.Errorf("re-claim ok=%v err=%v", ok, err)
		}
	}
	_, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if !errors.Is(err, ErrUploadInProgress) {
		t.Fatalf("stale completion err=%v, want in progress", err)
	}
	row := db.uploads[tk.UploadID]
	if row.Status != store.AttachmentUploadCompleting || row.ClaimNonce != otherNonce {
		t.Fatalf("row=%+v, want completing under the new claim", row)
	}
}

// When the reclaiming completion refuses the upload while the stale one is
// still promoting, the stale completion must not report success: it re-reads
// the row and returns the refusal the row actually carries.
func TestReclaimedCompletionReportsTheRowsFinalState(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("promoted while the reclaiming completion refuses\n")
	tk := begin(t, m, "node", "race2.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	const otherNonce = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	objs.beforePut = func(k string) {
		if !strings.HasPrefix(k, "hk/") {
			return
		}
		ctx := context.Background()
		now := m.now()
		if ok, _ := db.TransitionAttachmentUpload(ctx, tk.UploadID, store.AttachmentUploadCompleting, store.AttachmentUploadPending, "", now); !ok {
			t.Error("reclaim failed")
		}
		if ok, _ := db.ClaimAttachmentUpload(ctx, tk.UploadID, otherNonce, now); !ok {
			t.Error("re-claim failed")
		}
		if ok, _ := db.TransitionAttachmentUploadClaim(ctx, tk.UploadID, otherNonce, store.AttachmentUploadRefused, "upload_sha256_mismatch", now); !ok {
			t.Error("refusal failed")
		}
	}
	_, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_sha256_mismatch") {
		t.Fatalf("stale completion err=%v, want the row's refusal", err)
	}
	if row := db.uploads[tk.UploadID]; row.Status != store.AttachmentUploadRefused {
		t.Fatalf("row status=%q, want refused", row.Status)
	}
}

// A live completion refreshes its claim lease so a stream that takes longer
// than uploadLease is not reclaimed; the touch is claim-scoped, so it stops
// matching once the claim is truly lost.
func TestCompletionHeartbeatRefreshesTheClaimLease(t *testing.T) {
	m, db, _ := newUploadManager(t)
	const uploadID = "0123456789abcdef0123456789abcdef"
	const nonce = "cccccccccccccccccccccccccccccccc"
	start := m.now()
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingPrefix + uploadID, Name: "a.txt", MIME: "text/plain",
		SizeBytes: 3, SHA256: strings.Repeat("a", 64), CreatedBy: "node",
		Status: store.AttachmentUploadCompleting, ClaimNonce: nonce,
		CreatedAt: start.Add(-time.Hour), ExpiresAt: start.Add(time.Hour), UpdatedAt: start,
	}}
	old := uploadHeartbeat
	uploadHeartbeat = time.Millisecond
	defer func() { uploadHeartbeat = old }()
	row := db.uploads[uploadID]
	stop := m.heartbeatUpload(t.Context(), row)
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		cur, ok, err := db.GetAttachmentUpload(t.Context(), uploadID)
		if err == nil && ok && cur.UpdatedAt.After(start) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat did not refresh the claim lease")
		}
		time.Sleep(time.Millisecond)
	}
}

// closeInvalidatingReader fails reads after Close, like a real object-store
// response body.
type closeInvalidatingReader struct {
	rc     io.ReadCloser
	closed bool
}

func (c *closeInvalidatingReader) Read(p []byte) (int, error) {
	if c.closed {
		return 0, errors.New("read on closed body")
	}
	return c.rc.Read(p)
}
func (c *closeInvalidatingReader) Close() error {
	c.closed = true
	return c.rc.Close()
}

// sizeBoundObjects consumes at most the declared size from each Put, the way
// the object store's size-bound PUT does, and serves the staging object with
// a body that fails after Close.
type sizeBoundObjects struct {
	*fakeObjects
	stagingKey string
}

func (s *sizeBoundObjects) Put(_ context.Context, k string, r io.Reader, size int64, _ string) error {
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	s.mu.Lock()
	s.objects[k] = buf
	s.mu.Unlock()
	return nil
}

func (s *sizeBoundObjects) Get(ctx context.Context, k string) (io.ReadCloser, error) {
	rc, err := s.fakeObjects.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if k == s.stagingKey {
		return &closeInvalidatingReader{rc: rc}, nil
	}
	return rc, nil
}

// A staging object that grew past the declared size between Head and Get is
// refused as upload_size_mismatch: the Put consumes at most the declared
// size, and the byte past it is read before the body is closed — a real
// object store invalidates the body on Close, which is what made the check
// dead in production.
func TestStreamedCompletionRefusesGrowthBetweenHeadAndGet(t *testing.T) {
	m, db, objs := newUploadManager(t)
	m.Config.MaxBytes = DefaultMaxBytes
	m.Config.StorageCap = 1 << 40
	size := BufferedCompletionMaxBytes + 1
	body := make([]byte, size)
	copy(body, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	sha := shaOf(body)
	const uploadID = "0123456789abcdef0123456789abcdef"
	stagingKey := stagingPrefix + uploadID
	db.uploads = map[string]store.AttachmentUpload{uploadID: {
		ID: uploadID, StagingKey: stagingKey, Name: "big.png", MIME: "image/png", SizeBytes: size,
		SHA256: sha, CreatedBy: "node", Status: store.AttachmentUploadPending,
		CreatedAt: m.now(), ExpiresAt: m.now().Add(time.Hour), UpdatedAt: m.now(),
	}}
	// The object is one byte longer than declared while Head still reports the
	// declared size: it grew after the head check, inside the presigned window.
	objs.objects[stagingKey] = append(body, 0x00)
	objs.headSizes = map[string]int64{stagingKey: size}
	m.Objects = &sizeBoundObjects{fakeObjects: objs, stagingKey: stagingKey}

	_, _, err := m.CompleteUpload(t.Context(), "node", uploadID)
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "upload_size_mismatch") {
		t.Fatalf("grown staging err=%v, want upload_size_mismatch", err)
	}
}

// failingFinalize fails FinalizeAttachmentUpload a fixed number of times, the
// transient database error before the claim's commit landed.
type failingFinalize struct {
	*fakeDB
	fail int
}

func (f *failingFinalize) FinalizeAttachmentUpload(ctx context.Context, id, nonce string, x store.Attachment, storageCap, putCap int64) (store.Attachment, bool, error) {
	if f.fail > 0 {
		f.fail--
		return store.Attachment{}, false, errors.New("database unavailable")
	}
	return f.fakeDB.FinalizeAttachmentUpload(ctx, id, nonce, x, storageCap, putCap)
}

// If the atomic finalize fails, its transaction rolled back and the claim is
// released: the row is pending again, the already-written content object is
// deduplicated by Stat on the retry, and the client gets the result without
// waiting out the lease.
func TestRetryAfterFinalizeFailureRecovers(t *testing.T) {
	m, db, objs := newUploadManager(t)
	body := []byte("finalize may fail once\n")
	tk := begin(t, m, "node", "f.txt", "text/plain", body, "")
	clientPUT(objs, tk, body)
	m.Store = &failingFinalize{fakeDB: db, fail: 1}
	if _, _, err := m.CompleteUpload(t.Context(), "node", tk.UploadID); err == nil {
		t.Fatal("the failed finalize was reported as success")
	}
	x, created, err := m.CompleteUpload(t.Context(), "node", tk.UploadID)
	if err != nil {
		t.Fatalf("retry after the released claim: %v", err)
	}
	if x.SHA256 != shaOf(body) || !created || db.uploads[tk.UploadID].Status != store.AttachmentUploadDone {
		t.Fatalf("recovered sha=%s created=%v status=%q", x.SHA256, created, db.uploads[tk.UploadID].Status)
	}
	if _, ok := objs.objects[stagingPrefix+tk.UploadID]; ok {
		t.Fatal("staging object survived the recovered completion")
	}
}
