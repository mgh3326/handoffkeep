package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// bench_reps outcome columns (hk 2886): a rep may carry them on insert, and a
// later PATCH fills only the ones still unknown; a different value on a set
// column is 409 naming the column, and nothing of that patch is written.
func TestBenchRepOutcomePatch(t *testing.T) {
	srv := jobsAPIServer(t)
	origin := time.Now().UnixNano() % 1_000_000_000
	put := fmt.Sprintf(`{"reps":[{"origin_id":%d,"profile":"opus","task_ref":"hk:task/2886","role":"impl","recorded_at":"2026-11-05T00:00:00Z","experiment_id":"exp-1","arm":"arm","input_tokens":100}]}`, origin)
	code, body := jobsRequest(t, "PUT", srv.URL+"/v1/bench/reps", "tok-a", put)
	ids, _ := body["ids"].([]any)
	if code != http.StatusOK || len(ids) != 1 {
		t.Fatalf("put: %d %v", code, body)
	}
	url := fmt.Sprintf("%s/v1/bench/reps/%d", srv.URL, int64(ids[0].(float64)))
	// Resending the identical rep stays idempotent with the new columns.
	if code, body := jobsRequest(t, "PUT", srv.URL+"/v1/bench/reps", "tok-a", put); code != http.StatusOK {
		t.Fatalf("resend: %d %v", code, body)
	}
	if code, body := jobsRequest(t, "PUT", srv.URL+"/v1/bench/reps", "tok-a", strings.Replace(put, `"arm":"arm"`, `"arm":"baseline"`, 1)); code != http.StatusConflict || body["error"] != "bench_rep_conflict" {
		t.Fatalf("different arm on insert: %d %v", code, body)
	}

	code, body = jobsRequest(t, "PATCH", url, "tok-b", `{"merged":true,"high_findings":2,"first_ci_green":false,"output_tokens":40}`)
	if code != http.StatusOK || body["merged"] != true || body["high_findings"] != float64(2) || body["first_ci_green"] != false ||
		body["output_tokens"] != float64(40) || body["input_tokens"] != float64(100) || body["reverted_within_7d"] != nil || body["experiment_id"] != "exp-1" {
		t.Fatalf("patch: %d %v", code, body)
	}
	// The same value again is a no-op; unknown stays unknown.
	if code, body := jobsRequest(t, "PATCH", url, "tok-a", `{"merged":true,"arm":"arm"}`); code != http.StatusOK || body["merged"] != true {
		t.Fatalf("same value: %d %v", code, body)
	}
	// A different value on a set column rejects the whole patch.
	code, body = jobsRequest(t, "PATCH", url, "tok-a", `{"reverted_within_7d":true,"high_findings":3}`)
	if code != http.StatusConflict || body["error"] != "bench_rep_outcome_conflict" || body["field"] != "high_findings" {
		t.Fatalf("conflict: %d %v", code, body)
	}
	if _, got := jobsRequest(t, "PATCH", url, "tok-a", `{"spawn_to_merge_s":600}`); got["reverted_within_7d"] != nil || got["spawn_to_merge_s"] != float64(600) {
		t.Fatalf("a rejected patch wrote a column: %v", got)
	}
	for _, tc := range []struct {
		url, body string
		want      int
	}{
		{url, `{}`, http.StatusBadRequest},
		{url, `{"arm":"treatment"}`, http.StatusBadRequest},
		{url, `{"critical_findings":-1}`, http.StatusBadRequest},
		{url, `{"profile":"other"}`, http.StatusBadRequest},
		{srv.URL + "/v1/bench/reps/x", `{"merged":true}`, http.StatusBadRequest},
		{srv.URL + "/v1/bench/reps/999999999999", `{"merged":true}`, http.StatusNotFound},
	} {
		if code, body := jobsRequest(t, "PATCH", tc.url, "tok-a", tc.body); code != tc.want {
			t.Errorf("%s %s: %d %v, want %d", tc.url, tc.body, code, body, tc.want)
		}
	}
}
