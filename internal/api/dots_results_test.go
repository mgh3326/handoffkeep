package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

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
