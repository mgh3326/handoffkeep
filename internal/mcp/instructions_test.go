package mcp

import (
	"strings"
	"testing"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A client learns at initialize that a finding about a task is recorded on
// the task (hk 3592: a verification that lived only in a chat).
func TestInitializeCarriesRecordOnTaskInstructions(t *testing.T) {
	a, b := gmcp.NewInMemoryTransports()
	ss, err := New(&spyBackend{}, "mcp-test").Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	got := cs.InitializeResult().Instructions
	for _, want := range []string{"task_comment", "latest comments"} {
		if !strings.Contains(got, want) {
			t.Fatalf("initialize instructions %q lack %q", got, want)
		}
	}
}
