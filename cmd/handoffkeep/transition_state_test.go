package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `tasks transition <id> --state <s>` used to send no target and come back as
// a bare invalid_context. It is refused before any request, naming --to.
func TestTasksTransitionStateFlagRefusedBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	err := tasksCmd([]string{"transition", "7", "--state", "dropped"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--to") {
		t.Fatalf("transition --state err=%v, want a refusal naming --to", err)
	}
	if requests != 0 {
		t.Fatalf("refused transition --state still sent %d requests", requests)
	}
}
