package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	planeconnector "github.com/mgh3326/handoffkeep/internal/plane"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestPlaneSyncDefaultsOff(t *testing.T) {
	st := shutdownTestStore(t)
	keyRead := false
	workers, err := configurePlaneWorkers(false, false, st, "", "", "", "", func() string {
		keyRead = true
		return ""
	})
	if err != nil || len(workers) != 0 || keyRead || st.PlaneSyncEnabled() {
		t.Fatalf("workers=%d key_read=%t enabled=%t err=%v", len(workers), keyRead, st.PlaneSyncEnabled(), err)
	}
	task, err := st.CreateTask(t.Context(), store.Task{
		Lane:      "plane-off",
		Title:     "off remains local",
		Kind:      "implement",
		CreatedBy: "plane-off-test",
		Project:   projectPtr("experiment"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListPlaneOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("off outbox=%+v err=%v", rows, err)
	}
}

// TestPlaneLiveRequiresKey proves live writes cannot be armed from flags
// alone: --plane-live without HK_PLANE_API_KEY fails closed.
func TestPlaneLiveRequiresKey(t *testing.T) {
	st := shutdownTestStore(t)
	_, err := configurePlaneWorkers(true, true, st, "", "ws", "", "HK", func() string { return "" })
	if err == nil || !strings.Contains(err.Error(), "HK_PLANE_API_KEY") {
		t.Fatalf("plane-live without key err=%v", err)
	}
	// Sync without live is valid without a key — the drain is dry-run only.
	workers, err := configurePlaneWorkers(true, false, st, "", "ws", "", "HK", func() string { return "" })
	if err != nil || len(workers) != 1 || !st.PlaneSyncEnabled() {
		t.Fatalf("dry workers=%d enabled=%t err=%v", len(workers), st.PlaneSyncEnabled(), err)
	}
	drain, ok := workers[0].(*planeconnector.Drain)
	if !ok || !drain.DryRun || drain.Client != nil {
		t.Fatalf("dry-run drain=%+v", workers[0])
	}
	// Workspace is required once sync is on.
	_, err = configurePlaneWorkers(true, false, shutdownTestStore(t), "", "", "", "HK", func() string { return "" })
	if err == nil || !strings.Contains(err.Error(), "WORKSPACE") {
		t.Fatalf("missing workspace err=%v", err)
	}
}

// TestPlanePlanCLIReadsExportSnapshot proves the plan command consumes the
// read-only export endpoint and emits the pilot numbers.
func TestPlanePlanCLIReadsExportSnapshot(t *testing.T) {
	recent := time.Now().UTC()
	old := recent.Add(-8 * 24 * time.Hour)
	project := "experiment"
	export := store.TaskExport{
		Tasks: []store.Task{
			{ID: 1, State: "backlog", Title: "open a", Project: &project, UpdatedAt: recent},
			{ID: 2, State: "in_progress", Title: "open b", UpdatedAt: recent},
			{ID: 3, State: "merged", Title: "done", Project: &project, UpdatedAt: recent},
			{ID: 4, State: "dropped", Title: "old", Project: &project, UpdatedAt: old},
		},
		RowsReturned: 4,
		Counts:       store.TaskExportCounts{Total: 4},
		Complete:     true,
	}
	exportCalls := 0
	hkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/export":
			exportCalls++
			_ = json.NewEncoder(w).Encode(export)
		default:
			t.Errorf("unexpected hk request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer hkServer.Close()
	t.Setenv("HANDOFFKEEP_URL", hkServer.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")

	var output bytes.Buffer
	if err := planeCmd([]string{"plan", "--plane-default-project", "HK"}, &output); err != nil {
		t.Fatal(err)
	}
	if exportCalls != 1 {
		t.Fatalf("export calls=%d", exportCalls)
	}
	var plan planeconnector.MirrorPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("plan output: %v\n%s", err, output.String())
	}
	if plan.Mirrored != 2 || plan.Terminal != 2 || plan.Tasks != 4 || plan.LinearCap != 250 {
		t.Fatalf("plan=%+v", plan)
	}
	if plan.Unmapped != 0 || plan.LinearHeadroom != 248 || !plan.LinearFits {
		t.Fatalf("plan mapping=%+v", plan)
	}
	if err := planeCmd([]string{"bogus"}, &output); err == nil {
		t.Fatal("plane bogus should fail")
	}
}

// TestPlanePlanRejectsTruncatedExport proves the comparison numbers are
// never computed on a partial queue snapshot — a truncated or incomplete
// export is a hard error, not a quietly smaller denominator.
func TestPlanePlanRejectsTruncatedExport(t *testing.T) {
	export := store.TaskExport{
		Tasks:        []store.Task{{ID: 1, State: "backlog", Title: "open"}},
		RowsReturned: 1,
		Counts:       store.TaskExportCounts{Total: 500},
		Truncated:    true,
	}
	hkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/export" {
			_ = json.NewEncoder(w).Encode(export)
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	defer hkServer.Close()
	t.Setenv("HANDOFFKEEP_URL", hkServer.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "fixture-token")

	var output bytes.Buffer
	err := planeCmd([]string{"plan"}, &output)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("truncated export err=%v", err)
	}
}

// TestPlaneStatusEndpoint exercises /v1/plane/status through the real API
// server so the outbox diagnostic surface stays covered.
func TestPlaneStatusEndpoint(t *testing.T) {
	st := shutdownTestStore(t)
	st.EnablePlaneSync()
	hkServer := httptest.NewServer(api.Server{
		Service: api.Service{Store: st},
		Tokens:  api.Tokens{"fixture-client": "fixture-token"},
	}.Handler())
	defer hkServer.Close()
	task, err := st.CreateTask(t.Context(), store.Task{
		Lane: "plane-status", Title: "status row", Kind: "implement",
		CreatedBy: "plane-status-test", Project: projectPtr("experiment"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = task
	request, err := http.NewRequestWithContext(t.Context(), "GET", hkServer.URL+"/v1/plane/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fixture-token")
	response, err := hkServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code=%d", response.StatusCode)
	}
	var status store.PlaneOutboxStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Pending < 1 {
		t.Fatalf("status=%+v", status)
	}
}
