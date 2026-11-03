package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLinearManualCommandsUseAuthenticatedEndpoints(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fake-token" {
			t.Error(r.Method, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/v1/linear/metadata/refresh":
			if len(body) != 0 {
				t.Error(body)
			}
			fmt.Fprint(w, `{"status":"refreshed"}`)
		case "/v1/linear/board/reconcile":
			if body["task_id"] != float64(42) {
				t.Error(body)
			}
			fmt.Fprint(w, `{"status":"awaiting_webhook"}`)
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("HANDOFFKEEP_URL", srv.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fake-token")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", "fake-id")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", "fake-secret")
	for _, args := range [][]string{{"metadata", "refresh"}, {"board", "reconcile", "--task", "42"}} {
		var out bytes.Buffer
		if err := linearCmd(args, &out); err != nil {
			t.Fatal(err)
		}
		var result map[string]string
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["status"] == "" {
			t.Fatal(out.String(), err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
