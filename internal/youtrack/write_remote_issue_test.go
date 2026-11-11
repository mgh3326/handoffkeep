package youtrack

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// issueHit is one /api/issues search hit in YouTrack's JSON shape.
func issueHit(id, readable, projectID, summary string, hkID int) map[string]any {
	return map[string]any{
		"id": id, "idReadable": readable, "summary": summary, "description": "d-" + id, "$type": "Issue",
		"project":      map[string]any{"id": projectID, "shortName": "HK", "$type": "Project"},
		"customFields": []map[string]any{{"name": "hk ID", "$type": "SimpleIssueCustomField", "value": hkID}},
	}
}

// FindTaskIssues is one GET scoped to the project by short name, and keeps
// only the hits in the project id whose hk ID is the task's.
func TestWriteRemoteFindTaskIssues(t *testing.T) {
	var query, top string
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		query, top = r.URL.Query().Get("query"), r.URL.Query().Get("$top")
		_ = json.NewEncoder(w).Encode([]any{
			issueHit("2-1", "HK-1", "0-7", "#3898 mine", 3898),
			issueHit("2-2", "OT-2", "0-9", "#3898 other project", 3898),
			issueHit("2-3", "HK-3", "0-7", "#38980 other task", 38980),
		})
	}}
	r := y.serve(t)
	got, err := r.FindTaskIssues(t.Context(), "HK", "0-7", 3898)
	if err != nil {
		t.Fatal(err)
	}
	want := []ytwrite.RemoteIssue{{ID: "2-1", Readable: "HK-1", ProjectID: "0-7", Summary: "#3898 mine", Description: "d-2-1"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("hits %+v, want %+v", got, want)
	}
	if query != "project: HK {hk ID}: 3898" || top != "50" || y.count() != 1 || y.countOf(http.MethodGet) != 1 {
		t.Fatalf("query %q top %q requests %v", query, top, y.requests)
	}
}

// A full page cannot be proven complete, and an HTTP answer passes up as a
// RemoteError (one request, no retry).
func TestWriteRemoteFindTaskIssuesFailures(t *testing.T) {
	t.Run("full_page", func(t *testing.T) {
		y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
			page := make([]any, 0, taskIssueSearchTop)
			for i := range taskIssueSearchTop {
				page = append(page, issueHit(fmt.Sprintf("2-%d", i), "HK", "0-7", "#1", 1))
			}
			_ = json.NewEncoder(w).Encode(page)
		}}
		if _, err := y.serve(t).FindTaskIssues(t.Context(), "HK", "0-7", 1); err == nil || !strings.Contains(err.Error(), "not complete") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("rate_limited", func(t *testing.T) {
		y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}}
		_, err := y.serve(t).FindTaskIssues(t.Context(), "HK", "0-7", 1)
		var re *ytwrite.RemoteError
		if !errors.As(err, &re) || re.Status != http.StatusTooManyRequests || re.RetryAfter.Seconds() != 7 || y.count() != 1 {
			t.Fatalf("err %v, requests %d", err, y.count())
		}
	})
}

// CreateIssue is one POST with the project, summary, description and only the
// hk ID and hk 프로젝트 fields; a 2xx without a readable id is "", nil.
func TestWriteRemoteCreateIssue(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"2-99","$type":"Issue"}`))
	}}
	r := y.serve(t)
	id, err := r.CreateIssue(t.Context(), ytwrite.IssueCreate{ProjectID: "0-7", Summary: "#3898 t", Description: "body <!-- hk-op:5 -->", TaskID: 3898, HKProject: "handoffkeep"})
	if err != nil || id != "2-99" {
		t.Fatalf("id %q err %v", id, err)
	}
	if y.count() != 1 || y.requests[0] != "POST /api/issues" {
		t.Fatalf("requests %v", y.requests)
	}
	var body struct {
		Project      struct{ ID string } `json:"project"`
		Summary      string              `json:"summary"`
		Description  string              `json:"description"`
		CustomFields []struct {
			Name  string `json:"name"`
			Type  string `json:"$type"`
			Value any    `json:"value"`
		} `json:"customFields"`
	}
	if err := json.Unmarshal([]byte(y.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.Project.ID != "0-7" || body.Summary != "#3898 t" || body.Description != "body <!-- hk-op:5 -->" || len(body.CustomFields) != 2 ||
		body.CustomFields[0].Name != "hk ID" || body.CustomFields[0].Value != float64(3898) ||
		body.CustomFields[1].Name != "hk 프로젝트" || body.CustomFields[1].Type != fieldSingleEnum {
		t.Fatalf("body %s", y.bodies[0])
	}

	noID := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`not json`)) }}
	if id, err := noID.serve(t).CreateIssue(t.Context(), ytwrite.IssueCreate{ProjectID: "0-7", Summary: "s", TaskID: 1}); id != "" || err != nil {
		t.Fatalf("no id: %q %v", id, err)
	}
	refused := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) }}
	_, err = refused.serve(t).CreateIssue(t.Context(), ytwrite.IssueCreate{ProjectID: "0-7", Summary: "s", TaskID: 1})
	var re *ytwrite.RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusBadRequest || refused.count() != 1 {
		t.Fatalf("refused: %v, requests %d", err, refused.count())
	}
}
