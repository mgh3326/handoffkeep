package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeYouTrack struct {
	mu         sync.Mutex
	me         User
	issues     map[string]Issue
	activities []Activity
	cursor     string
	hasAfter   bool

	fieldValues map[string][]string
	projectIDs  map[string]string // short name -> id

	issueUpdates []map[string]any
	comments     []string
	bundleAdds   []string
	createCalls  int
	searchCalls  int
	getCalls     int
	failUpdate   bool
	// failFieldUpdate fails only a custom-field update, so a move before it
	// succeeds.
	failFieldUpdate bool
	failMe          bool
	savedQueries    []string

	articles       map[string]Article
	articleBodies  map[string]string
	articleParents map[string]string
	articleCreates int
	articleUpdates int
	// failSummary makes a create of an issue or an article with this summary
	// answer 500, so one item fails while the rest of a batch succeeds.
	failSummary string
}

func newFakeYouTrack() *fakeYouTrack {
	states := make([]string, 0, len(StateLabels))
	for _, label := range StateLabels {
		states = append(states, label)
	}
	return &fakeYouTrack{
		me:             User{ID: "bot-id", Login: "hk-bot", Name: "hk-bot"},
		issues:         map[string]Issue{},
		articles:       map[string]Article{},
		articleBodies:  map[string]string{},
		articleParents: map[string]string{},
		fieldValues: map[string][]string{
			"hk 상태":   states,
			"레인":      {"handoffkeep", "operator-desk", "rstack"},
			"종류":      {"implement", "verify", "fix", "decide", "ops"},
			"등급":      {"A", "B", "C"},
			"티어":      {"T0", "T1", "T2", "T3", "T4"},
			"hk 프로젝트": {"handoffkeep", "panewire", "other"},
			"멈춤":      {"멈춤"},
			"결정 상태":   {"대기", "기한 경과", "답변됨", "대체됨", "만료"},
		},
		projectIDs: map[string]string{"HK": "0-1", "PW": "0-2", "OTHER": "0-10", "DEC": "0-3"},
	}
}

func (f *fakeYouTrack) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeYouTrack) projectByID(id string) *ProjectRef {
	for short, candidate := range f.projectIDs {
		if candidate == id {
			return &ProjectRef{ID: id, ShortName: short, Name: short}
		}
	}
	return nil
}

func (f *fakeYouTrack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	path := r.URL.Path
	switch {
	case path == "/api/users/me":
		if f.failMe {
			write(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		write(http.StatusOK, f.me)
	case path == "/api/projects":
		projects := make([]ProjectRef, 0, len(f.projectIDs))
		for short, id := range f.projectIDs {
			projects = append(projects, ProjectRef{ID: id, ShortName: short, Name: short})
		}
		write(http.StatusOK, projects)
	case path == "/api/savedQueries" && r.Method == http.MethodGet:
		rows := make([]map[string]string, 0, len(f.savedQueries))
		for _, name := range f.savedQueries {
			rows = append(rows, map[string]string{"name": name})
		}
		write(http.StatusOK, rows)
	case path == "/api/savedQueries" && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.savedQueries = append(f.savedQueries, body.Name)
		write(http.StatusOK, map[string]string{"id": "sq-" + strconv.Itoa(len(f.savedQueries))})
	case strings.HasPrefix(path, "/api/articles/") && r.Method == http.MethodGet:
		article, ok := f.articles[strings.TrimPrefix(path, "/api/articles/")]
		if !ok {
			write(http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		write(http.StatusOK, article)
	case path == "/api/articles" && r.Method == http.MethodPost:
		var body struct {
			Summary       string `json:"summary"`
			Content       string `json:"content"`
			ParentArticle *struct {
				ID string `json:"id"`
			} `json:"parentArticle"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.failSummary != "" && body.Summary == f.failSummary {
			write(http.StatusInternalServerError, map[string]string{"error": "boom"})
			return
		}
		f.articleCreates++
		id := fmt.Sprintf("A-%d", f.articleCreates)
		article := Article{ID: id, IDReadable: "KB-A-" + strconv.Itoa(f.articleCreates), Summary: body.Summary}
		f.articles[id] = article
		f.articleBodies[id] = body.Content
		if body.ParentArticle != nil {
			f.articleParents[id] = body.ParentArticle.ID
		}
		write(http.StatusOK, article)
	case strings.HasPrefix(path, "/api/articles/") && r.Method == http.MethodPost:
		var body struct {
			Summary string `json:"summary"`
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimPrefix(path, "/api/articles/")
		article, ok := f.articles[id]
		if !ok {
			write(http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		f.articleUpdates++
		article.Summary = body.Summary
		f.articles[id] = article
		f.articleBodies[id] = body.Content
		write(http.StatusOK, article)
	case path == "/api/activitiesPage":
		page := ActivitiesPage{Activities: f.activities, AfterCursor: f.cursor, HasAfter: f.hasAfter}
		write(http.StatusOK, page)
	case path == "/api/issues" && r.Method == http.MethodGet:
		f.searchCalls++
		issues := make([]Issue, 0, len(f.issues))
		for _, issue := range f.issues {
			issues = append(issues, issue)
		}
		write(http.StatusOK, issues)
	case path == "/api/issues" && r.Method == http.MethodPost:
		f.createCalls++
		var body struct {
			Project struct {
				ID string `json:"id"`
			} `json:"project"`
			Summary      string        `json:"summary"`
			Description  string        `json:"description"`
			CustomFields []CustomField `json:"customFields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.failSummary != "" && body.Summary == f.failSummary {
			write(http.StatusInternalServerError, map[string]string{"error": "boom"})
			return
		}
		id := fmt.Sprintf("1-%d", f.createCalls)
		fields := make([]IssueField, 0, len(body.CustomFields))
		for _, field := range body.CustomFields {
			raw, _ := json.Marshal(field.Value)
			fields = append(fields, IssueField{Name: field.Name, Value: raw})
		}
		short := "HK"
		if project := f.projectByID(body.Project.ID); project != nil {
			short = project.ShortName
		}
		issue := Issue{ID: id, IDReadable: short + "-" + strconv.Itoa(f.createCalls), Summary: body.Summary, Description: body.Description, CustomFields: fields, Project: f.projectByID(body.Project.ID)}
		f.issues[id] = issue
		write(http.StatusOK, issue)
	case strings.HasPrefix(path, "/api/admin/projects/"):
		// An unresolved project id is an empty path segment. Real YouTrack
		// answers 404, so the fake does too; callers must pass the id that
		// projectForTask resolved.
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/admin/projects/"), "/customFields")
		if id == "" {
			write(http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		fields := make([]map[string]any, 0, len(f.fieldValues))
		for name, values := range f.fieldValues {
			bundleType, elementType := "EnumBundle", "EnumBundleElement"
			if name == "hk 상태" {
				bundleType, elementType = "StateBundle", "StateBundleElement"
			}
			rows := make([]map[string]any, 0, len(values))
			for i, value := range values {
				rows = append(rows, map[string]any{"id": "v-" + strconv.Itoa(i), "name": value, "$type": elementType})
			}
			fields = append(fields, map[string]any{
				"field":  map[string]any{"id": "f-" + name, "name": name, "$type": "ProjectCustomField"},
				"bundle": map[string]any{"id": "b-" + name, "$type": bundleType, "values": rows},
			})
		}
		write(http.StatusOK, fields)
	case strings.HasPrefix(path, "/api/admin/customFieldSettings/bundles/"):
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bundleAdds = append(f.bundleAdds, body.Name)
		write(http.StatusOK, map[string]any{"id": "v-new", "name": body.Name})
	case strings.HasSuffix(path, "/comments") && strings.HasPrefix(path, "/api/issues/") && r.Method == http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.comments = append(f.comments, body.Text)
		write(http.StatusOK, Comment{ID: "c-" + strconv.Itoa(len(f.comments)), Text: body.Text})
	case strings.HasPrefix(path, "/api/issues/") && r.Method == http.MethodGet:
		f.getCalls++
		id := strings.TrimPrefix(path, "/api/issues/")
		issue, ok := f.issues[id]
		if !ok {
			write(http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		write(http.StatusOK, issue)
	case strings.HasPrefix(path, "/api/issues/") && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.failUpdate || (f.failFieldUpdate && body["customFields"] != nil) {
			write(http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		f.issueUpdates = append(f.issueUpdates, body)
		id := strings.TrimPrefix(path, "/api/issues/")
		issue, ok := f.issues[id]
		if !ok {
			issue = Issue{ID: id, IDReadable: "HK-1"}
		}
		if project, ok := body["project"].(map[string]any); ok {
			if pid, ok := project["id"].(string); ok {
				issue.Project = f.projectByID(pid)
				if issue.Project != nil {
					issue.IDReadable = issue.Project.ShortName + "-1"
				}
				f.issues[id] = issue
			}
		}
		write(http.StatusOK, issue)
	default:
		write(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func newTestService(t *testing.T, f *fakeYouTrack, fs *fakeStore) *Service {
	t.Helper()
	srv := f.server(t)
	client, err := New(Config{BaseURL: srv.URL, Token: "fixture-token", Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(fs, client, Options{Tenant: "tenant", Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }})
}

func operator() *User { return &User{ID: "admin-id", Login: "admin", Name: "operator"} }

// mirrorIssue seeds both sides: the hk mirror row and the YouTrack issue the
// cache refresh and the pull will read.
func mirrorIssue(fs *fakeStore, yt *fakeYouTrack, taskID int64, issueID, readable string) {
	fs.mirror(taskID, issueID, readable)
	yt.mu.Lock()
	yt.issues[issueID] = Issue{ID: issueID, IDReadable: readable, Project: &ProjectRef{ID: "0-1", ShortName: "HK"}, CustomFields: []IssueField{
		{Name: "hk 우선순위", Value: json.RawMessage(`0`)},
		{Name: "레인", Value: json.RawMessage(`{"name":"handoffkeep"}`)},
		{Name: "hk 상태", Value: json.RawMessage(`{"name":"대기"}`)},
	}}
	yt.mu.Unlock()
}

func stateActivity(id, issueID, readable, to string) Activity {
	return Activity{
		ID: id, Author: operator(), Target: &Issue{ID: issueID, IDReadable: readable},
		Categories: []ActivityCategory{{
			Type:  "CustomFieldCategory",
			Field: &ActivityField{Name: "hk 상태"},
			Added: []json.RawMessage{json.RawMessage(`{"$type":"StateBundleElement","name":"` + to + `"}`)},
		}},
	}
}
