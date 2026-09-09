package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestLinearSyncDefaultsOff(t *testing.T) {
	st := shutdownTestStore(t)
	keyRead := false
	workers, err := configureLinearWorkers(false, st, "", "", func() string {
		keyRead = true
		return ""
	})
	if err != nil || len(workers) != 0 || keyRead || st.LinearSyncEnabled() {
		t.Fatalf("workers=%d key_read=%t enabled=%t err=%v", len(workers), keyRead, st.LinearSyncEnabled(), err)
	}
	task, err := st.CreateTask(t.Context(), store.Task{
		Lane:      "linear-off",
		Title:     "off remains local",
		Kind:      "implement",
		CreatedBy: "linear-off-test",
		Refs:      store.TaskRefs{Linear: &store.TaskLinear{Sync: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("off outbox=%+v err=%v", rows, err)
	}
}

func TestLinearReconcileCLIWritesIdempotentReport(t *testing.T) {
	fixture := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "linear", "testdata", "linear", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var mu sync.Mutex
	linearCounts := map[string]int{}
	linearServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request struct {
			OperationName string `json:"operationName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		linearCounts[request.OperationName]++
		mu.Unlock()
		if request.OperationName != "HKIssueSearch" {
			t.Errorf("unexpected Linear operation %q", request.OperationName)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture("issue_search_found.json"))
	}))
	defer linearServer.Close()

	task := store.Task{
		ID: 1, Lane: "builder-lane", Title: "Mirror connector contract", State: "backlog",
		Refs: store.TaskRefs{Linear: &store.TaskLinear{Sync: true}},
	}
	var reportBody, reportSHA string
	puts := 0
	hkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"tasks": []store.Task{task}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/linear/status":
			_ = json.NewEncoder(w).Encode(store.LinearOutboxStatus{})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/documents/"):
			var document store.Document
			if err := json.NewDecoder(r.Body).Decode(&document); err != nil {
				t.Error(err)
				return
			}
			puts++
			sum := sha256.Sum256([]byte(document.Body))
			sha := hex.EncodeToString(sum[:])
			changed := sha != reportSHA
			reportBody, reportSHA = document.Body, sha
			document.SHA256 = sha
			_ = json.NewEncoder(w).Encode(map[string]any{"document": document, "changed": changed})
		default:
			t.Errorf("unexpected hk request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer hkServer.Close()
	t.Setenv("HANDOFFKEEP_URL", hkServer.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")
	t.Setenv("HK_LINEAR_API_URL", linearServer.URL)
	t.Setenv("HK_LINEAR_API_KEY", "fixture-key")
	t.Setenv("HK_LINEAR_TEAM_ID", "team-1")

	for index := 0; index < 2; index++ {
		var output bytes.Buffer
		if err := linearCmd([]string{"reconcile"}, &output); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Changed bool `json:"changed"`
		}
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Changed != (index == 0) {
			t.Fatalf("run=%d changed=%t output=%s", index, result.Changed, output.String())
		}
	}
	if puts != 2 || reportBody == "" || reportSHA == "" {
		t.Fatalf("puts=%d body=%q sha=%q", puts, reportBody, reportSHA)
	}
	var dryOutput bytes.Buffer
	if err := linearCmd([]string{"reconcile", "--dry-run"}, &dryOutput); err != nil {
		t.Fatal(err)
	}
	if puts != 2 {
		t.Fatalf("dry-run wrote a report: puts=%d", puts)
	}
	mu.Lock()
	defer mu.Unlock()
	if linearCounts["HKIssueSearch"] != 3 {
		t.Fatalf("Linear reads=%v", linearCounts)
	}
}

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
		"add", "--lane", "builder-lane", "--title", "metadata", "--linear-sync",
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
	if _, err := context.WithCancel(t.Context()); err != nil {
		t.Fatal(err)
	}
}
