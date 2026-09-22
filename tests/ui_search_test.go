package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type uiSearchHit struct {
	ID      int64  `json:"id"`
	State   string `json:"state"`
	Lane    string `json:"lane"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Exact   bool   `json:"exact"`
}

type uiSearchPayload struct {
	Query   string        `json:"query"`
	Scope   string        `json:"scope"`
	Results []uiSearchHit `json:"results"`
	HasMore bool          `json:"has_more"`
	Limit   int           `json:"limit"`
}

func uiSearchIDs(xs []uiSearchHit) []int64 {
	out := []int64{}
	for _, x := range xs {
		out = append(out, x.ID)
	}
	return out
}

// /ui/api/search is the console's read-only task search: exact id first,
// closed tasks included, cap reported as has_more, Korean queries answered by
// the ILIKE fallback, and title/snippet forwarded as data.
func TestUISearchTasks(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "search")
	tag := fmt.Sprintf("srch%d", time.Now().UnixNano())
	searchURL := func(q string, extra string) string {
		return h.URL + "/ui/api/search?scope=tasks&q=" + url.QueryEscape(q) + extra
	}

	open := createUITask(t, s, lane, tag+" open work")
	merged := claimAndTransition(t, s, createUITask(t, s, lane, tag+" merged work"), "in_progress", "")
	for _, to := range []string{"join", "merged"} {
		var err error
		var refs *store.TaskRefs
		if to == "merged" {
			refs = &store.TaskRefs{PR: dispositionPR(t)}
		}
		if merged, err = s.TransitionTask(t.Context(), merged.ID, to, "test-node", "", refs); err != nil {
			t.Fatal(err)
		}
	}
	dropped := claimAndTransition(t, s, createUITask(t, s, lane, tag+" dropped work"), "dropped", "gone")
	var got uiSearchPayload
	if status := boardJSON(t, h.Client(), searchURL(tag, ""), assertion, &got); status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if got.HasMore || got.Scope != "tasks" || got.Limit != 20 || len(got.Results) != 3 {
		t.Fatalf("payload=%+v", got)
	}
	states := map[int64]string{}
	for _, x := range got.Results {
		states[x.ID] = x.State
		if x.Lane != lane || x.Kind != "implement" || x.Exact {
			t.Fatalf("hit=%+v", x)
		}
	}
	if states[open.ID] != "backlog" || states[merged.ID] != "merged" || states[dropped.ID] != "dropped" {
		t.Fatalf("closed tasks missing or mislabelled: %v", states)
	}

	// "#<id>" and "<id>": the exact task leads the page even when another
	// task's title contains the digits.
	createUITask(t, s, lane, fmt.Sprintf("%s mentions %d in title", tag, merged.ID))
	for _, q := range []string{fmt.Sprintf("#%d", merged.ID), fmt.Sprintf("%d", merged.ID)} {
		got = uiSearchPayload{}
		if status := boardJSON(t, h.Client(), searchURL(q, ""), assertion, &got); status != http.StatusOK {
			t.Fatalf("q=%q status=%d", q, status)
		}
		if len(got.Results) == 0 || got.Results[0].ID != merged.ID || !got.Results[0].Exact || got.Results[0].State != "merged" {
			t.Fatalf("q=%q ids=%v", q, uiSearchIDs(got.Results))
		}
		for _, x := range got.Results[1:] {
			if x.Exact {
				t.Fatalf("q=%q second exact %+v", q, x)
			}
		}
	}
	got = uiSearchPayload{}
	if status := boardJSON(t, h.Client(), searchURL("#999999999999", ""), assertion, &got); status != http.StatusOK || len(got.Results) != 0 || got.HasMore {
		t.Fatalf("missing id status=%d payload=%+v", status, got)
	}

	// The cap is reported, never presented as the whole set.
	capTag := tag + "cap"
	for i := 0; i < 4; i++ {
		createUITask(t, s, lane, fmt.Sprintf("%s row %d", capTag, i))
	}
	got = uiSearchPayload{}
	if status := boardJSON(t, h.Client(), searchURL(capTag, "&limit=3"), assertion, &got); status != http.StatusOK || len(got.Results) != 3 || !got.HasMore {
		t.Fatalf("cap status=%d payload=%+v", status, got)
	}
	got = uiSearchPayload{}
	if status := boardJSON(t, h.Client(), searchURL(capTag, "&limit=4"), assertion, &got); status != http.StatusOK || len(got.Results) != 4 || got.HasMore {
		t.Fatalf("exact-fit status=%d payload=%+v", status, got)
	}

	// Korean two-word and one-word queries (ILIKE fallback).
	deploy := createUITask(t, s, lane, tag+" 배포 대기 작업")
	install := createUITask(t, s, lane, tag+" 설치 절차")
	for q, want := range map[string]int64{"배포 대기": deploy.ID, "설치": install.ID} {
		got = uiSearchPayload{}
		if status := boardJSON(t, h.Client(), searchURL(q, "&limit=50"), assertion, &got); status != http.StatusOK {
			t.Fatalf("q=%q status=%d", q, status)
		}
		found := false
		for _, x := range got.Results {
			found = found || x.ID == want
		}
		if !found {
			t.Fatalf("q=%q ids=%v want %d", q, uiSearchIDs(got.Results), want)
		}
	}

	// Title and snippet are data: markup in a title comes back verbatim (the
	// console renders it as text) and the JSON body is not an HTML document.
	hostile := createUITask(t, s, lane, tag+`xss <script>alert(1)</script> <img src=x onerror=alert(2)> &lt;b&gt;`)
	response := uiRequest(t, h.Client(), http.MethodGet, searchURL(fmt.Sprintf("#%d", hostile.ID), ""), assertion, "")
	if ct := response.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("content-type=%q cache=%q", ct, response.Header.Get("Cache-Control"))
	}
	got = uiSearchPayload{}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(got.Results) != 1 || got.Results[0].Title != hostile.Title || got.Results[0].Snippet != hostile.Title {
		t.Fatalf("hostile payload=%+v", got)
	}

	// Invalid input is 400 with a code, distinct from a store failure.
	long := strings.Repeat("a", 513)
	for _, bad := range []string{
		h.URL + "/ui/api/search?scope=tasks&q=",
		h.URL + "/ui/api/search?scope=tasks&q=%20%20",
		h.URL + "/ui/api/search?scope=tasks&q=" + long,
		h.URL + "/ui/api/search?scope=docs&q=x",
		h.URL + "/ui/api/search?scope=all&q=x",
		searchURL("x", "&limit=0"),
		searchURL("x", "&limit=51"),
		searchURL("x", "&limit=abc"),
		h.URL + "/ui/api/search?scope=tasks&q=%ff",
	} {
		response = uiRequest(t, h.Client(), http.MethodGet, bad, assertion, "")
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status=%d, want 400", bad, response.StatusCode)
		}
		response.Body.Close()
	}
	// scope omitted defaults to tasks.
	got = uiSearchPayload{}
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/search?q="+url.QueryEscape(tag), assertion, &got); status != http.StatusOK || got.Scope != "tasks" || len(got.Results) == 0 {
		t.Fatalf("default scope status=%d payload=%+v", status, got)
	}

	// Authentication boundary: unauthenticated 401, write methods refused,
	// service principals 403 (no /ui/doc-class reading).
	response = uiRequest(t, h.Client(), http.MethodGet, searchURL(tag, ""), "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, h.Client(), http.MethodPost, searchURL(tag, ""), assertion, "")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", response.StatusCode)
	}
	response.Body.Close()
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	response = p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/api/search?scope=tasks&q="+url.QueryEscape(tag), p3ServiceAssertion(t, fixture), nil, "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service status=%d", response.StatusCode)
	}
	response.Body.Close()
}

// Searching is read-only: no task, event, or comment row changes.
func TestUISearchIsReadOnly(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "search-ro")
	task := createUITask(t, s, lane, fmt.Sprintf("readonly%d probe", time.Now().UnixNano()))
	before, _, err := s.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got uiSearchPayload
	for _, q := range []string{fmt.Sprintf("#%d", task.ID), "readonly"} {
		if status := boardJSON(t, h.Client(), h.URL+"/ui/api/search?scope=tasks&q="+url.QueryEscape(q), assertion, &got); status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
	}
	after, _, err := s.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != after.State || before.Priority != after.Priority || !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("search mutated task: before=%+v after=%+v", before, after)
	}
}
