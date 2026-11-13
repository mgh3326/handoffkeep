package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetTaskPriorityWireShape(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		prio := 0
		if p, ok := body["priority"].(float64); ok {
			prio = int(p)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "priority": prio})
	}))
	defer srv.Close()

	c := Client{URL: srv.URL, Token: "tok"}
	x, err := c.SetTaskPriority(context.Background(), 42, 3, "urgent")
	if err != nil || x.Priority != 3 || path != "/v1/tasks/42/priority" || body["priority"] != float64(3) || body["note"] != "urgent" {
		t.Fatalf("set priority: x=%+v err=%v path=%s body=%v", x, err, path, body)
	}
}
