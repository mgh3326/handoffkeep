// Package api is the one application core used by HTTP, CLI proxies, and MCP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type Service struct{ Store *store.Store }

func (s Service) Checkpoint(ctx context.Context, client string, x store.Checkpoint) (store.Checkpoint, error) {
	x.CreatedBy = client
	return s.Store.CreateCheckpoint(ctx, x)
}
func (s Service) Recent(ctx context.Context, session, kind string, limit int) ([]store.Checkpoint, error) {
	return s.Store.RecentCheckpoints(ctx, session, kind, limit)
}
func (s Service) PutMemory(ctx context.Context, client string, x store.Memory) (store.Memory, error) {
	x.UpdatedBy = client
	return s.Store.PutMemory(ctx, x)
}
func (s Service) GetMemory(ctx context.Context, agent, name string) (store.Memory, bool, error) {
	return s.Store.GetMemory(ctx, agent, name)
}
func (s Service) ListMemory(ctx context.Context, agent string, content bool) ([]store.Memory, error) {
	return s.Store.ListMemory(ctx, agent, content)
}
func (s Service) PutDocument(ctx context.Context, client string, x store.Document) (store.Document, bool, error) {
	x.CreatedBy = client
	return s.Store.PutDocument(ctx, x)
}
func (s Service) GetDocument(ctx context.Context, key string) (store.Document, bool, error) {
	return s.Store.GetDocument(ctx, key)
}
func (s Service) ListDocuments(ctx context.Context, prefix, kind, session string, limit int) ([]store.Document, error) {
	return s.Store.ListDocuments(ctx, prefix, kind, session, limit)
}
func (s Service) Search(ctx context.Context, q, scope, session string, limit int) ([]store.SearchResult, error) {
	return s.Store.Search(ctx, q, scope, session, limit)
}

type Tokens map[string]string

func LoadTokens(path string) (Tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := Tokens{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "HANDOFFKEEP_TOKEN_") || v == "" {
			return nil, errors.New("invalid HANDOFFKEEP_AUTH_FILE")
		}
		id := strings.TrimPrefix(k, "HANDOFFKEEP_TOKEN_")
		if id == "" {
			return nil, errors.New("invalid token client id")
		}
		out[id] = v
	}
	if len(out) == 0 {
		return nil, errors.New("HANDOFFKEEP_AUTH_FILE contains no tokens")
	}
	return out, nil
}
func (t Tokens) Client(r *http.Request) (string, bool) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if raw == "" {
		return "", false
	}
	for id, want := range t {
		if raw == want {
			return id, true
		}
	}
	return "", false
}

type Server struct {
	Service Service
	Tokens  Tokens
}

func (s Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, map[string]string{"status": "ok"}) })
	m.HandleFunc("/v1/checkpoints", s.checkpoints)
	m.HandleFunc("GET /v1/search", s.search)
	m.HandleFunc("GET /v1/documents", s.documents)
	m.HandleFunc("/v1/documents/{key...}", s.document)
	m.HandleFunc("GET /v1/memory/{agent}", s.memoryList)
	m.HandleFunc("/v1/memory/{agent}/{name}", s.memory)
	return m
}
func (s Server) auth(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := s.Tokens.Client(r)
	if !ok {
		jsonOut(w, 401, map[string]string{"error": "unauthorized"})
	}
	return id, ok
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func appErr(w http.ResponseWriter, e error) {
	if p, ok := strings.CutPrefix(e.Error(), "secret_like_content:"); ok {
		jsonOut(w, 400, map[string]string{"error": "secret_like_content", "pattern": p})
		return
	}
	jsonOut(w, 400, map[string]string{"error": "invalid_context"})
}
func decode(r *http.Request, v any, max int) error {
	de := json.NewDecoder(io.LimitReader(r.Body, int64(max+4096)))
	de.DisallowUnknownFields()
	return de.Decode(v)
}
func queryLimit(r *http.Request, def, max int) (int, error) {
	x := def
	if v := r.URL.Query().Get("limit"); v != "" {
		var e error
		x, e = strconv.Atoi(v)
		if e != nil || x < 1 || x > max {
			return 0, errors.New("limit")
		}
	}
	return x, nil
}
func (s Server) checkpoints(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		defer r.Body.Close()
		var x store.Checkpoint
		if e := decode(r, &x, store.MaxBytes); e != nil {
			appErr(w, e)
			return
		}
		x, e := s.Service.Checkpoint(r.Context(), client, x)
		if e != nil {
			appErr(w, e)
			return
		}
		jsonOut(w, 201, x)
		return
	}
	if r.Method != http.MethodGet {
		jsonOut(w, 405, map[string]string{"error": "method_not_allowed"})
		return
	}
	n, e := queryLimit(r, 3, store.CheckpointKeep)
	if e != nil {
		appErr(w, e)
		return
	}
	xs, e := s.Service.Recent(r.Context(), r.URL.Query().Get("session"), r.URL.Query().Get("kind"), n)
	if e != nil {
		appErr(w, e)
		return
	}
	jsonOut(w, 200, map[string]any{"checkpoints": xs})
}
func (s Server) search(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	n, e := queryLimit(r, 20, 100)
	if e != nil {
		appErr(w, e)
		return
	}
	xs, e := s.Service.Search(r.Context(), r.URL.Query().Get("q"), defaultString(r.URL.Query().Get("scope"), "all"), r.URL.Query().Get("session"), n)
	if e != nil {
		appErr(w, e)
		return
	}
	jsonOut(w, 200, map[string]any{"results": xs})
}
func (s Server) documents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	n, e := queryLimit(r, 100, 1000)
	if e != nil {
		appErr(w, e)
		return
	}
	xs, e := s.Service.ListDocuments(r.Context(), r.URL.Query().Get("prefix"), r.URL.Query().Get("kind"), r.URL.Query().Get("session"), n)
	if e != nil {
		appErr(w, e)
		return
	}
	jsonOut(w, 200, map[string]any{"documents": xs})
}
func (s Server) document(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if r.Method == http.MethodGet {
		x, found, e := s.Service.GetDocument(r.Context(), key)
		if e != nil {
			appErr(w, e)
			return
		}
		if !found {
			jsonOut(w, 404, map[string]string{"error": "not_found"})
			return
		}
		jsonOut(w, 200, x)
		return
	}
	if r.Method == http.MethodPut {
		defer r.Body.Close()
		var x store.Document
		if e := decode(r, &x, store.DocumentMaxBytes); e != nil {
			appErr(w, e)
			return
		}
		x.Key = key
		x, changed, e := s.Service.PutDocument(r.Context(), client, x)
		if e != nil {
			appErr(w, e)
			return
		}
		jsonOut(w, 200, map[string]any{"document": x, "changed": changed})
		return
	}
	jsonOut(w, 405, map[string]string{"error": "method_not_allowed"})
}
func (s Server) memoryList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, e := s.Service.ListMemory(r.Context(), r.PathValue("agent"), false)
	if e != nil {
		appErr(w, e)
		return
	}
	jsonOut(w, 200, map[string]any{"memory": xs})
}
func (s Server) memory(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	agent, name := r.PathValue("agent"), r.PathValue("name")
	if r.Method == http.MethodGet {
		x, found, e := s.Service.GetMemory(r.Context(), agent, name)
		if e != nil {
			appErr(w, e)
			return
		}
		if !found {
			jsonOut(w, 404, map[string]string{"error": "not_found"})
			return
		}
		jsonOut(w, 200, x)
		return
	}
	if r.Method == http.MethodPut {
		defer r.Body.Close()
		var x store.Memory
		if e := decode(r, &x, store.MaxBytes); e != nil {
			appErr(w, e)
			return
		}
		x.Agent, x.Name = agent, name
		x, e := s.Service.PutMemory(r.Context(), client, x)
		if e != nil {
			appErr(w, e)
			return
		}
		jsonOut(w, 200, x)
		return
	}
	jsonOut(w, 405, map[string]string{"error": "method_not_allowed"})
}
func defaultString(x, d string) string {
	if x == "" {
		return d
	}
	return x
}
