// Package mcp exposes the API service as MCP tools over stdio or streamable HTTP.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Backend interface {
	Checkpoint(context.Context, string, store.Checkpoint) (store.Checkpoint, error)
	Recent(context.Context, string, string, int) ([]store.Checkpoint, error)
	PutMemory(context.Context, string, store.Memory) (store.Memory, error)
	GetMemory(context.Context, string, string) (store.Memory, bool, error)
	ListMemory(context.Context, string, bool) ([]store.Memory, error)
	PutDocument(context.Context, string, store.Document) (store.Document, bool, error)
	GetDocument(context.Context, string) (store.Document, bool, error)
	ListDocuments(context.Context, string, string, string, int) ([]store.Document, error)
	Search(context.Context, string, string, string, int) ([]store.SearchResult, error)
}

func New(service Backend, client string) *gmcp.Server {
	s := gmcp.NewServer(&gmcp.Implementation{Name: "handoffkeep", Version: "v0"}, nil)
	result := func(v any) (*gmcp.CallToolResult, any, error) {
		b, e := json.Marshal(v)
		if e != nil {
			return nil, nil, e
		}
		return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: string(b)}}}, nil, nil
	}
	gmcp.AddTool(s, &gmcp.Tool{Name: "search_context", Description: "Search checkpoints, documents, or memory."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Q       string `json:"q"`
		Scope   string `json:"scope,omitempty"`
		Session string `json:"session,omitempty"`
		Limit   int    `json:"limit,omitempty"`
	}) (*gmcp.CallToolResult, any, error) {
		if x.Scope == "" {
			x.Scope = "all"
		}
		v, e := service.Search(ctx, x.Q, x.Scope, x.Session, x.Limit)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"results": v})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "recent_checkpoints", Description: "Read recent checkpoints for one session."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Session string `json:"session"`
		Limit   int    `json:"limit,omitempty"`
	}) (*gmcp.CallToolResult, any, error) {
		v, e := service.Recent(ctx, x.Session, "", x.Limit)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"checkpoints": v})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "put_checkpoint", Description: "Store a session checkpoint."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Session string            `json:"session"`
		Kind    string            `json:"kind"`
		Title   string            `json:"title"`
		Body    string            `json:"body"`
		Refs    map[string]string `json:"refs,omitempty"`
	}) (*gmcp.CallToolResult, any, error) {
		v, e := service.Checkpoint(ctx, client, store.Checkpoint{Session: x.Session, Kind: x.Kind, Title: x.Title, Body: x.Body, Refs: x.Refs})
		if e != nil {
			return nil, nil, e
		}
		return result(v)
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "get_document", Description: "Get a document by key."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Key string `json:"key"`
	}) (*gmcp.CallToolResult, any, error) {
		v, ok, e := service.GetDocument(ctx, x.Key)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"found": ok, "document": v})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "put_document", Description: "Create or update a document."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Key     string `json:"key"`
		Kind    string `json:"kind"`
		Body    string `json:"body"`
		Session string `json:"session,omitempty"`
		Job     string `json:"job,omitempty"`
	}) (*gmcp.CallToolResult, any, error) {
		v, changed, e := service.PutDocument(ctx, client, store.Document{Key: x.Key, Kind: x.Kind, Body: x.Body, Session: x.Session, Job: x.Job})
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"document": v, "changed": changed})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "list_documents", Description: "List document metadata."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Prefix  string `json:"prefix,omitempty"`
		Kind    string `json:"kind,omitempty"`
		Session string `json:"session,omitempty"`
		Limit   int    `json:"limit,omitempty"`
	}) (*gmcp.CallToolResult, any, error) {
		v, e := service.ListDocuments(ctx, x.Prefix, x.Kind, x.Session, x.Limit)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"documents": v})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "memory_get", Description: "Get one agent memory."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Agent string `json:"agent"`
		Name  string `json:"name"`
	}) (*gmcp.CallToolResult, any, error) {
		v, ok, e := service.GetMemory(ctx, x.Agent, x.Name)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"found": ok, "memory": v})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "memory_put", Description: "Create or update agent memory."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Agent       string `json:"agent"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Content     string `json:"content"`
	}) (*gmcp.CallToolResult, any, error) {
		v, e := service.PutMemory(ctx, client, store.Memory{Agent: x.Agent, Name: x.Name, Description: x.Description, Type: x.Type, Content: x.Content})
		if e != nil {
			return nil, nil, e
		}
		return result(v)
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "memory_list", Description: "List agent memory metadata."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Agent string `json:"agent"`
	}) (*gmcp.CallToolResult, any, error) {
		v, e := service.ListMemory(ctx, x.Agent, false)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"memory": v})
	})
	return s
}

// HTTPHandler authenticates before handing a request to the official streamable transport.
func HTTPHandler(service api.Service, tokens api.Tokens) http.Handler {
	h := gmcp.NewStreamableHTTPHandler(func(r *http.Request) *gmcp.Server { client, _ := tokens.Client(r); return New(service, client) }, &gmcp.StreamableHTTPOptions{Stateless: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := tokens.Client(r); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		h.ServeHTTP(w, r)
	})
}
