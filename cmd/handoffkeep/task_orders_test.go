package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type orderCall struct {
	method, path string
	body         map[string]any
}

func orderServer(t *testing.T, calls *[]orderCall, reply func(path string) (int, any)) *httptest.Server {
	t.Helper()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*calls = append(*calls, orderCall{r.Method, r.URL.RequestURI(), body})
		status, out := reply(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(h.Close)
	return h
}

func TestTaskOrderCLI(t *testing.T) {
	var calls []orderCall
	h := orderServer(t, &calls, func(string) (int, any) {
		return http.StatusCreated, map[string]any{"order": map[string]any{"id": "ord-aaaaaaaaaa", "open": true}, "duplicate": false}
	})
	var out bytes.Buffer
	args := []string{"tasks", "order", "42", "--words", "진행시켜", "--idempotency-key", "k1", "--url", h.URL, "--token", "tok"}
	if err := run(args, &out, io.Discard); err != nil {
		t.Fatalf("order: %v", err)
	}
	if len(calls) != 1 || calls[0].method != "POST" || calls[0].path != "/v1/tasks/42/orders" || calls[0].body["words"] != "진행시켜" || calls[0].body["idempotency_key"] != "k1" {
		t.Fatalf("calls=%+v", calls)
	}
	if !strings.Contains(out.String(), `"id":"ord-aaaaaaaaaa"`) {
		t.Fatalf("output %s lacks the order", out.String())
	}
	// Flags and the positional come in any order; the words and the key are required.
	reordered := []string{"tasks", "order", "--words", "go", "--idempotency-key", "k2", "42", "--url", h.URL, "--token", "tok"}
	if err := run(reordered, io.Discard, io.Discard); err != nil || len(calls) != 2 {
		t.Fatalf("reordered: err=%v calls=%d", err, len(calls))
	}
	for _, bad := range [][]string{
		{"tasks", "order", "42", "--idempotency-key", "k"},
		{"tasks", "order", "42", "--words", "go"},
		{"tasks", "order", "--words", "go", "--idempotency-key", "k"},
		{"tasks", "order", "0", "--words", "go", "--idempotency-key", "k"},
	} {
		if err := run(append(bad, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("a refused command reached the server: %+v", calls)
	}
}

func TestTaskOrderAckAndWithdrawCLI(t *testing.T) {
	var calls []orderCall
	h := orderServer(t, &calls, func(string) (int, any) {
		return http.StatusOK, map[string]any{"order": map[string]any{"id": "ord-aaaaaaaaaa"}, "duplicate": false}
	})
	if err := run([]string{"tasks", "order-ack", "ord-aaaaaaaaaa", "--comment", "읽었음", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := run([]string{"tasks", "order-withdraw", "ord-aaaaaaaaaa", "--reason", "취소", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if len(calls) != 2 || calls[0].path != "/v1/task-orders/ord-aaaaaaaaaa/ack" || calls[0].body["comment"] != "읽었음" ||
		calls[1].path != "/v1/task-orders/ord-aaaaaaaaaa/withdraw" || calls[1].body["reason"] != "취소" {
		t.Fatalf("calls=%+v", calls)
	}
	for _, bad := range [][]string{
		{"tasks", "order-ack", "ord-aaaaaaaaaa"},
		{"tasks", "order-ack", "--comment", "c"},
		{"tasks", "order-withdraw", "ord-aaaaaaaaaa"},
	} {
		if err := run(append(bad, "--url", h.URL, "--token", "tok"), io.Discard, io.Discard); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	// A server refusal reaches the caller with its code.
	refuse := orderServer(t, &calls, func(string) (int, any) { return http.StatusForbidden, map[string]string{"error": "operator_required"} })
	err := run([]string{"tasks", "order", "42", "--words", "go", "--idempotency-key", "k", "--url", refuse.URL, "--token", "tok"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "operator_required") {
		t.Fatalf("refusal: err=%v", err)
	}
}

func TestTasksShowListsOpenOrders(t *testing.T) {
	var calls []orderCall
	h := orderServer(t, &calls, func(path string) (int, any) {
		if strings.HasSuffix(path, "/orders") {
			return http.StatusOK, map[string]any{"orders": []map[string]any{{"id": "ord-aaaaaaaaaa", "waits_on_lane": "handoffkeep-design", "open": true}}}
		}
		return http.StatusOK, map[string]any{"id": 42, "title": "t", "state": "backlog"}
	})
	var out bytes.Buffer
	if err := run([]string{"tasks", "show", "42", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatalf("show: %v", err)
	}
	var shown struct {
		ID         int64            `json:"id"`
		OpenOrders []map[string]any `json:"open_orders"`
	}
	if err := json.Unmarshal(out.Bytes(), &shown); err != nil || shown.ID != 42 || len(shown.OpenOrders) != 1 || shown.OpenOrders[0]["waits_on_lane"] != "handoffkeep-design" {
		t.Fatalf("show output %s err=%v", out.String(), err)
	}
	if last := calls[len(calls)-1]; last.path != "/v1/tasks/42/orders?open=1" {
		t.Fatalf("orders were not listed open-only: %+v", last)
	}
	// A server without the orders route reads as no orders, not as a failure.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/orders") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"title":"t","state":"backlog"}`))
	}))
	defer old.Close()
	out.Reset()
	if err := run([]string{"tasks", "show", "42", "--url", old.URL, "--token", "tok"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), `"open_orders":[]`) {
		t.Fatalf("old server: err=%v out=%s", err, out.String())
	}
}
