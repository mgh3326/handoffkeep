package ui

import (
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// newAssetTestHandler builds a Handler with only the fields assetURL,
// staticFile, and render touch — no store, no verifier.
func newAssetTestHandler(t *testing.T, version string) *Handler {
	t.Helper()
	static, err := fs.Sub(assets, "static")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{static: static, assetVersion: version}
	tmpl, err := template.New("ui").Funcs(template.FuncMap{
		"formatTime":       formatTime,
		"shortHead":        shortHead,
		"githubLink":       githubLink,
		"message":          messageParts,
		"ingressLabel":     ingressLabel,
		"decisionFormData": decisionFormData,
		"eventFormData":    eventFormData,
		"assetURL":         h.assetURL,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	h.templates = tmpl
	return h
}

func serveStatic(t *testing.T, h *Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.staticFile(rec, req, strings.TrimPrefix(req.URL.Path, "/ui/static/"))
	return rec
}

func TestAssetURLStampsVersionedRequests(t *testing.T) {
	h := newAssetTestHandler(t, "abc123def")
	rec := httptest.NewRecorder()
	h.render(rec, "board_page", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "/ui/static/console/board.js?v=abc123def") || !strings.Contains(body, "/ui/static/console/board.css?v=abc123def") {
		t.Fatalf("stamped board page did not version asset URLs: %q", body)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("html cache-control=%q", rec.Header().Get("Cache-Control"))
	}

	if rec := serveStatic(t, h, "/ui/static/console/board.js?v=abc123def"); rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("versioned asset cache-control=%q", rec.Header().Get("Cache-Control"))
	}
	// A wrong or missing stamp must revalidate — this is what keeps the fixed
	// shared-*.js chunk name safe across deploys.
	for _, target := range []string{
		"/ui/static/console/board.js",
		"/ui/static/console/board.js?v=old-stamp",
		"/ui/static/console/shared-client.js",
		"/ui/static/console/shared-client.js?v=old-stamp",
	} {
		if rec := serveStatic(t, h, target); rec.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s cache-control=%q", target, rec.Header().Get("Cache-Control"))
		}
	}
}

func TestAssetURLUnstampedDisablesCaching(t *testing.T) {
	h := newAssetTestHandler(t, "")
	if got := h.assetURL("/ui/static/console/board.js"); got != "/ui/static/console/board.js" {
		t.Fatalf("unstamped assetURL emitted %q — empty ?v= must never appear", got)
	}
	rec := httptest.NewRecorder()
	h.render(rec, "board_page", nil)
	body := rec.Body.String()
	if strings.Contains(body, "?v=") {
		t.Fatalf("unstamped page emitted an empty version query: %q", body)
	}
	if !strings.Contains(body, "/ui/static/console/board.js") {
		t.Fatalf("unstamped page did not reference board.js: %q", body)
	}
	for _, target := range []string{"/ui/static/console/board.js", "/ui/static/console/shared-client.js", "/ui/static/htmx.min.js"} {
		if rec := serveStatic(t, h, target); rec.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("unstamped %s cache-control=%q — must not stay long-lived", target, rec.Header().Get("Cache-Control"))
		}
	}
}

// TestNewWiresBuildInfoIntoAssetStamp guards the wiring, not just the helper:
// New() must stamp asset URLs from the binary's own vcs.revision — a constant
// or empty stamp here is the original stale-cache bug. Building Handler by
// hand (newAssetTestHandler) cannot see this path.
func TestNewWiresBuildInfoIntoAssetStamp(t *testing.T) {
	orig := readBuildInfo
	defer func() { readBuildInfo = orig }()
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "testrev0123"}}}, true
	}
	h, err := New(Config{Store: &store.Store{}, Access: &cfaccess.Verifier{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.render(rec, "board_page", nil)
	if body := rec.Body.String(); !strings.Contains(body, "/ui/static/console/board.js?v=testrev0123") {
		t.Fatalf("New() did not stamp board assets from build info: %q", body)
	}
	if rec := serveStatic(t, h, "/ui/static/console/board.js?v=testrev0123"); rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("stamped asset cache-control=%q", rec.Header().Get("Cache-Control"))
	}
	if rec := serveStatic(t, h, "/ui/static/console/board.js?v=other"); rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("wrong stamp cache-control=%q", rec.Header().Get("Cache-Control"))
	}

	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	h2, err := New(Config{Store: &store.Store{}, Access: &cfaccess.Verifier{}})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h2.render(rec, "board_page", nil)
	if body := rec.Body.String(); strings.Contains(body, "?v=") {
		t.Fatalf("unstamped New() emitted a version query: %q", body)
	}
}

func TestTaskPageIDShape(t *testing.T) {
	h := newAssetTestHandler(t, "")
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"42", http.StatusOK},
		{"1", http.StatusOK},
		{"999999999999999", http.StatusOK},
		{"0", http.StatusBadRequest},
		{"01", http.StatusBadRequest},
		{"abc", http.StatusBadRequest},
		{"-1", http.StatusBadRequest},
		{"9999999999999999", http.StatusBadRequest},
		{"42/extra", http.StatusBadRequest},
		{"<script>", http.StatusBadRequest},
		{"42%0a", http.StatusBadRequest},
		{"", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, "/ui/tasks/"+tc.raw, nil)
		rec := httptest.NewRecorder()
		h.taskPage(rec, req, tc.raw)
		if rec.Code != tc.want {
			t.Fatalf("taskPage(%q)=%d, want %d", tc.raw, rec.Code, tc.want)
		}
	}
}
