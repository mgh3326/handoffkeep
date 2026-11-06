package tests

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var (
	absoluteURLRe  = regexp.MustCompile(`https?://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]+`)
	protocolRelRe  = regexp.MustCompile(`(?:^|[^a-zA-Z0-9:])//[A-Za-z0-9][A-Za-z0-9.-]*\.[A-Za-z]{2,}`)
	externalSrcRe  = regexp.MustCompile(`(?i)<script[^>]+src=["'](https?:)?//`)
	externalHrefRe = regexp.MustCompile(`(?i)<link[^>]+href=["'](https?:)?//`)
	importURLRe    = regexp.MustCompile(`@import\s+(?:url\()?["']?(https?:)?//`)
	fontURLRe      = regexp.MustCompile(`(?:src|url)\(\s*["']?(https?:)?//`)
	fetchQuotedRe  = regexp.MustCompile(`fetch\(["']([^"']+)["']`)
)

var allowedAbsoluteURLs = []string{
	"https://reactjs.org/docs/error-decoder.html?invariant=",
	"https://react.dev/errors/",
	"http://www.w3.org/1999/xlink",
	"http://www.w3.org/XML/1998/namespace",
	"http://www.w3.org/2000/svg",
	"http://www.w3.org/1998/Math/MathML",
	"http://www.w3.org/1999/xhtml",
	// RFC-2606 reserved TLD: structurally non-routable, so it can never
	// become an external load.
	"https://example.invalid/",
	// Error-message documentation links inside the lazily loaded markdown
	// renderer chunk (react-markdown, hast-util-to-jsx-runtime) — string
	// constants in thrown messages, never requested.
	"https://github.com/remarkjs/react-markdown/blob/main/changelog.md",
	"https://github.com/syntax-tree/hast-util-to-jsx-runtime",
}

func consoleDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "internal", "ui", "static", "console")
}

func shellPagePath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller path unavailable")
	}
	return filepath.Join(filepath.Dir(file), "..", "internal", "ui", "templates", "console.html")
}

func readConsoleArtifacts(t *testing.T) map[string]string {
	t.Helper()
	root := consoleDir(t)
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".map") {
			t.Fatalf("source map committed: %s", path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(shellPagePath(t))
	if err != nil {
		t.Fatal(err)
	}
	out["console.html"] = string(page)
	if len(out) < 2 {
		t.Fatalf("missing console artifacts: %v", keys(out))
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func allowedAbsolute(url string) bool {
	for _, prefix := range allowedAbsoluteURLs {
		if strings.HasPrefix(url, prefix) {
			return true
		}
	}
	return false
}

func TestFleetBundleNoAbsoluteURLs(t *testing.T) {
	artifacts := readConsoleArtifacts(t)
	for name, body := range artifacts {
		for _, match := range absoluteURLRe.FindAllString(body, -1) {
			if !allowedAbsolute(match) {
				t.Errorf("%s absolute URL %q", name, match)
			}
		}
		if protocolRelRe.FindString(body) != "" {
			t.Errorf("%s protocol-relative URL %q", name, protocolRelRe.FindString(body))
		}
	}
}

func TestFleetBundleFetchOnlyUIAPI(t *testing.T) {
	artifacts := readConsoleArtifacts(t)
	// The document viewer and the deploy screen each read one /ui/api route.
	for name, want := range map[string]string{"shared-DocView.js": "/ui/api/board/doc", "shell.js": "/ui/api/deploy-pending"} {
		if !strings.Contains(artifacts[name], want) {
			t.Fatalf("%s does not contain %s", name, want)
		}
	}
	for name, body := range artifacts {
		if strings.Contains(body, "/v1/nodes") || strings.Contains(body, "/v1/jobs") {
			t.Fatalf("%s contains a hub path", name)
		}
	}
	for name, body := range artifacts {
		for _, match := range fetchQuotedRe.FindAllStringSubmatch(body, -1) {
			target := match[1]
			if !strings.HasPrefix(target, "/ui/api/") {
				t.Errorf("%s fetch target %q is not a /ui/api/ relative path", name, target)
			}
		}
	}
}

func TestFleetRuntimeCDNZero(t *testing.T) {
	artifacts := readConsoleArtifacts(t)
	for name, body := range artifacts {
		if strings.Contains(body, "sourceMappingURL") {
			t.Errorf("%s contains a source map URL", name)
		}
		if externalSrcRe.FindString(body) != "" || externalHrefRe.FindString(body) != "" || importURLRe.FindString(body) != "" || fontURLRe.FindString(body) != "" {
			t.Errorf("%s has an external script/link/@import/font URL", name)
		}
	}
}

// Home and Work must not carry the markdown renderer: it ships only in the
// lazy document-viewer chunk. The check walks shell.js and everything it
// imports statically, so a static import of the viewer would fail it.
func TestShellBundleExcludesMarkdownRenderer(t *testing.T) {
	artifacts := readConsoleArtifacts(t)
	staticImport := regexp.MustCompile(`(?:from|import)\s*"\./([^"]+\.js)"`)
	seen := map[string]bool{}
	var walk func(name string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		body, ok := artifacts[name]
		if !ok {
			t.Fatalf("shell imports %s, which is not in the console build", name)
		}
		for _, m := range staticImport.FindAllStringSubmatch(body, -1) {
			walk(m[1])
		}
	}
	walk("shell.js")
	for name := range seen {
		body := artifacts[name]
		if strings.Contains(body, "DocMarkdown") || strings.Contains(body, "data-doc-renderer") || strings.Contains(body, "micromark") {
			t.Errorf("%s is in the shell's static graph and carries the markdown renderer", name)
		}
	}
	if !strings.Contains(artifacts["shell.js"], "shared-DocView.js") {
		t.Fatal("shell.js does not reach the document viewer chunk")
	}
}

func TestFleetBundleHasNoLocalPaths(t *testing.T) {
	artifacts := readConsoleArtifacts(t)
	for name, body := range artifacts {
		if strings.Contains(body, "/Users/") || strings.Contains(body, "/home/") {
			t.Errorf("%s leaked a local path", name)
		}
		if strings.Contains(body, fleetTestSecret) {
			t.Errorf("%s contains the hub test secret", name)
		}
	}
}
