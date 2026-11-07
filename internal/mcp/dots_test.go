package mcp

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
)

func TestDotsToolsRegistered(t *testing.T) {
	srv := httptest.NewServer(HTTPHandler(api.Service{}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]bool{}
	for _, tool := range list.Tools {
		if tool.Name == "dots_inbox" || tool.Name == "dots_reply" {
			tools[tool.Name] = true
		}
	}
	if !tools["dots_inbox"] || !tools["dots_reply"] {
		t.Fatalf("dots tools: %v", tools)
	}
}
