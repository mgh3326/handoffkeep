package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This is the same tool server used by stdio; in-memory transports make the
// round trip deterministic while PostgreSQL integration is opt-in for local runs.
func TestToolRoundTrip(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for MCP integration tests")
	}
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b := gmcp.NewInMemoryTransports()
	server := New(api.Service{Store: s}, "mcp-test")
	ss, err := server.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(t.Context(), &gmcp.CallToolParams{Name: "put_checkpoint", Arguments: map[string]any{"session": "mcp-test", "kind": "checkpoint", "title": "roundtrip", "body": "ok"}})
	if err != nil || res.IsError {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}
