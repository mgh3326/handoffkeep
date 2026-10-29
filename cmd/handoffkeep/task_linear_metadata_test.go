package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTaskLinearCLIFlagsAreExplicitMetadata(t *testing.T) {
	var received store.Task
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(received)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	args := []string{
		"add", "--lane", "builder-lane", "--title", "metadata", "--project", "experiment", "--linear-sync",
		"--tier", "T3", "--grade", "S+", "--brief-key", "brief/connector",
		"--label", "connector", "--label", "approved",
		"--linear-report-key", "report/connector", "--linear-verify-key", "report/connector/verify",
		"--linear-decision-key", "decision/connector", "--deploy-sha", "abcdef0123456789",
	}
	if err := tasksCmd(args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if received.Refs.Linear == nil || !received.Refs.Linear.Sync || received.Refs.Linear.Tier != "T3" || received.Refs.Linear.Grade != "S+" || len(received.Refs.Linear.Labels) != 2 {
		t.Fatalf("linear refs=%+v", received.Refs.Linear)
	}
}
