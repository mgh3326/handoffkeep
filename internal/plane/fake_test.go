package plane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePlane is an in-memory Plane REST server. It speaks the workspace-scoped
// /api/v1 envelope the client emits and counts requests by operation so tests
// can prove exactly which remote calls a drain or reconcile made.
type fakePlane struct {
	t         *testing.T
	server    *httptest.Server
	workspace string
	mu        sync.Mutex
	counts    map[string]int
	failures  map[string][]string
	always    map[string]string
	projects  []Project
	states    []State
	labels    []Label
	issues    map[string]*Issue
	nextID    int
}

func newFakePlane(t *testing.T) *fakePlane {
	t.Helper()
	fake := &fakePlane{
		t:         t,
		workspace: "ws-test",
		counts:    map[string]int{},
		failures:  map[string][]string{},
		always:    map[string]string{},
		projects: []Project{
			{ID: "proj-exp", Identifier: "EXP", Name: "Experiment"},
			{ID: "proj-hk", Identifier: "HK", Name: "handoffkeep"},
		},
		states: []State{
			{ID: "st-backlog", Name: "Backlog", Group: "backlog"},
			{ID: "st-progress", Name: "In Progress", Group: "started"},
			{ID: "st-done", Name: "Done", Group: "completed"},
			{ID: "st-cancelled", Name: "Cancelled", Group: "cancelled"},
		},
		labels: []Label{
			{ID: "lb-lane", Name: "lane:builder-lane"},
			{ID: "lb-kind", Name: "kind:implement"},
		},
		issues: map[string]*Issue{},
		nextID: 1,
	}
	mux := http.NewServeMux()
	base := "/api/v1/workspaces/" + fake.workspace
	mux.HandleFunc(base+"/projects/", func(w http.ResponseWriter, r *http.Request) {
		fake.route(w, r, base+"/projects")
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakePlane) failure(operation string) string {
	if mode := fake.always[operation]; mode != "" {
		return mode
	}
	queue := fake.failures[operation]
	if len(queue) == 0 {
		return ""
	}
	mode := queue[0]
	fake.failures[operation] = queue[1:]
	return mode
}

func listBody(items any) map[string]any {
	return map[string]any{
		"grouped_by":         nil,
		"next_cursor":        "",
		"prev_cursor":        "",
		"next_page_results":  false,
		"prev_page_results":  false,
		"count":              0,
		"total_pages":        1,
		"total_results":      0,
		"extra_stats":        nil,
		"results":            items,
		"total_count":        0,
		"sub_grouped_by":     nil,
		"subscribed_filters": []string{},
	}
}

func (fake *fakePlane) route(w http.ResponseWriter, r *http.Request, base string) {
	if r.Header.Get("X-API-Key") == "" {
		http.Error(w, "missing key", http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, base+"/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	w.Header().Set("Content-Type", "application/json")
	if rest == "" || rest == "/" {
		fake.respond(w, r, string(OperationProjectList), func() (int, any) {
			return http.StatusOK, listBody(fake.allProjects())
		})
		return
	}
	if len(parts) < 2 {
		http.Error(w, "bad path "+r.URL.Path, http.StatusBadRequest)
		return
	}
	projectID, collection := parts[0], parts[1]
	switch collection {
	case "states":
		fake.respond(w, r, string(OperationStateList), func() (int, any) {
			return http.StatusOK, listBody(fake.allStates())
		})
	case "labels":
		fake.respond(w, r, string(OperationLabelList), func() (int, any) {
			return http.StatusOK, listBody(fake.allLabels())
		})
	case "issues":
		fake.routeIssues(w, r, projectID, parts[2:])
	default:
		http.Error(w, "bad collection "+collection, http.StatusBadRequest)
	}
}

func (fake *fakePlane) routeIssues(w http.ResponseWriter, r *http.Request, projectID string, parts []string) {
	if len(parts) == 0 || parts[0] == "" {
		switch r.Method {
		case http.MethodGet:
			fake.respond(w, r, string(OperationIssueList), func() (int, any) {
				return http.StatusOK, listBody(fake.allIssues(projectID))
			})
		case http.MethodPost:
			fake.respond(w, r, string(OperationIssueCreate), func() (int, any) {
				var input IssueInput
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return http.StatusBadRequest, map[string]string{"error": err.Error()}
				}
				return http.StatusCreated, *fake.create(projectID, input)
			})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
		return
	}
	issueID := parts[0]
	switch r.Method {
	case http.MethodGet:
		fake.respond(w, r, string(OperationIssueGet), func() (int, any) {
			if issue := fake.get(issueID); issue != nil {
				return http.StatusOK, *issue
			}
			return http.StatusNotFound, map[string]string{"detail": "Not found."}
		})
	case http.MethodPatch:
		fake.respond(w, r, string(OperationIssueUpdate), func() (int, any) {
			var patch IssuePatch
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				return http.StatusBadRequest, map[string]string{"error": err.Error()}
			}
			issue := fake.patch(issueID, patch)
			if issue == nil {
				return http.StatusNotFound, map[string]string{"detail": "Not found."}
			}
			return http.StatusOK, *issue
		})
	case http.MethodDelete:
		fake.respond(w, r, string(OperationIssueDelete), func() (int, any) {
			if fake.delete(issueID) {
				return http.StatusNoContent, nil
			}
			return http.StatusNotFound, map[string]string{"detail": "Not found."}
		})
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// respond counts the operation, applies any scripted failure, then writes
// the handler's result. A "drop" failure closes the connection after the
// mutation already ran, simulating a lost response.
func (fake *fakePlane) respond(w http.ResponseWriter, r *http.Request, operation string, produce func() (int, any)) {
	fake.mu.Lock()
	fake.counts[operation]++
	mode := fake.failure(operation)
	fake.mu.Unlock()
	if mode == "drop" {
		status, body := produce()
		_ = status
		_ = body
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			fake.t.Error("response writer cannot hijack")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			fake.t.Errorf("hijack: %v", err)
			return
		}
		_ = connection.Close()
		return
	}
	if mode != "" {
		switch mode {
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
		case "403":
			w.WriteHeader(http.StatusForbidden)
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		case "503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "429":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			fake.t.Errorf("unknown fake failure %q", mode)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	status, body := produce()
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (fake *fakePlane) allProjects() []Project {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]Project{}, fake.projects...)
}

func (fake *fakePlane) allStates() []State {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]State{}, fake.states...)
}

func (fake *fakePlane) allLabels() []Label {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]Label{}, fake.labels...)
}

func (fake *fakePlane) allIssues(projectID string) []Issue {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	out := []Issue{}
	for _, issue := range fake.issues {
		if issue.Project == projectID {
			out = append(out, *issue)
		}
	}
	return out
}

func (fake *fakePlane) create(projectID string, input IssueInput) *Issue {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	issue := &Issue{
		ID:              fmt.Sprintf("wi-%d", fake.nextID),
		Name:            input.Name,
		DescriptionHTML: input.DescriptionHTML,
		State:           input.State,
		Priority:        input.Priority,
		Labels:          append([]string{}, input.Labels...),
		Project:         projectID,
	}
	fake.nextID++
	if issue.State == "" {
		issue.State = "st-backlog"
	}
	fake.issues[issue.ID] = issue
	return issue
}

func (fake *fakePlane) get(issueID string) *Issue {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if issue, ok := fake.issues[issueID]; ok {
		copy := *issue
		return &copy
	}
	return nil
}

// patch applies a work-item update to the stored issue, mirroring Plane's
// field semantics: empty name/state/priority are untouched, nil Labels keeps
// the existing set.
func (fake *fakePlane) patch(issueID string, patch IssuePatch) *Issue {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	issue, ok := fake.issues[issueID]
	if !ok {
		return nil
	}
	if patch.Name != "" {
		issue.Name = patch.Name
	}
	if patch.State != "" {
		issue.State = patch.State
	}
	if patch.Priority != "" {
		issue.Priority = patch.Priority
	}
	if patch.Labels != nil {
		issue.Labels = append([]string{}, patch.Labels...)
	}
	copy := *issue
	return &copy
}

func (fake *fakePlane) delete(issueID string) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.issues[issueID]; ok {
		delete(fake.issues, issueID)
		return true
	}
	return false
}

// seed installs a remote work item directly, for marker-adoption and drift
// scenarios that start from an already-present mirror.
func (fake *fakePlane) seed(issue Issue) *Issue {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if issue.ID == "" {
		issue.ID = fmt.Sprintf("wi-%d", fake.nextID)
		fake.nextID++
	}
	copy := issue
	fake.issues[issue.ID] = &copy
	return &copy
}

func (fake *fakePlane) count(operation string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.counts[operation]
}

func (fake *fakePlane) issueCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.issues)
}

func (fake *fakePlane) client(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(Config{
		APIURL:    fake.server.URL,
		APIKey:    "fixture-key",
		Workspace: fake.workspace,
		HTTP:      fake.server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
