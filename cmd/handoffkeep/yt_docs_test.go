package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The docs mode (hk 4206) alone starts the write worker, needs the YouTrack
// credentials, reads the /shared and /unassigned bucket projects once at
// start by name (none, two, or a failed read stops the start), and blocks
// the legacy push, which also writes articles.
func TestYTDocsModeStartsTheWriteWorker(t *testing.T) {
	for _, env := range []string{"HANDOFFKEEP_YT_COMMENTS_MODE", "HANDOFFKEEP_YT_ISSUES_MODE", "HANDOFFKEEP_YT_FIELDS_MODE",
		"HANDOFFKEEP_YT_DOC_COMMENTS_MODE", "HANDOFFKEEP_YT_DECISIONS_MODE"} {
		t.Setenv(env, "off")
	}
	t.Setenv("HANDOFFKEEP_YT_DOCS_MODE", "on")
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_DOCS_MODE=on") || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("on without credentials: %v", err)
	}
	if owner, err := youtrackProtocolOwner(); err != nil || owner != "HANDOFFKEEP_YT_DOCS_MODE=on" {
		t.Fatalf("protocol owner %q, %v", owner, err)
	}
	status := http.StatusOK
	list := `[{"id":"0-1","shortName":"HK","name":"Unassigned hk tasks"},{"id":"0-40","shortName":"SHRDOC2","name":"Shared documents"}]`
	reads := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/admin/projects" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		reads++
		if status != http.StatusOK {
			http.Error(w, "down", status)
			return
		}
		_, _ = w.Write([]byte(list))
	}))
	defer ts.Close()
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", readthroughTestKey)
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w == nil || reads != 1 {
		t.Fatalf("docs on: worker %v err %v reads %d", w, err, reads)
	}
	list = `[{"id":"0-40","shortName":"SHRDOC","name":"Shared documents"}]`
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err == nil || w != nil || !strings.Contains(err.Error(), "Unassigned hk tasks") {
		t.Fatalf("no unassigned bucket: worker %v err %v", w, err)
	}
	status = http.StatusForbidden
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err == nil || w != nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_DOCS_MODE=on") {
		t.Fatalf("unreadable projects: worker %v err %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_YT_DOCS_MODE", "yes")
	if _, err := ytCommentsWorkerFromEnv(&store.Store{}); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_DOCS_MODE") {
		t.Fatalf("an unknown docs mode: %v", err)
	}
	if _, err := youtrackProtocolOwner(); err == nil {
		t.Fatal("an unknown docs mode let the legacy push run")
	}
	t.Setenv("HANDOFFKEEP_YT_DOCS_MODE", "off")
	if w, err := ytCommentsWorkerFromEnv(&store.Store{}); err != nil || w != nil {
		t.Fatalf("all off: worker %v err %v", w, err)
	}
}
