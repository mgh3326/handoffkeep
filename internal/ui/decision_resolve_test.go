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

// TestConsoleDecisionResolve covers POST /ui/api/decision-requests/resolve:
// only an Access email identity answers (operator_direct), behind the origin
// and CSRF guards, and a re-submitted event_key replays (hk 3061).
func TestConsoleDecisionResolve(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "console_decision")
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
	h := &Handler{store: st, csrfKey: []byte("console-decision-key")}
	email := cfaccess.Identity{Email: "robin@example.com"}

	newRequest := func(t *testing.T, humanOnly bool) (store.Task, store.DecisionRequest) {
		t.Helper()
		project := "experiment"
		task, err := st.CreateTask(ctx, store.Task{Lane: "console-dr-" + time.Now().Format("150405.000000000"), Title: "console decision", Kind: "implement", CreatedBy: "console-test", Project: &project})
		if err != nil {
			t.Fatal(err)
		}
		if task, err = st.ClaimTask(ctx, task.ID, "console-test", "job-1", ""); err != nil {
			t.Fatal(err)
		}
		got, err := st.RecordDecisionRequest(ctx, task.ID, "console-test", store.DecisionRequestInput{
			Question: "어느 쪽으로?",
			Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{
				{Key: "A", Label: "first", Recommended: true}, {Key: "B", Label: "second"},
			}},
			DefaultAction: "자동 적용 없음",
			HumanOnly:     humanOnly,
		})
		if err != nil {
			t.Fatal(err)
		}
		return task, got.Request
	}
	// call posts body as identity. csrf and origin control the guards.
	call := func(t *testing.T, identity cfaccess.Identity, origin, body string, csrf bool) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "http://example.com/ui/api/decision-requests/resolve", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if csrf {
			token, err := h.newCSRFToken(identity.Email)
			if err != nil {
				t.Fatal(err)
			}
			r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
			r.Header.Set("X-CSRF-Token", token)
		}
		w := httptest.NewRecorder()
		h.serveAPI(w, r, identity)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	good := func(requestID, key string) string {
		raw, _ := json.Marshal(map[string]string{"request_id": requestID, "option": "A", "event_key": key})
		return string(raw)
	}

	t.Run("guards", func(t *testing.T) {
		task, request := newRequest(t, false)
		body := good(request.ID, "guard-1")
		if s, b := call(t, cfaccess.Identity{ServiceName: "glance-fixture"}, "http://example.com", body, false); s != http.StatusForbidden || b["error"] != "operator_only" {
			t.Fatalf("service identity: %d %v", s, b)
		}
		if s, _ := call(t, email, "http://evil.example", body, true); s != http.StatusForbidden {
			t.Fatalf("bad origin: %d", s)
		}
		if s, _ := call(t, email, "", body, true); s != http.StatusForbidden {
			t.Fatalf("missing origin: %d", s)
		}
		if s, _ := call(t, email, "http://example.com", body, false); s != http.StatusForbidden {
			t.Fatalf("missing csrf: %d", s)
		}
		got, _, _ := st.GetTask(ctx, task.ID)
		if got.Refs.DecisionRequest.Status != store.DecisionRequestOpen {
			t.Fatalf("a refused call closed the request: %+v", got.Refs.DecisionRequest)
		}
	})

	t.Run("body", func(t *testing.T) {
		_, request := newRequest(t, false)
		for name, body := range map[string]string{
			"unknown field":  `{"request_id":"` + request.ID + `","option":"A","event_key":"b-1","kind":"withdrawn"}`,
			"trailing data":  good(request.ID, "b-2") + `{}`,
			"not json":       `option=A`,
			"missing key":    `{"request_id":"` + request.ID + `","option":"A"}`,
			"too large body": `{"request_id":"` + request.ID + `","text":"` + strings.Repeat("x", decisionResolveBodyLimit) + `","event_key":"b-3"}`,
		} {
			if s, b := call(t, email, "http://example.com", body, true); s != http.StatusBadRequest {
				t.Fatalf("%s: %d %v", name, s, b)
			}
		}
		if s, b := call(t, email, "http://example.com", `{"request_id":"`+request.ID+`","option":"A"}`, true); b["error"] != "event_key_required" {
			t.Fatalf("missing event_key: %d %v", s, b)
		}
	})

	t.Run("answer_and_replay", func(t *testing.T) {
		task, request := newRequest(t, false)
		s, b := call(t, email, "http://example.com", good(request.ID, "click-1"), true)
		if s != http.StatusOK || b["result"] != "answered" || b["duplicate"] == true {
			t.Fatalf("answer: %d %v", s, b)
		}
		resolutionID := b["resolution_id"]
		var provenance, channel, recorder string
		if err := conn.QueryRow(ctx, `SELECT provenance,channel,recorded_by FROM decision_resolutions WHERE resolution_id=$1`, int64(resolutionID.(float64))).Scan(&provenance, &channel, &recorder); err != nil {
			t.Fatal(err)
		}
		if provenance != "operator_direct" || channel != "console" || recorder != "operator:robin@example.com" {
			t.Fatalf("provenance=%s channel=%s recorded_by=%s", provenance, channel, recorder)
		}
		s, b = call(t, email, "http://example.com", good(request.ID, "click-1"), true)
		if s != http.StatusOK || b["result"] != "answered" || b["duplicate"] != true || b["resolution_id"] != resolutionID {
			t.Fatalf("replay: %d %v", s, b)
		}
		var attempts int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM decision_answer_attempts WHERE task_id=$1`, task.ID).Scan(&attempts); err != nil || attempts != 1 {
			t.Fatalf("attempts=%d err=%v", attempts, err)
		}
		other := strings.Replace(good(request.ID, "click-1"), `"A"`, `"B"`, 1)
		if s, b = call(t, email, "http://example.com", other, true); s != http.StatusConflict || b["error"] != "decision_event_key_conflict" {
			t.Fatalf("same key, other answer: %d %v", s, b)
		}
		if s, b = call(t, email, "http://example.com", strings.Replace(good(request.ID, "click-2"), `"A"`, `"B"`, 1), true); s != http.StatusConflict || b["error"] != "decision_request_resolved" || b["result"] != "conflict" {
			t.Fatalf("another answer: %d %v", s, b)
		}
	})

	t.Run("human_only_is_answered_by_the_console", func(t *testing.T) {
		_, request := newRequest(t, true)
		if s, b := call(t, email, "http://example.com", good(request.ID, "human-1"), true); s != http.StatusOK || b["result"] != "answered" {
			t.Fatalf("human_only console answer: %d %v", s, b)
		}
	})

	t.Run("unknown_request", func(t *testing.T) {
		if s, b := call(t, email, "http://example.com", good("dr-999999999-1", "none-1"), true); s != http.StatusNotFound || b["error"] != "not_found" {
			t.Fatalf("unknown task: %d %v", s, b)
		}
		if s, _ := call(t, email, "http://example.com", good("not-a-request", "none-2"), true); s != http.StatusBadRequest {
			t.Fatalf("malformed id: %d", s)
		}
	})
}
