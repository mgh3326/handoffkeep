// Package api is the one application core used by HTTP, CLI proxies, and MCP.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type Service struct {
	Store       *store.Store
	Attachments *attachments.Manager
}

var decisionResolverRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// DecisionResolveInput records an answer made outside the web console and
// closes the matching open decision. It is shared by the HTTP route and the
// authenticated remote client payload.
type DecisionResolveInput struct {
	Type     string `json:"type"`
	ID       int64  `json:"id"`
	By       string `json:"by"`
	Answer   string `json:"answer"`
	Note     string `json:"note,omitempty"`
	NoInject bool   `json:"no_inject,omitempty"`
}

func validDecisionResolveText(value string, required bool) bool {
	if required && strings.TrimSpace(value) == "" {
		return false
	}
	if value == "" {
		return true
	}
	if len(value) > store.RelayLaneEventMaxBytes {
		return false
	}
	for _, r := range value {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

func decisionEventID(prefix string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// ResolveDecision writes a lane event before transitioning a task, preserving
// the same durable ordering as the web decision-answer route.
func (s Service) ResolveDecision(ctx context.Context, input DecisionResolveInput) (store.RelayEvent, error) {
	if (input.Type != "task" && input.Type != "escalation" && input.Type != "lane") || input.ID < 1 || !decisionResolverRE.MatchString(input.By) || !validDecisionResolveText(input.Answer, true) || !validDecisionResolveText(input.Note, false) {
		return store.RelayEvent{}, errors.New("invalid decision resolve")
	}

	lane := ""
	if input.Type == "task" {
		task, found, err := s.Store.GetTask(ctx, input.ID)
		if err != nil {
			return store.RelayEvent{}, err
		}
		if !found {
			return store.RelayEvent{}, store.ErrTaskNotFound
		}
		// Disposition items are answered only by the operator's Access-authenticated
		// web route; a bearer token identifies a machine, not the operator, and
		// By is a free string. Refuse before any lane event is written.
		if task.Refs.Disposition != nil {
			return store.RelayEvent{}, store.ErrDispositionOperatorOnly
		}
		if task.State != "needs_decision" {
			return store.RelayEvent{}, store.ErrTaskConflict
		}
		// An open structured request is closed with tasks decision-resolve,
		// which records the answer on that request_id. Resolving it here
		// would unblock the task and leave the request open.
		if task.Refs.DecisionRequest != nil && task.Refs.DecisionRequest.Status == store.DecisionRequestOpen {
			return store.RelayEvent{}, store.ErrDecisionRequestOpen
		}
		lane = task.Lane
	} else {
		var events []store.RelayEvent
		var err error
		if input.Type == "escalation" {
			events, err = s.Store.ListOpenEscalations(ctx, 1000)
		} else {
			events, err = s.Store.ListOpenLaneDecisions(ctx, 1000)
		}
		if err != nil {
			return store.RelayEvent{}, err
		}
		for _, event := range events {
			if event.ID == input.ID {
				lane = event.OwnerLane
				break
			}
		}
		if lane == "" {
			return store.RelayEvent{}, store.ErrTaskConflict
		}
	}

	prefix := "[decision]"
	if input.Type == "lane" {
		prefix = "[decision-answered]"
	}
	text := fmt.Sprintf("%s #%d: %s (from %s)", prefix, input.ID, input.Answer, input.By)
	if input.Note != "" {
		text += " — " + input.Note
	}
	if len([]byte(text)) > store.RelayLaneEventMaxBytes || !validDecisionResolveText(text, true) {
		return store.RelayEvent{}, errors.New("decision response is too long")
	}
	eventID, err := decisionEventID(fmt.Sprintf("cli-decision-%s-%d-", input.Type, input.ID))
	if err != nil {
		return store.RelayEvent{}, err
	}
	event, _, err := s.Store.AppendRelayEvent(ctx, store.RelayEvent{Kind: "lane.event", OwnerLane: lane, EventID: eventID, Text: text})
	if err != nil {
		return store.RelayEvent{}, err
	}
	if input.NoInject {
		event, err = s.Store.MarkRelayEventDelivered(ctx, event.ID, "resolve", input.By)
		if err != nil {
			return store.RelayEvent{}, err
		}
	}
	if input.Type == "task" {
		note := input.Answer
		if input.Note != "" {
			note += " — " + input.Note
		}
		note += " — resolved by " + input.By
		if _, err := s.Store.TransitionTask(ctx, input.ID, "claimed", input.By, note, nil); err != nil {
			return store.RelayEvent{}, err
		}
	}
	return event, nil
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
func (s Service) GetDocumentByID(ctx context.Context, id int64) (store.Document, bool, error) {
	return s.Store.GetDocumentByID(ctx, id)
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
func (s Service) CreateDisposition(ctx context.Context, client string, x store.DispositionInput) (store.Task, bool, error) {
	x.CreatedBy = client
	return s.Store.CreateDisposition(ctx, x)
}
func (s Service) DispositionSummary(ctx context.Context, asOf time.Time) (store.DispositionSummary, error) {
	return s.Store.DispositionSummary(ctx, asOf)
}
func (s Service) ApplyDisposition(ctx context.Context, id int64, client, note string) (store.Task, error) {
	return s.Store.ApplyDisposition(ctx, id, client, note)
}
func (s Service) TransitionTask(ctx context.Context, id int64, to, client, note string, refs *store.TaskRefs) (store.Task, error) {
	return s.Store.TransitionTask(ctx, id, to, client, note, refs)
}
func (s Service) RelaneTask(ctx context.Context, id int64, to, client, note string, allowNewLane bool) (store.Task, bool, error) {
	return s.Store.RelaneTask(ctx, id, to, client, note, allowNewLane)
}
func (s Service) ListTasks(ctx context.Context, lane, state, parentLane string, limit int) ([]store.Task, error) {
	return s.Store.ListTasks(ctx, lane, state, parentLane, limit)
}
func (s Service) ListTasksPage(ctx context.Context, lane, state, parentLane string, afterID int64, limit int) ([]store.Task, error) {
	return s.Store.ListTasksPage(ctx, lane, state, parentLane, afterID, limit)
}

// ExportTasks returns a consistent task snapshot and stamps the serving
// build's VCS revision into the source object. The revision is the binary's
// embedded stamp; "unknown" is emitted rather than invented when it is absent.
func (s Service) ExportTasks(ctx context.Context, lane, state, parentLane string, limit int) (store.TaskExport, error) {
	out, err := s.Store.ExportTasks(ctx, lane, state, parentLane, limit)
	if err != nil {
		return out, err
	}
	out.Source = store.TaskExportSource{VCSRevision: "unknown"}
	if info, ok := readBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				out.Source.VCSRevision = setting.Value
			}
		}
	}
	return out, nil
}
func (s Service) GetTask(ctx context.Context, id int64) (store.Task, bool, error) {
	return s.Store.GetTask(ctx, id)
}
func (s Service) LinearOutboxStatus(ctx context.Context) (store.LinearOutboxStatus, error) {
	return s.Store.GetLinearOutboxStatus(ctx)
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
func (s Service) ListBenchScores(ctx context.Context, modelID, source string, limit int) ([]store.BenchScore, error) {
	return s.Store.ListBenchScores(ctx, modelID, source, limit)
}
func (s Service) UpsertBenchScores(ctx context.Context, client string, xs []store.BenchScore) (int, error) {
	for i := range xs {
		xs[i].UpdatedBy = client
	}
	return s.Store.UpsertBenchScores(ctx, xs)
}
func (s Service) ListBenchReps(ctx context.Context, profile, grade, effort string, limit int) ([]store.BenchRep, error) {
	return s.Store.ListBenchReps(ctx, profile, grade, effort, limit)
}
func (s Service) UpsertBenchReps(ctx context.Context, client string, xs []store.BenchRep) (int, error) {
	for i := range xs {
		xs[i].CreatedBy = client
	}
	return s.Store.UpsertBenchReps(ctx, xs)
}
func (s Service) ListBenchGrades(ctx context.Context) ([]store.BenchGrade, error) {
	return s.Store.ListBenchGrades(ctx)
}
func (s Service) UpsertBenchGrades(ctx context.Context, client string, xs []store.BenchGrade) (int, error) {
	for i := range xs {
		xs[i].DecidedBy = client
	}
	return s.Store.UpsertBenchGrades(ctx, xs)
}
func (s Service) ListBenchCatalog(ctx context.Context, pool string, includeRetired bool) ([]store.BenchCatalogEntry, error) {
	return s.Store.ListBenchCatalog(ctx, pool, includeRetired)
}

// UpsertBenchCatalog deliberately does not overwrite DecidedBy with the
// authenticated client: the field records which decision produced the row,
// and the caller is always the operator anyway.
func (s Service) UpsertBenchCatalog(ctx context.Context, xs []store.BenchCatalogEntry) (int, error) {
	return s.Store.UpsertBenchCatalog(ctx, xs)
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
	m.HandleFunc("GET /healthz", healthz)
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
	m.HandleFunc("GET /v1/tasks/export", s.tasksExport)
	m.HandleFunc("POST /v1/tasks/next", s.tasksNext)
	m.HandleFunc("GET /v1/tasks/{id}", s.task)
	m.HandleFunc("POST /v1/tasks/{id}/claim", s.taskClaim)
	m.HandleFunc("POST /v1/tasks/{id}/transition", s.taskTransition)
	m.HandleFunc("POST /v1/tasks/relane", s.tasksRelane)
	m.HandleFunc("POST /v1/tasks/{id}/decision-request", s.taskDecisionRequest)
	m.HandleFunc("POST /v1/tasks/{id}/decision-request/resolve", s.taskDecisionResolve)
	m.HandleFunc("POST /v1/tasks/dispositions", s.dispositionCreate)
	m.HandleFunc("GET /v1/tasks/dispositions/summary", s.dispositionSummary)
	m.HandleFunc("POST /v1/tasks/dispositions/{id}/apply", s.dispositionApply)
	m.HandleFunc("GET /v1/tasks/{id}/comments", s.taskCommentsList)
	m.HandleFunc("POST /v1/tasks/{id}/comments", s.taskCommentCreate)
	m.HandleFunc("GET /v1/linear/status", s.linearStatus)
	m.HandleFunc("POST /v1/decisions/resolve", s.decisionResolve)
	m.HandleFunc("POST /v1/relay/events", s.relayEventsCreate)
	m.HandleFunc("POST /v1/relay/events/{id}/delivered", s.relayEventDelivered)
	m.HandleFunc("GET /v1/relay/events", s.relayEventsList)
	m.HandleFunc("GET /v1/bench/scores", s.benchScoresList)
	m.HandleFunc("PUT /v1/bench/scores", s.benchScoresPut)
	m.HandleFunc("GET /v1/bench/reps", s.benchRepsList)
	m.HandleFunc("PUT /v1/bench/reps", s.benchRepsPut)
	m.HandleFunc("GET /v1/bench/grades", s.benchGradesList)
	m.HandleFunc("PUT /v1/bench/grades", s.benchGradesPut)
	m.HandleFunc("GET /v1/bench/catalog", s.benchCatalogList)
	m.HandleFunc("PUT /v1/bench/catalog", s.benchCatalogPut)
	m.HandleFunc("PUT /v1/chat/questions/{id}", s.chatQuestionPut)
	m.HandleFunc("POST /v1/chat/questions", s.chatQuestionPost)
	m.HandleFunc("POST /v1/chat/questions/{id}/transition", s.chatQuestionTransition)
	m.HandleFunc("GET /v1/chat/questions", s.chatQuestionsList)
	m.HandleFunc("POST /v1/chat/messages", s.chatMessageCreate)
	m.HandleFunc("GET /v1/chat/messages", s.chatMessagesList)
	m.HandleFunc("POST /v1/chat/messages/{id}/delivered", s.chatMessageDelivered)
	m.HandleFunc("POST /v1/chat/messages/{id}/failed", s.chatMessageFailed)
	if s.UI != nil {
		m.Handle("/ui", s.UI)
		m.Handle("/ui/", s.UI)
	}
	return m
}

// readBuildInfo is a variable so tests can stamp a VCS-tagged build info.
var readBuildInfo = debug.ReadBuildInfo

// healthz is the unauthenticated liveness probe. Beyond "status":"ok" it
// reports the binary's embedded VCS stamp so a deploy can verify which commit
// is serving without shelling out to `strings` on the binary. The vcs_* keys
// are absent when the binary was built without VCS stamping (e.g. from a
// source export or with -buildvcs=false).
func healthz(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{"status": "ok"}
	if info, ok := readBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				out["vcs_revision"] = setting.Value
			case "vcs.time":
				out["vcs_time"] = setting.Value
			case "vcs.modified":
				out["vcs_modified"] = setting.Value
			}
		}
	}
	jsonOut(w, 200, out)
}

func (s Server) linearStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	status, err := s.Service.LinearOutboxStatus(r.Context())
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, status)
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
	case errors.Is(e, store.ErrDispositionOperatorOnly):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "disposition_operator_only"})
		return
	case errors.Is(e, store.ErrDispositionStale):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "disposition_stale"})
		return
	case errors.Is(e, store.ErrTaskNotFound):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	case errors.Is(e, store.ErrTaskTerminal):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "task_terminal"})
		return
	case errors.Is(e, store.ErrDecisionRequestOpen):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "decision_request_open"})
		return
	case errors.Is(e, store.ErrDecisionRequestStale):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "decision_request_stale"})
		return
	case errors.Is(e, store.ErrDecisionRequestResolved):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "decision_request_resolved"})
		return
	case errors.Is(e, store.ErrInvalidDecisionRequest):
		// The reason is produced by the store's own validators (limits and
		// field names), never echoed input, so it is safe to return.
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": e.Error()})
		return
	case errors.Is(e, store.ErrRelayEventNotFound):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	case errors.Is(e, store.ErrDeviationRefRequired):
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "deviation_ref_required"})
		return
	case errors.Is(e, store.ErrDecidedByRequired):
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "decided_by_required"})
		return
	case errors.Is(e, store.ErrBenchCatalogMonotonicity):
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "bench_catalog_not_monotonic"})
		return
	case errors.Is(e, store.ErrBenchCatalogSolGrade):
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "bench_catalog_sol_grade"})
		return
	case errors.Is(e, store.ErrQueueEmpty):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "queue_empty"})
		return
	case errors.Is(e, store.ErrChatQuestionNotFound), errors.Is(e, store.ErrChatMessageNotFound):
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	case errors.Is(e, store.ErrChatQuestionConflict):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "chat_question_conflict"})
		return
	case errors.Is(e, store.ErrChatMessageConflict):
		jsonOut(w, http.StatusConflict, map[string]string{"error": "chat_message_conflict"})
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
	lane, state, parentLane := r.URL.Query().Get("lane"), r.URL.Query().Get("state"), r.URL.Query().Get("parent_lane")
	var xs []store.Task
	if r.URL.Query().Has("after_id") {
		afterID, parseErr := queryAfterID(r)
		if parseErr != nil {
			appErr(w, parseErr)
			return
		}
		xs, err = s.Service.ListTasksPage(r.Context(), lane, state, parentLane, afterID, limit)
	} else {
		xs, err = s.Service.ListTasks(r.Context(), lane, state, parentLane, limit)
	}
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"tasks": xs})
}

// tasksExport serves one consistent task snapshot. It reuses the existing
// bearer authentication and filter validation; it adds no credential class.
// Error bodies carry only a stable code — never task content or scope data.
func (s Server) tasksExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 1000, store.ExportLimitMax)
	if err != nil {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_export_query"})
		return
	}
	lane, state, parentLane := r.URL.Query().Get("lane"), r.URL.Query().Get("state"), r.URL.Query().Get("parent_lane")
	out, err := s.Service.ExportTasks(r.Context(), lane, state, parentLane, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidExportQuery) {
			jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_export_query"})
			return
		}
		jsonOut(w, http.StatusInternalServerError, map[string]string{"error": "export_unavailable"})
		return
	}
	jsonOut(w, http.StatusOK, out)
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

// taskDecisionRequest records a structured decision request (#618). The
// requester is the authenticated client. 201 is a new record, 200 a
// duplicate re-send returning the already recorded request_id.
func (s Server) taskDecisionRequest(w http.ResponseWriter, r *http.Request) {
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
	var input store.DecisionRequestInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.Store.RecordDecisionRequest(r.Context(), id, client, input)
	if err != nil {
		appErr(w, err)
		return
	}
	status := http.StatusCreated
	if x.Duplicate {
		status = http.StatusOK
	}
	jsonOut(w, status, x)
}

// taskDecisionResolve closes the current request (answered, default_applied
// with a receipt, or withdrawn). It never changes the task state.
func (s Server) taskDecisionResolve(w http.ResponseWriter, r *http.Request) {
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
	var input store.DecisionResolveInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.Store.ResolveDecisionRequest(r.Context(), id, client, input)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

// TaskRelaneBatchMax bounds one relane request. It comfortably covers the
// triage migrations this route exists for while keeping a mistake bounded.
// Exported so the CLI can refuse an oversized batch before sending.
const TaskRelaneBatchMax = 500

type taskRelaneInput struct {
	IDs          []int64 `json:"ids"`
	To           string  `json:"to"`
	Note         string  `json:"note"`
	AllowNewLane bool    `json:"allow_new_lane,omitempty"`
}

// taskRelaneResult is one item's outcome. A failed item carries a stable
// error code; the request keeps processing the rest of the batch.
type taskRelaneResult struct {
	ID      int64       `json:"id"`
	OK      bool        `json:"ok"`
	Changed bool        `json:"changed"`
	Task    *store.Task `json:"task,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// relaneErrorCode maps a store failure to the closed error vocabulary items
// report. Validation errors collapse to invalid_task_relane; unrecognized
// errors (driver faults, cancelled contexts) report internal_error rather
// than leaking internals or posing as a retry-safe validation failure.
func relaneErrorCode(err error) string {
	switch {
	case errors.Is(err, store.ErrTaskNotFound):
		return "not_found"
	case errors.Is(err, store.ErrTaskTerminal):
		return "task_terminal"
	case errors.Is(err, store.ErrTaskLaneUnknown):
		return "unknown_lane"
	case errors.Is(err, store.ErrTaskConflict):
		return "task_conflict"
	}
	if strings.HasPrefix(err.Error(), "secret_like_content") {
		return "secret_like_content"
	}
	if strings.HasPrefix(err.Error(), "invalid task relane") {
		return "invalid_task_relane"
	}
	return "internal_error"
}

// tasksRelane moves one or more tasks between lanes. It reuses the existing
// bearer write authentication — no new credential class — and each id runs in
// its own store transaction so one failure never rolls back the batch's
// other items.
func (s Server) tasksRelane(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input taskRelaneInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	if len(input.IDs) < 1 || len(input.IDs) > TaskRelaneBatchMax || strings.TrimSpace(input.To) == "" || strings.TrimSpace(input.Note) == "" {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_task_relane"})
		return
	}
	for _, id := range input.IDs {
		if id < 1 {
			jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid_task_relane"})
			return
		}
	}
	results := make([]taskRelaneResult, 0, len(input.IDs))
	moved, unchanged, failed := 0, 0, 0
	for _, id := range input.IDs {
		task, changed, err := s.Service.RelaneTask(r.Context(), id, input.To, client, input.Note, input.AllowNewLane)
		item := taskRelaneResult{ID: id}
		if err != nil {
			item.Error = relaneErrorCode(err)
			failed++
		} else {
			item.OK = true
			item.Changed = changed
			item.Task = &task
			if changed {
				moved++
			} else {
				unchanged++
			}
		}
		results = append(results, item)
	}
	jsonOut(w, http.StatusOK, map[string]any{"results": results, "moved": moved, "unchanged": unchanged, "failed": failed})
}

func (s Server) dispositionCreate(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input store.DispositionInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, created, err := s.Service.CreateDisposition(r.Context(), client, input)
	if err != nil {
		appErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	jsonOut(w, status, map[string]any{"task": x, "created": created})
}

func (s Server) dispositionSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	asOf := time.Now().UTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("as_of")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid as_of"})
			return
		}
		asOf = parsed
	}
	summary, err := s.Service.DispositionSummary(r.Context(), asOf)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, summary)
}

func (s Server) dispositionApply(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	defer r.Body.Close()
	var input struct {
		Note string `json:"note"`
	}
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.ApplyDisposition(r.Context(), id, client, input.Note)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

func (s Server) decisionResolve(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input DecisionResolveInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	event, err := s.Service.ResolveDecision(r.Context(), input)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"event": event})
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

func queryAfterID(r *http.Request) (int64, error) {
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
	afterID, err := queryAfterID(r)
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

const benchRequestMaxBytes = 8 << 20

type benchScoresInput struct {
	Scores []store.BenchScore `json:"scores"`
}

type benchRepsInput struct {
	Reps []store.BenchRep `json:"reps"`
}

type benchGradesInput struct {
	Grades []store.BenchGrade `json:"grades"`
}

func benchBatchValid(n int) bool {
	return n >= 1 && n <= 1000
}

func (s Server) benchScoresList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 1000, 5000)
	if err != nil {
		appErr(w, err)
		return
	}
	xs, err := s.Service.ListBenchScores(r.Context(), r.URL.Query().Get("model_id"), r.URL.Query().Get("source"), limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"scores": xs})
}

func (s Server) benchScoresPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input benchScoresInput
	if err := decode(r, &input, benchRequestMaxBytes); err != nil || !benchBatchValid(len(input.Scores)) {
		if err == nil {
			err = errors.New("invalid bench scores")
		}
		appErr(w, err)
		return
	}
	n, err := s.Service.UpsertBenchScores(r.Context(), client, input.Scores)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
}

func (s Server) benchRepsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 1000, 5000)
	if err != nil {
		appErr(w, err)
		return
	}
	xs, err := s.Service.ListBenchReps(r.Context(), r.URL.Query().Get("profile"), r.URL.Query().Get("grade"), r.URL.Query().Get("effort"), limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"reps": xs})
}

func (s Server) benchRepsPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input benchRepsInput
	if err := decode(r, &input, benchRequestMaxBytes); err != nil || !benchBatchValid(len(input.Reps)) {
		if err == nil {
			err = errors.New("invalid bench reps")
		}
		appErr(w, err)
		return
	}
	n, err := s.Service.UpsertBenchReps(r.Context(), client, input.Reps)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
}

func (s Server) benchGradesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	xs, err := s.Service.ListBenchGrades(r.Context())
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"grades": xs})
}

func (s Server) benchGradesPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input benchGradesInput
	if err := decode(r, &input, benchRequestMaxBytes); err != nil || !benchBatchValid(len(input.Grades)) {
		if err == nil {
			err = errors.New("invalid bench grades")
		}
		appErr(w, err)
		return
	}
	n, err := s.Service.UpsertBenchGrades(r.Context(), client, input.Grades)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
}

// operatorClientID is the reserved auth-file client id carrying the operator
// credential (HANDOFFKEEP_TOKEN_operator), the same convention panewire #68
// established with HUB_TOKEN_operator. Deployments without the entry fail
// closed: every PUT /v1/bench/catalog gets 403.
const operatorClientID = "operator"

type benchCatalogInput struct {
	Catalog []store.BenchCatalogEntry `json:"catalog"`
}

func (s Server) benchCatalogList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	q := r.URL.Query()
	includeRetired := q.Get("include_retired") == "1" || q.Get("include_retired") == "true"
	xs, err := s.Service.ListBenchCatalog(r.Context(), q.Get("pool"), includeRetired)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"catalog": xs})
}

func (s Server) benchCatalogPut(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	if client != operatorClientID {
		jsonOut(w, http.StatusForbidden, map[string]string{"error": "operator_required"})
		return
	}
	defer r.Body.Close()
	var input benchCatalogInput
	if err := decode(r, &input, benchRequestMaxBytes); err != nil || !benchBatchValid(len(input.Catalog)) {
		if err == nil {
			err = errors.New("invalid bench catalog")
		}
		appErr(w, err)
		return
	}
	n, err := s.Service.UpsertBenchCatalog(r.Context(), input.Catalog)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"upserted": n})
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
	// ?id=<n> fetches one document by numeric id (#551): key is the public
	// handle but callers often only have the id. A document id is never a
	// valid key, so the {key...} wildcard route cannot express this lookup.
	if r.URL.Query().Has("id") {
		raw := r.URL.Query().Get("id")
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id < 1 {
			jsonOut(w, 400, map[string]string{"error": "invalid_document_id"})
			return
		}
		x, found, e := s.Service.GetDocumentByID(r.Context(), id)
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
