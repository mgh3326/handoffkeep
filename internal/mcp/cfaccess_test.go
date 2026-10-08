package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/remote"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The stdio attachment_get_url tool delegates to remote.Client.AttachmentURL:
// a presign 302 to an uppercase Access login host is an error result, never a
// returned URL. Driven through a real in-memory MCP round trip.
func TestAttachmentGetURLUppercaseLoginHost(t *testing.T) {
	hk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://TEAM.CloudflareAccess.COM/login?opaque=fixture")
		w.WriteHeader(http.StatusFound)
	}))
	defer hk.Close()
	server := NewStdio(remote.Client{
		URL:                  hk.URL,
		Token:                "fixture-token",
		CFAccessClientID:     "cf-access-id.fixture.invalid",
		CFAccessClientSecret: "cf-access-secret.fixture.invalid",
	}, "mcp-test")
	a, b := gmcp.NewInMemoryTransports()
	ss, e := server.Connect(context.Background(), a, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	client := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, e := client.Connect(context.Background(), b, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	res, e := cs.CallTool(context.Background(), &gmcp.CallToolParams{Name: "attachment_get_url", Arguments: map[string]any{"sha256": "fixture"}})
	if e == nil && res != nil && !res.IsError {
		t.Fatalf("uppercase Access login host returned as a presigned URL")
	}
	if e == nil && res != nil && res.IsError {
		raw, _ := json.Marshal(res)
		for _, leak := range []string{"cf-access-id.fixture.invalid", "cf-access-secret.fixture.invalid", "fixture-token", "opaque=fixture"} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("tool error result leaks %q", leak)
			}
		}
	}
}
