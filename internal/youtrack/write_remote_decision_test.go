package youtrack

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// decisionHit is one DEC issue search hit in YouTrack's JSON shape.
func decisionHit(id, readable, projectID, summary string, decisionID int) map[string]any {
	return map[string]any{
		"id": id, "idReadable": readable, "summary": summary, "description": "d-" + id, "$type": "Issue",
		"project":      map[string]any{"id": projectID, "shortName": "DEC", "$type": "Project"},
		"customFields": []map[string]any{{"name": "결정 ID", "$type": "SimpleIssueCustomField", "value": decisionID}},
	}
}

// FindDecisionIssues is one GET by 결정 ID scoped to the DEC project, keeping
// only the hits in the project id whose 결정 ID is the decision's.
func TestWriteRemoteFindDecisionIssues(t *testing.T) {
	var query string
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		_ = json.NewEncoder(w).Encode([]any{
			decisionHit("2-1", "DEC-40", "0-12", "D-40 mine", 40),
			decisionHit("2-2", "OT-2", "0-9", "D-40 other project", 40),
			decisionHit("2-3", "DEC-400", "0-12", "D-400 other", 400),
		})
	}}
	got, err := y.serve(t).FindDecisionIssues(t.Context(), "DEC", "0-12", 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (ytwrite.RemoteIssue{ID: "2-1", Readable: "DEC-40", ProjectID: "0-12", Summary: "D-40 mine", Description: "d-2-1"}) {
		t.Fatalf("hits %+v", got)
	}
	if query != "project: DEC {결정 ID}: 40" || y.count() != 1 {
		t.Fatalf("query %q requests %v", query, y.requests)
	}
}

// CreateDecisionIssue is one POST with the project, the text and only the hk
// ID and 결정 ID fields.
func TestWriteRemoteCreateDecisionIssue(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"2-77","$type":"Issue"}`))
	}}
	id, err := y.serve(t).CreateDecisionIssue(t.Context(), ytwrite.DecisionIssueCreate{ProjectID: "0-12", Summary: "D-40 q", Description: "block <!-- hk-op:9 -->", DecisionID: 40})
	if err != nil || id != "2-77" || y.count() != 1 || y.requests[0] != "POST /api/issues" {
		t.Fatalf("id %q err %v requests %v", id, err, y.requests)
	}
	var body struct {
		Project      struct{ ID string } `json:"project"`
		Summary      string              `json:"summary"`
		Description  string              `json:"description"`
		CustomFields []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"customFields"`
	}
	if err := json.Unmarshal([]byte(y.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.Project.ID != "0-12" || body.Summary != "D-40 q" || body.Description != "block <!-- hk-op:9 -->" || len(body.CustomFields) != 2 ||
		body.CustomFields[0].Name != "hk ID" || body.CustomFields[0].Value != float64(40) ||
		body.CustomFields[1].Name != "결정 ID" || body.CustomFields[1].Value != float64(40) {
		t.Fatalf("body %s", y.bodies[0])
	}
}

// CheckDecisionFields holds a project without the 결정 ID field.
func TestWriteRemoteCheckDecisionFields(t *testing.T) {
	fields := func(names ...string) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			out := []any{}
			for _, n := range names {
				out = append(out, map[string]any{"field": map[string]any{"name": n, "fieldType": map[string]any{"id": "integer"}}})
			}
			_ = json.NewEncoder(w).Encode(out)
		}
	}
	ok := &recordedYouTrack{answer: fields("hk ID", "결정 ID", "결정 상태")}
	if err := ok.serve(t).CheckDecisionFields(t.Context(), "0-12"); err != nil || ok.requests[0] != "GET /api/admin/projects/0-12/customFields" {
		t.Fatalf("err %v requests %v", err, ok.requests)
	}
	missing := &recordedYouTrack{answer: fields("hk ID")}
	if err := missing.serve(t).CheckDecisionFields(t.Context(), "0-12"); err == nil || !strings.Contains(err.Error(), "결정 ID") {
		t.Fatalf("missing field: %v", err)
	}
}

func TestVerifyDecisionIssue(t *testing.T) {
	good := func() Issue {
		return Issue{ID: "2-1", IDReadable: "DEC-40", Summary: "D-40 질문", Project: &ProjectRef{ID: "0-12"},
			CustomFields: []IssueField{{Name: "결정 ID", Value: json.RawMessage(`40`)}}}
	}
	if reason := VerifyDecisionIssue(good(), 40, "0-12"); reason != "" {
		t.Fatalf("good issue refused: %s", reason)
	}
	for name, edit := range map[string]func(*Issue){
		"readable":    func(i *Issue) { i.IDReadable = "DEC-41" },
		"project":     func(i *Issue) { i.Project = &ProjectRef{ID: "0-9"} },
		"no_project":  func(i *Issue) { i.Project = nil },
		"summary":     func(i *Issue) { i.Summary = "D-400 질문" },
		"legacy":      func(i *Issue) { i.Summary = "[결정 40] 질문" },
		"field":       func(i *Issue) { i.CustomFields[0].Value = json.RawMessage(`41`) },
		"field_null":  func(i *Issue) { i.CustomFields[0].Value = json.RawMessage(`null`) },
		"field_none":  func(i *Issue) { i.CustomFields = nil },
		"no_issue_id": func(i *Issue) { i.ID = "" },
	} {
		i := good()
		edit(&i)
		if reason := VerifyDecisionIssue(i, 40, "0-12"); reason == "" {
			t.Errorf("%s: verified", name)
		}
	}
}
