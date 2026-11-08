package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// jobsAPIServer runs a real Server against a throwaway schema with three
// clients: a and b share host-a (a also has host-b), c has no host line.
func jobsAPIServer(t *testing.T) *httptest.Server {
	t.Helper()
	_, dbURL := testdb.Schema(t, "jobs_api")
	st, err := store.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Server{
		Service: Service{Store: st},
		Tokens:  Tokens{"a": "tok-a", "b": "tok-b", "c": "tok-c"},
		HostGrants: HostGrants{
			"a": {"host-a": true, "host-b": true},
			"b": {"host-a": true},
		},
	}.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(st.Close)
	return srv
}

func jobsRequest(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func jobsClaim(t *testing.T, srv *httptest.Server, token, jobID, host string, taskID int64) (int, map[string]any) {
	t.Helper()
	return jobsRequest(t, "POST", srv.URL+"/v1/jobs", token,
		fmt.Sprintf(`{"job_id":%q,"task_id":%d,"host":%q}`, jobID, taskID, host))
}

func TestJobsClaim(t *testing.T) {
	srv := jobsAPIServer(t)
	id := fmt.Sprintf("1-builder-%d", time.Now().UnixNano())

	code, body := jobsClaim(t, srv, "tok-a", id, "host-a", 1)
	if code != http.StatusCreated || body["state"] != "claimed" || body["host"] != "host-a" {
		t.Fatalf("claim: %d %v", code, body)
	}

	// An identical replay returns the stored row with 200.
	code, body = jobsClaim(t, srv, "tok-a", id, "host-a", 1)
	if code != http.StatusOK || body["job_id"] != id {
		t.Fatalf("replay: %d %v", code, body)
	}

	future := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	code, body = jobsRequest(t, "POST", srv.URL+"/v1/jobs", "tok-a", fmt.Sprintf(`{"job_id":%q,"task_id":2,"host":"host-a","at":%q}`, id, future))
	if code != http.StatusConflict || body["error"] != "job_id_taken" {
		t.Fatalf("future conflicting claim: %d %v", code, body)
	}

	// A different claim on the taken id conflicts, from either client.
	code, body = jobsClaim(t, srv, "tok-a", id, "host-b", 1)
	if code != http.StatusConflict || body["error"] != "job_id_taken" {
		t.Fatalf("different host: %d %v", code, body)
	}
	code, body = jobsClaim(t, srv, "tok-b", id, "host-a", 2)
	if code != http.StatusConflict || body["error"] != "job_id_taken" {
		t.Fatalf("different task: %d %v", code, body)
	}
}

func TestJobsClaimHostGrants(t *testing.T) {
	srv := jobsAPIServer(t)
	now := time.Now().UnixNano()

	// A host not on the client's line is forbidden.
	code, body := jobsClaim(t, srv, "tok-b", fmt.Sprintf("2-builder-%d", now), "host-b", 2)
	if code != http.StatusForbidden || body["error"] != "job_host_forbidden" {
		t.Fatalf("foreign host: %d %v", code, body)
	}
	// A client with no HOSTS line is forbidden too.
	code, body = jobsClaim(t, srv, "tok-c", fmt.Sprintf("3-builder-%d", now), "host-a", 3)
	if code != http.StatusForbidden || body["error"] != "job_host_forbidden" {
		t.Fatalf("no line: %d %v", code, body)
	}
	// Malformed ids and hosts are validation failures, not grant failures.
	code, _ = jobsClaim(t, srv, "tok-a", "bad/id", "host-a", 4)
	if code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", code)
	}
	code, _ = jobsClaim(t, srv, "tok-a", fmt.Sprintf("4-builder-%d", now), "Host-A", 4)
	if code != http.StatusBadRequest {
		t.Fatalf("bad host: %d", code)
	}
}

func TestJobsUpdateMerge(t *testing.T) {
	srv := jobsAPIServer(t)
	id := fmt.Sprintf("5-builder-%d", time.Now().UnixNano())
	if code, _ := jobsClaim(t, srv, "tok-a", id, "host-a", 5); code != http.StatusCreated {
		t.Fatalf("claim: %d", code)
	}
	url := srv.URL + "/v1/jobs/" + id

	// An unknown field is rejected.
	code, _ := jobsRequest(t, "PUT", url, "tok-a", `{"state":"spawned","brief":"text"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", code)
	}

	// The allowlisted fields merge over the record.
	code, body := jobsRequest(t, "PUT", url, "tok-a",
		`{"state":"spawned","role":"builder","label":"wk","family":"anthropic","pane_id":"%1","worktree":{"origin":"repo","branch":null,"head":"abc123"}}`)
	if code != http.StatusOK || body["state"] != "spawned" || body["role"] != "builder" {
		t.Fatalf("spawned: %d %v", code, body)
	}
	wt, _ := body["worktree"].(map[string]any)
	if wt["origin"] != "repo" || wt["head"] != "abc123" {
		t.Fatalf("worktree: %v", wt)
	}
	if _, present := wt["branch"]; !present {
		t.Fatalf("detached worktree must keep an explicit null branch: %v", wt)
	}

	// Secret-shaped or credential-embedding field values are refused.
	code, body = jobsRequest(t, "PUT", url, "tok-a", `{"label":"AKIAIOSFODNN7EXAMPLE"}`)
	if code != http.StatusBadRequest || body["error"] != "secret_like_content" {
		t.Fatalf("secret label: %d %v", code, body)
	}
	code, _ = jobsRequest(t, "PUT", url, "tok-a",
		`{"worktree":{"origin":"https://user:hunter2@example.invalid/repo","branch":null,"head":"abc123"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("userinfo origin: %d", code)
	}
	code, _ = jobsRequest(t, "PUT", url, "tok-a",
		`{"worktree":{"origin":"/abs/path/repo","branch":null,"head":"abc123"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("path origin: %d", code)
	}

	// A second client granted the same host may write.
	code, _ = jobsRequest(t, "PUT", url, "tok-b", `{"label":"b-wrote"}`)
	if code != http.StatusOK {
		t.Fatalf("same-host client: %d", code)
	}
	// A client granted only other hosts may not.
	code, body = jobsRequest(t, "PUT", url, "tok-c", `{"label":"x"}`)
	if code != http.StatusForbidden || body["error"] != "job_host_forbidden" {
		t.Fatalf("no-line client write: %d %v", code, body)
	}

	// The merged record reads back through GET.
	code, body = jobsRequest(t, "GET", url, "tok-b", "")
	if code != http.StatusOK || body["label"] != "b-wrote" || body["role"] != "builder" {
		t.Fatalf("get: %d %v", code, body)
	}
}

func TestJobsStateMoves(t *testing.T) {
	srv := jobsAPIServer(t)
	now := time.Now().UnixNano()

	// claimed -> spawned -> terminal is the only direction.
	id := fmt.Sprintf("6-builder-%d", now)
	if code, _ := jobsClaim(t, srv, "tok-a", id, "host-a", 6); code != http.StatusCreated {
		t.Fatalf("claim: %d", code)
	}
	url := srv.URL + "/v1/jobs/" + id
	// completed needs a recorded spawn.
	if code, body := jobsRequest(t, "PUT", url, "tok-a", `{"state":"completed"}`); code != http.StatusConflict || body["error"] != "job_state_conflict" {
		t.Fatalf("claimed->completed: %d %v", code, body)
	}
	if code, _ := jobsRequest(t, "PUT", url, "tok-a", `{"state":"spawned"}`); code != http.StatusOK {
		t.Fatalf("spawned: %d", code)
	}
	code, body := jobsRequest(t, "PUT", url, "tok-a", `{"state":"claimed"}`)
	if code != http.StatusConflict || body["error"] != "job_state_conflict" {
		t.Fatalf("backward: %d %v", code, body)
	}
	if code, _ := jobsRequest(t, "PUT", url, "tok-a", `{"state":"completed"}`); code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	// A terminal record is never rewritten — not by a state move and not
	// by a field merge.
	code, _ = jobsRequest(t, "PUT", url, "tok-a", `{"state":"lost"}`)
	if code != http.StatusConflict {
		t.Fatalf("terminal state move: %d", code)
	}
	code, _ = jobsRequest(t, "PUT", url, "tok-a", `{"label":"again"}`)
	if code != http.StatusConflict {
		t.Fatalf("terminal merge: %d", code)
	}
	// An unknown state is a validation error.
	id = fmt.Sprintf("7-builder-%d", now)
	if code, _ := jobsClaim(t, srv, "tok-a", id, "host-a", 7); code != http.StatusCreated {
		t.Fatalf("claim 2: %d", code)
	}
	code, _ = jobsRequest(t, "PUT", srv.URL+"/v1/jobs/"+id, "tok-a", `{"state":"revoked"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown state: %d", code)
	}
}

func TestJobsResultRules(t *testing.T) {
	srv := jobsAPIServer(t)
	now := time.Now().UnixNano()
	claim := func(suffix string) string {
		id := fmt.Sprintf("8-%s-%d", suffix, now)
		if code, _ := jobsClaim(t, srv, "tok-a", id, "host-a", 8); code != http.StatusCreated {
			t.Fatalf("claim %s: %d", suffix, code)
		}
		u := srv.URL + "/v1/jobs/" + id
		if code, _ := jobsRequest(t, "PUT", u, "tok-a", `{"state":"spawned"}`); code != http.StatusOK {
			t.Fatalf("spawn %s: %d", suffix, code)
		}
		return u
	}

	// Result without state=completed is refused.
	url := claim("a")
	code, _ := jobsRequest(t, "PUT", url, "tok-a", `{"result":{"ok":true}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("result without completed: %d", code)
	}

	// An oversized result is refused.
	big := fmt.Sprintf(`{"state":"completed","result":{"pad":%q}}`, strings.Repeat("x", store.JobResultMaxBytes))
	code, _ = jobsRequest(t, "PUT", url, "tok-a", big)
	if code != http.StatusBadRequest {
		t.Fatalf("oversized result: %d", code)
	}

	// A secret-shaped value inside the result is refused like a text
	// attachment — including one hidden behind a JSON unicode escape.
	url = claim("b")
	code, body := jobsRequest(t, "PUT", url, "tok-a",
		`{"state":"completed","result":{"finding":"AKIAIOSFODNN7EXAMPLE"}}`)
	if code != http.StatusBadRequest || body["error"] != "secret_like_content" {
		t.Fatalf("secret result: %d %v", code, body)
	}
	url = claim("b2")
	code, body = jobsRequest(t, "PUT", url, "tok-a",
		`{"state":"completed","result":{"finding":"AKIAIOSFODNN7EXAMPL\u0045"}}`)
	if code != http.StatusBadRequest || body["error"] != "secret_like_content" {
		t.Fatalf("escaped secret result: %d %v", code, body)
	}
	// A NUL escape is a 400, not a database error.
	url = claim("b3")
	code, _ = jobsRequest(t, "PUT", url, "tok-a",
		`{"state":"completed","result":{"finding":"a\u0000b"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("nul result: %d", code)
	}

	// One completed write with a result succeeds; a second write conflicts.
	url = claim("c")
	code, _ = jobsRequest(t, "PUT", url, "tok-a",
		`{"state":"completed","report_doc":"reports/job/summary","result":{"findings":0}}`)
	if code != http.StatusOK {
		t.Fatalf("completed with result: %d", code)
	}
	code, _ = jobsRequest(t, "PUT", url, "tok-a",
		`{"state":"completed","result":{"findings":1}}`)
	if code != http.StatusConflict {
		t.Fatalf("second result: %d", code)
	}
}

func TestJobsGet(t *testing.T) {
	srv := jobsAPIServer(t)
	now := time.Now().UnixNano()
	id := fmt.Sprintf("9-builder-%d", now)
	if code, _ := jobsClaim(t, srv, "tok-a", id, "host-a", 9); code != http.StatusCreated {
		t.Fatalf("claim: %d", code)
	}
	if code, _ := jobsClaim(t, srv, "tok-b", fmt.Sprintf("9-reviewer-%d", now), "host-a", 9); code != http.StatusCreated {
		t.Fatalf("claim 2: %d", code)
	}
	// A job on another task must not leak into the list.
	if code, _ := jobsClaim(t, srv, "tok-a", fmt.Sprintf("9-other-%d", now), "host-a", 10); code != http.StatusCreated {
		t.Fatalf("claim 3: %d", code)
	}

	code, body := jobsRequest(t, "GET", srv.URL+"/v1/jobs/"+id, "tok-c", "")
	if code != http.StatusOK || body["job_id"] != id || body["task_id"] != float64(9) {
		t.Fatalf("get one: %d %v", code, body)
	}
	code, body = jobsRequest(t, "GET", srv.URL+"/v1/jobs/"+id+"-missing", "tok-a", "")
	if code != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("get missing: %d %v", code, body)
	}
	code, body = jobsRequest(t, "GET", srv.URL+"/v1/jobs?task=9", "tok-c", "")
	jobs, _ := body["jobs"].([]any)
	if code != http.StatusOK || len(jobs) != 2 {
		t.Fatalf("list: %d %v", code, body)
	}
	code, _ = jobsRequest(t, "GET", srv.URL+"/v1/jobs", "tok-a", "")
	if code != http.StatusBadRequest {
		t.Fatalf("no task param: %d", code)
	}
}

// Token usage (hk 3005): null is unknown, the object replaces as a whole,
// and it is the one field a terminal job still accepts, since usage is read
// at completion or reap.
func TestJobsUsage(t *testing.T) {
	srv := jobsAPIServer(t)
	id := fmt.Sprintf("8-builder-%d", time.Now().UnixNano())
	if code, _ := jobsClaim(t, srv, "tok-a", id, "host-a", 8); code != http.StatusCreated {
		t.Fatalf("claim: %d", code)
	}
	url := srv.URL + "/v1/jobs/" + id
	if _, body := jobsRequest(t, "GET", url, "tok-a", ""); body["usage"] != nil {
		t.Fatalf("usage before any write: %v", body["usage"])
	}
	for _, step := range []string{`{"state":"spawned"}`, `{"state":"completed"}`} {
		if code, body := jobsRequest(t, "PUT", url, "tok-a", step); code != http.StatusOK {
			t.Fatalf("%s: %d %v", step, code, body)
		}
	}
	code, body := jobsRequest(t, "PUT", url, "tok-a", `{"usage":{"input_tokens":1200,"output_tokens":340,"reasoning_tokens":null,"cache_read_tokens":9000,"source":"claude-session-jsonl","recorded_at":"2000-01-01T00:00:00Z"}}`)
	if code != http.StatusOK {
		t.Fatalf("usage on a completed job: %d %v", code, body)
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1200) || usage["output_tokens"] != float64(340) || usage["cache_read_tokens"] != float64(9000) ||
		usage["reasoning_tokens"] != nil || usage["cache_write_tokens"] != nil || usage["source"] != "claude-session-jsonl" {
		t.Fatalf("usage: %v", usage)
	}
	if at, _ := usage["recorded_at"].(string); at == "" || strings.HasPrefix(at, "2000") {
		t.Fatalf("recorded_at must be the server's: %v", usage["recorded_at"])
	}
	// A later, fuller read replaces the whole object.
	code, body = jobsRequest(t, "PUT", url, "tok-a", `{"usage":{"input_tokens":1500,"reasoning_tokens":12}}`)
	usage, _ = body["usage"].(map[string]any)
	if code != http.StatusOK || usage["input_tokens"] != float64(1500) || usage["reasoning_tokens"] != float64(12) || usage["output_tokens"] != nil || usage["source"] != nil {
		t.Fatalf("replace: %d %v", code, usage)
	}
	if _, got := jobsRequest(t, "GET", url, "tok-b", ""); got["usage"].(map[string]any)["input_tokens"] != float64(1500) {
		t.Fatalf("GET usage: %v", got["usage"])
	}
	// Usage does not reopen a terminal job for anything else.
	if code, _ := jobsRequest(t, "PUT", url, "tok-a", `{"usage":{"input_tokens":1},"label":"again"}`); code != http.StatusConflict {
		t.Fatalf("terminal usage plus a field: %d", code)
	}
	for _, bad := range []string{`{"usage":{"input_tokens":-1}}`, `{"usage":{"tokens":5}}`, `{"usage":{"source":"` + strings.Repeat("s", 65) + `"}}`} {
		if code, _ := jobsRequest(t, "PUT", url, "tok-a", bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
}

func TestTasklessJob(t *testing.T) {
	srv := jobsAPIServer(t)
	url := srv.URL + "/v1/jobs"
	claim := `{"job_id":"taskless","host":"host-a"}`
	code, body := jobsRequest(t, "POST", url, "tok-a", claim)
	if code != http.StatusCreated || body["task_id"] != nil {
		t.Fatalf("claim: %d %v", code, body)
	}
	if code, body = jobsRequest(t, "POST", url, "tok-a", claim); code != http.StatusOK {
		t.Fatalf("replay: %d %v", code, body)
	}
	if code, body = jobsRequest(t, "PUT", url+"/taskless", "tok-a", `{"summary":{"text":"x"}}`); code != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("summary input: %d %v", code, body)
	}
	for _, step := range []string{`{"state":"spawned","role":"builder"}`, `{"state":"completed"}`} {
		if code, body = jobsRequest(t, "PUT", url+"/taskless", "tok-a", step); code != http.StatusOK {
			t.Fatalf("update: %d %v", code, body)
		}
	}
	if code, body = jobsRequest(t, "POST", url, "tok-a", `{"job_id":"bad-task","host":"host-a","task_id":0}`); code != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("bad task: %d %v", code, body)
	}
}
func TestJobSkillsReport(t *testing.T) {
	srv := jobsAPIServer(t)
	code, body := jobsRequest(t, "GET", srv.URL+"/v1/job-skills?since=bad", "tok-a", "")
	if code != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("%d %v", code, body)
	}
}

// A job records why panewire chose its model (hk 3516): any JSON object up to
// 32 KiB, replaced as a whole, unknown keys kept.
func TestJobsSelection(t *testing.T) {
	srv := jobsAPIServer(t)
	id := fmt.Sprintf("9-builder-%d", time.Now().UnixNano())
	if code, _ := jobsClaim(t, srv, "tok-a", id, "host-a", 9); code != http.StatusCreated {
		t.Fatalf("claim: %d", code)
	}
	url := srv.URL + "/v1/jobs/" + id
	if _, body := jobsRequest(t, "GET", url, "tok-a", ""); body["selection"] != nil {
		t.Fatalf("selection before any write: %v", body["selection"])
	}
	put := `{"state":"spawned","selection":{"chosen":{"model_id":"gpt-6.1-terra","why":"waste_risk tier=waste grade=A+ for task B"},"unknown_key":[1,2]}}`
	code, body := jobsRequest(t, "PUT", url, "tok-a", put)
	sel, _ := body["selection"].(map[string]any)
	if code != http.StatusOK || sel["unknown_key"] == nil || sel["chosen"].(map[string]any)["model_id"] != "gpt-6.1-terra" {
		t.Fatalf("put selection: %d %v", code, body)
	}
	if _, got := jobsRequest(t, "GET", url, "tok-b", ""); got["selection"].(map[string]any)["unknown_key"] == nil {
		t.Fatalf("GET selection: %v", got["selection"])
	}
	for name, bad := range map[string]string{
		"null":      `{"selection":null}`, // there is no way to clear a selection
		"array":     `{"selection":[]}`,
		"string":    `{"selection":"x"}`,
		"too large": `{"selection":{"pad":"` + strings.Repeat("x", store.JobSelectionMaxBytes) + `"}}`,
	} {
		code, body := jobsRequest(t, "PUT", url, "tok-a", bad)
		if code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 (%v)", name, code, body)
		}
	}
	if code, body := jobsRequest(t, "PUT", url, "tok-a", `{"selection":{"a":"AKIA`+strings.Repeat("A", 16)+`"}}`); code != http.StatusBadRequest || body["error"] != "secret_like_content" {
		t.Fatalf("secret selection: %d %v", code, body)
	}
	if _, got := jobsRequest(t, "GET", url, "tok-a", ""); got["selection"].(map[string]any)["unknown_key"] == nil {
		t.Fatalf("a refused write changed the selection: %v", got["selection"])
	}
}
