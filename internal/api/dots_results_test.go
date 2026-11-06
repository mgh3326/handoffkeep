package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The Comment payload carries no description: the webhook reads it from
// Linear and hands it, with the issue UUID, to the store for routing.
func TestLinearWebhookDotsDescription(t *testing.T) {
	const issueUUID = "0a1b2c3d-0000-4000-8000-0000000000c1"
	build := func(comment, issue string) []byte {
		return []byte(fmt.Sprintf(`{"action":"create","type":"Comment","webhookTimestamp":%d,"data":{"id":"0a1b2c3d-0000","body":%q,"createdAt":"2026-11-02T01:02:03.000Z","issueId":%q,"issue":%s}}`, time.Now().UnixMilli(), comment, issueUUID, issue))
	}
	const complete = `"identifier":"ZZZ-2","title":"t","url":"u","labels":[{"name":"dots"}],"state":{"type":"started"}`
	for _, tc := range []struct {
		name        string
		issue       string
		fetches     int
		description string
	}{
		{"payload without description", `{` + complete + `}`, 1, "hk-lane: hk-design"},
		{"payload without issue", `null`, 1, "hk-lane: hk-design"},
		{"payload with description", `{` + complete + `,"description":"hk-lane: own"}`, 0, "hk-lane: own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetched := dotsLinearIssue()
			fetched.Description = "hk-lane: hk-design"
			issues, dots := &fakeIssues{issue: fetched}, &fakeDots{}
			body := build("[dots] BLOCKED need a key", tc.issue)
			status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: issues, Store: dots}, body, linearSign(fakeLinearSigningValue, body))
			if status != http.StatusOK || out["status"] != "recorded" || issues.calls != tc.fetches || len(dots.issues) != 1 {
				t.Fatalf("status=%d out=%v fetches=%d dots=%+v", status, out, issues.calls, dots)
			}
			if got := dots.issues[0]; got.Description != tc.description || got.IssueUUID != issueUUID {
				t.Fatalf("issue=%+v", got)
			}
		})
	}
	// A failed description read is retried by Linear, not recorded without it.
	body := build("[dots] RESULT done", `{`+complete+`}`)
	dots := &fakeDots{}
	status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: &fakeIssues{err: errors.New("boom")}, Store: dots}, body, linearSign(fakeLinearSigningValue, body))
	if status != http.StatusServiceUnavailable || out["error"] != "linear_unavailable" || len(dots.results) != 0 {
		t.Fatalf("status=%d out=%v", status, out)
	}
}

// fakeDotsResults answers like the store: id 1 is pending, id 2 superseded,
// any other id is missing.
type fakeDotsResults struct {
	listTask  int64
	listIssue string
	by        string
}

func (f *fakeDotsResults) ListDotsResults(_ context.Context, taskID int64, issue string, _ int) ([]store.DotsResult, error) {
	f.listTask, f.listIssue = taskID, issue
	return []store.DotsResult{{ID: 1, IssueIdentifier: "ZZZ-1", AdoptionState: store.DotsAdoptionPending}}, nil
}

func (f *fakeDotsResults) SetDotsAdoption(_ context.Context, id int64, by string, in store.DotsAdoptionInput) (store.DotsResult, bool, error) {
	f.by = by
	switch {
	case id == 2:
		return store.DotsResult{}, false, store.ErrDotsAdoptionFinal
	case id != 1:
		return store.DotsResult{}, false, store.ErrDotsResultNotFound
	case in.State == store.DotsAdoptionSuperseded && in.SupersededBy == 0:
		return store.DotsResult{}, false, errors.New("invalid dots adoption: superseded_by is required with superseded and only with it")
	}
	return store.DotsResult{ID: 1, AdoptionState: in.State, AdoptedRef: in.Ref}, true, nil
}

func TestDotsResultsRoutes(t *testing.T) {
	fake := &fakeDotsResults{}
	h := httptest.NewServer(Server{Tokens: Tokens{"reviewer": "rev-token"}, Service: Service{Dots: fake}}.Handler())
	defer h.Close()

	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/dots/results?task=7", "rev-token", nil)
	if xs, _ := out["results"].([]any); code != http.StatusOK || len(xs) != 1 || fake.listTask != 7 {
		t.Fatalf("list by task code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/dots/results?issue=ZZZ-1", "rev-token", nil); code != http.StatusOK || fake.listIssue != "ZZZ-1" {
		t.Fatalf("list by issue code=%d body=%v", code, out)
	}
	for _, q := range []string{"", "?task=7&issue=ZZZ-1", "?task=0", "?task=x"} {
		if code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/dots/results"+q, "rev-token", nil); code != http.StatusBadRequest || out["error"] != "invalid_context" {
			t.Fatalf("list %q code=%d body=%v", q, code, out)
		}
	}
	if code, _ = projectRequest(t, h.URL, http.MethodGet, "/v1/dots/results?task=7", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code=%d", code)
	}

	accept := map[string]any{"state": "accepted", "ref": "https://github.com/example/repo/pull/1"}
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/dots/results/1/adoption", "rev-token", accept)
	if res, _ := out["result"].(map[string]any); code != http.StatusOK || out["changed"] != true || res["adoption_state"] != "accepted" || fake.by != "reviewer" {
		t.Fatalf("adopt code=%d body=%v by=%q", code, out, fake.by)
	}
	for _, tc := range []struct {
		path string
		body map[string]any
		code int
		err  string
	}{
		{"/v1/dots/results/9/adoption", accept, http.StatusNotFound, "not_found"},
		{"/v1/dots/results/2/adoption", accept, http.StatusConflict, "dots_adoption_final"},
		{"/v1/dots/results/1/adoption", map[string]any{"state": "superseded"}, http.StatusBadRequest, "invalid_context"},
		{"/v1/dots/results/1/adoption", map[string]any{"state": "accepted", "extra": 1}, http.StatusBadRequest, "invalid_context"},
		{"/v1/dots/results/0/adoption", accept, http.StatusBadRequest, "invalid_context"},
	} {
		if code, out = projectRequest(t, h.URL, http.MethodPost, tc.path, "rev-token", tc.body); code != tc.code || out["error"] != tc.err {
			t.Fatalf("%s %v code=%d body=%v, want %d %s", tc.path, tc.body, code, out, tc.code, tc.err)
		}
	}
}
