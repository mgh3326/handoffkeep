package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestMarkNotificationAppliedCarriesAuthHeaders(t *testing.T) {
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/assistant/outbox/applied" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.NotificationOutbox{ID: 1, EventID: "dr-1-1-answered"})
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	if _, err := c.MarkNotificationApplied(context.Background(), "dr-1-1-answered"); err != nil {
		t.Fatal(err)
	}
	got := hk.requests()
	if len(got) != 1 {
		t.Fatalf("requests=%d want 1", len(got))
	}
	if h := got[0]; h.id != cfFixtureID || h.secret != cfFixtureSecret || h.auth != "Bearer fixture-token" || h.ua != UserAgent {
		t.Fatalf("request headers=%+v", h)
	}
}

func TestMarkNotificationAppliedScrubsRedirectErrors(t *testing.T) {
	const loginURL = "https://team-one.cloudflareaccess.com/cdn-cgi/access/login?redirect_url=signed"
	hk := newRequestRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loginURL)
		w.WriteHeader(http.StatusFound)
	})
	c := Client{URL: hk.URL, Token: "fixture-token", CFAccessClientID: cfFixtureID, CFAccessClientSecret: cfFixtureSecret}
	_, err := c.MarkNotificationApplied(context.Background(), "dr-1-1-answered")
	if err == nil || !strings.Contains(err.Error(), "cf_access_login_redirect") {
		t.Fatalf("err=%v want cf_access_login_redirect", err)
	}
	for _, leak := range []string{cfFixtureID, cfFixtureSecret, "fixture-token", loginURL, "redirect_url"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
}
