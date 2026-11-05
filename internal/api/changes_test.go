package api

import (
	"bytes"
	"context"
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

const changeTestSHA = "1111111111111111111111111111111111111111"

func changesServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	_, dbURL := testdb.Schema(t, "changes_api")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"operator": "op-token", "operator-desk": "desk-token", "peer": "peer-token"}}.Handler())
	t.Cleanup(h.Close)
	t.Cleanup(st.Close)
	return h, st
}

func changesCall(t *testing.T, h *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// changeEvent posts a mergewake event through the API and returns the change
// id it wrote.
func changeEvent(t *testing.T, h *httptest.Server, st *store.Store, ev map[string]any) {
	t.Helper()
	if status, body := changesCall(t, h, http.MethodPost, "/v1/task-events", "peer-token", ev); status != http.StatusOK {
		t.Fatalf("event %v: %d %v", ev, status, body)
	}
}

func changeRepoName(name string) string {
	return fmt.Sprintf("example/hk3037-%s-%d", name, time.Now().UnixNano())
}

func changeIDFor(t *testing.T, h *httptest.Server, pr string) int64 {
	t.Helper()
	status, body := changesCall(t, h, http.MethodGet, "/v1/changes?pr="+pr, "peer-token", nil)
	list, _ := body["changes"].([]any)
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("change for %s: %d %v", pr, status, body)
	}
	return int64(list[0].(map[string]any)["id"].(float64))
}

func TestChangeMergeFlow(t *testing.T) {
	h, st := changesServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "flow"), "flow target")
	repo := changeRepoName("flow")
	prA, prB := fmt.Sprintf("https://github.com/%s/pull/1", repo), fmt.Sprintf("https://github.com/%s/pull/2", repo)
	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": prA, "task_id": x.ID})
	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": prB, "task_id": x.ID})
	a, b := changeIDFor(t, h, prA), changeIDFor(t, h, prB)
	path := func(id int64) string { return fmt.Sprintf("/v1/changes/%d/merge-flow", id) }

	for _, token := range []string{"op-token", "desk-token"} {
		status, body := changesCall(t, h, http.MethodPost, path(a), token, map[string]any{"priority": 3, "hold_reason": "wait for B", "depends_on_change": b, "depends_on_state": "merged"})
		if status != http.StatusOK || body["priority"] != float64(3) || body["hold_reason"] != "wait for B" || body["depends_on_change_id"] != float64(b) || body["depends_on_state"] != "merged" {
			t.Fatalf("%s: %d %v", token, status, body)
		}
	}
	if status, body := changesCall(t, h, http.MethodPost, path(a), "peer-token", map[string]any{"priority": 9}); status != http.StatusForbidden || body["error"] != "operator_required" {
		t.Fatalf("peer: %d %v", status, body)
	}
	// A depends on B, so B on A closes a cycle.
	if status, body := changesCall(t, h, http.MethodPost, path(b), "op-token", map[string]any{"depends_on_change": a, "depends_on_state": "deployed"}); status != http.StatusConflict || body["error"] != "change_dependency_cycle" {
		t.Fatalf("cycle: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodPost, path(a), "op-token", map[string]any{"depends_on_change": a, "depends_on_state": "merged"}); status == http.StatusOK {
		t.Fatalf("self dependency accepted: %v", body)
	}
	if status, body := changesCall(t, h, http.MethodPost, path(987654321), "op-token", map[string]any{"priority": 1}); status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("unknown id: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodPost, path(a), "op-token", map[string]any{"depends_on_change": b, "depends_on_state": "open"}); status != http.StatusBadRequest {
		t.Fatalf("bad depends_on_state: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodPost, path(a), "op-token", map[string]any{"priority": 1, "unknown": true}); status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodPost, "/v1/changes/zero/merge-flow", "op-token", map[string]any{}); status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("bad id: %d %v", status, body)
	}
	// The refused writes changed nothing; an omitted field is cleared.
	status, body := changesCall(t, h, http.MethodPost, path(a), "op-token", map[string]any{"priority": 7})
	if status != http.StatusOK || body["priority"] != float64(7) {
		t.Fatalf("replace: %d %v", status, body)
	}
	for _, key := range []string{"hold_reason", "depends_on_change_id", "depends_on_state"} {
		if _, present := body[key]; present {
			t.Errorf("%s not cleared: %v", key, body[key])
		}
	}
	if status, body = changesCall(t, h, http.MethodPost, path(a), "op-token", map[string]any{"hold_reason": strings.Repeat("x", 301)}); status != http.StatusBadRequest {
		t.Fatalf("long hold_reason: %d %v", status, body)
	}
}

func TestChangesRead(t *testing.T) {
	h, st := changesServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "read"), "read target")
	y := relaneAPITask(t, st, relaneLane(t, "read"), "read other")
	repo := changeRepoName("read")
	pr := fmt.Sprintf("https://github.com/%s/pull/1", repo)
	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": x.ID, "head_sha": changeTestSHA})
	changeEvent(t, h, st, map[string]any{"source": "mergewake.review", "source_ref": pr, "task_id": x.ID, "reviews": []any{
		map[string]any{"reviewed_head": changeTestSHA, "round": 1, "pass": 1, "lens": "correctness", "profile": "sonnet@high", "high": 2},
	}})
	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": y.ID})

	status, body := changesCall(t, h, http.MethodGet, fmt.Sprintf("/v1/changes?task=%d", x.ID), "peer-token", nil)
	list, _ := body["changes"].([]any)
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("?task=: %d %v", status, body)
	}
	c := list[0].(map[string]any)
	if c["pr"] != pr || c["state"] != "open" || c["head_sha"] != changeTestSHA || c["repo"] != repo {
		t.Fatalf("change %v", c)
	}
	if tasks, _ := c["tasks"].([]any); len(tasks) != 2 || tasks[0].(map[string]any)["relation"] != "implements" {
		t.Fatalf("tasks %v", c["tasks"])
	}
	reviews, _ := c["reviews"].([]any)
	if len(reviews) != 1 || reviews[0].(map[string]any)["lens"] != "correctness" || reviews[0].(map[string]any)["high"] != float64(2) || reviews[0].(map[string]any)["profile"] != "sonnet@high" {
		t.Fatalf("reviews %v", c["reviews"])
	}

	status, body = changesCall(t, h, http.MethodGet, "/v1/changes?pr="+pr, "peer-token", nil)
	if list, _ = body["changes"].([]any); status != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["id"] != c["id"] {
		t.Fatalf("?pr=: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodGet, "/v1/changes?pr="+pr+"9", "peer-token", nil)
	if list, ok := body["changes"].([]any); status != http.StatusOK || !ok || len(list) != 0 {
		t.Fatalf("unknown PR: %d %v", status, body)
	}
	for _, q := range []string{"", "?task=1&pr=" + pr, "?pr=not-a-url", "?task=0", "?task=x", "?task=1&limit=0"} {
		if status, body = changesCall(t, h, http.MethodGet, "/v1/changes"+q, "peer-token", nil); status != http.StatusBadRequest || body["error"] != "invalid_context" {
			t.Errorf("%q: %d %v", q, status, body)
		}
	}
	if status, _ = changesCall(t, h, http.MethodGet, "/v1/changes?pr="+pr, "wrong", nil); status != http.StatusUnauthorized {
		t.Errorf("bad token: %d", status)
	}
}

func TestTasksByChange(t *testing.T) {
	h, st := changesServer(t)
	lane := relaneLane(t, "bychange")
	x, y := relaneAPITask(t, st, lane, "first"), relaneAPITask(t, st, lane, "second")
	other := relaneAPITask(t, st, lane, "unrelated")
	repo := changeRepoName("bychange")
	pr := fmt.Sprintf("https://github.com/%s/pull/1", repo)
	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": y.ID})
	changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": pr, "task_id": x.ID})
	id := changeIDFor(t, h, pr)

	status, body := changesCall(t, h, http.MethodGet, fmt.Sprintf("/v1/tasks?change=%d", id), "peer-token", nil)
	tasks, _ := body["tasks"].([]any)
	if status != http.StatusOK || body["change"] != float64(id) || len(tasks) != 2 {
		t.Fatalf("?change=: %d %v", status, body)
	}
	if first, second := tasks[0].(map[string]any)["id"], tasks[1].(map[string]any)["id"]; first != float64(x.ID) || second != float64(y.ID) {
		t.Fatalf("order %v %v, want %d %d (id order; %d is unrelated)", first, second, x.ID, y.ID, other.ID)
	}
	status, body = changesCall(t, h, http.MethodGet, fmt.Sprintf("/v1/tasks?change=%d", id+1000), "peer-token", nil)
	if tasks, ok := body["tasks"].([]any); status != http.StatusOK || !ok || len(tasks) != 0 || body["change"] != float64(id+1000) {
		t.Fatalf("unknown change: %d %v", status, body)
	}
	for _, extra := range []string{"lane=" + lane, "state=reviewing", "parent_lane=x", "project=p", "area=a", "stalled=true", "after_id=1"} {
		if status, body = changesCall(t, h, http.MethodGet, fmt.Sprintf("/v1/tasks?change=%d&%s", id, extra), "peer-token", nil); status != http.StatusBadRequest || body["error"] != "invalid_context" {
			t.Errorf("change with %s: %d %v", extra, status, body)
		}
	}
	if status, body = changesCall(t, h, http.MethodGet, "/v1/tasks?change=0", "peer-token", nil); status != http.StatusBadRequest {
		t.Errorf("change=0: %d %v", status, body)
	}
}

// TestTaskEventPayloadFitsBodyEvent: sixteen realistic review items stay
// inside bodyEvent, so the cap is not raised for them.
func TestTaskEventPayloadFitsBodyEvent(t *testing.T) {
	yes, n := true, 3
	rv := store.TaskEventReview{
		ReviewedHead: changeTestSHA, ReviewedBase: changeTestSHA, PatchID: strings.Repeat("a", 40), Round: 2, Pass: 3, Lens: "correctness",
		ReviewerJobID: "job-20261105-abcdef", Family: "anthropic", Profile: "sonnet@high", Model: "claude-sonnet-5-5", Harness: "claude-code",
		Effort: "high", Executor: "headless", Fallback: &yes, PolicyRevision: "policy-2026-11-05.3", ContractSource: "pr_body", Verdict: "changes_requested",
		Critical: &n, High: &n, Medium: &n, Low: &n,
	}
	now := time.Now().UTC()
	rv.StartedAt, rv.FinishedAt = &now, &now
	in := store.TaskSourceEvent{Source: "mergewake.review", SourceRef: "https://github.com/example/app/pull/1", TaskID: 3037, At: &now, HeadSHA: changeTestSHA, BaseSHA: changeTestSHA}
	for range 16 {
		in.Reviews = append(in.Reviews, rv)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= bodyEvent {
		t.Fatalf("16 reviews take %d bytes, bodyEvent is %d", len(raw), bodyEvent)
	}
	var back store.TaskSourceEvent
	req := httptest.NewRequest(http.MethodPost, "/v1/task-events", bytes.NewReader(raw))
	if err = decode(req, &back, bodyEvent); err != nil || len(back.Reviews) != 16 {
		t.Fatalf("decode: %v reviews=%d", err, len(back.Reviews))
	}
}
