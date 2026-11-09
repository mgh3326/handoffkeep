package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// memObjects and memAttachmentDB are in-memory stand-ins for R2 and the
// attachment tables. The real attachments.Manager runs over them, so validate,
// the secret guard, dedupe and the lookup paths are the production code.
type memObjects struct{ objects map[string][]byte }

func (m *memObjects) Stat(_ context.Context, k string) error {
	if _, ok := m.objects[k]; ok {
		return nil
	}
	return errors.New("missing")
}
func (m *memObjects) Put(_ context.Context, k string, r io.Reader, _ int64, _ string) error {
	b, e := io.ReadAll(r)
	if e != nil {
		return e
	}
	m.objects[k] = b
	return nil
}
func (m *memObjects) Get(_ context.Context, k string) (io.ReadCloser, error) {
	b, ok := m.objects[k]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (*memObjects) Presign(_ context.Context, k string, _ time.Duration) (string, error) {
	return "https://objects.invalid/" + k + "?X-Amz-Signature=test", nil
}

// memAttachmentDB keeps one row per sha. Dedupe does not add a second ref
// here, which the round trip below does not need: every put uses new bytes.
type memAttachmentDB struct {
	rows    map[string]store.Attachment
	puts    int
	uploads map[string]store.AttachmentUpload
}

func (m *memAttachmentDB) GetAttachment(_ context.Context, sha string) (store.Attachment, bool, error) {
	x, ok := m.rows[sha]
	return x, ok, nil
}
func (*memAttachmentDB) CheckAttachmentCapacity(context.Context, int64, int64, int64) error {
	return nil
}
func (m *memAttachmentDB) PutAttachment(_ context.Context, x store.Attachment, _, _ int64) (store.Attachment, bool, error) {
	m.puts++
	if old, ok := m.rows[x.SHA256]; ok {
		return old, false, nil
	}
	m.rows[x.SHA256] = x
	return x, true, nil
}
func (m *memAttachmentDB) ListAttachments(_ context.Context, kind, id string, _ int) ([]store.Attachment, error) {
	var out []store.Attachment
	for _, x := range m.rows {
		if kind == "" || (x.RefKind == kind && x.RefID == id) {
			out = append(out, x)
		}
	}
	return out, nil
}
func (*memAttachmentDB) RecordAttachmentGet(context.Context, string, int64) error { return nil }
func (*memAttachmentDB) AttachmentUsage(context.Context) (store.AttachmentUsage, error) {
	return store.AttachmentUsage{}, nil
}

// attachmentBackend routes the three attachment calls to the real manager and
// leaves everything else to spyBackend, which fails any unexpected call.
type attachmentBackend struct {
	*spyBackend
	m *attachments.Manager
}

func (b attachmentBackend) PutAttachment(ctx context.Context, client, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	return b.m.Put(ctx, client, name, mime, ref, body)
}
func (b attachmentBackend) ListAttachments(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	return b.m.List(ctx, ref, limit)
}
func (b attachmentBackend) AttachmentURL(ctx context.Context, sha string) (string, error) {
	return b.m.Presign(ctx, sha)
}

func newAttachmentTestServer(t *testing.T) (*gmcp.ClientSession, *memAttachmentDB, *memObjects, *attachments.Manager) {
	t.Helper()
	db := &memAttachmentDB{rows: map[string]store.Attachment{}}
	objs := &memObjects{objects: map[string][]byte{}}
	allow := map[string]bool{}
	for _, x := range strings.Split(attachments.DefaultMIMEAllow, ",") {
		allow[x] = true
	}
	m := &attachments.Manager{Store: db, Objects: objs, Enabled: true, Config: attachments.Config{MaxBytes: 50 << 20, StorageCap: 1 << 30, PutCap: 1000, GetCap: 1000, Allow: allow, DirectUpload: true, StagingCap: 1 << 30}}
	server := New(attachmentBackend{spyBackend: &spyBackend{}, m: m}, "mcp-test")
	a, b := gmcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, db, objs, m
}

// attachmentCall returns the tool's JSON object, or the error text when the tool refused.
func attachmentCall(t *testing.T, cs *gmcp.ClientSession, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &gmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*gmcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return nil, errors.New(text)
	}
	var out map[string]any
	if e := json.Unmarshal([]byte(text), &out); e != nil {
		t.Fatalf("%s returned non-JSON %q: %v", name, text, e)
	}
	return out, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestAttachmentPutRoundTripsThroughMCP(t *testing.T) {
	cs, _, _, m := newAttachmentTestServer(t)
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0}, 64)...)
	pdf := []byte("%PDF-1.4\n% handoffkeep round trip\n")
	text := []byte("first line\nsecond line\n")
	for _, tc := range []struct {
		name, mime string
		body       []byte
	}{
		{"shot.png", "image/png", png},
		{"spec.pdf", "application/pdf", pdf},
		{"notes.txt", "text/plain", text},
	} {
		t.Run(tc.mime, func(t *testing.T) {
			out, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": tc.name, "mime": tc.mime, "content_base64": base64.StdEncoding.EncodeToString(tc.body)})
			if err != nil {
				t.Fatal(err)
			}
			sha := sha256Hex(tc.body)
			if out["created"] != true || out["sha256"] != sha || out["mime"] != tc.mime || out["ref"] != "none" {
				t.Fatalf("put response=%v", out)
			}
			if int(out["size_bytes"].(float64)) != len(tc.body) {
				t.Fatalf("size_bytes=%v want %d", out["size_bytes"], len(tc.body))
			}
			if out["reference"] != "hk:attachment/"+sha || out["lookup_path"] != "/v1/attachments/"+sha {
				t.Fatalf("stable reference=%v lookup=%v", out["reference"], out["lookup_path"])
			}
			if _, signed := out["url"]; signed {
				t.Fatalf("put response carries a URL: %v", out)
			}
			url, err := attachmentCall(t, cs, "attachment_get_url", map[string]any{"sha256": sha})
			if err != nil {
				t.Fatal(err)
			}
			if u, _ := url["url"].(string); !strings.HasPrefix(u, "https://") || url["expires_in_seconds"] != float64(600) {
				t.Fatalf("download url=%v", url)
			}
			// The stored bytes are what the GET route streams (Manager.Get).
			x, r, err := m.Get(t.Context(), sha)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(r)
			_ = r.Close()
			if !bytes.Equal(got, tc.body) || sha256Hex(got) != sha || x.SizeBytes != int64(len(tc.body)) {
				t.Fatalf("downloaded %d bytes sha=%s, want %d bytes sha=%s", len(got), sha256Hex(got), len(tc.body), sha)
			}
			again, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": tc.name, "mime": tc.mime, "content_base64": base64.StdEncoding.EncodeToString(tc.body)})
			if err != nil || again["created"] != false || again["sha256"] != sha {
				t.Fatalf("repeat put created=%v err=%v", again["created"], err)
			}
		})
	}
}

func TestAttachmentPutTaskRefIsListedByThatRef(t *testing.T) {
	cs, _, _, _ := newAttachmentTestServer(t)
	body := []byte("handoff for task twelve\n")
	out, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "handoff.txt", "mime": "text/plain", "content_base64": base64.StdEncoding.EncodeToString(body), "ref": "task:12"})
	if err != nil {
		t.Fatal(err)
	}
	if out["ref"] != "task:12" {
		t.Fatalf("ref=%v", out["ref"])
	}
	list, err := attachmentCall(t, cs, "attachment_list", map[string]any{"ref": "task:12"})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := list["attachments"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["sha256"] != sha256Hex(body) {
		t.Fatalf("attachment_list task:12=%v", list)
	}
	if _, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "x.txt", "mime": "text/plain", "content_base64": base64.StdEncoding.EncodeToString([]byte("x")), "ref": "task:012"}); err == nil {
		t.Fatal("non-canonical task id accepted")
	}
}

func TestAttachmentPutEnforcesTheRawCap(t *testing.T) {
	cs, db, _, _ := newAttachmentTestServer(t)
	atCap := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, attachmentPutMaxRawBytes-8)...)
	if len(base64.StdEncoding.EncodeToString(atCap)) != attachmentPutMaxBase64 {
		t.Fatalf("cap fixture is %d base64 characters, want %d", len(base64.StdEncoding.EncodeToString(atCap)), attachmentPutMaxBase64)
	}
	if _, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "max.png", "mime": "image/png", "content_base64": base64.StdEncoding.EncodeToString(atCap)}); err != nil {
		t.Fatalf("10 MiB raw payload refused: %v", err)
	}
	// One byte over shares the encoded length of the cap (base64 quantizes to
	// 4 characters), so only the post-decode raw check refuses it.
	oneOver := append(append([]byte{}, atCap...), 0)
	if len(base64.StdEncoding.EncodeToString(oneOver)) != attachmentPutMaxBase64 {
		t.Fatalf("one-over fixture no longer shares the cap's base64 length; the post-decode check is untested")
	}
	_, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "over.png", "mime": "image/png", "content_base64": base64.StdEncoding.EncodeToString(oneOver)})
	if err == nil || !strings.Contains(err.Error(), "attachment_too_large") {
		t.Fatalf("one byte over the raw cap err=%v", err)
	}
	// Three bytes over is longer than the cap in base64, refused before decoding.
	threeOver := append(append([]byte{}, atCap...), 0, 0, 0)
	_, err = attachmentCall(t, cs, "attachment_put", map[string]any{"name": "over3.png", "mime": "image/png", "content_base64": base64.StdEncoding.EncodeToString(threeOver)})
	if err == nil || !strings.Contains(err.Error(), "attachment_too_large") {
		t.Fatalf("three bytes over the raw cap err=%v", err)
	}
	if db.puts != 1 {
		t.Fatalf("over-cap payloads reached the store: puts=%d", db.puts)
	}
	if _, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "bad.txt", "mime": "text/plain", "content_base64": "!!not base64!!"}); err == nil || !strings.Contains(err.Error(), "standard base64") {
		t.Fatalf("invalid base64 err=%v", err)
	}
}

func TestAttachmentPutIsRegisteredOnBothTransportsWithItsContract(t *testing.T) {
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
			var desc string
			for _, tool := range tools.Tools {
				if tool.Name == "attachment_put" {
					desc = tool.Description
				}
			}
			if desc == "" {
				t.Fatal("attachment_put is not registered")
			}
			for _, mime := range strings.Split(attachments.DefaultMIMEAllow, ",") {
				if !strings.Contains(desc, mime) {
					t.Errorf("description omits allowed type %s", mime)
				}
			}
			for _, want := range []string{"10 MiB", "task:<id>", "hk:attachment/<sha256>", "/v1/attachments/<sha256>", "attachment_get_url"} {
				if !strings.Contains(desc, want) {
					t.Errorf("description omits %q", want)
				}
			}
		})
	}
}
