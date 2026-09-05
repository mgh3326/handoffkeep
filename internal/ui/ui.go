// Package ui serves the read-only fleet console.
package ui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
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

// Handler is a read-only, Cloudflare Access-authenticated UI handler.
type Handler struct {
	store        *store.Store
	access       *cfaccess.Verifier
	templates    *template.Template
	hub          *hubProxy
	pollInterval time.Duration
	static       fs.FS
}

// New builds the optional fleet console.
func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Access == nil {
		return nil, errors.New("UI requires store and Cloudflare Access verifier")
	}
	tmpl, err := template.New("ui").Funcs(template.FuncMap{
		"formatTime": formatTime,
		"shortHead":  shortHead,
		"githubLink": githubLink,
		"message":    eventMessage,
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
	return &Handler{
		store:        config.Store,
		access:       config.Access,
		templates:    tmpl,
		hub:          newHubProxy(config.HubURL, config.HubToken, config.HubHTTPClient),
		pollInterval: poll,
		static:       static,
	}, nil
}

// ServeHTTP keeps the UI authentication and method boundary separate from the
// existing bearer-token API. No UI request can reach a write method.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.access.Authenticate(r) {
		w.WriteHeader(http.StatusUnauthorized)
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
		h.decisions(w, r, false)
	case "/ui/fleet":
		h.fleet(w, r, false)
	case "/ui/events":
		h.events(w, r)
	default:
		h.serveSubroute(w, r)
	}
}

func (h *Handler) serveSubroute(w http.ResponseWriter, r *http.Request) {
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
		case "decisions":
			h.decisions(w, r, true)
		case "fleet":
			h.fleet(w, r, true)
		default:
			http.NotFound(w, r)
		}
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
	State string
	Tasks []store.Task
}

type queueLane struct {
	Name  string
	Cells []queueCell
}

type queueData struct {
	States []string
	Lanes  []queueLane
}

func (h *Handler) queueData(r *http.Request) (queueData, error) {
	tasks, err := h.store.ListTasks(r.Context(), "", "", "", 1000)
	if err != nil {
		return queueData{}, err
	}
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
	data := queueData{States: taskStates}
	for _, lane := range lanes {
		row := queueLane{Name: lane}
		for _, state := range taskStates {
			row.Cells = append(row.Cells, queueCell{State: state, Tasks: byLane[lane][state]})
		}
		data.Lanes = append(data.Lanes, row)
	}
	return data, nil
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

func (h *Handler) decisions(w http.ResponseWriter, r *http.Request, fragment bool) {
	data, err := h.decisionData(r)
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
	Tasks       []store.TaskDecision
	Escalations []store.RelayEvent
	LaneEvents  []store.RelayEvent
}

func (h *Handler) decisionData(r *http.Request) (decisionData, error) {
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
	return decisionData{Tasks: tasks, Escalations: escalations, LaneEvents: laneEvents}, nil
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
