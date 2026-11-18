package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetTaskCloseRuleWireShape(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		rule, _ := body["close_rule"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "close_rule": rule})
	}))
	defer srv.Close()

	c := Client{URL: srv.URL, Token: "tok"}
	x, err := c.SetTaskCloseRule(context.Background(), 42, "explicit", "programme task")
	if err != nil || x.CloseRule != "explicit" || path != "/v1/tasks/42/close-rule" || body["close_rule"] != "explicit" || body["note"] != "programme task" {
		t.Fatalf("set close rule: x=%+v err=%v path=%s body=%v", x, err, path, body)
	}
}

// A server that answers a different rule than the one sent is refused, not
// reported as applied.
func TestSetTaskCloseRuleRefusesAnotherRule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "close_rule": "auto"})
	}))
	defer srv.Close()

	c := Client{URL: srv.URL, Token: "tok"}
	if _, err := c.SetTaskCloseRule(context.Background(), 42, "explicit", ""); err == nil {
		t.Fatal("a server answering auto for explicit was reported as applied")
	}
}
