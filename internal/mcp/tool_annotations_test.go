package mcp

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Clients such as ChatGPT ask for confirmation on any tool without a
// readOnlyHint. Every registered tool must say which side it is on, and the
// core tools below must stay read-only so they are not confirmed each call.
func TestEveryRegisteredToolHasAnnotations(t *testing.T) {
	s := newServer(&spyBackend{}, "annotations-test", true)
	registerOperatorTools(s, api.Service{Store: &store.Store{}}, principal{})
	registerDotsTools(s, api.Service{Store: &store.Store{}}, principal{})

	a, b := gmcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), a, nil)
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
	if len(tools.Tools) == 0 {
		t.Fatal("no tools registered")
	}
	byName := map[string]*gmcp.Tool{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool
		if tool.Annotations == nil {
			t.Errorf("tool %s has nil Annotations: set readOnlyAnnotations() or writeAnnotations()", tool.Name)
		}
	}

	readOnly := []string{
		"search_context", "recent_checkpoints", "get_document", "list_documents",
		"memory_get", "memory_list", "attachment_list", "attachment_get_url",
	}
	for _, name := range readOnly {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("tool %s is not registered", name)
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s must have ReadOnlyHint=true, got %+v", name, tool.Annotations)
		}
	}

	writes := []string{
		"put_checkpoint", "put_document", "memory_put", "attachment_put",
		"attachment_upload_begin", "attachment_upload_complete", "attachment_put_from_path",
	}
	for _, name := range writes {
		tool, ok := byName[name]
		if !ok {
			t.Errorf("tool %s is not registered", name)
			continue
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s must have ReadOnlyHint=false, got %+v", name, tool.Annotations)
		}
	}
}
