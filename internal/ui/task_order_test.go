package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestTaskOrderRoute: the console action 진행시키기 records a verified order
// from an Access operator only, replays on the same event key, refuses a task
// that has started, and writes the order's notice in the same call.
func TestTaskOrderRoute(t *testing.T) {
	ctx := context.Background()
	_, url := testdb.Schema(t, "ui_order")
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "experiment"
	task, err := st.CreateTask(ctx, store.Task{Lane: "ui-order", Title: "order me", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st, csrfKey: []byte("test-key")}
	person := cfaccess.Identity{Email: "test@example.com"}
	call := func(id int64, identity cfaccess.Identity, origin, body string, csrf bool) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://example.com/ui/api/tasks/%d/order", id), strings.NewReader(body))
		r.Header.Set("Origin", origin)
		if csrf {
			token, e := h.newCSRFToken(identity.Email)
			if e != nil {
				t.Fatal(e)
			}
			r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
			r.Header.Set("X-CSRF-Token", token)
		}
		w := httptest.NewRecorder()
		h.serveAPI(w, r, identity)
		var result map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		return w.Code, result
	}
	body := `{"words":"진행시켜","event_key":"console-1"}`
	for _, c := range []struct {
		who          cfaccess.Identity
		origin, body string
		csrf         bool
		code         int
		errorCode    string
	}{
		{cfaccess.Identity{ServiceName: "service"}, "http://example.com", body, false, 403, "operator_only"},
		{person, "http://evil.example", body, true, 403, "origin_rejected"},
		{person, "http://example.com", body, false, 403, "csrf_rejected"},
		{person, "http://example.com", `{"words":"go","event_key":"k","channel":"dots"}`, true, 400, "invalid_request"},
		{person, "http://example.com", `{"words":"go"}`, true, 400, "event_key_required"},
		{person, "http://example.com", `{"words":"  ","event_key":"k-blank"}`, true, 400, "invalid_request"},
	} {
		status, got := call(task.ID, c.who, c.origin, c.body, c.csrf)
		if status != c.code || got["error"] != c.errorCode {
			t.Fatalf("guard %+v: %d %v", c, status, got)
		}
	}
	if orders, err := st.ListTaskOrders(ctx, task.ID, false); err != nil || len(orders) != 0 {
		t.Fatalf("a refused call left orders: %+v err=%v", orders, err)
	}

	status, first := call(task.ID, person, "http://example.com", body, true)
	order, _ := first["order"].(map[string]any)
	if status != 200 || order["channel"] != "console" || order["verified"] != true || order["actor"] != "operator:test@example.com" || order["words"] != "진행시켜" || first["duplicate"] != false {
		t.Fatalf("order: %d %v", status, first)
	}
	status, again := call(task.ID, person, "http://example.com", body, true)
	if replay, _ := again["order"].(map[string]any); status != 200 || replay["id"] != order["id"] || again["duplicate"] != true {
		t.Fatalf("replay: %d %v", status, again)
	}
	status, got := call(task.ID, person, "http://example.com", `{"words":"다른 말","event_key":"console-1"}`, true)
	if status != 409 || got["error"] != "order_key_conflict" {
		t.Fatalf("same key, other words: %d %v", status, got)
	}
	if status, got = call(1<<40, person, "http://example.com", `{"words":"go","event_key":"k-missing"}`, true); status != 404 || got["error"] != "not_found" {
		t.Fatalf("missing task: %d %v", status, got)
	}
	// A task that has started is refused.
	if _, err := st.ClaimTask(ctx, task.ID, "worker", "", "ui order test"); err != nil {
		t.Fatal(err)
	}
	if status, got = call(task.ID, person, "http://example.com", `{"words":"go","event_key":"k-started"}`, true); status != 409 || got["error"] != "order_task_started" {
		t.Fatalf("started task: %d %v", status, got)
	}
	if orders, err := st.ListTaskOrders(ctx, task.ID, false); err != nil || len(orders) != 1 {
		t.Fatalf("orders after the console calls: %+v err=%v", orders, err)
	}
}
