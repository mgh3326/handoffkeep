package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// uploadObjects and uploadDB are in-memory stand-ins for the object store and
// the attachment tables; the real attachments.Manager runs over them.
type uploadObjects struct{ objects map[string][]byte }

func (o *uploadObjects) Stat(_ context.Context, k string) error {
	if _, ok := o.objects[k]; ok {
		return nil
	}
	return errors.New("missing")
}
func (o *uploadObjects) Put(_ context.Context, k string, r io.Reader, _ int64, _ string) error {
	b, _ := io.ReadAll(r)
	o.objects[k] = b
	return nil
}
func (o *uploadObjects) Get(_ context.Context, k string) (io.ReadCloser, error) {
	b, ok := o.objects[k]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (o *uploadObjects) Presign(context.Context, string, time.Duration) (string, error) {
	return "https://objects.invalid/get", nil
}
func (o *uploadObjects) PresignPut(_ context.Context, k string, _ time.Duration, _ int64) (string, error) {
	return "https://objects.invalid/put/" + k + "?X-Amz-Signature=test", nil
}
func (o *uploadObjects) Head(_ context.Context, k string) (attachments.ObjectInfo, error) {
	b, ok := o.objects[k]
	if !ok {
		return attachments.ObjectInfo{}, attachments.ErrObjectNotFound
	}
	return attachments.ObjectInfo{Key: k, Size: int64(len(b)), Modified: time.Now().UTC()}, nil
}
func (o *uploadObjects) Delete(_ context.Context, k string) error {
	delete(o.objects, k)
	return nil
}
func (o *uploadObjects) List(_ context.Context, prefix string) ([]attachments.ObjectInfo, error) {
	var out []attachments.ObjectInfo
	for k := range o.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, attachments.ObjectInfo{Key: k, Modified: time.Now().UTC()})
		}
	}
	return out, nil
}

type uploadDB struct {
	rows    map[string]store.Attachment
	uploads map[string]store.AttachmentUpload
}

func (d *uploadDB) GetAttachment(_ context.Context, sha string) (store.Attachment, bool, error) {
	x, ok := d.rows[sha]
	return x, ok, nil
}
func (*uploadDB) CheckAttachmentCapacity(context.Context, int64, int64, int64) error { return nil }
func (d *uploadDB) PutAttachment(_ context.Context, x store.Attachment, _, _ int64) (store.Attachment, bool, error) {
	if old, ok := d.rows[x.SHA256]; ok {
		return old, false, nil
	}
	d.rows[x.SHA256] = x
	return x, true, nil
}
func (*uploadDB) RecordAttachmentGet(context.Context, string, int64) error { return nil }
func (*uploadDB) ListAttachments(context.Context, string, string, int) ([]store.Attachment, error) {
	return nil, nil
}
func (*uploadDB) AttachmentUsage(context.Context) (store.AttachmentUsage, error) {
	return store.AttachmentUsage{}, nil
}
func (d *uploadDB) ReserveAttachmentUpload(_ context.Context, u store.AttachmentUpload, stagingCap, clientCap int64) error {
	return reserveFake(d.uploads, u, stagingCap, clientCap)
}
func (d *uploadDB) ListUnreleasedAttachmentUploads(_ context.Context, createdBefore time.Time, limit int) ([]store.AttachmentUpload, error) {
	return unreleasedFake(d.uploads, createdBefore, limit), nil
}
func (d *uploadDB) MarkAttachmentUploadStagingDeleted(_ context.Context, id string, now time.Time) error {
	markReleasedFake(d.uploads, id, now)
	return nil
}

func (d *uploadDB) GetAttachmentUpload(_ context.Context, id string) (store.AttachmentUpload, bool, error) {
	u, ok := d.uploads[id]
	return u, ok, nil
}
func (d *uploadDB) TransitionAttachmentUpload(_ context.Context, id, from, to, reason string, now time.Time) (bool, error) {
	u, ok := d.uploads[id]
	if !ok || u.Status != from {
		return false, nil
	}
	u.Status, u.Reason, u.UpdatedAt = to, reason, now
	d.uploads[id] = u
	return true, nil
}

func uploadServer(t *testing.T) (*httptest.Server, *uploadObjects) {
	t.Helper()
	objs := &uploadObjects{objects: map[string][]byte{}}
	allow := map[string]bool{}
	for _, x := range strings.Split(attachments.DefaultMIMEAllow, ",") {
		allow[x] = true
	}
	m := &attachments.Manager{Store: &uploadDB{rows: map[string]store.Attachment{}, uploads: map[string]store.AttachmentUpload{}}, Objects: objs, Enabled: true, Config: attachments.Config{MaxBytes: 1 << 20, StorageCap: 1 << 30, PutCap: 1000, GetCap: 1000, Allow: allow, DirectUpload: true, StagingCap: 1 << 30, StagingClientCap: 1 << 30}}
	h := httptest.NewServer(Server{Service: Service{Attachments: m}, Tokens: Tokens{"node": "node-token"}}.Handler())
	t.Cleanup(h.Close)
	return h, objs
}

func uploadCall(t *testing.T, h *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestDirectUploadRoutesBeginCompleteAndRefuse(t *testing.T) {
	h, objs := uploadServer(t)
	body := []byte("handoff notes\n")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	status, tk := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads", "node-token", map[string]any{"name": "notes.txt", "mime": "text/plain", "size": len(body), "sha256": sha})
	if status != http.StatusCreated || tk["method"] != "PUT" || tk["upload_id"] == "" {
		t.Fatalf("begin status=%d body=%v", status, tk)
	}
	id := tk["upload_id"].(string)
	if status, _ := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads", "", map[string]any{"name": "x.txt", "mime": "text/plain", "size": 1, "sha256": sha}); status != http.StatusUnauthorized {
		t.Fatalf("begin without a token status=%d", status)
	}

	objs.objects["incoming/"+id] = body
	status, done := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads/"+id+"/complete", "node-token", map[string]any{})
	if status != http.StatusOK || done["created"] != true || done["reference"] != "hk:attachment/"+sha || done["lookup_path"] != "/v1/attachments/"+sha || done["url"] != nil {
		t.Fatalf("complete status=%d body=%v", status, done)
	}
	if _, staged := objs.objects["incoming/"+id]; staged {
		t.Fatal("staging object survived completion")
	}
	if status, again := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads/"+id+"/complete", "node-token", map[string]any{}); status != http.StatusOK || again["created"] != false {
		t.Fatalf("repeat complete status=%d body=%v", status, again)
	}

	// A second upload whose bytes do not match its declaration is refused with
	// a 422 and the fixed reason code, and the refusal is final.
	bad := []byte("different bytes\n")
	status, tk2 := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads", "node-token", map[string]any{"name": "bad.txt", "mime": "text/plain", "size": len(bad), "sha256": sha})
	if status != http.StatusCreated {
		t.Fatalf("second begin status=%d", status)
	}
	id2 := tk2["upload_id"].(string)
	objs.objects["incoming/"+id2] = bad
	status, refused := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads/"+id2+"/complete", "node-token", map[string]any{})
	if status != http.StatusUnprocessableEntity || refused["error"] != "attachment_upload_refused" || refused["reason"] != "upload_sha256_mismatch" {
		t.Fatalf("refused status=%d body=%v", status, refused)
	}
	// Only the begin answer may carry the presigned URL; no completion or refusal
	// answer may.
	for name, body := range map[string]map[string]any{"complete": done, "refusal": refused} {
		if raw, _ := json.Marshal(body); strings.Contains(string(raw), "X-Amz-Signature") {
			t.Fatalf("%s answer carries a signed URL: %s", name, raw)
		}
	}
	if raw, _ := json.Marshal(tk); !strings.Contains(string(raw), "X-Amz-Signature") {
		t.Fatalf("begin answer lost its URL: %s", raw)
	}

	if status, body := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads/"+strings.Repeat("0", 32)+"/complete", "node-token", map[string]any{}); status != http.StatusNotFound || body["error"] != "attachment_upload_not_found" {
		t.Fatalf("unknown id status=%d body=%v", status, body)
	}
	if status, _ := uploadCall(t, h, http.MethodPost, "/v1/attachments/uploads", "node-token", map[string]any{"name": "a.exe", "mime": "application/x-msdownload", "size": 1, "sha256": sha}); status < 400 {
		t.Fatalf("disallowed mime status=%d", status)
	}
}

// reserveFake applies the store's budget: rows whose staging object may still
// exist count against the global and the per-creator cap.
func reserveFake(uploads map[string]store.AttachmentUpload, u store.AttachmentUpload, stagingCap, clientCap int64) error {
	var used, usedByClient int64
	for _, o := range uploads {
		if !o.StagingDeletedAt.IsZero() {
			continue
		}
		used += o.SizeBytes
		if o.CreatedBy == u.CreatedBy {
			usedByClient += o.SizeBytes
		}
	}
	if used+u.SizeBytes > stagingCap || usedByClient+u.SizeBytes > clientCap {
		return store.ErrAttachmentStagingCap
	}
	uploads[u.ID] = u
	return nil
}

func unreleasedFake(uploads map[string]store.AttachmentUpload, createdBefore time.Time, limit int) []store.AttachmentUpload {
	var out []store.AttachmentUpload
	for _, o := range uploads {
		if o.StagingDeletedAt.IsZero() && o.CreatedAt.Before(createdBefore) && len(out) < limit {
			out = append(out, o)
		}
	}
	return out
}

func markReleasedFake(uploads map[string]store.AttachmentUpload, id string, now time.Time) {
	if u, ok := uploads[id]; ok && u.StagingDeletedAt.IsZero() {
		u.StagingDeletedAt = now
		uploads[id] = u
	}
}
