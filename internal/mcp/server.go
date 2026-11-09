// Package mcp exposes the API service as MCP tools over stdio or streamable HTTP.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/buildinfo"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
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
	ListDocuments(context.Context, string, string, string, int64, int) ([]store.Document, error)
	Search(context.Context, string, string, string, int) ([]store.SearchResult, error)
	PutAttachment(context.Context, string, string, string, string, []byte) (store.Attachment, bool, error)
	ListAttachments(context.Context, string, int) ([]store.Attachment, error)
	AttachmentURL(context.Context, string) (string, error)
}

// attachmentPutMaxRawBytes is the largest file attachment_put accepts before
// base64 decoding: 10 MiB, the size dots asked to round-trip. Its base64 form
// is attachmentPutMaxBase64 characters; a larger payload is refused by length.
const attachmentPutMaxRawBytes = 10 << 20

var attachmentPutMaxBase64 = base64.StdEncoding.EncodedLen(attachmentPutMaxRawBytes)

// mcpRequestBodyLimit caps one HTTP MCP request body: the largest attachment_put
// payload plus 1 MiB for the JSON-RPC envelope.
var mcpRequestBodyLimit = int64(attachmentPutMaxBase64) + 1<<20

// attachmentPutDescription states the allowed types from the same constant the
// attachment manager reads, so the list cannot drift from the enforced one.
var attachmentPutDescription = "Upload one attachment from base64 content; the remote-safe path, with no server file path. " +
	"content_base64 is standard padded base64 of at most 10 MiB raw (" + strconv.Itoa(attachmentPutMaxBase64) + " base64 characters). " +
	"name is the original file name; mime is optional and defaults to the sniffed type. " +
	"Allowed types by default: " + strings.ReplaceAll(attachments.DefaultMIMEAllow, ",", ", ") + ". " +
	"Text is refused when the secret guard matches it. " +
	"ref is optional: checkpoint:<id>, document:<key>, memory:<agent>/<name>, task:<id>, or none. " +
	"Returns sha256, size_bytes, mime, ref, reference (hk:attachment/<sha256>, the stable reference records store) " +
	"and lookup_path (GET /v1/attachments/<sha256> with the bearer token). " +
	"The signed download URL is never returned; attachment_get_url issues one valid for ten minutes."

// attachmentRefString renders an attachment's ref the way ParseRef reads it.
func attachmentRefString(x store.Attachment) string {
	if x.RefKind == "" || x.RefKind == "none" {
		return "none"
	}
	return x.RefKind + ":" + x.RefID
}

func New(service Backend, client string) *gmcp.Server {
	return newServer(service, client, false)
}

// NewStdio is the only constructor that exposes a server-local path upload.
func NewStdio(service Backend, client string) *gmcp.Server {
	return newServer(service, client, true)
}
func newServer(service Backend, client string, allowPathUpload bool) *gmcp.Server {
	s := gmcp.NewServer(&gmcp.Implementation{Name: "handoffkeep", Version: buildinfo.Revision()}, nil)
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
		Session string     `json:"session"`
		Kind    string     `json:"kind"`
		Title   string     `json:"title"`
		Body    string     `json:"body"`
		Refs    store.Refs `json:"refs,omitempty"`
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
		v, e := service.ListDocuments(ctx, x.Prefix, x.Kind, x.Session, 0, x.Limit)
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
	gmcp.AddTool(s, &gmcp.Tool{Name: "attachment_list", Description: "List immutable attachment metadata, optionally by reference."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		Ref   string `json:"ref,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}) (*gmcp.CallToolResult, any, error) {
		v, e := service.ListAttachments(ctx, x.Ref, x.Limit)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"attachments": v})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "attachment_get_url", Description: "Create a ten-minute private presigned attachment URL."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		SHA256 string `json:"sha256"`
	}) (*gmcp.CallToolResult, any, error) {
		v, e := service.AttachmentURL(ctx, x.SHA256)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{"url": v, "expires_in_seconds": 600})
	})
	gmcp.AddTool(s, &gmcp.Tool{Name: "attachment_put", Description: attachmentPutDescription}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
		ContentBase64 string `json:"content_base64"`
		Name          string `json:"name"`
		MIME          string `json:"mime,omitempty"`
		Ref           string `json:"ref,omitempty"`
	}) (*gmcp.CallToolResult, any, error) {
		// The base64 length is checked before decoding so an oversized payload
		// is refused without allocating its decoded form.
		if len(x.ContentBase64) > attachmentPutMaxBase64 {
			return nil, nil, attachments.ErrTooLarge
		}
		body, e := base64.StdEncoding.DecodeString(x.ContentBase64)
		if e != nil {
			return nil, nil, errors.New("content_base64 must be standard base64 (RFC 4648, padded)")
		}
		// base64 length is quantized to 4 characters, so a payload one byte over
		// the raw ceiling can share the encoded length of one at it; the raw
		// size is checked here so the ceiling is exact.
		if len(body) > attachmentPutMaxRawBytes {
			return nil, nil, attachments.ErrTooLarge
		}
		v, created, e := service.PutAttachment(ctx, client, x.Name, x.MIME, x.Ref, body)
		if e != nil {
			return nil, nil, e
		}
		return result(map[string]any{
			"attachment":  v,
			"created":     created,
			"sha256":      v.SHA256,
			"size_bytes":  v.SizeBytes,
			"mime":        v.MIME,
			"ref":         attachmentRefString(v),
			"reference":   "hk:attachment/" + v.SHA256,
			"lookup_path": "/v1/attachments/" + v.SHA256,
		})
	})
	if allowPathUpload {
		gmcp.AddTool(s, &gmcp.Tool{Name: "attachment_put_from_path", Description: "stdio only: upload a local file as a private attachment."}, func(ctx context.Context, _ *gmcp.CallToolRequest, x struct {
			Path string `json:"path"`
			Name string `json:"name,omitempty"`
			MIME string `json:"mime"`
			Ref  string `json:"ref,omitempty"`
		}) (*gmcp.CallToolResult, any, error) {
			b, e := os.ReadFile(x.Path)
			if e != nil {
				return nil, nil, e
			}
			if x.Name == "" {
				x.Name = filepath.Base(x.Path)
			}
			v, created, e := service.PutAttachment(ctx, client, x.Name, x.MIME, x.Ref, b)
			if e != nil {
				return nil, nil, e
			}
			return result(map[string]any{"attachment": v, "created": created})
		})
	}
	return s
}

// HTTPHandler authenticates before handing a request to the official streamable transport.
// An hk bearer token keeps its previous behavior; when access is configured, a
// Cloudflare Access email assertion for the MCP application is the second
// credential. A service-token identity is never accepted here.
func HTTPHandler(service api.Service, tokens api.Tokens, access *cfaccess.Verifier) http.Handler {
	h := gmcp.NewStreamableHTTPHandler(func(r *http.Request) *gmcp.Server {
		p, _ := principalFrom(r.Context())
		s := newServer(service, p.Actor, false)
		registerOperatorTools(s, service, p)
		registerDotsTools(s, service, p)
		return s
	}, &gmcp.StreamableHTTPOptions{
		Stateless: true,
		// The SDK default (4 MiB) would refuse a 10 MiB attachment_put before the
		// tool runs: its base64 form is attachmentPutMaxBase64 bytes. The limit
		// is that plus headroom for the JSON-RPC envelope.
		MaxRequestBodyBytes: mcpRequestBodyLimit,
		// The SDK's localhost guard rejects a loopback request whose Host is
		// not loopback. It defends unauthenticated localhost servers against
		// DNS rebinding; /mcp authenticates every request before the SDK sees
		// it, and a local proxy (cloudflared) reaches it on 127.0.0.1:8800
		// with the public Host, so the guard only blocks legitimate traffic.
		DisableLocalhostProtection: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := authenticate(r, tokens, access)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		ctx := withPrincipal(r.Context(), p)
		// A bearer token is fenced as the transport-authenticated client id it
		// always was. An Access identity is an operator path like the console,
		// which deliberately has no implicit central-role principal.
		if !p.Access {
			ctx = store.WithWrkPrincipal(ctx, p.Actor, nil)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}
