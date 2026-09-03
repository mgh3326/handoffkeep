package mcp

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type spyBackend struct{ checkpoints int }

func (s *spyBackend) Checkpoint(_ context.Context, _ string, x store.Checkpoint) (store.Checkpoint, error) {
	s.checkpoints++
	return x, nil
}
func (*spyBackend) Recent(context.Context, string, string, int) ([]store.Checkpoint, error) {
	return nil, errors.New("unexpected")
}
func (*spyBackend) PutMemory(context.Context, string, store.Memory) (store.Memory, error) {
	return store.Memory{}, errors.New("unexpected")
}
func (*spyBackend) GetMemory(context.Context, string, string) (store.Memory, bool, error) {
	return store.Memory{}, false, errors.New("unexpected")
}
func (*spyBackend) ListMemory(context.Context, string, bool) ([]store.Memory, error) {
	return nil, errors.New("unexpected")
}
func (*spyBackend) PutDocument(context.Context, string, store.Document) (store.Document, bool, error) {
	return store.Document{}, false, errors.New("unexpected")
}
func (*spyBackend) GetDocument(context.Context, string) (store.Document, bool, error) {
	return store.Document{}, false, errors.New("unexpected")
}
func (*spyBackend) ListDocuments(context.Context, string, string, string, int) ([]store.Document, error) {
	return nil, errors.New("unexpected")
}
func (*spyBackend) Search(context.Context, string, string, string, int) ([]store.SearchResult, error) {
	return nil, errors.New("unexpected")
}
func (*spyBackend) PutAttachment(context.Context, string, string, string, string, []byte) (store.Attachment, bool, error) {
	return store.Attachment{}, false, errors.New("unexpected")
}
func (*spyBackend) ListAttachments(context.Context, string, int) ([]store.Attachment, error) {
	return nil, errors.New("unexpected")
}
func (*spyBackend) AttachmentURL(context.Context, string) (string, error) {
	return "", errors.New("unexpected")
}

func TestPutCheckpointUsesBackendCore(t *testing.T) {
	backend := &spyBackend{}
	a, b := gmcp.NewInMemoryTransports()
	server := New(backend, "mcp-test")
	ss, err := server.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if _, err = cs.CallTool(t.Context(), &gmcp.CallToolParams{Name: "put_checkpoint", Arguments: map[string]any{"session": "test", "kind": "checkpoint", "title": "title", "body": "body"}}); err != nil {
		t.Fatal(err)
	}
	if backend.checkpoints != 1 {
		t.Fatalf("checkpoint core calls=%d", backend.checkpoints)
	}
}

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
