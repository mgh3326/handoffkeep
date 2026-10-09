package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/remote"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A caller-supplied redirect policy that quotes the refused URL in its error
// text must not reach the MCP tool result: every redirect-following tool
// answers the fixed redirect_refused text with no Location or URL query.
func TestRefusedRedirectPolicyToolTextHasNoLocation(t *testing.T) {
	const marker = "mcp-location-canary.fixture"
	const fixtureSecret = "cf-access-secret.fixture.invalid"
	hk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/"+marker+"?echo="+fixtureSecret)
		w.WriteHeader(http.StatusFound)
	}))
	defer hk.Close()
	policy := &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error {
		return fmt.Errorf("fixture policy refused %s from %s", r.URL.String(), via[0].URL.String())
	}}
	server := NewStdio(remote.Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: "cf-access-id.fixture.invalid", CFAccessClientSecret: fixtureSecret, HTTP: policy}, "mcp-test")
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
	hu, _ := url.Parse(hk.URL)
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"get_document", map[string]any{"key": "fixture"}},
		{"list_documents", map[string]any{}},
		{"put_document", map[string]any{"key": "fixture", "kind": "note", "body": "b"}},
		{"memory_get", map[string]any{"agent": "a", "name": "n"}},
		{"recent_checkpoints", map[string]any{"session": "s"}},
		{"attachment_list", map[string]any{}},
	} {
		res, e := cs.CallTool(context.Background(), &gmcp.CallToolParams{Name: call.name, Arguments: call.args})
		if e != nil || res == nil || !res.IsError {
			t.Fatalf("%s: want a tool error result", call.name)
		}
		raw, _ := json.Marshal(res)
		if !strings.Contains(string(raw), "redirect_refused") {
			t.Errorf("%s: tool text is not the fixed redirect_refused error", call.name)
		}
		for _, leak := range []string{marker, fixtureSecret, "cf-access-id.fixture.invalid", "fixture-token", hu.Host, "/v1/", "fixture policy"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s: tool text carries a Location or request URL value", call.name)
			}
		}
	}
}
