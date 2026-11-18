package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func orderAPIServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	_, dbURL := testdb.Schema(t, "task_orders_api")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"operator": "op-tok", "lane-session": "lane-tok", "operator-desk": "desk-tok"}}.Handler())
	t.Cleanup(h.Close)
	t.Cleanup(st.Close)
	return h, st
}

// TestTaskOrderRoutesAPI covers the /v1 order routes: only the operator bearer
// writes or withdraws, any client acknowledges, every refusal carries its code,
// and a replay answers 200 with the first order.
func TestTaskOrderRoutesAPI(t *testing.T) {
	h, st := orderAPIServer(t)
	task := relaneAPITask(t, st, relaneLane(t, "orders-api"), "order over http")
	path := fmt.Sprintf("/v1/tasks/%d/orders", task.ID)
	body := map[string]any{"words": "진행시켜", "idempotency_key": "api-1"}

	for _, tok := range []string{"lane-tok", "desk-tok", ""} {
		code, out := projectRequest(t, h.URL, http.MethodPost, path, tok, body)
		want, errCode := http.StatusForbidden, "operator_required"
		if tok == "" {
			want, errCode = http.StatusUnauthorized, ""
		}
		if code != want || (errCode != "" && out["error"] != errCode) {
			t.Fatalf("token %q: code=%d body=%v, want %d %s", tok, code, out, want, errCode)
		}
	}
	if orders, err := st.ListTaskOrders(context.Background(), task.ID, false); err != nil || len(orders) != 0 {
		t.Fatalf("a refused caller left orders: %+v err=%v", orders, err)
	}

	code, out := projectRequest(t, h.URL, http.MethodPost, path, "op-tok", body)
	order, _ := out["order"].(map[string]any)
	if code != http.StatusCreated || order["verified"] != true || order["channel"] != "cli" || order["actor"] != "operator" || out["duplicate"] != false {
		t.Fatalf("create: code=%d body=%v", code, out)
	}
	id, _ := order["id"].(string)
	code, out = projectRequest(t, h.URL, http.MethodPost, path, "op-tok", body)
	if again, _ := out["order"].(map[string]any); code != http.StatusOK || again["id"] != id || out["duplicate"] != true {
		t.Fatalf("replay: code=%d body=%v", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, path, "op-tok", map[string]any{"words": "다른 말", "idempotency_key": "api-1"})
	if code != http.StatusConflict || out["error"] != "order_key_conflict" {
		t.Fatalf("other words: code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPost, path, "op-tok", map[string]any{"words": "진행시켜"}); code != http.StatusBadRequest {
		t.Fatalf("no idempotency key: code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPost, path, "op-tok", map[string]any{"words": "x", "idempotency_key": "k", "channel": "dots"}); code != http.StatusBadRequest {
		t.Fatalf("a client-chosen channel was accepted: code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/999999999/orders", "op-tok", body); code != http.StatusNotFound || out["error"] != "not_found" {
		t.Fatalf("missing task: code=%d body=%v", code, out)
	}

	// The open list carries the order and what it waits on.
	code, out = projectRequest(t, h.URL, http.MethodGet, path+"?open=1", "lane-tok", nil)
	list, _ := out["orders"].([]any)
	if code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["waits_on_lane"] == "" {
		t.Fatalf("open list: code=%d body=%v", code, out)
	}

	// Any client acknowledges; only the operator withdraws; the wait clears once.
	ackPath := "/v1/task-orders/" + id + "/ack"
	if code, out = projectRequest(t, h.URL, http.MethodPost, ackPath, "lane-tok", map[string]any{"comment": ""}); code != http.StatusBadRequest {
		t.Fatalf("ack without a comment: code=%d body=%v", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, ackPath, "lane-tok", map[string]any{"comment": "읽었음"})
	acked, _ := out["order"].(map[string]any)
	if code != http.StatusOK || acked["acked_by"] != "lane-session" || acked["open"] != false || acked["waits_on_lane"] != "" {
		t.Fatalf("ack: code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/task-orders/"+id+"/withdraw", "lane-tok", map[string]any{"reason": "x"}); code != http.StatusForbidden || out["error"] != "operator_required" {
		t.Fatalf("withdraw by a lane: code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/task-orders/"+id+"/withdraw", "op-tok", map[string]any{"reason": "late"}); code != http.StatusConflict || out["error"] != "order_not_open" {
		t.Fatalf("withdraw of an acked order: code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/task-orders/ord-aaaaaaaaaa/ack", "lane-tok", map[string]any{"comment": "x"}); code != http.StatusNotFound || out["error"] != "not_found" {
		t.Fatalf("unknown order: code=%d body=%v", code, out)
	}
}

// TestTaskOrderStartedTaskAPI: a task past backlog is refused with the state
// as the reason, and nothing is recorded.
func TestTaskOrderStartedTaskAPI(t *testing.T) {
	h, st := orderAPIServer(t)
	task := relaneAPITask(t, st, relaneLane(t, "orders-api-started"), "already going")
	if _, err := st.ClaimTask(context.Background(), task.ID, "worker", "", "orders api test"); err != nil {
		t.Fatal(err)
	}
	code, out := projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/orders", task.ID), "op-tok", map[string]any{"words": "go", "idempotency_key": "k-started"})
	if code != http.StatusConflict || out["error"] != "order_task_started" || out["reason"] == nil {
		t.Fatalf("code=%d body=%v", code, out)
	}
	// A withdraw by the operator posts its own notice.
	open := relaneAPITask(t, st, relaneLane(t, "orders-api-withdraw"), "to withdraw")
	_, made := projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/orders", open.ID), "op-tok", map[string]any{"words": "go", "idempotency_key": "k-w"})
	id := made["order"].(map[string]any)["id"].(string)
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/task-orders/"+id+"/withdraw", "op-tok", map[string]any{"reason": "no"})
	got, _ := out["order"].(map[string]any)
	if code != http.StatusOK || got["withdrawn_by"] != "operator" || got["open"] != false {
		t.Fatalf("withdraw: code=%d body=%v", code, out)
	}
}
