package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type commentRequest struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

func commentServer(t *testing.T) (*httptest.Server, *[]commentRequest) {
	t.Helper()
	var seen []commentRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := commentRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&x.Body)
		}
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1,"task_id":42,"body":"b","author":"node","created_at":"2026-09-21T00:00:00Z"}`))
			return
		}
		_, _ = w.Write([]byte(`{"comments":[]}`))
	}))
	t.Cleanup(h.Close)
	return h, &seen
}

// The CLI sends only the body; there is no way to name an author.
func TestTasksCommentCLISendsOnlyBody(t *testing.T) {
	h, seen := commentServer(t)
	file := filepath.Join(t.TempDir(), "comment.md")
	if err := os.WriteFile(file, []byte("from file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"tasks", "comment", "42", "--body", "hello", "--url", h.URL, "--token", "tok"},
		{"tasks", "comment", "--url", h.URL, "--token", "tok", "--file", file, "42"},
	} {
		var out bytes.Buffer
		if err := run(args, &out, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), `"author":"node"`) {
			t.Fatalf("out=%s", out.String())
		}
	}
	want := []commentRequest{
		{Method: "POST", Path: "/v1/tasks/42/comments", Auth: "Bearer tok", Body: map[string]any{"body": "hello"}},
		{Method: "POST", Path: "/v1/tasks/42/comments", Auth: "Bearer tok", Body: map[string]any{"body": "from file\n"}},
	}
	if !reflect.DeepEqual(*seen, want) {
		t.Fatalf("requests=%+v", *seen)
	}
}

func TestTasksCommentsCLIListsWithCursor(t *testing.T) {
	h, seen := commentServer(t)
	var out bytes.Buffer
	if err := run([]string{"tasks", "comments", "42", "--after-id", "7", "--limit", "5", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != `{"comments":[]}` {
		t.Fatalf("out=%s", out.String())
	}
	if len(*seen) != 1 || (*seen)[0].Method != "GET" || (*seen)[0].Path != "/v1/tasks/42/comments" || (*seen)[0].Query != "after_id=7&limit=5" {
		t.Fatalf("requests=%+v", *seen)
	}
}

func TestTasksCommentCLIRejectsBadInputBeforeSending(t *testing.T) {
	h, seen := commentServer(t)
	base := []string{"--url", h.URL, "--token", "tok"}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"tasks", "comment", "42"}, "exactly one of --body or --file"},
		{[]string{"tasks", "comment", "42", "--body", "a", "--file", "x"}, "exactly one of --body or --file"},
		{[]string{"tasks", "comment", "42", "--body", "  "}, "comment_empty"},
		{[]string{"tasks", "comment", "42", "--body", strings.Repeat("a", 64<<10+1)}, "comment_too_long"},
		{[]string{"tasks", "comment", "42", "--author", "operator", "--body", "a"}, "flag provided but not defined"},
		{[]string{"tasks", "comment", "0", "--body", "a"}, "task id must be positive"},
		{[]string{"tasks", "comment", "--body", "a"}, "usage: tasks comment <id>"},
		{[]string{"tasks", "comments", "42", "--body", "a"}, "tasks comments takes"},
		{[]string{"tasks", "comments", "42", "--limit", "0"}, "--limit between"},
	} {
		err := run(append(tc.args, base...), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err=%v want %q", tc.args, err, tc.want)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("invalid input reached the server: %+v", *seen)
	}
}
