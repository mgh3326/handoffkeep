package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const (
	cfFixtureID     = "cf-access-id.fixture.invalid"
	cfFixtureSecret = "cf-access-secret.fixture.invalid"
)

// isolateConfig points HOME at a temp dir and clears every env key config()
// reads, so a developer's real config.env cannot leak into a test.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"HANDOFFKEEP_URL", "HANDOFFKEEP_TOKEN", "HANDOFFKEEP_CF_ACCESS_CLIENT_ID", "HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET"} {
		t.Setenv(k, "")
	}
}

// config reads the CF pair from config.env with the same per-key precedence
// as URL/TOKEN: environment wins, the file only fills unset keys.
func TestConfigCFAccessEnvAndFile(t *testing.T) {
	isolateConfig(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".config", "handoffkeep")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "HANDOFFKEEP_URL=http://file-hk.invalid\n" +
		"HANDOFFKEEP_TOKEN=file-token\n" +
		"HANDOFFKEEP_CF_ACCESS_CLIENT_ID=file-cf-id\n" +
		"HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET=file-cf-secret\n"
	if err := os.WriteFile(filepath.Join(dir, "config.env"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	k := config()
	if k.url != "http://file-hk.invalid" || k.token != "file-token" || k.cfAccessID != "file-cf-id" || k.cfAccessSecret != "file-cf-secret" {
		t.Fatalf("config()=%+v", k)
	}

	// Environment wins per key over the file, exactly as URL/TOKEN behave.
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", "env-cf-id")
	k = config()
	if k.cfAccessID != "env-cf-id" || k.cfAccessSecret != "file-cf-secret" {
		t.Fatalf("env precedence: config()=%+v", k)
	}
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", "env-cf-secret")
	k = config()
	if k.cfAccessSecret != "env-cf-secret" {
		t.Fatalf("env precedence: cfAccessSecret=%q", k.cfAccessSecret)
	}
}

// A half-configured pair is a named config error that names the missing key
// and never prints the configured value, on both the flag-driven client path
// and the stdio mcp path.
func TestCFAccessHalfConfigIsNamedError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      string
		secret  string
		missing string
	}{
		{"id only", cfFixtureID, "", "HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET"},
		{"secret only", "", cfFixtureSecret, "HANDOFFKEEP_CF_ACCESS_CLIENT_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("HANDOFFKEEP_URL", "http://hk.invalid")
			t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
			t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", tc.id)
			t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", tc.secret)

			var out bytes.Buffer
			err := run([]string{"doc", "get", "--id", "1"}, &out, &out)
			if err == nil || !strings.Contains(err.Error(), "cf_access_config_incomplete") {
				t.Fatalf("run err=%v want cf_access_config_incomplete", err)
			}
			if !strings.Contains(err.Error(), tc.missing) {
				t.Fatalf("err=%v must name %s", err, tc.missing)
			}
			for _, leak := range []string{tc.id, tc.secret} {
				if leak != "" && strings.Contains(err.Error(), leak) {
					t.Fatalf("err=%v leaks configured value %q", err, leak)
				}
			}
			if err := runMCP(); err == nil || !strings.Contains(err.Error(), "cf_access_config_incomplete") {
				t.Fatalf("runMCP err=%v want cf_access_config_incomplete", err)
			}
		})
	}
}

// headerRecorder is a fake hk server recording the auth surface of each
// request.
type headerRecorder struct {
	mu  sync.Mutex
	got []map[string]string
}

func (h *headerRecorder) handler(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.got = append(h.got, map[string]string{
		"id":     r.Header.Get("CF-Access-Client-Id"),
		"secret": r.Header.Get("CF-Access-Client-Secret"),
		"ua":     r.Header.Get("User-Agent"),
	})
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(store.Document{ID: 5, Key: "k/five"})
}

func (h *headerRecorder) last(t *testing.T) map[string]string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.got) == 0 {
		t.Fatal("no request reached the server")
	}
	return h.got[len(h.got)-1]
}

// doc get through run() exercises remoteClient(fs): the pair and the explicit
// User-Agent must arrive at the hk host by equality.
func TestCLISendsCFAccessHeaders(t *testing.T) {
	isolateConfig(t)
	rec := &headerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", cfFixtureID)
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", cfFixtureSecret)

	var out bytes.Buffer
	if err := run([]string{"doc", "get", "--id", "5"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	got := rec.last(t)
	if got["id"] != cfFixtureID || got["secret"] != cfFixtureSecret {
		t.Fatalf("CF headers=(%q,%q)", got["id"], got["secret"])
	}
	if got["ua"] != remote.UserAgent {
		t.Fatalf("User-Agent=%q want %q", got["ua"], remote.UserAgent)
	}
}

// The client runMCP hands to NewStdio and the r2usage local comparison client
// are both built by configuredClient — prove it carries the pair by firing a
// real request at a fake hk server.
func TestConfiguredClientCarriesCFAccess(t *testing.T) {
	isolateConfig(t)
	rec := &headerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", cfFixtureID)
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", cfFixtureSecret)

	c := configuredClient()
	if c.CFAccessClientID != cfFixtureID || c.CFAccessClientSecret != cfFixtureSecret {
		t.Fatalf("configuredClient=%+v dropped the CF pair", c)
	}
	if _, _, err := c.GetDocument(t.Context(), "k"); err != nil {
		t.Fatal(err)
	}
	got := rec.last(t)
	if got["id"] != cfFixtureID || got["secret"] != cfFixtureSecret || got["ua"] != remote.UserAgent {
		t.Fatalf("headers=%v", got)
	}
}

// A Cloudflare Access login redirect through the full CLI surfaces the named
// error; the captured output and error must not contain the secret, the id,
// or any part of the Location.
func TestCLILoginRedirectFailsLoudWithoutLeaks(t *testing.T) {
	isolateConfig(t)
	const loginURL = "https://team-one.cloudflareaccess.com/cdn-cgi/access/login?redirect_url=signed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loginURL)
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", cfFixtureID)
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", cfFixtureSecret)

	var out bytes.Buffer
	err := run([]string{"doc", "get", "--id", "5"}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "cf_access_login_redirect") {
		t.Fatalf("err=%v want cf_access_login_redirect", err)
	}
	combined := err.Error() + out.String()
	for _, leak := range []string{cfFixtureID, cfFixtureSecret, "fixture-token", loginURL, "redirect_url", "cloudflareaccess.com"} {
		if strings.Contains(combined, leak) {
			t.Fatalf("output leaks %q: %s", leak, combined)
		}
	}
}
