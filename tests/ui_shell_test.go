package tests

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ui"
)

// noRedirectClient answers with the first response instead of following a
// redirect. The default client follows the old 302 to /ui/home and would pass
// against the old code.
func noRedirectClient(base *http.Client) *http.Client {
	return &http.Client{
		Transport:     base.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestUIShellServesHomeWithoutRedirect(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	response := uiRequest(t, noRedirectClient(h.Client()), http.MethodGet, h.URL+"/ui", assertion, "")
	location := response.Header.Get("Location")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui status=%d, want exactly 200 (no redirect)", response.StatusCode)
	}
	if location != "" {
		t.Fatalf("GET /ui Location=%q, want none", location)
	}
	if !strings.Contains(body, `id="shell-root"`) || !strings.Contains(body, "/ui/static/console/shell.js") {
		t.Fatalf("GET /ui did not mount the shell: %q", body)
	}
	if strings.Contains(body, "board.css") {
		t.Fatalf("Home links board.css: %q", body)
	}
	if !strings.Contains(body, `<link rel="modulepreload" href="/ui/static/console/shared-client.js">`) {
		t.Fatalf("shell page does not preload the react chunk: %q", body)
	}
	if response.Header.Get("Content-Security-Policy") != ui.ConsoleCSP {
		t.Fatalf("GET /ui csp=%q", response.Header.Get("Content-Security-Policy"))
	}
}

func TestUIShellRoutes(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	client := noRedirectClient(h.Client())

	for _, path := range []string{"/ui/home", "/ui/work", "/ui/queue", "/ui/fleet", "/ui/grades", "/ui/docs", "/ui/doc/decision/2026-11-05/example", "/ui/tasks/42"} {
		response := uiRequest(t, client, http.MethodGet, h.URL+path, assertion, "")
		csp := response.Header.Get("Content-Security-Policy")
		body := responseText(t, response)
		if response.StatusCode != http.StatusOK || !strings.Contains(body, `id="shell-root"`) || strings.Contains(body, "board.css") {
			t.Fatalf("%s status=%d body=%q", path, response.StatusCode, body)
		}
		if csp != ui.ConsoleCSP {
			t.Fatalf("%s csp=%q", path, csp)
		}
	}
	// A missing document is the viewer's "document not found", not a server 404:
	// the page is the same shell and the JSON route answers 404.
	response := uiRequest(t, client, http.MethodGet, h.URL+"/ui/api/board/doc?key=missing.md", assertion, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing document JSON status=%d", response.StatusCode)
	}
	response.Body.Close()
	for _, path := range []string{"/ui/doc/a..b", "/ui/doc/bad%20key", "/ui/tasks/0", "/ui/tasks/abc", "/ui/tasks/01"} {
		response := uiRequest(t, client, http.MethodGet, h.URL+path, assertion, "")
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400", path, response.StatusCode)
		}
		response.Body.Close()
	}
}
