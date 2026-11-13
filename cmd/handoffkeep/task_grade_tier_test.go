package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// tasks add --grade/--tier sends grade and tier as refs.linear.{grade,tier}
// for one release (hk 4425): the previous server refuses refs.grade, and this
// server stores both keys. No other refs.linear field is sent.
func TestTaskGradeTierCLIFlagsWriteRefs(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		project := "experiment"
		_ = json.NewEncoder(w).Encode(store.Task{ID: 1, Lane: "builder-lane", Title: "metadata (grade SS, T2)", Project: &project})
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	args := []string{"add", "--lane", "builder-lane", "--title", "metadata (grade SS, T2)", "--project", "experiment", "--tier", "T3", "--grade", "SS"}
	if err := tasksCmd(args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	refs, _ := received["refs"].(map[string]any)
	linear, _ := refs["linear"].(map[string]any)
	if linear["grade"] != "SS" || linear["tier"] != "T3" || linear["sync"] != false || len(linear) != 3 {
		t.Fatalf("refs=%v", refs)
	}
	if _, ok := refs["grade"]; ok {
		t.Fatalf("refs.grade sent to a server that may refuse it: %v", refs)
	}
	// The server turns that patch into refs.grade/refs.tier.
	var patch store.TaskRefs
	raw, _ := json.Marshal(refs)
	if err := json.Unmarshal(raw, &patch); err != nil {
		t.Fatal(err)
	}
	if g, tr := patch.GradeTier(); g != "SS" || tr != "T3" {
		t.Fatalf("GradeTier %q %q", g, tr)
	}
}

// The retired Linear flags are gone: the CLI refuses them before any request.
func TestTaskRetiredLinearFlagsRefused(t *testing.T) {
	t.Setenv("HANDOFFKEEP_URL", "http://127.0.0.1:1")
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	for _, flag := range []string{"--linear-sync", "--brief-key=x", "--label=x", "--linear-report-key=x", "--linear-verify-key=x", "--linear-decision-key=x", "--deploy-sha=abcdef0"} {
		if err := tasksCmdE([]string{"add", "--lane", "l", "--title", "t", "--project", "p", flag}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s accepted", flag)
		}
	}
}
