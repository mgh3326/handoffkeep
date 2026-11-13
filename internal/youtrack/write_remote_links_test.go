package youtrack

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func TestWriteRemoteUpdatesAndReadsOwnedIssueLinks(t *testing.T) {
	type issueRef struct {
		ID string `json:"id"`
	}
	type link struct {
		ID     string     `json:"id"`
		Issues []issueRef `json:"issues"`
	}
	var body struct {
		Links []link `json:"links"`
	}
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issueLinkTypes":
			_, _ = w.Write([]byte(`[{"id":"105-0","name":"Relates","directed":false},{"id":"105-1","name":"Duplicate","directed":true},{"id":"105-3","name":"Subtask","directed":true}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/2-9":
			_, _ = w.Write([]byte(`{"id":"2-9"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/2-9/links":
			_, _ = w.Write([]byte(`[{"id":"105-3t","linkType":{"name":"Subtask"},"issues":[{"id":"2-1"}]},{"id":"105-0","linkType":{"name":"Relates"},"issues":[{"id":"2-3"}]},{"id":"105-1s","linkType":{"name":"Duplicate"},"issues":[{"id":"2-4"}]}]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}}
	r := y.serve(t)
	want := ytwrite.IssueLinks{Parent: "2-1", Related: []string{"2-3"}, Duplicates: []string{"2-4"}}
	if id, err := r.UpdateIssueLinks(t.Context(), "2-9", want); err != nil || id != "2-9" {
		t.Fatalf("update id=%q err=%v", id, err)
	}
	if err := json.Unmarshal([]byte(y.bodies[1]), &body); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if len(body.Links) != 3 || body.Links[0].ID != "105-3t" || body.Links[0].Issues[0].ID != "2-1" || body.Links[1].ID != "105-0" || body.Links[1].Issues[0].ID != "2-3" || body.Links[2].ID != "105-1s" || body.Links[2].Issues[0].ID != "2-4" {
		t.Fatalf("link update body %+v", body)
	}
	got, err := r.ReadIssueLinks(t.Context(), "2-9")
	if err != nil || got.Parent != want.Parent || len(got.Related) != 1 || got.Related[0] != "2-3" || len(got.Duplicates) != 1 || got.Duplicates[0] != "2-4" {
		t.Fatalf("read links %+v err=%v", got, err)
	}
}

func TestWriteRemoteClearsOwnedIssueLinks(t *testing.T) {
	type link struct {
		ID     string `json:"id"`
		Issues []any  `json:"issues"`
	}
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"id":"105-0","name":"Relates","directed":false},{"id":"105-1","name":"Duplicate","directed":true},{"id":"105-3","name":"Subtask","directed":true}]`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"2-9"}`))
	}}
	var body struct {
		Links []link `json:"links"`
	}
	if _, err := y.serve(t).UpdateIssueLinks(t.Context(), "2-9", ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(y.bodies[1]), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Links) != 3 || len(body.Links[0].Issues) != 0 || len(body.Links[1].Issues) != 0 || len(body.Links[2].Issues) != 0 {
		t.Fatalf("empty snapshot body %s", y.bodies[1])
	}
}
