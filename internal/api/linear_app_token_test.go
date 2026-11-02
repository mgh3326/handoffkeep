package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestLinearAppTokenMintsOnceAndRenewsBeforeExpiry(t *testing.T) {
	var mints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != "id" || r.Form.Get("client_secret") != "secret" {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		n := mints.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok" + string(rune('0'+n)), "expires_in": 3 * 24 * 3600})
	}))
	defer srv.Close()
	now := time.Unix(1_800_000_000, 0)
	tok := &LinearAppToken{ClientID: "id", ClientSecret: "secret", URL: srv.URL, Now: func() time.Time { return now }}
	a, err := tok.Token(t.Context())
	if err != nil || a != "tok1" {
		t.Fatalf("first token %q %v", a, err)
	}
	if b, _ := tok.Token(t.Context()); b != "tok1" || mints.Load() != 1 {
		t.Fatalf("cached token %q after %d mints", b, mints.Load())
	}
	now = now.Add(2*24*time.Hour + time.Minute) // inside the renewal window
	if c, _ := tok.Token(t.Context()); c != "tok2" || mints.Load() != 2 {
		t.Fatalf("renewed token %q after %d mints", c, mints.Load())
	}
}

func TestLinearAPIUsesBearerAppToken(t *testing.T) {
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "app-token", "expires_in": 30 * 24 * 3600})
	}))
	defer tokens.Close()
	var auth string
	gql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":{"issue":{"identifier":"ROB-1","title":"t","url":"u","state":{"type":"started"},"labels":{"nodes":[]}}}}`))
	}))
	defer gql.Close()
	a := LinearAPI{Key: "personal-key", App: &LinearAppToken{ClientID: "id", ClientSecret: "s", URL: tokens.URL}, URL: gql.URL}
	if _, err := a.LinearIssue(t.Context(), "i"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer app-token" {
		t.Fatalf("authorization header %q, want the app token", auth)
	}
}

func TestLinearAppTokenRefusalReportsStatusOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "client_secret=secret is invalid", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := (&LinearAppToken{ClientID: "id", ClientSecret: "secret", URL: srv.URL}).Token(t.Context())
	if err == nil || err.Error() != "linear token status 401" {
		t.Fatalf("error %v", err)
	}
}
