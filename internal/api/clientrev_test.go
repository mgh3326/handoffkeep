package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestClientRevisionFromUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		"handoffkeep/0123456789abcdef0123456789abcdef01234567": "0123456789abcdef0123456789abcdef01234567",
		"handoffkeep/abc1234":                    "abc1234",
		"handoffkeep/unknown":                    "unknown",
		"handoffkeep/abc123":                     "", // too short for a commit id
		"handoffkeep/ABC1234":                    "", // vcs.revision is lowercase
		"handoffkeep/abc1234 extra":              "",
		`handoffkeep/abc1234"`:                   "",
		"handoffkeep/" + strings.Repeat("a", 65): "",
		"Go-http-client/1.1":                     "",
		"":                                       "",
	} {
		got, ok := clientRevisionFromUserAgent(ua)
		if got != want || ok != (want != "") {
			t.Errorf("%q -> %q,%v want %q", ua, got, ok, want)
		}
	}
}

// An authenticated CLI request records its revision; GET /metrics reports the
// last one per client; the log line appears only when a client's revision
// changes. Unauthenticated requests and foreign user agents record nothing.
func TestClientRevisionsOnMetrics(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	h := Server{Tokens: Tokens{"node-a": "token-a", "node-b": "token-b"}, ClientRevisions: NewClientRevisions()}.Handler()
	get := func(token, ua string) string {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	get("", "handoffkeep/1111111")       // unauthenticated: not recorded
	get("token-b", "Go-http-client/1.1") // not an hk CLI: not recorded
	get("token-a", "handoffkeep/aaaaaaa")
	get("token-a", "handoffkeep/aaaaaaa")
	body := get("token-a", "handoffkeep/bbbbbbb")
	if !strings.Contains(body, `handoffkeep_client_revision_info{client="node-a",rev="bbbbbbb"} 1`+"\n") ||
		!strings.Contains(body, `handoffkeep_client_last_seen_seconds{client="node-a"} `) {
		t.Fatalf("metrics lack node-a revision:\n%s", body)
	}
	if strings.Contains(body, "node-b") || strings.Contains(body, "1111111") || strings.Contains(body, "aaaaaaa") {
		t.Fatalf("metrics report an unrecorded or stale revision:\n%s", body)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], `api.client_revision client="node-a" rev=aaaaaaa prev=none`) ||
		!strings.HasSuffix(lines[1], `api.client_revision client="node-a" rev=bbbbbbb prev=aaaaaaa`) {
		t.Fatalf("log lines:\n%s", logs.String())
	}
}

func TestPromLabelEscapes(t *testing.T) {
	if got := promLabel("a\\b\"c\nd"); got != `"a\\b\"c\nd"` {
		t.Fatalf("promLabel=%s", got)
	}
}
