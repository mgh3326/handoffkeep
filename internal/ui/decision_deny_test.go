package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// postDecisionDeny posts body to /ui/api/decision-requests/deny as identity.
// csrf "" sends no token, "valid" a matching one, anything else a header
// that does not match its cookie.
func postDecisionDeny(t *testing.T, h *Handler, identity cfaccess.Identity, origin, body, csrf string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://example.com/ui/api/decision-requests/deny", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if csrf != "" {
		token, err := h.newCSRFToken(identity.Email)
		if err != nil {
			t.Fatal(err)
		}
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
		if csrf != "valid" {
			token = csrf
		}
		r.Header.Set("X-CSRF-Token", token)
	}
	w := httptest.NewRecorder()
	h.serveAPI(w, r, identity)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestDecisionDenyGuards: the deny route refuses before it reaches the
// store, with the same guards as /resolve.
func TestDecisionDenyGuards(t *testing.T) {
	h := &Handler{csrfKey: []byte("console-deny-key")}
	email := cfaccess.Identity{Email: "robin@example.com"}
	body := `{"request_id":"dr-1-1","attempt_id":1,"receipt":"wrong relay","event_key":"deny-1"}`
	for _, c := range []struct {
		name         string
		identity     cfaccess.Identity
		origin, body string
		csrf         string
		wantStatus   int
		wantCode     string
	}{
		{"service identity", cfaccess.Identity{ServiceName: "glance-fixture"}, "http://example.com", body, "", http.StatusForbidden, "operator_only"},
		{"cross origin", email, "http://evil.example", body, "valid", http.StatusForbidden, "origin_rejected"},
		{"bad csrf", email, "http://example.com", body, "not-the-token", http.StatusForbidden, "csrf_rejected"},
		{"missing event_key", email, "http://example.com", `{"request_id":"dr-1-1","attempt_id":1}`, "valid", http.StatusBadRequest, "event_key_required"},
		{"unknown field", email, "http://example.com", `{"request_id":"dr-1-1","attempt_id":1,"event_key":"deny-2","option":"A"}`, "valid", http.StatusBadRequest, "invalid_request"},
	} {
		if s, b := postDecisionDeny(t, h, c.identity, c.origin, c.body, c.csrf); s != c.wantStatus || b["error"] != c.wantCode {
			t.Errorf("%s: %d %v, want %d %s", c.name, s, b, c.wantStatus, c.wantCode)
		}
	}
}

// TestDecisionDenyRecordsDenied: a relayed answer to a human_only request
// waits as pending_confirmation; the console deny answers 200 denied and
// leaves the request open.
func TestDecisionDenyRecordsDenied(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "console_deny")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	h := &Handler{store: st, csrfKey: []byte("console-deny-key")}
	email := cfaccess.Identity{Email: "robin@example.com"}

	project := "experiment"
	task, err := st.CreateTask(ctx, store.Task{Lane: "console-deny-" + time.Now().Format("150405.000000000"), Title: "console deny", Kind: "implement", CreatedBy: "console-test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	if task, err = st.ClaimTask(ctx, task.ID, "console-test", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	got, err := st.RecordDecisionRequest(ctx, task.ID, "console-test", store.DecisionRequestInput{
		Question:      "배포할까?",
		Options:       store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "yes"}, {Key: "B", Label: "no"}}},
		DefaultAction: "자동 적용 없음",
		HumanOnly:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := st.ResolveDecision(ctx, store.ResolveDecisionInput{RequestID: got.Request.ID, Kind: store.DecisionRequestAnswered, Option: "A", EventKey: "relay-1", Channel: store.DecisionChannelCLI, Actor: "session-a"})
	if err != nil || relay.Result != store.DecisionResultPendingConfirmation {
		t.Fatalf("relay: %+v %v", relay, err)
	}

	raw, _ := json.Marshal(map[string]any{"request_id": got.Request.ID, "attempt_id": relay.AttemptID, "receipt": "I never said A", "event_key": "deny-1"})
	s, b := postDecisionDeny(t, h, email, "http://example.com", string(raw), "valid")
	if s != http.StatusOK || b["result"] != "denied" {
		t.Fatalf("deny: %d %v", s, b)
	}
	var denies int64
	var receipt, actor string
	if err := conn.QueryRow(ctx, `SELECT denies_attempt_id,receipt,actor FROM decision_answer_attempts WHERE id=$1`, int64(b["attempt_id"].(float64))).Scan(&denies, &receipt, &actor); err != nil {
		t.Fatal(err)
	}
	if denies != relay.AttemptID || receipt != "I never said A" || actor != "operator:robin@example.com" {
		t.Fatalf("denied row: denies=%d receipt=%q actor=%q", denies, receipt, actor)
	}
	after, _, _ := st.GetTask(ctx, task.ID)
	if after.Refs.DecisionRequest.Status != store.DecisionRequestOpen {
		t.Fatalf("deny closed the request: %+v", after.Refs.DecisionRequest)
	}
	raw, _ = json.Marshal(map[string]any{"request_id": got.Request.ID, "attempt_id": relay.AttemptID, "event_key": "deny-2"})
	if s, b = postDecisionDeny(t, h, email, "http://example.com", string(raw), "valid"); s != http.StatusConflict || b["error"] != "decision_attempt_denied" {
		t.Fatalf("second deny: %d %v", s, b)
	}
}
