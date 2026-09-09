package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	linearconnector "github.com/mgh3326/handoffkeep/internal/linear"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestLinearSyncDefaultsOff(t *testing.T) {
	st := shutdownTestStore(t)
	baseline := runtime.NumGoroutine()
	keyRead := false
	workers, err := configureLinearWorkers(false, st, "", "", func() string {
		keyRead = true
		return ""
	})
	if err != nil || len(workers) != 0 || keyRead || st.LinearSyncEnabled() {
		t.Fatalf("workers=%d key_read=%t enabled=%t err=%v", len(workers), keyRead, st.LinearSyncEnabled(), err)
	}
	if got := runtime.NumGoroutine(); got != baseline {
		t.Fatalf("disabled connector started a goroutine: baseline=%d got=%d", baseline, got)
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
}

func TestTasksTransitionSucceedsWhileLinearUnavailableAndStatusReportsBacklog(t *testing.T) {
	st := shutdownTestStore(t)
	st.EnableLinearSync()
	hkServer := httptest.NewServer(api.Server{
		Service: api.Service{Store: st},
		Tokens:  api.Tokens{"fixture-client": "fixture-token"},
	}.Handler())
	defer hkServer.Close()
	t.Setenv("HANDOFFKEEP_URL", hkServer.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")

	var addOutput bytes.Buffer
	if err := tasksCmd([]string{
		"add", "--lane", "linear-unavailable", "--title", "transition remains available",
		"--linear-sync", "--tier", "T3", "--grade", "S+",
	}, &addOutput); err != nil {
		t.Fatalf("tasks add returned an error: %v", err)
	}
	var task store.Task
	if err := json.Unmarshal(addOutput.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if err := tasksCmd([]string{"claim", fmt.Sprint(task.ID), "--by", "builder"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("tasks claim returned an error: %v", err)
	}
	var transitionOutput bytes.Buffer
	if err := tasksCmd([]string{
		"transition", fmt.Sprint(task.ID), "--to", "in_progress", "--note", "local transition",
	}, &transitionOutput); err != nil {
		t.Fatalf("tasks transition returned an error: %v", err)
	}
	var transitioned store.Task
	if err := json.Unmarshal(transitionOutput.Bytes(), &transitioned); err != nil {
		t.Fatal(err)
	}
	if transitioned.State != "in_progress" {
		t.Fatalf("state=%q", transitioned.State)
	}

	unavailable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := unavailable.URL
	unavailable.Close()
	linearClient, err := linearconnector.NewClient(linearconnector.Config{
		APIURL: endpoint,
		APIKey: "fixture-key",
		TeamID: "team-1",
		HTTP:   &http.Client{Timeout: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&linearconnector.Drain{
			Store: st, Client: linearClient, PollInterval: 5 * time.Millisecond,
			Jitter: func(time.Duration) time.Duration { return 250 * time.Millisecond },
		}).Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Linear drain did not stop")
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	var rows []store.LinearOutbox
	for time.Now().Before(deadline) {
		rows, err = st.ListLinearOutbox(t.Context(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 3 && rows[0].Attempts == 1 && rows[0].State == "pending" && rows[0].LastError != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(rows) != 3 || rows[0].Attempts != 1 || rows[0].State != "pending" || rows[0].LastError == "" {
		t.Fatalf("outbox did not retain visible failure: %+v", rows)
	}
	status, err := (remote.Client{URL: hkServer.URL, Token: "fixture-token"}).LinearOutboxStatus(t.Context())
	if err != nil {
		t.Fatalf("diagnostic status returned an error: %v", err)
	}
	if status.Pending < 3 || status.LastError == "" {
		t.Fatalf("status=%+v", status)
	}
}
