package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// The authority switch over HTTP (hk 4122): set youtrack empties the import
// pages of that kind only and refuses an unsendable comment with 400; set
// back to hk restores both.
func TestYTAuthoritySwitchOverHTTP(t *testing.T) {
	_, dbURL := testdb.Schema(t, "yt_authority_api")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	st.SetYTCommentsMode(store.YTCommentsOn)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"operator": "op-token", "peer": "peer-token", "yt": "yt-token"},
		ImportReadGrants: map[string]bool{"yt": true}}.Handler())
	t.Cleanup(h.Close)

	do := func(method, path, token string, body any) (int, map[string]any) {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, h.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	items := func(path string) int {
		t.Helper()
		status, body := do(http.MethodGet, path, "yt-token", nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d %v", path, status, body)
		}
		list, ok := body["items"].([]any)
		if !ok {
			t.Fatalf("GET %s items = %v", path, body)
		}
		return len(list)
	}
	set := func(token string, kinds []string, side string) (int, map[string]any) {
		t.Helper()
		return do(http.MethodPost, "/v1/yt/authority", token, map[string]any{"kinds": kinds, "authority": side, "by": "operator", "note": "drill"})
	}
	comment := func(task int64, text string) (int, map[string]any) {
		t.Helper()
		return do(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/comments", task), "peer-token", map[string]any{"body": text})
	}

	task, err := st.CreateTask(t.Context(), store.Task{Lane: "yt-authority-api", Title: "authority", Kind: "implement", CreatedBy: "t", Project: &apiTestProject})
	if err != nil {
		t.Fatal(err)
	}
	const unsendable = "quotes the marker <!-- hk-op:12 --> in prose"
	// Authority hk: today's behaviour. The unsendable body commits.
	if status, body := comment(task.ID, unsendable); status != http.StatusCreated {
		t.Fatalf("hk: unsendable comment = %d %v", status, body)
	}
	if status, body := comment(task.ID, "plain"); status != http.StatusCreated {
		t.Fatalf("hk: comment = %d %v", status, body)
	}
	if n := items("/v1/import/comments"); n != 2 {
		t.Fatalf("hk: %d import comments, want 2", n)
	}
	tasks := items("/v1/import/tasks")
	if tasks < 1 {
		t.Fatalf("hk: %d import tasks", tasks)
	}

	if status, body := do(http.MethodGet, "/v1/yt/authority", "peer-token", nil); status != http.StatusOK || len(body["authority"].([]any)) != 4 {
		t.Fatalf("show = %d %v", status, body)
	}
	if status, body := set("peer-token", []string{"comments"}, "youtrack"); status != http.StatusForbidden || body["error"] != "operator_required" {
		t.Fatalf("peer set = %d %v", status, body)
	}
	if status, body := set("op-token", []string{"documents"}, "youtrack"); status != http.StatusConflict || body["error"] != "yt_authority_refused" || body["reason"] == nil {
		t.Fatalf("documents with its mode off = %d %v", status, body)
	}
	if status, body := set("op-token", []string{"nope"}, "youtrack"); status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("unknown kind = %d %v", status, body)
	}
	status, body := set("op-token", []string{"comments"}, "youtrack")
	if status != http.StatusOK {
		t.Fatalf("set comments youtrack = %d %v", status, body)
	}
	sideOf := func(rows any, kind string) string {
		for _, r := range rows.([]any) {
			if m := r.(map[string]any); m["kind"] == kind {
				return m["authority"].(string)
			}
		}
		return ""
	}
	if sideOf(body["before"], "comments") != "hk" || sideOf(body["after"], "comments") != "youtrack" || sideOf(body["after"], "tasks") != "hk" {
		t.Fatalf("set answer %v", body)
	}

	// Authority youtrack for comments: its import pages are empty, the other
	// kinds' are not, and an unsendable comment is refused.
	for _, path := range []string{"/v1/import/comments", "/v1/import/comments?task_ids=" + fmt.Sprint(task.ID), "/v1/import/comment-authors"} {
		if n := items(path); n != 0 {
			t.Fatalf("youtrack: GET %s has %d items", path, n)
		}
	}
	if n := items("/v1/import/tasks"); n != tasks {
		t.Fatalf("youtrack comments: %d import tasks, want %d", n, tasks)
	}
	if status, body := comment(task.ID, unsendable); status != http.StatusBadRequest || body["error"] != "yt_comment_unsendable" || body["reason"] == nil {
		t.Fatalf("youtrack: unsendable comment = %d %v", status, body)
	}
	if status, body := comment(task.ID, "still sendable"); status != http.StatusCreated {
		t.Fatalf("youtrack: sendable comment = %d %v", status, body)
	}

	// Rollback: set back to hk restores everything.
	if status, body := set("op-token", []string{"comments"}, "hk"); status != http.StatusOK || sideOf(body["after"], "comments") != "hk" {
		t.Fatalf("set comments hk = %d %v", status, body)
	}
	if status, body := comment(task.ID, unsendable); status != http.StatusCreated {
		t.Fatalf("hk again: unsendable comment = %d %v", status, body)
	}
	if n := items("/v1/import/comments"); n != 4 {
		t.Fatalf("hk again: %d import comments, want 4", n)
	}
}
