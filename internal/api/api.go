// Package api is the one application core used by HTTP, CLI proxies, and MCP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type Service struct {
	Store       *store.Store
	Attachments *attachments.Manager
}

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
func (s Service) PutAttachment(ctx context.Context, client, name, mime, ref string, body []byte) (store.Attachment, bool, error) {
	if s.Attachments == nil {
		return store.Attachment{}, false, attachments.ErrDisabled
	}
	return s.Attachments.Put(ctx, client, name, mime, ref, body)
}
func (s Service) GetAttachment(ctx context.Context, sha string) (store.Attachment, io.ReadCloser, error) {
	if s.Attachments == nil {
		return store.Attachment{}, nil, attachments.ErrDisabled
	}
	return s.Attachments.Get(ctx, sha)
}
func (s Service) AttachmentURL(ctx context.Context, sha string) (string, error) {
	if s.Attachments == nil {
		return "", attachments.ErrDisabled
	}
	return s.Attachments.Presign(ctx, sha)
}
func (s Service) ListAttachments(ctx context.Context, ref string, limit int) ([]store.Attachment, error) {
	if s.Attachments == nil {
		return nil, attachments.ErrDisabled
	}
	return s.Attachments.List(ctx, ref, limit)
}
func (s Service) AttachmentUsage(ctx context.Context) (store.AttachmentUsage, error) {
	if s.Attachments == nil {
		return store.AttachmentUsage{}, attachments.ErrDisabled
	}
	return s.Attachments.Usage(ctx)
}
func (s Service) CreateTask(ctx context.Context, client string, x store.Task) (store.Task, error) {
	x.CreatedBy = client
	return s.Store.CreateTask(ctx, x)
}
func (s Service) ClaimTask(ctx context.Context, id int64, by string) (store.Task, error) {
	return s.Store.ClaimTask(ctx, id, by)
}
func (s Service) NextTask(ctx context.Context, lane, by string) (store.Task, error) {
	return s.Store.NextTask(ctx, lane, by)
}
func (s Service) TransitionTask(ctx context.Context, id int64, to, client, note string, refs *store.TaskRefs) (store.Task, error) {
	return s.Store.TransitionTask(ctx, id, to, client, note, refs)
}
func (s Service) ListTasks(ctx context.Context, lane, state, parentLane string, limit int) ([]store.Task, error) {
	return s.Store.ListTasks(ctx, lane, state, parentLane, limit)
}
func (s Service) GetTask(ctx context.Context, id int64) (store.Task, bool, error) {
	return s.Store.GetTask(ctx, id)
}
func (s Service) AppendRelayEvent(ctx context.Context, x store.RelayEvent) (store.RelayEvent, bool, error) {
	return s.Store.AppendRelayEvent(ctx, x)
}
func (s Service) MarkRelayEventDelivered(ctx context.Context, id int64, machine, pane string) (store.RelayEvent, error) {
	return s.Store.MarkRelayEventDelivered(ctx, id, machine, pane)
}
func (s Service) ListRelayEvents(ctx context.Context, lane, kind string, undelivered bool, afterID int64, limit int) ([]store.RelayEvent, error) {
	return s.Store.ListRelayEventsPage(ctx, lane, kind, undelivered, afterID, limit)
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
	// UI is independently authenticated by Cloudflare Access. A nil UI leaves
	// the route unregistered so deployments without complete Access settings
	// fail closed with the standard ServeMux 404.
	UI http.Handler
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
	m.HandleFunc("PUT /v1/attachments", s.attachmentPut)
	m.HandleFunc("GET /v1/attachments", s.attachments)
	m.HandleFunc("GET /v1/attachments/{sha}", s.attachment)
	m.HandleFunc("GET /v1/usage", s.usage)
	m.HandleFunc("GET /metrics", s.metrics)
	m.HandleFunc("POST /v1/tasks", s.tasksCreate)
	m.HandleFunc("GET /v1/tasks", s.tasksList)
	m.HandleFunc("POST /v1/tasks/next", s.tasksNext)
	m.HandleFunc("GET /v1/tasks/{id}", s.task)
	m.HandleFunc("POST /v1/tasks/{id}/claim", s.taskClaim)
	m.HandleFunc("POST /v1/tasks/{id}/transition", s.taskTransition)
	m.HandleFunc("POST /v1/relay/events", s.relayEventsCreate)
	m.HandleFunc("POST /v1/relay/events/{id}/delivered", s.relayEventDelivered)
	m.HandleFunc("GET /v1/relay/events", s.relayEventsList)
	if s.UI != nil {
		m.Handle("/ui", s.UI)
		m.Handle("/ui/", s.UI)
	}
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
	switch {
	case errors.Is(e, attachments.ErrDisabled):
		jsonOut(w, 503, map[string]string{"error": "attachments_disabled"})
		return
	case errors.Is(e, attachments.ErrTooLarge):
		jsonOut(w, 413, map[string]string{"error": "attachment_too_large"})
		return
	case errors.Is(e, attachments.ErrMIME), errors.Is(e, attachments.ErrExtension):
		jsonOut(w, 400, map[string]string{"error": e.Error()})
		return
	case errors.Is(e, store.ErrAttachmentStorageCap):
		jsonOut(w, 507, map[string]string{"error": "attachment_storage_cap"})
		return
	case errors.Is(e, store.ErrAttachmentPutCap):
		jsonOut(w, 429, map[string]string{"error": "attachment_put_cap"})
		return
	case errors.Is(e, attachments.ErrR2):
		jsonOut(w, 502, map[string]string{"error": "attachment_r2_unavailable"})
		return
	case errors.Is(e, store.ErrTaskConflict):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "task_conflict"})
		return
	case errors.Is(e, store.ErrTaskNotFound):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	case errors.Is(e, store.ErrRelayEventNotFound):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	case errors.Is(e, store.ErrQueueEmpty):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "queue_empty"})
		return
	}
	if p, ok := strings.CutPrefix(e.Error(), "secret_like_content:"); ok {
		jsonOut(w, 400, map[string]string{"error": "secret_like_content", "pattern": p})
		return
	}
	jsonOut(w, 400, map[string]string{"error": "invalid_context"})
}

func taskID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s Server) tasksCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var x store.Task
	if err := decode(r, &x, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.CreateTask(r.Context(), client, x)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusCreated, x)
}

func (s Server) tasksList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 20, 1000)
	if err != nil {
		appErr(w, err)
		return
	}
	xs, err := s.Service.ListTasks(r.Context(), r.URL.Query().Get("lane"), r.URL.Query().Get("state"), r.URL.Query().Get("parent_lane"), limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"tasks": xs})
}

func (s Server) task(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("task id"))
		return
	}
	x, found, err := s.Service.GetTask(r.Context(), id)
	if err != nil {
		appErr(w, err)
		return
	}
	if !found {
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	jsonOut(w, http.StatusOK, x)
}

type taskClaimInput struct {
	ClaimedBy string `json:"claimed_by"`
}

func (s Server) taskClaim(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("task id"))
		return
	}
	defer r.Body.Close()
	var input taskClaimInput
	if err := decode(r, &input, 4096); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.ClaimTask(r.Context(), id, input.ClaimedBy)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) tasksNext(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input struct {
		Lane      string `json:"lane"`
		ClaimedBy string `json:"claimed_by"`
	}
	if err := decode(r, &input, 4096); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.NextTask(r.Context(), input.Lane, input.ClaimedBy)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

type taskTransitionInput struct {
	To   string          `json:"to"`
	Note string          `json:"note"`
	Refs *store.TaskRefs `json:"refs,omitempty"`
}

func (s Server) taskTransition(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := taskID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("task id"))
		return
	}
	defer r.Body.Close()
	var input taskTransitionInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.TransitionTask(r.Context(), id, input.To, client, input.Note, input.Refs)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func relayEventID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s Server) relayEventsCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var x store.RelayEvent
	if err := decode(r, &x, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, created, err := s.Service.AppendRelayEvent(r.Context(), x)
	if err != nil {
		appErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, x)
}

type relayEventDeliveryInput struct {
	Machine string `json:"machine"`
	Pane    string `json:"pane"`
}

func (s Server) relayEventDelivered(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	id, err := relayEventID(r)
	if err != nil || id < 1 {
		appErr(w, errors.New("relay event id"))
		return
	}
	defer r.Body.Close()
	var input relayEventDeliveryInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.MarkRelayEventDelivered(r.Context(), id, input.Machine, input.Pane)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func relayEventsLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("limit")
	}
	return limit, nil
}

func relayEventsAfterID(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("after_id")
	if v == "" {
		return 0, nil
	}
	afterID, err := strconv.ParseInt(v, 10, 64)
	if err != nil || afterID < 0 {
		return 0, errors.New("after_id")
	}
	return afterID, nil
}

func (s Server) relayEventsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := relayEventsLimit(r)
	if err != nil {
		appErr(w, err)
		return
	}
	afterID, err := relayEventsAfterID(r)
	if err != nil {
		appErr(w, err)
		return
	}
	undeliveredValue := r.URL.Query().Get("undelivered")
	undelivered := undeliveredValue == "1" || undeliveredValue == "true"
	xs, err := s.Service.ListRelayEvents(r.Context(), r.URL.Query().Get("lane"), r.URL.Query().Get("kind"), undelivered, afterID, limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"events": xs})
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
func (s Server) attachmentPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	if s.Service.Attachments == nil || !s.Service.Attachments.Enabled {
		appErr(w, attachments.ErrDisabled)
		return
	}
	max := s.Service.Attachments.Config.MaxBytes
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if e != nil {
		appErr(w, attachments.ErrTooLarge)
		return
	}
	x, new, e := s.Service.PutAttachment(r.Context(), client, r.Header.Get("X-HK-Name"), r.Header.Get("Content-Type"), r.Header.Get("X-HK-Ref"), b)
	if e != nil {
		appErr(w, e)
		return
	}
	jsonOut(w, 201, map[string]any{"attachment": x, "created": new, "url": "/v1/attachments/" + x.SHA256})
}
func (s Server) attachments(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, e := s.Service.ListAttachments(r.Context(), r.URL.Query().Get("ref"), 100)
	if e != nil {
		appErr(w, e)
		return
	}
	jsonOut(w, 200, map[string]any{"attachments": xs})
}
func (s Server) attachment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	sha := r.PathValue("sha")
	if r.URL.Query().Get("presign") == "1" {
		u, e := s.Service.AttachmentURL(r.Context(), sha)
		if e != nil {
			appErr(w, e)
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	x, body, e := s.Service.GetAttachment(r.Context(), sha)
	if e != nil {
		if e.Error() == "attachment_not_found" {
			jsonOut(w, 404, map[string]string{"error": "not_found"})
		} else {
			appErr(w, e)
		}
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", x.MIME)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(x.OriginalName, "\"", "")+`"`)
	_, _ = io.Copy(w, body)
}
func (s Server) usage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	if s.Service.Attachments == nil || !s.Service.Attachments.Enabled {
		jsonOut(w, 200, map[string]any{"attachments": "disabled"})
		return
	}
	u, e := s.Service.AttachmentUsage(r.Context())
	if e != nil {
		appErr(w, e)
		return
	}
	jsonOut(w, 200, map[string]any{"attachments": "enabled", "usage": u, "cap_bytes": s.Service.Attachments.Config.StorageCap, "cap_ratio": float64(u.TotalBytes) / float64(s.Service.Attachments.Config.StorageCap)})
}
func (s Server) metrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	if s.Service.Attachments == nil || !s.Service.Attachments.Enabled {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("handoffkeep_attach_enabled 0\n"))
		return
	}
	u, e := s.Service.AttachmentUsage(r.Context())
	if e != nil {
		appErr(w, e)
		return
	}
	c := s.Service.Attachments.Config
	objects, e := s.Service.Store.AttachmentObjectCount(r.Context())
	if e != nil {
		appErr(w, e)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "handoffkeep_attach_bytes_total %d\nhandoffkeep_attach_objects %d\nhandoffkeep_attach_puts_month %d\nhandoffkeep_attach_gets_month %d\nhandoffkeep_attach_cap_bytes %d\nhandoffkeep_attach_cap_ratio %.8f\n", u.TotalBytes, objects, u.Puts, u.Gets, c.StorageCap, float64(u.TotalBytes)/float64(c.StorageCap))
}
