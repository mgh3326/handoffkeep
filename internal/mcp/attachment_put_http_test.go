package mcp

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerTransport adds the bearer header the HTTP MCP handler authenticates.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r2)
}

// The advertised 10 MiB raw cap must work over the real HTTP handler, not only
// over the in-memory transport the other tests use: the SDK's default request
// body limit (4 MiB) is applied on the HTTP path and nowhere else.
func TestAttachmentPutOverHTTPAcceptsTheAdvertisedCap(t *testing.T) {
	db := &memAttachmentDB{rows: map[string]store.Attachment{}}
	objs := &memObjects{objects: map[string][]byte{}}
	allow := map[string]bool{}
	for _, x := range strings.Split(attachments.DefaultMIMEAllow, ",") {
		allow[x] = true
	}
	m := &attachments.Manager{Store: db, Objects: objs, Enabled: true, Config: attachments.Config{MaxBytes: 50 << 20, StorageCap: 1 << 30, PutCap: 1000, GetCap: 1000, Allow: allow}}
	srv := httptest.NewServer(HTTPHandler(api.Service{Attachments: m}, api.Tokens{"http-client": "test-token"}, nil))
	t.Cleanup(srv.Close)
	cs, err := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(t.Context(), &gmcp.StreamableClientTransport{
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Transport: bearerTransport{token: "test-token"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	atCap := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0}, attachmentPutMaxRawBytes-8)...)
	out, err := attachmentCall(t, cs, "attachment_put", map[string]any{"name": "max.png", "mime": "image/png", "content_base64": base64.StdEncoding.EncodeToString(atCap)})
	if err != nil {
		t.Fatalf("10 MiB upload over HTTP refused: %v", err)
	}
	if out["created"] != true || out["size_bytes"] != float64(len(atCap)) {
		t.Fatalf("put response=%v", out)
	}
}
