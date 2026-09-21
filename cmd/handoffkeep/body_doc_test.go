package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// tasks add --doc files the body pointer through the real API into the store;
// show returns it. The document itself need not exist.
func TestTasksAddDocRoundTrip(t *testing.T) {
	st := shutdownTestStore(t)
	hkServer := httptest.NewServer(api.Server{
		Service: api.Service{Store: st},
		Tokens:  api.Tokens{"fixture-client": "fixture-token"},
	}.Handler())
	defer hkServer.Close()
	t.Setenv("HANDOFFKEEP_URL", hkServer.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")

	var addOutput bytes.Buffer
	if err := tasksCmd([]string{"add", "--lane", "doc-lane", "--title", "one operator line", "--doc", "design/2026-09-21/body#3"}, &addOutput); err != nil {
		t.Fatal(err)
	}
	var task store.Task
	if err := json.Unmarshal(addOutput.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if task.BodyDoc != "design/2026-09-21/body#3" {
		t.Fatalf("add body_doc=%q", task.BodyDoc)
	}
	var showOutput bytes.Buffer
	if err := tasksCmd([]string{"show", fmt.Sprint(task.ID)}, &showOutput); err != nil {
		t.Fatal(err)
	}
	var shown store.Task
	if err := json.Unmarshal(showOutput.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.BodyDoc != "design/2026-09-21/body#3" {
		t.Fatalf("show body_doc=%q", shown.BodyDoc)
	}

	// Server-side shape check: a raw API client cannot store body text.
	req, _ := http.NewRequest(http.MethodPost, hkServer.URL+"/v1/tasks", strings.NewReader(`{"lane":"doc-lane","title":"raw","kind":"implement","body_doc":"this is body text"}`))
	req.Header.Set("Authorization", "Bearer fixture-token")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("raw body text body_doc status=%d, want 400", res.StatusCode)
	}
}

func TestTasksDocFlagShapeAndScope(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	for _, bad := range []string{"body text", "../x", "k#a b"} {
		err := tasksCmd([]string{"add", "--lane", "l", "--title", "t", "--doc", bad}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "--doc") {
			t.Fatalf("--doc %q err=%v", bad, err)
		}
	}
	// No other subcommand writes body_doc; the flag is refused, never ignored.
	err := tasksCmd([]string{"transition", "7", "--to", "hold", "--doc", "k/x"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "only by tasks add") {
		t.Fatalf("transition --doc err=%v", err)
	}
	if requests != 0 {
		t.Fatalf("refused --doc still sent %d requests", requests)
	}
}
