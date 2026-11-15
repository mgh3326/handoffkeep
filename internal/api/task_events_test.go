package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func taskEventPost(t *testing.T, h *httptest.Server, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.URL+"/v1/task-events", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestTaskEventsAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "events"), "event target")
	pr := fmt.Sprintf("https://github.com/example/app/pull/%d", x.ID)

	// Applied: an unclaimed backlog task moves to reviewing.
	status, body := taskEventPost(t, h, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": x.ID})
	if status != http.StatusOK || body["applied"] != true || body["redelivered"] != false || body["state"] != "reviewing" {
		t.Fatalf("applied: %d %v", status, body)
	}
	// Redelivered: the same event changes nothing.
	status, body = taskEventPost(t, h, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": x.ID})
	if status != http.StatusOK || body["applied"] != false || body["redelivered"] != true || body["state"] != "reviewing" {
		t.Fatalf("redelivered: %d %v", status, body)
	}
	// Not applied: a revert of a task that never merged is recorded only.
	status, body = taskEventPost(t, h, map[string]any{"source": "mergewake.revert", "source_ref": pr + "0", "reverted_pr": pr, "task_id": x.ID})
	if status != http.StatusOK || body["applied"] != false || body["redelivered"] != false || body["state"] != "reviewing" || body["reason"] == "" {
		t.Fatalf("not applied: %d %v", status, body)
	}
	got, _, err := st.GetTask(context.Background(), x.ID)
	if err != nil || got.State != "reviewing" {
		t.Fatalf("task after events: %s %v", got.State, err)
	}
	for _, tc := range []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"unknown source", map[string]any{"source": "mergewake.closed", "source_ref": pr, "task_id": x.ID}, http.StatusBadRequest, "invalid_context"},
		{"bad url", map[string]any{"source": "mergewake.merge", "source_ref": "not-a-url", "task_id": x.ID}, http.StatusBadRequest, "invalid_context"},
		{"unknown field", map[string]any{"source": "mergewake.merge", "source_ref": pr, "task_id": x.ID, "extra": 1}, http.StatusBadRequest, "invalid_context"},
		{"missing task", map[string]any{"source": "mergewake.merge", "source_ref": pr, "task_id": 999999999}, http.StatusNotFound, "not_found"},
	} {
		status, body := taskEventPost(t, h, tc.body)
		if status != tc.status || body["error"] != tc.code {
			t.Fatalf("%s: %d %v, want %d %s", tc.name, status, body, tc.status, tc.code)
		}
	}

	// hk 3200: store validation errors return 400 invalid_context with store-authored reason.
	status, body = taskEventPost(t, h, map[string]any{
		"source":     "mergewake.review",
		"source_ref": pr,
		"task_id":    x.ID,
		"reviews": []any{
			map[string]any{
				"reviewed_head": "1111111111111111111111111111111111111111",
				"patch_id":      "not-a-valid-hex-patch-id!",
				"round":         1,
				"pass":          1,
				"lens":          "correctness",
			},
		},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_context" || body["reason"] != "reviews[0]: patch_id must be 7 to 64 lower-case hex digits" {
		t.Fatalf("hk 3200 invalid review patch_id: %d %v", status, body)
	}
}

func TestTasksStalledFilterAndSweepPreviewAPI(t *testing.T) {
	h, _ := relaneAPIServer(t)
	get := func(path string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, body := get("/v1/tasks?stalled=true"); status != http.StatusOK || body["stalled"] != true {
		t.Fatalf("stalled list: %d %v", status, body)
	}
	if status, body := get("/v1/tasks?stalled=maybe"); status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("bad stalled: %d %v", status, body)
	}
	if status, body := get("/v1/tasks/sweep?days=14"); status != http.StatusOK || body["dry_run"] != true || body["days"] != float64(14) {
		t.Fatalf("sweep preview: %d %v", status, body)
	}
	for _, q := range []string{"", "?days=0", "?days=x", "?days=3651", "?days=106752"} {
		if status, body := get("/v1/tasks/sweep" + q); status != http.StatusBadRequest || body["error"] != "invalid_context" {
			t.Fatalf("sweep %q: %d %v", q, status, body)
		}
	}
}

func TestSweepPreviewRefusesOutOfRangeDaysAPI(t *testing.T) {
	// No store behind the server: each request must be refused before the
	// service reads anything.
	h := areaNoDBServer(t)
	for _, q := range []string{"?days=-1", "?days=0", "?days=3651", "?days=106752"} {
		req, _ := http.NewRequest(http.MethodGet, h.URL+"/v1/tasks/sweep"+q, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_context" {
			t.Fatalf("%s: %d %v", q, resp.StatusCode, out)
		}
	}
}

func TestTasksDriftRefusesBadMergedDaysAPI(t *testing.T) {
	// No store behind the server: each request must be refused before the
	// service reads anything.
	h := areaNoDBServer(t)
	for _, q := range []string{"?merged_days=0", "?merged_days=-1", "?merged_days=3651", "?merged_days=x"} {
		req, _ := http.NewRequest(http.MethodGet, h.URL+"/v1/tasks/drift"+q, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_context" {
			t.Fatalf("%s: %d %v", q, resp.StatusCode, out)
		}
	}
}

func TestTasksHygieneAPI(t *testing.T) {
	h := areaNoDBServer(t)

	post := func(path string, body any, token string) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, h.URL+path, bytes.NewReader(raw))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Unauthenticated request returns 401
	status, body := post("/v1/tasks/hygiene", map[string]any{}, "")
	if status != http.StatusUnauthorized || body["error"] != "unauthorized" {
		t.Fatalf("unauthorized: %d %v", status, body)
	}

	// Unknown field returns 400 invalid_context
	status, body = post("/v1/tasks/hygiene", map[string]any{"bad_field": 123}, "tok")
	if status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("unknown field: %d %v", status, body)
	}
}

func TestTasksHygieneWithStoreAPI(t *testing.T) {
	h, _ := relaneAPIServer(t)
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, h.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Default dry_run is true
	status, body := post("/v1/tasks/hygiene", map[string]any{"project": "test-p"})
	if status != http.StatusOK {
		t.Fatalf("hygiene default dry_run: %d %v", status, body)
	}
	if body["dry_run"] != true || body["project"] != "test-p" {
		t.Fatalf("hygiene body: %v", body)
	}
}
