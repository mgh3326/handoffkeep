package youtrack

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The issue_move kind (hk 4055): one POST sets the project, and the reply
// settles it only when it names the same issue in the target project.
func TestWriteRemoteMoveIssueToProject(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/issues/2-9" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"2-9","project":{"id":"0-9"}}`))
	}}
	r := y.serve(t)
	id, err := r.MoveIssueToProject(t.Context(), "2-9", "0-9")
	if err != nil || id != "2-9" {
		t.Fatalf("id %q err %v", id, err)
	}
	var body struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal([]byte(y.bodies[0]), &body); err != nil || body.Project.ID != "0-9" {
		t.Fatalf("body %q: %v", y.bodies[0], err)
	}
	if y.countOf(http.MethodPost) != 1 {
		t.Fatalf("requests %v", y.requests)
	}
}

// A reply that does not name the target project, or the issue, is uncertain:
// the empty id sends the op to evidence instead of applying it.
func TestWriteRemoteMoveIssueToProjectUncertainReplies(t *testing.T) {
	for name, answer := range map[string]string{
		"other_project": `{"id":"2-9","project":{"id":"0-1"}}`,
		"other_issue":   `{"id":"2-10","project":{"id":"0-9"}}`,
		"no_id":         `{"project":{"id":"0-9"}}`,
		"not_json":      `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(answer))
			}}
			id, err := y.serve(t).MoveIssueToProject(t.Context(), "2-9", "0-9")
			if err != nil || id != "" {
				t.Fatalf("id %q err %v, want an uncertain empty id", id, err)
			}
		})
	}
}

// A refused move is the HTTP answer, so the protocol can reject or defer it.
func TestWriteRemoteMoveIssueToProjectRefused(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"project.id","error_description":"unknown project"}`))
	}}
	id, err := y.serve(t).MoveIssueToProject(t.Context(), "2-9", "0-9")
	if err == nil || id != "" {
		t.Fatalf("id %q err %v, want a refusal", id, err)
	}
}

// The evidence read is the issue's project id.
func TestWriteRemoteIssueProjectID(t *testing.T) {
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/issues/2-9" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"2-9","project":{"id":"0-9"}}`))
	}}
	got, err := y.serve(t).IssueProjectID(t.Context(), "2-9")
	if err != nil || got != "0-9" {
		t.Fatalf("project %q err %v", got, err)
	}
}
