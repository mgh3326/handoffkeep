package ui

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// staticImportsOf lists the chunks an entry imports statically by scanning for
// every "./x.js" specifier and keeping those not opened by `import(`. It is a
// different reading of the file than shellPreloads uses, so the two agree only
// when both are right.
func staticImportsOf(t *testing.T, name string) []string {
	t.Helper()
	body, err := fs.ReadFile(assets, "static/console/"+name)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	var out []string
	for _, m := range regexp.MustCompile(`"\./([^"]+\.js)"`).FindAllStringSubmatchIndex(text, -1) {
		before := strings.TrimRight(text[:m[0]], " ")
		if strings.HasSuffix(before, "import(") {
			continue
		}
		out = append(out, "/ui/static/console/"+text[m[2]:m[3]])
	}
	sort.Strings(out)
	return out
}

// The shell page preloads exactly the static imports of shell.js, at the URL
// the entry imports. A stamped URL is a different module and would load twice.
func TestShellPagePreloadsExactlyTheEntryImports(t *testing.T) {
	want := staticImportsOf(t, "shell.js")
	if len(want) == 0 {
		t.Fatal("shell.js has no static imports to preload")
	}
	for _, version := range []string{"", "abc123def"} {
		h := newAssetTestHandler(t, version)
		rec := httptest.NewRecorder()
		h.shell(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))
		body := rec.Body.String()
		var got []string
		for _, m := range regexp.MustCompile(`<link rel="modulepreload" href="([^"]*)">`).FindAllStringSubmatch(body, -1) {
			got = append(got, m[1])
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("version=%q preloads=%v, want the entry's static imports %v", version, got, want)
		}
		for _, href := range got {
			if strings.Contains(href, "?") {
				t.Fatalf("preload %q is stamped: the entry imports the bare URL", href)
			}
		}
	}
}

func TestShellPageLinksOnlyShellAssets(t *testing.T) {
	h := newAssetTestHandler(t, "abc123def")
	rec := httptest.NewRecorder()
	h.shell(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))
	body := rec.Body.String()
	for _, want := range []string{`id="shell-root"`, "/ui/static/console/shell.js?v=abc123def", "/ui/static/console/shell.css?v=abc123def", "/ui/static/console/tokens.css?v=abc123def"} {
		if !strings.Contains(body, want) {
			t.Fatalf("shell page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "board.css") {
		t.Fatalf("shell page links board.css: %s", body)
	}
	// The markdown renderer is a lazy chunk: it is never preloaded.
	if strings.Contains(body, "DocMarkdown") || strings.Contains(body, "DocView") {
		t.Fatalf("shell page preloads the document viewer: %s", body)
	}
}

// The shell is the mount point of every write view, so it must hand out the
// CSRF form token like the old pages did: the hk_ui_csrf cookie plus the
// <meta name="hk-csrf"> the browser reads (main CI e4db65e..aaebbe6 broke when
// it stopped). Without a verified email it renders no token.
func TestShellIssuesCSRFToken(t *testing.T) {
	h := newAssetTestHandler(t, "abc123def")
	r := httptest.NewRequest(http.MethodGet, "/ui", nil)
	r = r.WithContext(context.WithValue(r.Context(), shellEmailKey{}, "admin@example.com"))
	rec := httptest.NewRecorder()
	h.shell(rec, r)
	var cookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookie {
			cookie = c.Value
		}
	}
	if cookie == "" || !strings.Contains(rec.Body.String(), `<meta name="hk-csrf" content="`+cookie+`">`) {
		t.Fatalf("shell did not issue the CSRF cookie and meta tag: cookie=%q", cookie)
	}
	anon := httptest.NewRecorder()
	h.shell(anon, httptest.NewRequest(http.MethodGet, "/ui", nil))
	if strings.Contains(anon.Body.String(), "hk-csrf") || len(anon.Result().Cookies()) != 0 {
		t.Fatal("shell issued a CSRF token without a verified email")
	}
}
