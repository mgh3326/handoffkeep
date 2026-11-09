package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Fake object-store and metadata methods for the direct-upload tools. They sit
// beside the fakes in attachment_put_test.go so the manager runs over them.

func (m *memObjects) PresignPut(_ context.Context, k string, _ time.Duration) (string, error) {
	return "https://objects.invalid/put/" + k + "?X-Amz-Signature=test", nil
}
func (m *memObjects) Head(_ context.Context, k string) (attachments.ObjectInfo, error) {
	b, ok := m.objects[k]
	if !ok {
		return attachments.ObjectInfo{}, errors.New("missing")
	}
	return attachments.ObjectInfo{Key: k, Size: int64(len(b)), Modified: time.Now().UTC()}, nil
}
func (m *memObjects) Delete(_ context.Context, k string) error {
	delete(m.objects, k)
	return nil
}
func (m *memObjects) List(_ context.Context, prefix string) ([]attachments.ObjectInfo, error) {
	var out []attachments.ObjectInfo
	for k, b := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, attachments.ObjectInfo{Key: k, Size: int64(len(b)), Modified: time.Now().UTC()})
		}
	}
	return out, nil
}

func (m *memAttachmentDB) CreateAttachmentUpload(_ context.Context, u store.AttachmentUpload) error {
	if m.uploads == nil {
		m.uploads = map[string]store.AttachmentUpload{}
	}
	m.uploads[u.ID] = u
	return nil
}
func (m *memAttachmentDB) GetAttachmentUpload(_ context.Context, id string) (store.AttachmentUpload, bool, error) {
	u, ok := m.uploads[id]
	return u, ok, nil
}
func (m *memAttachmentDB) TransitionAttachmentUpload(_ context.Context, id, from, to, reason string, now time.Time) (bool, error) {
	u, ok := m.uploads[id]
	if !ok || u.Status != from {
		return false, nil
	}
	u.Status, u.Reason, u.UpdatedAt = to, reason, now
	m.uploads[id] = u
	return true, nil
}

func (b attachmentBackend) BeginAttachmentUpload(ctx context.Context, client, name, mime string, size int64, sha, ref string) (attachments.UploadTicket, error) {
	return b.m.BeginUpload(ctx, client, name, mime, size, sha, ref)
}
func (b attachmentBackend) CompleteAttachmentUpload(ctx context.Context, client, id string) (store.Attachment, bool, error) {
	return b.m.CompleteUpload(ctx, client, id)
}

// The upload tools, driven through MCP, store the same bytes that attachment_put
// stores and answer with the same fields.
func TestDirectUploadToolsRoundTripThroughMCP(t *testing.T) {
	cs, db, objs, m := newAttachmentTestServer(t)
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{3}, 64)...)
	sha := sha256Hex(png)

	begin, err := attachmentCall(t, cs, "attachment_upload_begin", map[string]any{"name": "shot.png", "mime": "image/png", "size": len(png), "sha256": sha})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := begin["upload_id"].(string)
	if id == "" || begin["method"] != "PUT" || begin["expires_in_seconds"] != float64(600) {
		t.Fatalf("begin=%v", begin)
	}
	if u, _ := begin["url"].(string); !strings.HasPrefix(u, "https://") {
		t.Fatalf("begin url=%v", begin["url"])
	}
	// The client PUTs the bytes straight to the store; hk never sees them.
	objs.objects["incoming/"+id] = png

	done, err := attachmentCall(t, cs, "attachment_upload_complete", map[string]any{"upload_id": id})
	if err != nil {
		t.Fatal(err)
	}
	if done["created"] != true || done["sha256"] != sha || done["reference"] != "hk:attachment/"+sha || done["lookup_path"] != "/v1/attachments/"+sha || done["size_bytes"] != float64(len(png)) {
		t.Fatalf("complete=%v", done)
	}
	if _, staged := objs.objects["incoming/"+id]; staged {
		t.Fatal("staging object survived completion")
	}
	_, r, err := m.Get(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, png) {
		t.Fatalf("stored %d bytes, want %d", len(got), len(png))
	}

	// Same response keys as attachment_put, so a client can use either.
	put, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "again.png", "mime": "image/png", "content_base64": base64.StdEncoding.EncodeToString(png)})
	if err != nil {
		t.Fatal(err)
	}
	if keysOf(done) != keysOf(put) {
		t.Fatalf("complete keys %s != put keys %s", keysOf(done), keysOf(put))
	}
	if put["created"] != false {
		t.Fatalf("second store of the same bytes created=%v", put["created"])
	}
	if len(db.uploads) != 1 {
		t.Fatalf("upload rows=%d", len(db.uploads))
	}
}

func TestDirectUploadRefusalIsReturnedAsAToolError(t *testing.T) {
	cs, _, objs, _ := newAttachmentTestServer(t)
	body := []byte("declared\n")
	begin, err := attachmentCall(t, cs, "attachment_upload_begin", map[string]any{"name": "a.txt", "mime": "text/plain", "size": len(body), "sha256": sha256Hex(body)})
	if err != nil {
		t.Fatal(err)
	}
	id := begin["upload_id"].(string)
	objs.objects["incoming/"+id] = []byte(strings.ToUpper(string(body)))
	_, err = attachmentCall(t, cs, "attachment_upload_complete", map[string]any{"upload_id": id})
	if err == nil || !strings.Contains(err.Error(), "attachment_upload_refused") || !strings.Contains(err.Error(), "upload_sha256_mismatch") {
		t.Fatalf("refused complete err=%v", err)
	}
	if _, err := attachmentCall(t, cs, "attachment_upload_begin", map[string]any{"name": "a.exe", "mime": "application/x-msdownload", "size": 3, "sha256": sha256Hex(body)}); err == nil {
		t.Fatal("disallowed mime declared")
	}
}

func TestDirectUploadToolsDescribeTheContract(t *testing.T) {
	for name, server := range map[string]*gmcp.Server{"http": New(&spyBackend{}, "stdio"), "stdio": NewStdio(&spyBackend{}, "any-token-name")} {
		t.Run(name, func(t *testing.T) {
			a, b := gmcp.NewInMemoryTransports()
			ss, err := server.Connect(t.Context(), a, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(t.Context(), b, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			tools, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			descs := map[string]string{}
			for _, tool := range tools.Tools {
				descs[tool.Name] = tool.Description
			}
			begin, complete := descs["attachment_upload_begin"], descs["attachment_upload_complete"]
			if begin == "" || complete == "" {
				t.Fatalf("upload tools registered: begin=%t complete=%t", begin != "", complete != "")
			}
			for _, want := range []string{"presigned PUT", "no authorization header", "ten minutes", "Never log the url", "attachment_upload_complete"} {
				if !strings.Contains(begin, want) {
					t.Errorf("begin description omits %q", want)
				}
			}
			for _, want := range []string{"attachment_upload_refused", "attachment_upload_not_received", "hk:attachment/<sha256>"} {
				if !strings.Contains(complete, want) {
					t.Errorf("complete description omits %q", want)
				}
			}
		})
	}
}

func keysOf(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func (*spyBackend) BeginAttachmentUpload(context.Context, string, string, string, int64, string, string) (attachments.UploadTicket, error) {
	return attachments.UploadTicket{}, errors.New("unexpected")
}
func (*spyBackend) CompleteAttachmentUpload(context.Context, string, string) (store.Attachment, bool, error) {
	return store.Attachment{}, false, errors.New("unexpected")
}
