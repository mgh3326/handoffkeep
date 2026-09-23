package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The fixtures below reproduce the real hub wire shapes, field for field:
//
//   - nodes: panewire hub.go:108 HubNode (machine_id, alert_class, accepting*,
//     connected_since, last_ping_ms, load, memory, remote_meta, state,
//     session_snapshot) with hub.go:127 HubNodeLoad (load1/5/15, ncpu — every
//     field a pointer, nil = unmeasured), checks.go:58 HubHostMemory
//     (free_pct already 0..100, source names the measurement) and
//     session_snapshot.go:59 HubSessionSnapshot / :43 HubSession.
//   - jobs: panewire hub_jobs.go:414 hubConsoleJob (machine, job_id,
//     owner_lane, pane, tier, role, started_at, last_event_kind,
//     last_event_at) inside the {"jobs":[...]} envelope of hub.go:838.
//
// No fixture invents a shape the hub cannot send.

func liveNodeFixture(machine, state string, lastPing *int64, load, memory, snap map[string]any) map[string]any {
	node := map[string]any{
		"machine_id":          machine,
		"alert_class":         "",
		"accepting":           true,
		"accepting_effective": true,
		"accepting_override":  "",
		"connected_since":     "2026-09-23T03:00:00Z",
		"remote_meta":         map[string]any{"version": "1.2.3"},
		"state":               state,
	}
	if lastPing != nil {
		node["last_ping_ms"] = *lastPing
	}
	if load != nil {
		node["load"] = load
	}
	if memory != nil {
		node["memory"] = memory
	}
	if snap != nil {
		node["session_snapshot"] = snap
	}
	return node
}

func liveJobFixture(machine, jobID, ownerLane, pane, role string) map[string]any {
	return map[string]any{
		"machine":         machine,
		"job_id":          jobID,
		"owner_lane":      ownerLane,
		"pane":            pane,
		"tier":            "A+",
		"role":            role,
		"started_at":      "2026-09-23T11:00:00Z",
		"last_event_kind": "heartbeat",
		"last_event_at":   "2026-09-23T11:59:00Z",
	}
}

// liveHubFixture serves the two endpoints /ui/api/live reads. The jobs
// endpoint status is switchable so 404/405 → "unsupported" can be exercised.
type liveHubFixture struct {
	nodes      []map[string]any
	jobs       []map[string]any
	jobsStatus atomic.Int32
}

func (f *liveHubFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fleetTestSecret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/nodes":
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": f.nodes})
	case "/v1/jobs":
		if status := int(f.jobsStatus.Load()); status != 0 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": f.jobs})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func getLiveAPI(t *testing.T, server *httptest.Server, assertion string) (int, string, map[string]any) {
	t.Helper()
	response := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/api/live", assertion, "")
	body := responseText(t, response)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode live JSON: %v body=%q", err, body)
	}
	return response.StatusCode, body, decoded
}

func liveSection(t *testing.T, decoded map[string]any, name string) map[string]any {
	t.Helper()
	section, ok := decoded[name].(map[string]any)
	if !ok {
		t.Fatalf("missing %s section in %v", name, decoded)
	}
	return section
}

func liveItems(t *testing.T, decoded map[string]any, section string) []map[string]any {
	t.Helper()
	raw, _ := liveSection(t, decoded, section)["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		node, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s item type %T", section, item)
		}
		out = append(out, node)
	}
	return out
}

func liveLinkFor(t *testing.T, decoded map[string]any, taskID int64) map[string]any {
	t.Helper()
	raw, _ := decoded["links"].([]any)
	for _, item := range raw {
		link, _ := item.(map[string]any)
		if int64(link["task_id"].(float64)) == taskID {
			return link
		}
	}
	return nil
}

func createJobTask(t *testing.T, s *store.Store, lane, title, jobID string) store.Task {
	t.Helper()
	task, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "test-node", Refs: store.TaskRefs{JobID: jobID}})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestLiveAPIJoinsTasksJobsAndNodes(t *testing.T) {
	s := uiStore(t)
	ping := int64(800)
	hub := &liveHubFixture{
		nodes: []map[string]any{
			// m1a: fully measured node with a session snapshot.
			liveNodeFixture("m1a", "ready", &ping,
				map[string]any{"load1": 1.2, "load5": 2.5, "load15": 3.1, "ncpu": 10},
				map[string]any{"free_pct": 41.5, "compressed_mb": 2048.0, "swap_used_mb": 128.0, "psi_some_avg10": 0.4, "source": "memory_pressure"},
				sessionSnapshot("ok", false, false, "2026-09-23T12:00:00Z", []map[string]any{
					syntheticSession("w1:p1", "b598-console-live-load", "working", readyTrue()),
				})),
			// m1b: vm_stat memory (unusable % per panewire checks.go), stale
			// snapshot with zero sessions — must surface as stale, never idle.
			liveNodeFixture("m1b", "ready", nil, nil,
				map[string]any{"free_pct": 12.5, "source": "vm_stat"},
				sessionSnapshot("ok", false, true, "2026-09-23T10:00:00Z", nil)),
			// m1c: no load, no memory, no session_snapshot at all.
			liveNodeFixture("m1c", "ready", nil, nil, nil, nil),
		},
		jobs: []map[string]any{
			liveJobFixture("m1a", "job-b598", "b598-lane", "w1:p1", "builder"),
			liveJobFixture("m1a", "job-t598", "b598-lane", "w16:p2", "tester"),
			liveJobFixture("m1b", "job-lane-mate", "b598-lane-x", "w2:p1", "worker"),
			liveJobFixture("m1b", "job-orphan", "", "w2:p2", "worker"),
		},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	ui, assertion := newFleetUI(t, server.URL, fleetTestSecret, 10*time.Second)

	lane := uiLane(t, "live")
	linked := createJobTask(t, s, lane, "linked in-progress task", "job-b598")
	claimAndTransition(t, s, linked, "in_progress", "builder started")
	// Prefix trap: refs.job_id "job-b59" must not match hub job "job-b598".
	prefix := createJobTask(t, s, lane, "prefix job_id task", "job-b59")
	claimAndTransition(t, s, prefix, "in_progress", "started")
	// A live-state task with no recorded job_id is the same mismatch shape.
	bare := createUITask(t, s, lane, "claimed without job")
	claimAndTransition(t, s, bare, "in_progress", "started without refs")
	// A non-live task keeps its link record but is never a mismatch.
	done := createJobTask(t, s, lane, "merged task", "job-b598")
	claimAndTransition(t, s, done, "in_progress", "worked")
	if _, err := s.TransitionTask(t.Context(), done.ID, "verifying", "test-node", "verify", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), done.ID, "merged", "test-node", "done", nil); err != nil {
		t.Fatal(err)
	}

	status, body, decoded := getLiveAPI(t, ui, assertion)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%q", status, body)
	}
	assertNoSecret(t, body, http.Header{}, fleetTestSecret, server.URL)

	if got := liveSection(t, decoded, "jobs")["status"]; got != "ok" {
		t.Fatalf("jobs.status=%v", got)
	}
	if got := liveSection(t, decoded, "nodes")["status"]; got != "ok" {
		t.Fatalf("nodes.status=%v", got)
	}
	if len(liveItems(t, decoded, "jobs")) != 4 {
		t.Fatalf("jobs.items=%v", liveItems(t, decoded, "jobs"))
	}
	job := liveItems(t, decoded, "jobs")[0]
	if job["role"] != "builder" || job["last_event_kind"] != "heartbeat" || job["last_event_at"] == nil || job["started_at"] == nil {
		t.Fatalf("job fields dropped: %v", job)
	}

	nodes := liveItems(t, decoded, "nodes")
	if len(nodes) != 3 {
		t.Fatalf("nodes.items=%v", nodes)
	}
	m1a := nodes[0]
	if got := m1a["active_jobs"]; got != float64(2) {
		t.Fatalf("m1a.active_jobs=%v want 2", got)
	}
	load, _ := m1a["load"].(map[string]any)
	if load["load5"] != 2.5 || load["ncpu"] != float64(10) {
		t.Fatalf("m1a.load=%v", load)
	}
	memory, _ := m1a["memory"].(map[string]any)
	if memory["free_pct"] != 41.5 || memory["source"] != "memory_pressure" {
		t.Fatalf("m1a.memory=%v", memory)
	}
	snap, _ := m1a["session_snapshot"].(map[string]any)
	if m1a["display_state"] != "sessions" || snap == nil || snap["stale"] != false {
		t.Fatalf("m1a snapshot=%v display=%v", snap, m1a["display_state"])
	}

	m1b := nodes[1]
	if got := m1b["active_jobs"]; got != float64(2) {
		t.Fatalf("m1b.active_jobs=%v want 2", got)
	}
	if m1b["load"] != nil {
		t.Fatalf("m1b.load must stay null, got %v", m1b["load"])
	}
	memoryB, _ := m1b["memory"].(map[string]any)
	// vm_stat free_pct travels verbatim — the client renders it as 측정 불가.
	if memoryB["source"] != "vm_stat" || memoryB["free_pct"] != 12.5 {
		t.Fatalf("m1b.memory=%v", memoryB)
	}
	snapB, _ := m1b["session_snapshot"].(map[string]any)
	if snapB["stale"] != true {
		t.Fatalf("m1b stale snapshot flag lost: %v", snapB)
	}

	m1c := nodes[2]
	if m1c["session_snapshot"] != nil || m1c["display_state"] != "missing" {
		t.Fatalf("m1c snapshot=%v display=%v", m1c["session_snapshot"], m1c["display_state"])
	}
	if m1c["memory"] != nil {
		t.Fatalf("m1c.memory must stay null, got %v", m1c["memory"])
	}
	if got := m1c["active_jobs"]; got != float64(0) {
		t.Fatalf("m1c.active_jobs=%v want real 0", got)
	}

	link := liveLinkFor(t, decoded, linked.ID)
	if link == nil || link["job_found"] != true {
		t.Fatalf("linked task link=%v", link)
	}
	children, _ := link["children"].([]any)
	if len(children) != 1 || children[0] != "job-t598" {
		// job-lane-mate (owner_lane "b598-lane-x") must not join on a prefix.
		t.Fatalf("children=%v want [job-t598]", children)
	}

	prefixLink := liveLinkFor(t, decoded, prefix.ID)
	if prefixLink == nil || prefixLink["job_found"] != false {
		t.Fatalf("prefix task link=%v — substring match leaked", prefixLink)
	}
	if liveLinkFor(t, decoded, bare.ID) != nil {
		t.Fatal("task without refs.job_id must not get a link entry")
	}
	doneLink := liveLinkFor(t, decoded, done.ID)
	if doneLink == nil || doneLink["job_found"] != true {
		t.Fatalf("merged task link=%v", doneLink)
	}

	mismatch := decoded["mismatch"].(map[string]any)
	if mismatch["basis"] != "current" {
		t.Fatalf("mismatch.basis=%v", mismatch["basis"])
	}
	withoutJob, _ := mismatch["tasks_without_job"].([]any)
	gotIDs := map[int64]bool{}
	for _, item := range withoutJob {
		gotIDs[int64(item.(float64))] = true
	}
	if !gotIDs[prefix.ID] || !gotIDs[bare.ID] || gotIDs[linked.ID] || gotIDs[done.ID] {
		t.Fatalf("tasks_without_job=%v want {%d,%d}", withoutJob, prefix.ID, bare.ID)
	}
	withoutTask, _ := mismatch["jobs_without_task"].([]any)
	gotJobs := map[string]bool{}
	for _, item := range withoutTask {
		gotJobs[item.(string)] = true
	}
	if !gotJobs["job-lane-mate"] || !gotJobs["job-orphan"] || gotJobs["job-b598"] || gotJobs["job-t598"] {
		t.Fatalf("jobs_without_task=%v", withoutTask)
	}
}

func TestLiveAPIFailureAndUnavailableStates(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	lane := uiLane(t, "live-fail")
	task := createJobTask(t, s, lane, "jobbed task", "job-x")
	claimAndTransition(t, s, task, "in_progress", "started")

	// Hub 5xx: both sections explicit, mismatch basis unavailable — an empty
	// items list is never presented as an idle fleet.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	ui, assertion := newFleetUI(t, broken.URL, fleetTestSecret, 10*time.Second)
	status, body, decoded := getLiveAPI(t, ui, assertion)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%q", status, body)
	}
	jobs := liveSection(t, decoded, "jobs")
	nodes := liveSection(t, decoded, "nodes")
	if jobs["status"] != "http_error" || nodes["status"] != "http_error" {
		t.Fatalf("statuses jobs=%v nodes=%v", jobs["status"], nodes["status"])
	}
	if jobs["fetched_at"] != "" || len(liveItems(t, decoded, "jobs")) != 0 {
		t.Fatalf("failed jobs section must carry no fabricated data: %v", jobs)
	}
	mismatch := decoded["mismatch"].(map[string]any)
	if mismatch["basis"] != "unavailable" {
		t.Fatalf("mismatch.basis=%v want unavailable", mismatch["basis"])
	}
	if got, _ := mismatch["tasks_without_job"].([]any); len(got) != 0 {
		// With no jobs data the comparison did not run — listing the task
		// would claim "no active job", a fabricated mismatch.
		t.Fatalf("tasks_without_job must be empty when jobs data is absent: %v", got)
	}

	// 401 → auth_failed, distinct from a generic HTTP error.
	unauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauth.Close()
	ui2, assertion2 := newFleetUI(t, unauth.URL, fleetTestSecret, 10*time.Second)
	_, _, decoded = getLiveAPI(t, ui2, assertion2)
	if liveSection(t, decoded, "jobs")["status"] != "auth_failed" {
		t.Fatalf("jobs.status=%v want auth_failed", liveSection(t, decoded, "jobs")["status"])
	}

	// Old hub without /v1/jobs → unsupported; nodes still serve.
	legacy := &liveHubFixture{nodes: []map[string]any{}}
	legacy.jobsStatus.Store(http.StatusNotFound)
	legacyServer := httptest.NewServer(legacy)
	defer legacyServer.Close()
	ui3, assertion3 := newFleetUI(t, legacyServer.URL, fleetTestSecret, 10*time.Second)
	_, _, decoded = getLiveAPI(t, ui3, assertion3)
	if liveSection(t, decoded, "jobs")["status"] != "unsupported" {
		t.Fatalf("jobs.status=%v want unsupported", liveSection(t, decoded, "jobs")["status"])
	}
	if liveSection(t, decoded, "nodes")["status"] != "ok" {
		t.Fatalf("nodes.status=%v want ok", liveSection(t, decoded, "nodes")["status"])
	}

	// Hub slower than the proxy's client timeout → timeout, not empty.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []any{}, "jobs": []any{}})
	}))
	defer slow.Close()
	ui4 := newUITestServerWithClient(t, s, fixture, slow.URL, fleetTestSecret, 0, &http.Client{Timeout: 50 * time.Millisecond})
	defer ui4.Close()
	assertion4 := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	_, _, decoded = getLiveAPI(t, ui4, assertion4)
	if got := liveSection(t, decoded, "jobs")["status"]; got != "timeout" {
		t.Fatalf("jobs.status=%v want timeout", got)
	}
	if got := liveSection(t, decoded, "nodes")["status"]; got != "timeout" {
		t.Fatalf("nodes.status=%v want timeout", got)
	}

	// No hub configured at all.
	ui5, assertion5 := newFleetUI(t, "", "", 10*time.Second)
	_, _, decoded = getLiveAPI(t, ui5, assertion5)
	if liveSection(t, decoded, "jobs")["status"] != "unconfigured" || liveSection(t, decoded, "nodes")["status"] != "unconfigured" {
		t.Fatalf("unconfigured statuses: %v", decoded)
	}
}

func TestLiveAPIDegradedServesLastGood(t *testing.T) {
	ping := int64(50)
	hub := &liveHubFixture{
		nodes: []map[string]any{liveNodeFixture("m1b", "ready", &ping, nil, nil, nil)},
		jobs:  []map[string]any{liveJobFixture("m1b", "job-b", "lane-b", "", "tester")},
	}

	// Exercise the last-good path: fresh handler, ok fetch, then flip the
	// fixture to fail and fetch again with a 1ms TTL — the degraded section
	// keeps last-good items + the original fetched_at.
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		hub.ServeHTTP(w, r)
	}))
	defer server.Close()
	ui, assertion := newFleetUI(t, server.URL, fleetTestSecret, time.Millisecond)

	_, _, decoded := getLiveAPI(t, ui, assertion)
	if liveSection(t, decoded, "jobs")["status"] != "ok" || len(liveItems(t, decoded, "jobs")) != 1 {
		t.Fatalf("first fetch not ok: %v", decoded["jobs"])
	}
	fetchedAt := liveSection(t, decoded, "jobs")["fetched_at"].(string)

	fail.Store(true)
	time.Sleep(2 * time.Millisecond)
	_, _, decoded = getLiveAPI(t, ui, assertion)
	jobs := liveSection(t, decoded, "jobs")
	if jobs["status"] != "http_error" {
		t.Fatalf("degraded jobs.status=%v", jobs["status"])
	}
	if jobs["fetched_at"] != fetchedAt {
		t.Fatalf("degraded fetched_at=%v want last-good %v", jobs["fetched_at"], fetchedAt)
	}
	items := liveItems(t, decoded, "jobs")
	if len(items) != 1 || items[0]["job_id"] != "job-b" {
		t.Fatalf("degraded items=%v want last-good job-b", items)
	}
	if got, _ := decoded["mismatch"].(map[string]any)["basis"]; got != "cached" {
		t.Fatalf("mismatch.basis=%v want cached", got)
	}
	nodes := liveSection(t, decoded, "nodes")
	if nodes["status"] != "http_error" || len(liveItems(t, decoded, "nodes")) != 1 {
		t.Fatalf("degraded nodes=%v", nodes)
	}
}

func TestLiveAPIIsReadOnly(t *testing.T) {
	hub := &liveHubFixture{nodes: []map[string]any{}, jobs: []map[string]any{}}
	server := httptest.NewServer(hub)
	defer server.Close()
	ui, assertion := newFleetUI(t, server.URL, fleetTestSecret, 10*time.Second)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		response := uiRequest(t, ui.Client(), method, ui.URL+"/ui/api/live", assertion, "")
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s /ui/api/live status=%d, want 405", method, response.StatusCode)
		}
		response.Body.Close()
	}
}

func TestLiveAPITaskScanCoversAllStates(t *testing.T) {
	s := uiStore(t)
	hub := &liveHubFixture{
		nodes: []map[string]any{},
		jobs:  []map[string]any{liveJobFixture("m1a", "job-hold", "lane-h", "", "worker")},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	ui, assertion := newFleetUI(t, server.URL, fleetTestSecret, 10*time.Second)

	lane := uiLane(t, "live-scan")
	held := createJobTask(t, s, lane, "held task with a live job", "job-hold")
	claimAndTransition(t, s, held, "in_progress", "started")
	if _, err := s.TransitionTask(t.Context(), held.ID, "hold", "test-node", "paused", nil); err != nil {
		t.Fatal(err)
	}
	_, _, decoded := getLiveAPI(t, ui, assertion)
	link := liveLinkFor(t, decoded, held.ID)
	if link == nil || link["job_found"] != true {
		t.Fatalf("hold-state task link=%v — scan must cover non-live states", link)
	}
	mismatch := decoded["mismatch"].(map[string]any)
	for _, item := range mismatch["jobs_without_task"].([]any) {
		// job-hold is claimed by the held task — not an orphan.
		if item.(string) == "job-hold" {
			t.Fatalf("jobs_without_task contains held task's job: %v", mismatch["jobs_without_task"])
		}
	}
	for _, item := range mismatch["tasks_without_job"].([]any) {
		// hold is not a live state — a held task is never a "no active job" flag.
		if int64(item.(float64)) == held.ID {
			t.Fatalf("tasks_without_task contains held task: %v", mismatch["tasks_without_job"])
		}
	}
}
