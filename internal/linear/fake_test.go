package linear

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeLinear struct {
	t             *testing.T
	server        *httptest.Server
	mu            sync.Mutex
	counts        map[string]int
	effects       map[string]int
	failures      map[string][]string
	alwaysFailure map[string]string
	issueExists   bool
	state         string
	archived      bool
	commentExists bool
	lastVariables map[string]map[string]any
}

func newFakeLinear(t *testing.T) *fakeLinear {
	t.Helper()
	fake := &fakeLinear{
		t:             t,
		counts:        map[string]int{},
		effects:       map[string]int{},
		failures:      map[string][]string{},
		alwaysFailure: map[string]string{},
		state:         "Backlog",
		lastVariables: map[string]map[string]any{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func linearFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "linear", name))
	if err != nil {
		t.Fatalf("read Linear fixture %s: %v", name, err)
	}
	return raw
}

func searchFixtureContainsMarker(t *testing.T, name, marker string) bool {
	t.Helper()
	var response struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					Description string `json:"description"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(linearFixture(t, name), &response); err != nil {
		t.Errorf("decode Linear search fixture %s: %v", name, err)
		return false
	}
	if len(response.Data.Issues.Nodes) != 1 {
		t.Errorf("Linear search fixture %s has %d issues, want 1", name, len(response.Data.Issues.Nodes))
		return false
	}
	return marker != "" && strings.Contains(response.Data.Issues.Nodes[0].Description, marker)
}

func issueFixtureHasID(t *testing.T, name, issueID string) bool {
	t.Helper()
	var response struct {
		Data struct {
			Issue *struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(linearFixture(t, name), &response); err != nil {
		t.Errorf("decode Linear issue fixture %s: %v", name, err)
		return false
	}
	return issueID != "" && response.Data.Issue != nil && response.Data.Issue.ID == issueID
}

func (fake *fakeLinear) failure(operation string) string {
	if mode := fake.alwaysFailure[operation]; mode != "" {
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

func (fake *fakeLinear) apply(operation string, variables map[string]any) {
	switch operation {
	case "HKIssueCreate":
		fake.issueExists = true
		fake.state = "Backlog"
		fake.effects[operation]++
	case "HKIssueUpdate":
		input, _ := variables["input"].(map[string]any)
		switch input["stateId"] {
		case "state-progress":
			fake.state = "In Progress"
		case "state-review":
			fake.state = "In Review"
		case "state-done":
			fake.state = "Done"
		case "state-canceled":
			fake.state = "Canceled"
		case "state-backlog":
			fake.state = "Backlog"
		}
		fake.effects[operation]++
	case "HKCommentCreate":
		fake.commentExists = true
		fake.effects[operation]++
	case "HKIssueArchive":
		fake.archived = true
		fake.effects[operation]++
	}
}

func (fake *fakeLinear) handle(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		fake.t.Errorf("decode request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	fake.mu.Lock()
	fake.counts[request.OperationName]++
	fake.lastVariables[request.OperationName] = request.Variables
	mode := fake.failure(request.OperationName)
	if mode == "drop" {
		fake.apply(request.OperationName, request.Variables)
		fake.mu.Unlock()
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
		fake.mu.Unlock()
		switch mode {
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
		case "403":
			w.WriteHeader(http.StatusForbidden)
		case "503":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "auth":
			_, _ = w.Write(linearFixture(fake.t, "authentication_error.json"))
		case "forbidden":
			_, _ = w.Write(linearFixture(fake.t, "forbidden_error.json"))
		case "ratelimited":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(linearFixture(fake.t, "ratelimited_error.json"))
		default:
			fake.t.Errorf("unknown fake failure %q", mode)
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	if request.OperationName == "HKIssueCreate" || request.OperationName == "HKIssueUpdate" || request.OperationName == "HKCommentCreate" || request.OperationName == "HKIssueArchive" {
		fake.apply(request.OperationName, request.Variables)
	}
	issueExists, state := fake.issueExists, fake.state
	archived, commentExists := fake.archived, fake.commentExists
	fake.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	fixture := ""
	switch request.OperationName {
	case "HKIssueSearch":
		fixture = "issue_search_empty.json"
		marker, _ := request.Variables["marker"].(string)
		if issueExists && searchFixtureContainsMarker(fake.t, "issue_search_found.json", marker) {
			fixture = "issue_search_found.json"
		}
	case "HKTeamLookup":
		fixture = "team_lookup.json"
	case "HKIssueCreate":
		fixture = "issue_create.json"
	case "HKIssueUpdate":
		fixture = "issue_update.json"
	case "HKCommentList":
		fixture = "comment_list_empty.json"
		if commentExists {
			fixture = "comment_list_found.json"
		}
	case "HKCommentCreate":
		fixture = "comment_create.json"
	case "HKIssueGet":
		if archived {
			fixture = "issue_get_archived.json"
		} else {
			switch state {
			case "Backlog", "Canceled":
				fixture = "issue_get_active.json"
			case "In Progress":
				fixture = "issue_get_progress.json"
			case "In Review":
				fixture = "issue_get_review.json"
			case "Done":
				fixture = "issue_get_done.json"
			default:
				fake.t.Errorf("unknown issue state %q", state)
			}
		}
		issueID, _ := request.Variables["id"].(string)
		if fixture != "" && !issueFixtureHasID(fake.t, fixture, issueID) {
			fixture = "issue_get_missing.json"
		}
	case "HKIssueArchive":
		fixture = "issue_archive.json"
	default:
		fake.t.Errorf("unexpected operation %q", request.OperationName)
		http.Error(w, fmt.Sprintf("unexpected operation %q", request.OperationName), http.StatusBadRequest)
		return
	}
	_, _ = w.Write(linearFixture(fake.t, fixture))
}

func (fake *fakeLinear) count(operation string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.counts[operation]
}

func (fake *fakeLinear) effect(operation string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.effects[operation]
}

func (fake *fakeLinear) variables(operation string) map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.lastVariables[operation]
}

func (fake *fakeLinear) client(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(Config{APIURL: fake.server.URL, APIKey: "fixture-key", TeamID: "team-1", HTTP: fake.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
