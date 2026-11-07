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

	issueUpdates []map[string]any
	comments     []string
	bundleAdds   []string
	createCalls  int
	searchCalls  int
	getCalls     int
	failUpdate   bool
	failMe       bool
}

func newFakeYouTrack() *fakeYouTrack {
	states := make([]string, 0, len(StateLabels))
	for _, label := range StateLabels {
		states = append(states, label)
	}
	return &fakeYouTrack{
		me:     User{ID: "bot-id", Login: "hk-bot", Name: "hk-bot"},
		issues: map[string]Issue{},
		fieldValues: map[string][]string{
			"hk 상태":   states,
			"레인":      {"handoffkeep", "operator-desk", "rstack"},
			"hk 프로젝트": {"handoffkeep", "other"},
			"종류":      {"implement", "verify", "fix", "decide", "ops"},
			"등급":      {"A", "B", "C"},
			"티어":      {"T0", "T1", "T2", "T3", "T4"},
		},
	}
}

func (f *fakeYouTrack) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
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
		write(http.StatusOK, []ProjectRef{{ID: "0-1", ShortName: "HK", Name: "HK"}})
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
			Summary      string        `json:"summary"`
			Description  string        `json:"description"`
			CustomFields []CustomField `json:"customFields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := fmt.Sprintf("1-%d", f.createCalls)
		fields := make([]IssueField, 0, len(body.CustomFields))
		for _, field := range body.CustomFields {
			raw, _ := json.Marshal(field.Value)
			fields = append(fields, IssueField{Name: field.Name, Value: raw})
		}
		issue := Issue{ID: id, IDReadable: fmt.Sprintf("HK-%d", f.createCalls), Summary: body.Summary, Description: body.Description, CustomFields: fields}
		f.issues[id] = issue
		write(http.StatusOK, issue)
	case strings.HasPrefix(path, "/api/admin/projects/"):
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
		if f.failUpdate {
			write(http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.issueUpdates = append(f.issueUpdates, body)
		id := strings.TrimPrefix(path, "/api/issues/")
		issue, ok := f.issues[id]
		if !ok {
			issue = Issue{ID: id, IDReadable: "HK-1"}
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
	return NewService(fs, client, Options{Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }})
}

func operator() *User { return &User{ID: "admin-id", Login: "admin", Name: "operator"} }

func stateActivity(id, readable, from, to string) Activity {
	return Activity{
		ID: id, Author: operator(), Target: &Issue{ID: "9-" + id, IDReadable: readable},
		Categories: []ActivityCategory{{
			Type:  "CustomFieldCategory",
			Field: &ActivityField{Name: "hk 상태"},
			Added: []json.RawMessage{json.RawMessage(`{"$type":"StateBundleElement","name":"` + to + `"}`)},
		}},
	}
}
