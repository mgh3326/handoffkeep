package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// MGH-36 PR-1: the assistant methods must behave like every other client
// call — the shared call() path carries Bearer and CF-Access headers, and
// error strings never carry credentials or a redirect Location.

// assistantHK serves the four /v1/assistant routes the client can hit.
func assistantHK(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/assistant/pending":
			_ = json.NewEncoder(w).Encode(store.AssistantPending{ServerTime: time.Now().UTC()})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/assistant/outbox":
			_ = json.NewEncoder(w).Encode(map[string]any{"notifications": []store.NotificationOutbox{{ID: 1, Kind: store.OutboxKindDecisionAnswered, EventID: "dr-1-1-answered"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/assistant/outbox/sent":
			_ = json.NewEncoder(w).Encode(store.NotificationOutbox{ID: 1, EventID: "dr-1-1-answered"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/assistant/decisions/resolve":
			_ = json.NewEncoder(w).Encode(store.DecisionRequestResult{Request: store.DecisionRequest{ID: "dr-1-1"}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

func exerciseAssistant(t *testing.T, c Client) {
	t.Helper()
	ctx := context.Background()
	if _, e := c.AssistantPending(ctx); e != nil {
		t.Fatalf("AssistantPending: %v", e)
	}
	if _, e := c.ListUnsentNotifications(ctx, 0); e != nil {
		t.Fatalf("ListUnsentNotifications: %v", e)
	}
	if _, e := c.MarkNotificationSent(ctx, "dr-1-1-answered", 7); e != nil {
		t.Fatalf("MarkNotificationSent: %v", e)
	}
	if _, e := c.AssistantResolve(ctx, store.DecisionAssistantResolveInput{RequestID: "dr-1-1", Option: "A"}); e != nil {
		t.Fatalf("AssistantResolve: %v", e)
	}
}

// Every new method is a call() JSON request — the CF-Access pair and Bearer
// token ride along exactly as on the established routes.
func TestAssistantMethodsCarryCFHeaders(t *testing.T) {
	hk := newRequestRecorder(t, assistantHK(t))
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}

	exerciseAssistant(t, c)

	got := hk.requests()
	if len(got) != 4 {
		t.Fatalf("requests=%d want 4", len(got))
	}
	for i, h := range got {
		if h.id != cfFixtureID || h.secret != cfFixtureSecret {
			t.Fatalf("request %d CF headers=(%q,%q)", i, h.id, h.secret)
		}
		if h.auth != "Bearer fixture-token" {
			t.Fatalf("request %d Authorization=%q", i, h.auth)
		}
		if h.ua != UserAgent {
			t.Fatalf("request %d User-Agent=%q", i, h.ua)
		}
	}
}

// An error on a new method is scrubbed the same way: the CF login redirect
// names itself and leaks neither the token, the pair, nor the Location value.
func TestAssistantMethodsScrubRedirectErrors(t *testing.T) {
	const loginURL = "https://team-one.cloudflareaccess.com/cdn-cgi/access/login?redirect_url=signed"
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loginURL)
		w.WriteHeader(http.StatusFound)
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	ctx := context.Background()

	check := func(name string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "cf_access_login_redirect") {
			t.Fatalf("%s: err=%v want cf_access_login_redirect", name, err)
		}
		for _, leak := range []string{cfFixtureID, cfFixtureSecret, "fixture-token", loginURL, "redirect_url"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: error leaks %q: %v", name, leak, err)
			}
		}
	}
	_, e := c.AssistantPending(ctx)
	check("AssistantPending", e)
	_, e = c.ListUnsentNotifications(ctx, 0)
	check("ListUnsentNotifications", e)
	_, e = c.MarkNotificationSent(ctx, "dr-1-1-answered", 7)
	check("MarkNotificationSent", e)
	_, e = c.AssistantResolve(ctx, store.DecisionAssistantResolveInput{RequestID: "dr-1-1", Option: "A"})
	check("AssistantResolve", e)
}

// The assistant resolve body carries no responder or by — only the request
// identity and the answer travel.
func TestAssistantResolveSendsNoAttribution(t *testing.T) {
	var got map[string]any
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(store.DecisionRequestResult{Request: store.DecisionRequest{ID: "dr-1-1"}})
	})
	c := Client{URL: hk.URL, Token: "fixture-token"}
	if _, e := c.AssistantResolve(context.Background(), store.DecisionAssistantResolveInput{RequestID: "dr-1-1", Option: "A", Text: "yes"}); e != nil {
		t.Fatalf("AssistantResolve: %v", e)
	}
	if _, sent := got["responder"]; sent {
		t.Fatalf("body carries responder: %v", got)
	}
	if _, sent := got["by"]; sent {
		t.Fatalf("body carries by: %v", got)
	}
	if got["request_id"] != "dr-1-1" || got["option"] != "A" {
		t.Fatalf("body=%v", got)
	}
}
