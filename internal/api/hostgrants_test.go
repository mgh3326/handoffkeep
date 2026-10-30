package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func writeAuthFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.env")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A bad host line is logged and skipped like a quota grant: it never refuses
// startup. Unlike a quota grant it fails closed: the line's client stays under
// host grants, so a typo cannot lift a restriction.
func TestHostGrantFile(t *testing.T) {
	for name, tc := range map[string]struct {
		line      string
		restrict  bool // the client is left with a line that lists no hosts
		logReason string
	}{
		"uppercase id":   {"HANDOFFKEEP_HOSTS_a=Host-A", true, "invalid_line"},
		"underscore id":  {"HANDOFFKEEP_HOSTS_a=host_a", true, "invalid_line"},
		"long id":        {"HANDOFFKEEP_HOSTS_a=" + strings.Repeat("h", 33), true, "invalid_line"},
		"empty entry":    {"HANDOFFKEEP_HOSTS_a=host-a,,host-b", true, "invalid_line"},
		"trailing comma": {"HANDOFFKEEP_HOSTS_a=host-a,", true, "invalid_line"},
		"one bad entry":  {"HANDOFFKEEP_HOSTS_a=host-a,host/b", true, "invalid_line"},
		"wildcard":       {"HANDOFFKEEP_HOSTS_a=*", true, "invalid_line"},
		"empty value":    {"HANDOFFKEEP_HOSTS_a=", true, "invalid_line"},
		"no equals":      {"HANDOFFKEEP_HOSTS_a", true, "invalid_line"},
		"empty client":   {"HANDOFFKEEP_HOSTS_=host-a", false, "invalid_line"},
		"unknown client": {"HANDOFFKEEP_HOSTS_b=host-a", false, "unknown_client"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			p := writeAuthFile(t, "HANDOFFKEEP_TOKEN_a=x\n"+tc.line+"\n")
			tokens, _, err := LoadTokens(p)
			if err != nil {
				t.Fatalf("host line refused startup: %v", err)
			}
			grants, err := LoadHostGrants(p, tokens)
			if err != nil {
				t.Fatal(err)
			}
			allowed, listed := grants.Allows("a", "host-a")
			if tc.restrict && (allowed || !listed) {
				t.Fatalf("Allows = %v, %v; a bad line must leave the client listed with no hosts", allowed, listed)
			}
			if !tc.restrict && (!allowed || listed) {
				t.Fatalf("Allows = %v, %v; a dropped line must leave the client with no line", allowed, listed)
			}
			if !strings.Contains(logs.String(), "api.host_grants_skipped") || !strings.Contains(logs.String(), "reason="+tc.logReason) {
				t.Fatalf("skip not logged: %q", logs)
			}
		})
	}

	// A bad line next to a good one drops only the bad line.
	logs := captureLog(t)
	p := writeAuthFile(t, "HANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_HOSTS_a=host-a\nHANDOFFKEEP_HOSTS_a=host-b,Bad\n")
	tokens, _, err := LoadTokens(p)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := LoadHostGrants(p, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, listed := grants.Allows("a", "host-a"); !allowed || !listed {
		t.Errorf("good line lost: %v, %v", allowed, listed)
	}
	if allowed, listed := grants.Allows("a", "host-b"); allowed || !listed {
		t.Errorf("bad line granted a host: %v, %v", allowed, listed)
	}
	if !strings.Contains(logs.String(), "api.host_grants_skipped") {
		t.Errorf("bad line not logged: %q", logs)
	}

	logs = captureLog(t)
	p = writeAuthFile(t, "# HANDOFFKEEP_HOSTS_a=ignored\nHANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_HOSTS_a=host-a, host-b\nHANDOFFKEEP_HOSTS_a=host-c\nHANDOFFKEEP_TOKEN_b=y\nHANDOFFKEEP_HOSTS_b =host-d\nHANDOFFKEEP_TOKEN_c=z\n")
	tokens, _, err = LoadTokens(p)
	if err != nil {
		t.Fatal(err)
	}
	grants, err = LoadHostGrants(p, tokens)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		client, host    string
		allowed, listed bool
	}{
		{"a", "host-a", true, true},
		{"a", "host-b", true, true}, // spaces around an entry are trimmed
		{"a", "host-c", true, true}, // lines for one client add up
		{"a", "host-d", false, true},
		{"b", "host-d", true, true},
		{"b", "host-a", false, true},
		{"c", "host-a", true, false}, // a token without a line
		{"nobody", "host-a", true, false},
	} {
		if allowed, listed := grants.Allows(tc.client, tc.host); allowed != tc.allowed || listed != tc.listed {
			t.Errorf("Allows(%s, %s) = %v, %v; want %v, %v", tc.client, tc.host, allowed, listed, tc.allowed, tc.listed)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("valid file logged: %q", logs)
	}
}

// An unlisted host is refused before the store, so these need no database.
func TestQuotaPresenceHostGrantGuards(t *testing.T) {
	const key = "codex:0123456789abcdef"
	h := Server{
		Tokens:        Tokens{"listed": "token-l", "other": "token-o"},
		QuotaAccounts: QuotaAccounts{"listed": {key: true}, "other": {key: true}},
		HostGrants:    HostGrants{"listed": {"host-a": true, "host-b": true}},
	}.Handler()
	now := time.Now().UTC().Format(time.RFC3339)
	body := func(host string) string {
		return `{"host_id":"` + host + `","account_key":"` + key + `","last_seen":"` + now + `"}`
	}
	for _, tc := range []struct {
		name, body, token, err string
		status                 int
	}{
		{"unlisted host", body("host-c"), "token-l", "quota_host_forbidden", http.StatusForbidden},
		{"another clients host", body("host-d"), "token-l", "quota_host_forbidden", http.StatusForbidden},
		{"case differs", body("Host-A"), "token-l", "quota_host_forbidden", http.StatusForbidden},
		// Validation and the account grant still run first.
		{"invalid host", body("host/a"), "token-l", "invalid_quota_presence", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("PUT", "/v1/quota/presence", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.err) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

// The account grant answers before the host grant, so a client that is not
// granted the account learns nothing about its host line.
func TestQuotaPresenceAccountGrantBeforeHost(t *testing.T) {
	h := Server{
		Tokens:        Tokens{"listed": "token-l"},
		QuotaAccounts: QuotaAccounts{"listed": {"codex:0123456789abcdef": true}},
		HostGrants:    HostGrants{"listed": {"host-a": true}},
	}.Handler()
	now := time.Now().UTC().Format(time.RFC3339)
	r := httptest.NewRequest("PUT", "/v1/quota/presence", strings.NewReader(`{"host_id":"host-c","account_key":"claude:0123456789abcdef","last_seen":"`+now+`"}`))
	r.Header.Set("Authorization", "Bearer token-l")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "quota_account_forbidden") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// The accepted paths reach the store, so they run against Postgres in CI: a
// listed host is stored, a client with no line is stored and warned about
// (once per route), and an unlisted host leaves no row.
func TestQuotaPresenceHostGrantsPostgres(t *testing.T) {
	_, st := relaneAPIServer(t)
	const key = "codex:0123456789abcdef"
	srv := httptest.NewServer(Server{
		Service: Service{Store: st},
		Tokens:  Tokens{"hosts-listed": "token-l", "hosts-nolines": "token-n"},
		QuotaAccounts: QuotaAccounts{
			"hosts-listed":  {key: true},
			"hosts-nolines": {key: true},
		},
		HostGrants: HostGrants{"hosts-listed": {"host-a": true}},
	}.Handler())
	t.Cleanup(srv.Close)
	noHostLineWarned.Delete("hosts-nolines\x00quota_presence")
	logs := captureLog(t)
	put := func(token, host string) int {
		t.Helper()
		body := `{"host_id":"` + host + `","account_key":"` + key + `","last_seen":"` + time.Now().UTC().Add(-time.Minute).Format(time.RFC3339) + `"}`
		req, err := http.NewRequest("PUT", srv.URL+"/v1/quota/presence", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put("token-l", "host-a"); code != http.StatusOK {
		t.Fatalf("listed host: %d", code)
	}
	if code := put("token-l", "host-z"); code != http.StatusForbidden {
		t.Fatalf("unlisted host: %d", code)
	}
	if strings.Contains(logs.String(), "api.host_grants_missing") {
		t.Fatalf("a client with a line was warned about: %q", logs)
	}
	if code := put("token-n", "host-n1"); code != http.StatusOK {
		t.Fatalf("no line, first: %d", code)
	}
	if code := put("token-n", "host-n2"); code != http.StatusOK {
		t.Fatalf("no line, second: %d", code)
	}
	if n := strings.Count(logs.String(), "api.host_grants_missing client=hosts-nolines route=quota_presence host=host-n1"); n != 1 {
		t.Fatalf("want one warning for the client, got %d: %q", n, logs)
	}
	rows, _, err := st.QuotaPresence(t.Context(), "", key, quotaPresenceLimit)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.HostID] = true
	}
	if len(got) != 3 || !got["host-a"] || !got["host-n1"] || !got["host-n2"] || got["host-z"] {
		t.Fatalf("stored hosts = %v", got)
	}
}
