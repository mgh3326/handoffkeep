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

func TestTaskPriorityRoute(t *testing.T) {
	ctx := context.Background()
	_, url := testdb.Schema(t, "ui_priority")
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "experiment"
	task, err := st.CreateTask(ctx, store.Task{Lane: "ui-priority", Title: "priority", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st, csrfKey: []byte("test-key")}
	person := cfaccess.Identity{Email: "test@example.com"}
	call := func(identity cfaccess.Identity, origin, body string, csrf bool) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://example.com/ui/api/tasks/%d/priority", task.ID), strings.NewReader(body))
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
	body := `{"priority":2,"event_key":"one"}`
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
		{person, "http://example.com", `{"priority":2,"event_key":"one","extra":true}`, true, 400, "invalid_request"},
	} {
		status, got := call(c.who, c.origin, c.body, c.csrf)
		if status != c.code || got["error"] != c.errorCode {
			t.Fatalf("guard: %d %v", status, got)
		}
	}
	status, first := call(person, "http://example.com", body, true)
	if status != 200 {
		t.Fatalf("first: %d %v", status, first)
	}
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://example.com/ui/api/board/tasks/%d", task.ID), nil)
	w := httptest.NewRecorder()
	h.serveAPI(w, r, person)
	var detail struct {
		Events []struct {
			Field     string `json:"field"`
			FromValue string `json:"from_value"`
			ToValue   string `json:"to_value"`
		} `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != http.StatusOK {
		t.Fatalf("detail: %d %s %v", w.Code, w.Body.String(), err)
	}
	var found bool
	for _, event := range detail.Events {
		if event.Field == "priority" && event.FromValue == "0" && event.ToValue == "2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("priority values absent from detail: %s", w.Body.String())
	}
	status, replay := call(person, "http://example.com", `{"priority":4,"event_key":"one"}`, true)
	if status != 200 || replay["priority"] != first["priority"] || replay["previous"] != first["previous"] {
		t.Fatalf("replay: %d %v", status, replay)
	}
}
