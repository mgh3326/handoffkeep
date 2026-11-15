package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// spawnDo sends one request to the test server and decodes its JSON answer.
func spawnDo(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return res.StatusCode, out
}

// spawnGetRows sends a GET whose answer is a JSON array.
func spawnGetRows(t *testing.T, url string) (int, []map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var rows []map[string]any
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, rows
}

func TestTaskSpawnRoutesAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	lane := relaneLane(t, "spawn-api")
	x := relaneAPITask(t, st, lane, "spawn api")
	blocker := relaneAPITask(t, st, lane, "blocker")
	url := fmt.Sprintf("%s/v1/tasks/%d/spawn", h.URL, x.ID)
	waiting := fmt.Sprintf(`{"request_id":"req-1","job":"job-1","role":"worker","state":"waiting","reason":"","attempt":1,"host":"m1b","updated_at":"2026-11-13T01:00:00Z","start_condition":{"task_id":%d,"until":"merged"}}`, blocker.ID)

	status, out := spawnDo(t, http.MethodPut, url, waiting)
	if status != http.StatusOK || out["state"] != "waiting" {
		t.Fatalf("PUT waiting status=%d out=%v", status, out)
	}
	cond, _ := out["start_condition"].(map[string]any)
	if cond == nil || cond["task_id"] != float64(blocker.ID) || cond["until"] != "merged" {
		t.Fatalf("start_condition=%v", out["start_condition"])
	}

	// An older replay answers 200 with the stored record, not the replayed one.
	stale := strings.Replace(waiting, `"waiting"`, `"parked"`, 1)
	stale = strings.Replace(stale, "2026-11-13T01:00:00Z", "2026-11-13T00:30:00Z", 1)
	if status, out = spawnDo(t, http.MethodPut, url, stale); status != http.StatusOK || out["state"] != "waiting" {
		t.Fatalf("stale PUT status=%d out=%v", status, out)
	}

	// An unknown state is refused with the reason, and nothing is stored.
	unknown := strings.Replace(waiting, `"waiting"`, `"queued"`, 1)
	unknown = strings.Replace(unknown, "2026-11-13T01:00:00Z", "2026-11-13T02:00:00Z", 1)
	status, out = spawnDo(t, http.MethodPut, url, unknown)
	if status != http.StatusBadRequest || out["error"] != "invalid_context" || out["reason"] == nil || out["reason"] == "" {
		t.Fatalf("unknown state status=%d out=%v", status, out)
	}

	// The record shows on the task and on the task list.
	if status, out = spawnDo(t, http.MethodGet, fmt.Sprintf("%s/v1/tasks/%d", h.URL, x.ID), ""); status != http.StatusOK {
		t.Fatalf("GET task status=%d", status)
	} else if sp, _ := out["spawn"].(map[string]any); sp == nil || sp["state"] != "waiting" {
		t.Fatalf("task spawn=%v", out["spawn"])
	}
	if status, out = spawnDo(t, http.MethodGet, fmt.Sprintf("%s/v1/tasks?lane=%s&limit=10", h.URL, lane), ""); status != http.StatusOK {
		t.Fatalf("GET list status=%d", status)
	} else if tasks, _ := out["tasks"].([]any); len(tasks) != 2 {
		t.Fatalf("list=%v", out)
	}

	// The states route answers the blocker and the unmet condition.
	statesURL := fmt.Sprintf("%s/v1/tasks/states?ids=%d,%d", h.URL, x.ID, blocker.ID)
	status, rows := spawnGetRows(t, statesURL)
	if status != http.StatusOK {
		t.Fatalf("GET states status=%d", status)
	}
	if len(rows) != 2 || rows[0]["id"] != float64(x.ID) || rows[0]["condition_met"] != false {
		t.Fatalf("states rows=%v", rows)
	}

	// Malformed id lists and an unknown until are refused as invalid_context.
	for _, q := range []string{"", "ids=", "ids=abc", "ids=0", fmt.Sprintf("ids=%d&until=released", x.ID)} {
		status, out = spawnDo(t, http.MethodGet, h.URL+"/v1/tasks/states?"+q, "")
		if status != http.StatusBadRequest || out["error"] != "invalid_context" {
			t.Fatalf("states %q status=%d out=%v", q, status, out)
		}
	}

	// A task that does not exist is not_found, for both routes' shapes.
	if status, out = spawnDo(t, http.MethodPut, fmt.Sprintf("%s/v1/tasks/%d/spawn", h.URL, x.ID+1000000), waiting); status != http.StatusNotFound || out["error"] != "not_found" {
		t.Fatalf("missing task PUT status=%d out=%v", status, out)
	}
}

// GET /v1/tasks/states/snapshot answers present tasks in request order,
// absent ids ascending, and the snapshot's as_of; every requested id appears
// exactly once. The legacy route keeps its bare array.
func TestTaskStatesSnapshotRoute(t *testing.T) {
	h, st := relaneAPIServer(t)
	lane := relaneLane(t, "states-snapshot")
	first := relaneAPITask(t, st, lane, "first")
	second := relaneAPITask(t, st, lane, "second")
	missingLow, missingHigh := second.ID+1, second.ID+2
	url := fmt.Sprintf("%s/v1/tasks/states/snapshot?ids=%d,%d,%d,%d,%d", h.URL, first.ID, missingHigh, second.ID, missingLow, first.ID)
	status, out := spawnDo(t, http.MethodGet, url, "")
	if status != http.StatusOK {
		t.Fatalf("GET snapshot status=%d out=%v", status, out)
	}
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks=%v", out["tasks"])
	}
	for i, want := range []store.Task{first, second} {
		e, _ := tasks[i].(map[string]any)
		if e == nil || e["id"] != float64(want.ID) || e["state"] != want.State || e["category"] != store.TaskCategory(want.State) {
			t.Fatalf("task[%d]=%v want %+v", i, tasks[i], want)
		}
	}
	missing, _ := out["missing"].([]any)
	if len(missing) != 2 || missing[0] != float64(missingLow) || missing[1] != float64(missingHigh) {
		t.Fatalf("missing=%v", out["missing"])
	}
	asOf, _ := out["as_of"].(string)
	if at, err := time.Parse(time.RFC3339Nano, asOf); err != nil || at.Location() != time.UTC {
		t.Fatalf("as_of=%q err=%v", asOf, err)
	}

	// Malformed id lists and an unknown until are refused as invalid_context.
	for _, q := range []string{"", "ids=", "ids=abc", "ids=0", fmt.Sprintf("ids=%d&until=released", first.ID)} {
		if status, out := spawnDo(t, http.MethodGet, h.URL+"/v1/tasks/states/snapshot?"+q, ""); status != http.StatusBadRequest || out["error"] != "invalid_context" {
			t.Fatalf("snapshot %q status=%d out=%v", q, status, out)
		}
	}
	// One id over the cap is refused with the store's reason, never truncated.
	tooMany := "ids=" + strings.Repeat("1,", store.TaskStatesMax) + "1"
	if status, out := spawnDo(t, http.MethodGet, h.URL+"/v1/tasks/states/snapshot?"+tooMany, ""); status != http.StatusBadRequest || out["error"] != "invalid_context" || out["reason"] == nil || out["reason"] == "" {
		t.Fatalf("snapshot over the cap status=%d out=%v", status, out)
	}
}
