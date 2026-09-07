// Package ui serves the Cloudflare Access-authenticated fleet console.
package ui

import (
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

var taskStates = []string{"backlog", "claimed", "in_progress", "verifying", "join", "hold", "needs_decision", "merged", "dropped"}

// Config supplies only server-side dependencies. HubToken is intentionally not
// represented in any template model.
type Config struct {
	Store         *store.Store
	Access        *cfaccess.Verifier
	HubURL        string
	HubToken      string
	HubHTTPClient *http.Client
	PollInterval  time.Duration
}

// Handler is a Cloudflare Access-authenticated UI handler.
type Handler struct {
	store        *store.Store
	access       *cfaccess.Verifier
	templates    *template.Template
	hub          *hubProxy
	pollInterval time.Duration
	static       fs.FS
	csrfKey      []byte
	lanes        []string
	laneSet      map[string]bool
	admiralLanes map[string]bool
}

// New builds the optional fleet console.
func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Access == nil {
		return nil, errors.New("UI requires store and Cloudflare Access verifier")
	}
	tmpl, err := template.New("ui").Funcs(template.FuncMap{
		"formatTime":       formatTime,
		"shortHead":        shortHead,
		"githubLink":       githubLink,
		"message":          messageParts,
		"ingressLabel":     ingressLabel,
		"decisionFormData": decisionFormData,
		"eventFormData":    eventFormData,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	poll := config.PollInterval
	if poll <= 0 {
		poll = 10 * time.Second
	}
	csrfKey := make([]byte, 32)
	if _, err := rand.Read(csrfKey); err != nil {
		return nil, fmt.Errorf("generate UI CSRF key: %w", err)
	}
	lanes := splitList(os.Getenv("HANDOFFKEEP_UI_LANES"))
	laneSet := make(map[string]bool, len(lanes))
	for _, lane := range lanes {
		laneSet[lane] = true
	}
	admiralLanes := make(map[string]bool)
	for _, lane := range splitList(os.Getenv("HANDOFFKEEP_UI_ADMIRAL_LANES")) {
		admiralLanes[lane] = true
	}
	return &Handler{
		store:        config.Store,
		access:       config.Access,
		templates:    tmpl,
		hub:          newHubProxy(config.HubURL, config.HubToken, config.HubHTTPClient),
		pollInterval: poll,
		static:       static,
		csrfKey:      csrfKey,
		lanes:        lanes,
		laneSet:      laneSet,
		admiralLanes: admiralLanes,
	}, nil
}

// ServeHTTP keeps the UI authentication and method boundary separate from the
// existing bearer-token API. Only the explicit UI write routes can reach a
// mutating operation.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, authenticated := h.access.AuthenticatedIdentity(r)
	if !authenticated {
		if r.Method == http.MethodPost {
			h.audit("-", writeAction(r.URL.Path), "-", "-", "unauthorized")
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if identity.ServiceName != "" && !strings.HasPrefix(r.URL.Path, "/ui/api/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/ui/api/") {
		h.serveAPI(w, r, identity)
		return
	}
	email := identity.Email
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/ui/decisions/answer":
			h.answerDecision(w, r, email)
		case "/ui/decisions/answer-batch":
			h.answerDecisionBatch(w, r, email)
		case "/ui/compose":
			h.composePost(w, r, email)
		default:
			h.audit(email, writeAction(r.URL.Path), "-", "-", "invalid")
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/ui":
		http.Redirect(w, r, "/ui/timeline", http.StatusFound)
	case "/ui/timeline":
		h.timeline(w, r, false)
	case "/ui/queue":
		h.queue(w, r, false)
	case "/ui/decisions":
		h.decisions(w, r, false, email, r.URL.Query().Get("result"))
	case "/ui/compose":
		h.compose(w, r, false, email, r.URL.Query().Get("result"))
	case "/ui/fleet":
		h.fleet(w, r, false)
	case "/ui/events":
		h.events(w, r)
	default:
		h.serveSubroute(w, r, email)
	}
}

func (h *Handler) serveSubroute(w http.ResponseWriter, r *http.Request, email string) {
	if name, ok := strings.CutPrefix(r.URL.Path, "/ui/fragments/task/"); ok {
		h.taskEvents(w, r, name)
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, "/ui/fragments/"); ok {
		switch name {
		case "timeline":
			h.timeline(w, r, true)
		case "queue":
			h.queue(w, r, true)
		case "queue-backlog":
			h.queueBacklog(w, r)
		case "decisions":
			h.decisions(w, r, true, email, r.URL.Query().Get("result"))
		case "fleet":
			h.fleet(w, r, true)
		case "compose":
			h.compose(w, r, true, email, r.URL.Query().Get("result"))
		default:
			http.NotFound(w, r)
		}
		return
	}
	if key, ok := strings.CutPrefix(r.URL.Path, "/ui/doc/"); ok {
		h.document(w, r, key)
		return
	}
	if file, ok := strings.CutPrefix(r.URL.Path, "/ui/static/"); ok {
		h.staticFile(w, r, file)
		return
	}
	http.NotFound(w, r)
}

func (h *Handler) staticFile(w http.ResponseWriter, r *http.Request, file string) {
	if file != "htmx.min.js" && file != "htmx.LICENSE" {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(h.static, path.Clean(file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if file == "htmx.min.js" {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

func (h *Handler) timeline(w http.ResponseWriter, r *http.Request, fragment bool) {
	data, err := h.timelineData(r)
	if err != nil {
		http.Error(w, "invalid timeline query", http.StatusBadRequest)
		return
	}
	if fragment {
		h.render(w, "timeline_content", data)
		return
	}
	h.render(w, "page", pageData{Title: "Timeline", Page: "timeline", Body: data})
}

type timelineData struct {
	Events      []store.RelayEvent
	Checkpoints []store.Checkpoint
	Lane        string
	Kind        string
	Since       string
	Until       string
	MoreURL     string
}

func (h *Handler) timelineData(r *http.Request) (timelineData, error) {
	q := r.URL.Query()
	data := timelineData{Lane: strings.TrimSpace(q.Get("lane")), Kind: strings.TrimSpace(q.Get("kind")), Since: strings.TrimSpace(q.Get("since")), Until: strings.TrimSpace(q.Get("until"))}
	var since, until *time.Time
	if data.Since != "" {
		parsed, err := time.Parse("2006-01-02", data.Since)
		if err != nil {
			return data, err
		}
		since = &parsed
	}
	if data.Until != "" {
		parsed, err := time.Parse("2006-01-02", data.Until)
		if err != nil {
			return data, err
		}
		parsed = parsed.AddDate(0, 0, 1)
		until = &parsed
	}
	beforeID := int64(0)
	if raw := q.Get("before_id"); raw != "" {
		var err error
		beforeID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || beforeID < 1 {
			return data, errors.New("invalid cursor")
		}
	}
	events, err := h.store.ListRelayEventsTimeline(r.Context(), data.Lane, data.Kind, since, until, beforeID, 200)
	if err != nil {
		return data, err
	}
	data.Events = events
	if len(events) == 200 {
		next := url.Values{}
		if data.Lane != "" {
			next.Set("lane", data.Lane)
		}
		if data.Kind != "" {
			next.Set("kind", data.Kind)
		}
		if data.Since != "" {
			next.Set("since", data.Since)
		}
		if data.Until != "" {
			next.Set("until", data.Until)
		}
		next.Set("before_id", strconv.FormatInt(events[len(events)-1].ID, 10))
		data.MoreURL = "/ui/timeline?" + next.Encode()
	}
	data.Checkpoints, err = h.store.LatestCheckpointsBySession(r.Context(), 1000)
	return data, err
}

func (h *Handler) queue(w http.ResponseWriter, r *http.Request, fragment bool) {
	data, err := h.queueData(r)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if fragment {
		h.render(w, "queue_content", data)
		return
	}
	h.render(w, "page", pageData{Title: "Queue", Page: "queue", Body: data})
}

type queueCell struct {
	Lane         string
	State        string
	Tasks        []store.Task
	BacklogCount int
}

type queueLane struct {
	Name  string
	Cells []queueCell
}

type queueData struct {
	States        []string
	Lanes         []queueLane
	OperatorOnly  bool
	OpenDecisions []queueOpenDecision
}

type queueOpenDecision struct {
	Lane string
	Text string
}

func (h *Handler) queueData(r *http.Request) (queueData, error) {
	tasks, err := h.store.ListTasks(r.Context(), "", "", "", 1000)
	if err != nil {
		return queueData{}, err
	}
	operatorOnly := r.URL.Query().Get("view") != "all"
	byLane := map[string]map[string][]store.Task{}
	for _, task := range tasks {
		if byLane[task.Lane] == nil {
			byLane[task.Lane] = map[string][]store.Task{}
		}
		byLane[task.Lane][task.State] = append(byLane[task.Lane][task.State], task)
	}
	lanes := make([]string, 0, len(byLane))
	for lane := range byLane {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	data := queueData{States: taskStates, OperatorOnly: operatorOnly}
	for _, lane := range lanes {
		row := queueLane{Name: lane}
		for _, state := range taskStates {
			cell := queueCell{Lane: lane, State: state, Tasks: byLane[lane][state], BacklogCount: len(byLane[lane]["backlog"])}
			if operatorOnly {
				cell.Tasks = nil
				if state != "backlog" {
					for _, task := range byLane[lane][state] {
						if task.State == "needs_decision" || (task.Kind == "decide" && task.State != "merged" && task.State != "dropped") {
							cell.Tasks = append(cell.Tasks, task)
						}
					}
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		data.Lanes = append(data.Lanes, row)
	}
	if operatorOnly {
		escalations, err := h.store.ListOpenEscalations(r.Context(), 1000)
		if err != nil {
			return queueData{}, err
		}
		for _, event := range escalations {
			if !isSignalEscalation(event) {
				data.OpenDecisions = append(data.OpenDecisions, queueOpenDecision{Lane: event.OwnerLane, Text: escalationText(event)})
			}
		}
		laneDecisions, err := h.store.ListOpenLaneDecisions(r.Context(), 1000)
		if err != nil {
			return queueData{}, err
		}
		for _, event := range laneDecisions {
			data.OpenDecisions = append(data.OpenDecisions, queueOpenDecision{Lane: event.OwnerLane, Text: strings.TrimSpace(strings.TrimPrefix(event.Text, "[decision-needed]"))})
		}
	}
	return data, nil
}

func (h *Handler) queueBacklog(w http.ResponseWriter, r *http.Request) {
	lane := strings.TrimSpace(r.URL.Query().Get("lane"))
	tasks, err := h.store.ListTasks(r.Context(), lane, "backlog", "", 1000)
	if err != nil {
		http.Error(w, "invalid queue lane", http.StatusBadRequest)
		return
	}
	h.render(w, "queue_backlog", tasks)
}

func (h *Handler) taskEvents(w http.ResponseWriter, r *http.Request, rawID string) {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	task, found, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.render(w, "task_events", task)
}

func (h *Handler) decisions(w http.ResponseWriter, r *http.Request, fragment bool, email, notice string) {
	data, err := h.decisionData(r, h.csrfForForm(w, r, email), notice)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if fragment {
		h.render(w, "decisions_content", data)
		return
	}
	h.render(w, "page", pageData{Title: "Decisions", Page: "decisions", Body: data})
}

type decisionData struct {
	ApprovalTasks []taskDecisionView
	Tasks         []taskDecisionView
	Escalations   []eventDecisionView
	Signals       []store.RelayEvent
	LaneEvents    []eventDecisionView
	CSRF          string
	CanWrite      bool
	WriteReason   string
	Notice        string
	HasApproval   bool
	Results       []decisionBatchResult
	Rendered      int
	Retrieved     int
	RenderNotice  string
}

type taskDecisionView struct {
	Type       string
	ID         int64
	Task       store.Task
	Question   string
	Options    []string
	Structured *store.DecisionOptions
	Index      int
	CanWrite   bool
}

type eventDecisionView struct {
	Type       string
	ID         int64
	Event      store.RelayEvent
	Question   string
	Options    []string
	Structured *store.DecisionOptions
	Index      int
	CanWrite   bool
}

type decisionForm struct {
	Type     string
	ID       int64
	Question string
	Options  []string
	Task     store.Task
	CSRF     string
	CanWrite bool
}

func decisionFormData(kind string, id int64, question string, options []string, task store.Task, csrf string, canWrite bool) decisionForm {
	return decisionForm{Type: kind, ID: id, Question: question, Options: options, Task: task, CSRF: csrf, CanWrite: canWrite}
}

type eventForm struct {
	Type     string
	ID       int64
	Event    store.RelayEvent
	Options  []string
	CSRF     string
	CanWrite bool
}

func eventFormData(kind string, event store.RelayEvent, csrf string, canWrite bool) eventForm {
	question := event.Text
	if kind == "escalation" {
		question = escalationText(event)
	} else {
		question = strings.TrimSpace(strings.TrimPrefix(question, "[decision-needed]"))
	}
	return eventForm{Type: kind, ID: event.ID, Event: event, Options: decisionOptions(question), CSRF: csrf, CanWrite: canWrite}
}

// decisionData caps how many answerable cards the page renders. Every card
// contributes several successful controls to one page-wide form, and the form
// parser rejects a request outright past its parameter limit -- which locked
// the console at 2500 cards, before any of our own limits were consulted.
// Rendering no more cards than one request can parse keeps that unreachable.
// Signals are not answerable and carry no form fields, so they are neither
// capped nor counted in the notice.
func (h *Handler) decisionData(r *http.Request, csrf, notice string) (decisionData, error) {
	tasks, err := h.store.ListOpenTaskDecisions(r.Context(), 1000)
	if err != nil {
		return decisionData{}, err
	}
	escalations, err := h.store.ListOpenEscalations(r.Context(), 1000)
	if err != nil {
		return decisionData{}, err
	}
	laneEvents, err := h.store.ListOpenLaneDecisions(r.Context(), 1000)
	if err != nil {
		return decisionData{}, err
	}
	data := decisionData{CSRF: csrf, CanWrite: h.hub.configured(), Notice: notice, HasApproval: len(h.admiralLanes) > 0}
	if !data.CanWrite {
		data.WriteReason = "Hub is not configured."
	}
	// Each list is already capped at 1000 by its query, so this sum is what the
	// console retrieved, not a claim about how many decisions exist.
	retrieved := len(tasks) + len(laneEvents)
	for _, escalation := range escalations {
		if !isSignalEscalation(escalation) {
			retrieved++
		}
	}
	// A full source slice may have more rows behind its SQL limit even when the
	// combined render budget did not discard a retrieved card. Tell the operator
	// about that boundary without pretending we know the true backlog total.
	retrievalMayBeTruncated := len(tasks) == decisionBatchParseLimit || len(escalations) == decisionBatchParseLimit || len(laneEvents) == decisionBatchParseLimit
	index := 0
	for _, decision := range tasks {
		if index >= decisionBatchParseLimit {
			break
		}
		view := taskDecisionView{Type: "task", ID: decision.Task.ID, Task: decision.Task, Question: decision.Question, Options: decisionOptions(decision.Question), Structured: decision.Task.Refs.DecisionOptions, Index: index, CanWrite: data.CanWrite}
		index++
		if h.admiralLanes[decision.Task.Lane] {
			data.ApprovalTasks = append(data.ApprovalTasks, view)
		} else {
			data.Tasks = append(data.Tasks, view)
		}
	}
	for _, escalation := range escalations {
		if isSignalEscalation(escalation) {
			data.Signals = append(data.Signals, escalation)
			continue
		}
		if index >= decisionBatchParseLimit {
			continue
		}
		view := eventDecisionViewFor("escalation", escalation, index)
		view.CanWrite = data.CanWrite
		data.Escalations = append(data.Escalations, view)
		index++
	}
	for _, event := range laneEvents {
		if index >= decisionBatchParseLimit {
			break
		}
		view := eventDecisionViewFor("lane", event, index)
		view.CanWrite = data.CanWrite
		data.LaneEvents = append(data.LaneEvents, view)
		index++
	}
	data.Rendered, data.Retrieved = index, retrieved
	if retrieved > index {
		data.RenderNotice = fmt.Sprintf("현재 조회된 %d건 중 %d건 표시 — 표시된 항목을 처리하면 나머지가 이어서 표시됩니다.", retrieved, index)
	} else if retrievalMayBeTruncated {
		data.RenderNotice = fmt.Sprintf("현재 조회된 %d건 중 %d건 표시 — 각 목록은 최대 %d건까지만 조회되므로 추가 항목이 있을 수 있습니다.", retrieved, index, decisionBatchParseLimit)
	}
	return data, nil
}

func eventDecisionViewFor(kind string, event store.RelayEvent, index int) eventDecisionView {
	question := escalationText(event)
	optionQuestion := question
	if kind == "lane" {
		optionQuestion = strings.TrimSpace(strings.TrimPrefix(optionQuestion, "[decision-needed]"))
	}
	view := eventDecisionView{Type: kind, ID: event.ID, Event: event, Question: question, Options: decisionOptions(optionQuestion), Index: index}
	if body, options, ok := store.ParseDecisionOptions(question); ok {
		view.Question, view.Structured, view.Options = body, &options, nil
	}
	return view
}

func (h *Handler) fleet(w http.ResponseWriter, r *http.Request, fragment bool) {
	data, err := h.fleetData(r)
	if err != nil {
		http.Error(w, "fleet console unavailable", http.StatusInternalServerError)
		return
	}
	if fragment {
		h.render(w, "fleet_content", data)
		return
	}
	h.render(w, "page", pageData{Title: "Fleet", Page: "fleet", Body: data})
}

type activeLane struct {
	Lane  string
	Tasks []store.Task
}

type fleetData struct {
	Hub         hubView
	ActiveLanes []activeLane
}

func (h *Handler) fleetData(r *http.Request) (fleetData, error) {
	tasks, err := h.store.ListTasks(r.Context(), "", "", "", 1000)
	if err != nil {
		return fleetData{}, err
	}
	byLane := map[string][]store.Task{}
	for _, task := range tasks {
		if task.State == "claimed" || task.State == "in_progress" {
			byLane[task.Lane] = append(byLane[task.Lane], task)
		}
	}
	lanes := make([]string, 0, len(byLane))
	for lane := range byLane {
		lanes = append(lanes, lane)
	}
	sort.Strings(lanes)
	active := make([]activeLane, 0, len(lanes))
	for _, lane := range lanes {
		active = append(active, activeLane{Lane: lane, Tasks: byLane[lane]})
	}
	return fleetData{Hub: h.hub.current(r.Context()), ActiveLanes: active}, nil
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		// A static embedded-template error is not safe to render as a response.
		// It is only observable in the server's error log via the standard HTTP
		// server logger if the write fails.
		return
	}
}

type pageData struct {
	Title string
	Page  string
	Body  any
}

func splitList(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

type link struct {
	Value string
	Valid bool
}

func githubLink(value string) link {
	return link{Value: value, Valid: strings.HasPrefix(value, "https://github.com/")}
}

func eventMessage(event store.RelayEvent) string {
	if event.Text != "" {
		return event.Text
	}
	if event.Question != "" {
		return event.Question
	}
	return event.ReportLastLine
}

func ingressLabel(event store.RelayEvent) string {
	if event.Kind != "lane.event" {
		return ""
	}
	label, ok := strings.CutPrefix(event.Reason, "http_ingress:")
	if !ok {
		return ""
	}
	return label
}

func shortHead(value string) string {
	if len(value) > 9 {
		return value[:9]
	}
	return value
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func writeSSE(w http.ResponseWriter, relayMaxID, taskEventMaxID int64) {
	payload, _ := json.Marshal(struct {
		RelayMaxID     int64 `json:"relay_max_id"`
		TaskEventMaxID int64 `json:"task_event_max_id"`
	}{relayMaxID, taskEventMaxID})
	fmt.Fprintf(w, "event: delta\ndata: %s\n\n", payload)
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}
