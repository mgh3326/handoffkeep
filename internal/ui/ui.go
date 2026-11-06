// Package ui serves the Cloudflare Access-authenticated fleet console.
package ui

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

//go:embed templates/*.html static
var assets embed.FS

var taskStates = store.TaskStates()

// dwellStates adds the old state names, which task_events keeps as written, so
// dwell time over history reads both vocabularies.
var dwellStates = append(store.TaskStates(), store.LegacyTaskStates()...)

// Config supplies only server-side dependencies. HubToken is intentionally not
// represented in any template model.
type Config struct {
	Store         *store.Store
	Access        *cfaccess.Verifier
	HubURL        string
	HubToken      string
	HubHTTPClient *http.Client
	PollInterval  time.Duration
	HubCacheTTL   time.Duration
	// SSEHeartbeat is how often /ui/events writes a comment line to an idle
	// stream; zero means 25 seconds.
	SSEHeartbeat time.Duration
}

// Handler is a Cloudflare Access-authenticated UI handler.
type Handler struct {
	store         *store.Store
	access        *cfaccess.Verifier
	templates     *template.Template
	hub           *hubProxy
	pollInterval  time.Duration
	sseHeartbeat  time.Duration
	static        fs.FS
	csrfKey       []byte
	lanes         []string
	laneSet       map[string]bool
	directorLanes map[string]bool
	// assetVersion is the build stamp appended to static asset URLs (?v=). It
	// is empty on unstamped builds, where the static handler must not emit a
	// long-lived cache policy.
	assetVersion string
	// preloads caches shellPreloads: the embedded bundle never changes.
	preloadOnce sync.Once
	preloads    []string
}

// readBuildInfo is a variable so tests can stamp or unstamp build info.
var readBuildInfo = debug.ReadBuildInfo

// vcsRevision returns the binary's VCS stamp, or "" on unstamped builds
// (source export, -buildvcs=false, plain go run).
func vcsRevision() string {
	if info, ok := readBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return ""
}

// New builds the optional fleet console.
func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Access == nil {
		return nil, errors.New("UI requires store and Cloudflare Access verifier")
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	poll := config.PollInterval
	if poll <= 0 {
		poll = 10 * time.Second
	}
	heartbeat := config.SSEHeartbeat
	if heartbeat <= 0 {
		heartbeat = 25 * time.Second
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
	directorLanes := make(map[string]bool)
	for _, envName := range []string{"HANDOFFKEEP_UI_ADMIRAL_LANES", "HANDOFFKEEP_UI_DIRECTOR_LANES"} {
		for _, lane := range splitList(os.Getenv(envName)) {
			directorLanes[lane] = true
		}
	}
	hub := newHubProxy(config.HubURL, config.HubToken, config.HubHTTPClient)
	if config.HubCacheTTL > 0 {
		hub.cacheTTL = config.HubCacheTTL
	} else {
		hub.cacheTTL = 10 * time.Second
	}
	h := &Handler{
		store:         config.Store,
		access:        config.Access,
		hub:           hub,
		pollInterval:  poll,
		sseHeartbeat:  heartbeat,
		static:        static,
		csrfKey:       csrfKey,
		lanes:         lanes,
		laneSet:       laneSet,
		directorLanes: directorLanes,
		assetVersion:  vcsRevision(),
	}
	tmpl, err := template.New("ui").Funcs(template.FuncMap{
		"formatTime":       formatTime,
		"formatTimePtr":    formatTimePtr,
		"shortHead":        shortHead,
		"githubLink":       githubLink,
		"message":          messageParts,
		"ingressLabel":     ingressLabel,
		"decisionFormData": decisionFormData,
		"eventFormData":    eventFormData,
		"assetURL":         h.assetURL,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h.templates = tmpl
	return h, nil
}

// assetURL appends the build stamp to a static asset path so a deploy changes
// the URL browsers fetch. On unstamped builds it returns the path bare —
// never a bare ?v= — and the static handler answers such requests no-cache.
func (h *Handler) assetURL(p string) string {
	if h.assetVersion == "" {
		return p
	}
	return p + "?v=" + url.QueryEscape(h.assetVersion)
}

// ServeHTTP keeps the UI authentication and method boundary separate from the
// existing bearer-token API. Only the explicit UI write routes can reach a
// mutating operation.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Pattern = routePattern(r.URL.Path)
	if r.URL.Path == "/ui" || r.URL.Path == "/ui/work" || r.URL.Path == "/ui/fleet" || r.URL.Path == "/ui/queue" || r.URL.Path == "/ui/grades" || r.URL.Path == "/ui/deploys" || r.URL.Path == "/ui/docs" || r.URL.Path == "/ui/home" || r.URL.Path == "/ui/decisions" || strings.HasPrefix(r.URL.Path, "/ui/decisions/") || strings.HasPrefix(r.URL.Path, "/ui/doc/") || strings.HasPrefix(r.URL.Path, "/ui/tasks/") || strings.HasPrefix(r.URL.Path, "/ui/api/") {
		setConsoleCSP(w)
	}
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
	r = r.WithContext(context.WithValue(r.Context(), shellEmailKey{}, email))
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/ui/dispositions/answer":
			h.answerDisposition(w, r, identity)
		case "/ui/dispositions/accept-batch":
			h.acceptDispositionBatch(w, r, identity)
		case "/ui/dispositions/renotify":
			h.renotifyDisposition(w, r, identity)
		case "/ui/decisions/answer":
			h.answerDecision(w, r, email)
		case "/ui/decisions/answer-batch":
			h.answerDecisionBatch(w, r, email)
		case "/ui/compose":
			h.composePost(w, r, email)
		default:
			if id, ok := pathTaskID(taskCommentsPathRE, r.URL.Path); ok {
				h.createTaskComment(w, r, email, id)
				return
			}
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
	case "/ui", "/ui/work", "/ui/deploys", "/ui/decisions":
		h.shell(w, r)
	case "/ui/timeline":
		h.timeline(w, r, false)
	case "/ui/queue":
		h.board(w, r, email)
	case "/ui/decisions/legacy":
		h.decisions(w, r, false, email, r.URL.Query().Get("result"))
	case "/ui/compose":
		h.compose(w, r, false, email, r.URL.Query().Get("result"))
	case "/ui/fleet":
		h.fleet(w, r, false)
	case "/ui/grades":
		h.grades(w, r)
	case "/ui/docs":
		h.docsPage(w, r)
	case "/ui/home":
		h.dashboardPage(w, r)
	case "/ui/events":
		h.events(w, r)
	default:
		h.serveSubroute(w, r, email)
	}
}

func (h *Handler) serveSubroute(w http.ResponseWriter, r *http.Request, email string) {
	if name, ok := strings.CutPrefix(r.URL.Path, "/ui/fragments/"); ok {
		switch name {
		case "timeline":
			h.timeline(w, r, true)
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
	if raw, ok := strings.CutPrefix(r.URL.Path, "/ui/tasks/"); ok {
		h.taskPage(w, r, raw, email)
		return
	}
	if raw, ok := strings.CutPrefix(r.URL.Path, "/ui/decisions/"); ok {
		h.taskPage(w, r, raw, email)
		return
	}
	if file, ok := strings.CutPrefix(r.URL.Path, "/ui/static/"); ok {
		h.staticFile(w, r, file)
		return
	}
	http.NotFound(w, r)
}

// taskPage serves the deep-link task page /ui/tasks/<id>. The shell reads the
// id from the path; the server only gates the id shape — a non-numeric or
// out-of-range id is a 400, never a silently different page.
func (h *Handler) taskPage(w http.ResponseWriter, r *http.Request, raw, email string) {
	if raw == "" {
		http.NotFound(w, r)
		return
	}
	if !taskIDPattern.MatchString(raw) {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	h.shell(w, r)
}

var taskIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,14}$`)

func (h *Handler) staticFile(w http.ResponseWriter, r *http.Request, file string) {
	clean := path.Clean(file)
	switch {
	case clean == "htmx.min.js" || clean == "htmx.LICENSE":
	case strings.HasPrefix(clean, "console/") && !strings.HasSuffix(clean, ".map"):
	default:
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(h.static, clean)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(clean, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(clean, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case clean == "htmx.LICENSE":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// Long-lived caching is only safe when the request URL carries this exact
	// build's stamp. Requests without it — including the fixed shared-*.js
	// chunk paths inside bundles — must revalidate so a deploy cannot serve a
	// stale bundle or chunk. Unstamped builds (no vcs.revision) disable the
	// cache outright rather than emitting an empty ?v=.
	if h.assetVersion != "" && r.URL.Query().Get("v") == h.assetVersion {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
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

// board serves the React queue board page on the same /ui/queue URL the htmx
// table used to occupy. All task data reaches the browser through the
// /ui/api/board/* BFF routes; this page is only the mount point. A ?task=
// deep-link id is shape-checked here so a malformed value gets a 400 instead
// of an arbitrary script-level string.
func (h *Handler) board(w http.ResponseWriter, r *http.Request, email string) {
	if raw := r.URL.Query().Get("task"); raw != "" && !taskIDPattern.MatchString(raw) {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	h.shell(w, r)
}

func (h *Handler) decisions(w http.ResponseWriter, r *http.Request, fragment bool, email, notice string) {
	data, err := h.decisionData(r, h.csrfForForm(w, r, email), email, notice)
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
	Disposition   *dispositionSection
	Requests      decisionRequestSection
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
		question = decisionEscalationQuestion(event)
	} else {
		question = strings.TrimSpace(strings.TrimPrefix(question, decisionNeededMarker))
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
func (h *Handler) decisionData(r *http.Request, csrf, email, notice string) (decisionData, error) {
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
	data := decisionData{CSRF: csrf, CanWrite: h.hub.configured(), Notice: notice, HasApproval: len(h.directorLanes) > 0}
	if !data.CanWrite {
		data.WriteReason = "Hub is not configured."
	}
	if data.Disposition, err = h.dispositionData(r, csrf, email, data.CanWrite); err != nil {
		return decisionData{}, err
	}
	// A failed read fails the page: an unreadable list is never shown as
	// "0 pending".
	if data.Requests, err = h.decisionRequestData(r); err != nil {
		return decisionData{}, err
	}
	// Each list is already capped at 1000 by its query, so this sum is what the
	// console retrieved, not a claim about how many decisions exist.
	retrieved := len(tasks) + len(laneEvents)
	for _, escalation := range escalations {
		if isDecisionEscalation(escalation) {
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
		if h.directorLanes[decision.Task.Lane] {
			data.ApprovalTasks = append(data.ApprovalTasks, view)
		} else {
			data.Tasks = append(data.Tasks, view)
		}
	}
	for _, escalation := range escalations {
		if !isDecisionEscalation(escalation) {
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
		optionQuestion = strings.TrimSpace(strings.TrimPrefix(optionQuestion, decisionNeededMarker))
	} else {
		question = decisionEscalationQuestion(event)
		optionQuestion = question
	}
	view := eventDecisionView{Type: kind, ID: event.ID, Event: event, Question: question, Options: decisionOptions(optionQuestion), Index: index}
	if body, options, ok := store.ParseDecisionOptions(question); ok {
		view.Question, view.Structured, view.Options = body, &options, nil
	}
	return view
}

func (h *Handler) fleet(w http.ResponseWriter, r *http.Request, fragment bool) {
	if fragment {
		h.render(w, "fleet_content", nil)
		return
	}
	h.shell(w, r)
}

// shellPageData is the model of shell_page. Preloads are the unstamped URLs of
// the static imports of shell.js, in the order the entry lists them.
type shellPageData struct {
	Preloads []string
	// CSRF is the form token for the signed-in operator. The shell's write
	// views (decision answers, comments) read it from <meta name="hk-csrf">,
	// and the same request sets the hk_ui_csrf cookie it is checked against.
	CSRF string
}

// shellEmailKey carries the verified operator email from ServeHTTP to the
// shell page, so every mount point issues the same CSRF token without each
// caller threading the email through.
type shellEmailKey struct{}

// shell serves the one page behind Home, Work, the document viewer and the
// task deep link; the browser picks the view from the path. It also renews
// the CSRF form token, as every old page with a write form did (main CI
// e4db65e..aaebbe6 failed when the shell dropped it).
func (h *Handler) shell(w http.ResponseWriter, r *http.Request) {
	email, _ := r.Context().Value(shellEmailKey{}).(string)
	csrf := ""
	if email != "" {
		csrf = h.csrfForForm(w, r, email)
	}
	h.render(w, "shell_page", shellPageData{Preloads: h.shellPreloads(), CSRF: csrf})
}

// staticImportRE matches `from"./x.js"` and `import"./x.js"` in the minified
// entry. A dynamic import() has its path inside parentheses and never matches.
var staticImportRE = regexp.MustCompile(`(?:from|import)\s*"\./([A-Za-z0-9._-]+\.js)"`)

// shellPreloads lists the chunks shell.js imports statically, so the page can
// fetch them in parallel with the entry. The URL is exactly the one the entry
// imports: a stamped URL is a different module and would load twice.
func (h *Handler) shellPreloads() []string {
	h.preloadOnce.Do(func() {
		entry, err := fs.ReadFile(h.static, "console/shell.js")
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, m := range staticImportRE.FindAllSubmatch(entry, -1) {
			file := string(m[1])
			if !seen[file] {
				seen[file] = true
				h.preloads = append(h.preloads, "/ui/static/console/"+file)
			}
		}
	})
	return h.preloads
}

func (h *Handler) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// HTML must always be revalidated: it is the carrier of the ?v= asset
	// stamps, so a cached page would pin a stale bundle.
	w.Header().Set("Cache-Control", "no-cache")
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

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatTime(*value)
}
