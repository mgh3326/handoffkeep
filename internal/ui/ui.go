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
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
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
	Attachments   *attachments.Manager
	Access        *cfaccess.Verifier
	HubURL        string
	HubToken      string
	HubHTTPClient *http.Client
	PollInterval  time.Duration
	HubCacheTTL   time.Duration
	// YouTrack is serve's legacy read-through service (api.Service.YouTrack):
	// it overlays the cached YouTrack priority and lane on linked tasks and
	// queues priority and lane changes for export after the PG write. serve
	// sets it only with HANDOFFKEEP_YT_READTHROUGH_MODE=on. Nil keeps the
	// console exactly as it was.
	YouTrack *youtrack.Service
}

// Handler is a Cloudflare Access-authenticated UI handler.
type Handler struct {
	store        *store.Store
	attachments  *attachments.Manager
	access       *cfaccess.Verifier
	templates    *template.Template
	hub          *hubProxy
	youTrack     *youtrack.Service
	pollInterval time.Duration
	static       fs.FS
	csrfKey      []byte
	lanes        []string
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
	csrfKey := make([]byte, 32)
	if _, err := rand.Read(csrfKey); err != nil {
		return nil, fmt.Errorf("generate UI CSRF key: %w", err)
	}
	hub := newHubProxy(config.HubURL, config.HubToken, config.HubHTTPClient)
	if config.HubCacheTTL > 0 {
		hub.cacheTTL = config.HubCacheTTL
	} else {
		hub.cacheTTL = 10 * time.Second
	}
	h := &Handler{
		store:        config.Store,
		attachments:  config.Attachments,
		access:       config.Access,
		hub:          hub,
		youTrack:     config.YouTrack,
		pollInterval: poll,
		static:       static,
		csrfKey:      csrfKey,
		assetVersion: vcsRevision(),
	}
	tmpl, err := template.New("ui").Funcs(template.FuncMap{
		"assetURL": h.assetURL,
	}).ParseFS(assets, "templates/console.html")
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
	if r.URL.Path == "/ui" || r.URL.Path == "/ui/work" || r.URL.Path == "/ui/queue" || r.URL.Path == "/ui/deploys" || r.URL.Path == "/ui/home" || r.URL.Path == "/ui/decisions" || r.URL.Path == "/ui/work-status" || strings.HasPrefix(r.URL.Path, "/ui/decisions/") || strings.HasPrefix(r.URL.Path, "/ui/doc/") || strings.HasPrefix(r.URL.Path, "/ui/tasks/") || strings.HasPrefix(r.URL.Path, "/ui/files/") || strings.HasPrefix(r.URL.Path, "/ui/api/") {
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
	if strings.HasPrefix(r.URL.Path, "/ui/files/") {
		sha := strings.TrimPrefix(r.URL.Path, "/ui/files/")
		if identity.ServiceName != "" {
			log.Printf("ui.file_download email=%q sha=%q result=service_reject", identity.ServiceName, sha)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.fileDownload(w, r, identity.Email, sha)
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
	case "/ui", "/ui/home", "/ui/work", "/ui/deploys", "/ui/decisions", "/ui/work-status":
		h.shell(w, r)
	case "/ui/queue":
		h.board(w, r, email)
	default:
		h.serveSubroute(w, r, email)
	}
}

func (h *Handler) serveSubroute(w http.ResponseWriter, r *http.Request, email string) {
	if key, ok := strings.CutPrefix(r.URL.Path, "/ui/doc/"); ok {
		h.document(w, r, key)
		return
	}
	if raw, ok := strings.CutPrefix(r.URL.Path, "/ui/tasks/"); ok {
		h.taskPage(w, r, raw, email)
		return
	}
	if raw, ok := strings.CutPrefix(r.URL.Path, "/ui/decisions/"); ok {
		if !taskIDPattern.MatchString(raw) {
			http.Error(w, "invalid decision id", http.StatusBadRequest)
			return
		}
		h.shell(w, r)
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

// board serves the React queue board page on /ui/queue. All task data reaches the browser through the
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
