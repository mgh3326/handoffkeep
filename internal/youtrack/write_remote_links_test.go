package youtrack

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func TestWriteRemoteAddsIssueLinksWithSupportedMutationAPI(t *testing.T) {
	want := ytwrite.IssueLinks{Parent: "2-1", Related: []string{"2-3"}, Duplicates: []string{"2-4"}}
	linkReads := 0
	var y *recordedYouTrack
	y = &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issueLinkTypes":
			_, _ = w.Write(linkTypeFixture())
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/2-9/links":
			linkReads++
			if linkReads == 1 {
				_, _ = w.Write(issueLinkFixture(ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}))
			} else {
				_, _ = w.Write(issueLinkFixture(want))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/2-9/links/105-3t/issues":
			targetBodyIs(t, y, "2-1")
			_, _ = w.Write([]byte(`{"id":"2-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/2-9/links/105-0/issues":
			targetBodyIs(t, y, "2-3")
			_, _ = w.Write([]byte(`{"id":"2-3"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues/2-9/links/105-1s/issues":
			targetBodyIs(t, y, "2-4")
			_, _ = w.Write([]byte(`{"id":"2-4"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}}
	r := y.serve(t)
	id, err := r.UpdateIssueLinks(t.Context(), "2-9", ytwrite.IssueLinksSnapshot{Desired: want})
	if err != nil || id != "2-9" {
		t.Fatalf("update id=%q err=%v", id, err)
	}
	if got, expected := y.requests, []string{
		"GET /api/issueLinkTypes", "GET /api/issues/2-9/links",
		"POST /api/issues/2-9/links/105-3t/issues", "POST /api/issues/2-9/links/105-0/issues", "POST /api/issues/2-9/links/105-1s/issues",
		"GET /api/issues/2-9/links",
	}; !reflect.DeepEqual(got, expected) {
		t.Fatalf("requests %v, want %v", got, expected)
	}
}

func TestWriteRemoteRemovesOnlyPreviouslyProjectedIssueLinks(t *testing.T) {
	previous := ytwrite.IssueLinks{Parent: "2-old-parent", Related: []string{"2-old-related"}, Duplicates: []string{"2-old-duplicate"}}
	linkReads := 0
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issueLinkTypes":
			_, _ = w.Write(linkTypeFixture())
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues/2-9/links":
			linkReads++
			if linkReads == 1 {
				_, _ = w.Write(issueLinkFixture(previous))
			} else {
				_, _ = w.Write(issueLinkFixture(ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}))
			}
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"id":"2-9"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}}
	r := y.serve(t)
	_, err := r.UpdateIssueLinks(t.Context(), "2-9", ytwrite.IssueLinksSnapshot{Previous: previous, Desired: ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	wantDeletes := []string{
		"DELETE /api/issues/2-9/links/105-3t/issues/2-old-parent",
		"DELETE /api/issues/2-9/links/105-0/issues/2-old-related",
		"DELETE /api/issues/2-9/links/105-1s/issues/2-old-duplicate",
	}
	var gotDeletes []string
	for _, request := range y.requests {
		if len(request) >= 7 && request[:7] == "DELETE " {
			gotDeletes = append(gotDeletes, request)
		}
	}
	if !reflect.DeepEqual(gotDeletes, wantDeletes) {
		t.Fatalf("delete requests %v, want %v", gotDeletes, wantDeletes)
	}
}

func TestWriteRemoteRefusesUnmanagedIssueLinkWithoutMutation(t *testing.T) {
	human := ytwrite.IssueLinks{Related: []string{"2-human"}, Duplicates: []string{}}
	y := &recordedYouTrack{answer: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issueLinkTypes":
			_, _ = w.Write(linkTypeFixture())
		case "/api/issues/2-9/links":
			_, _ = w.Write(issueLinkFixture(human))
		default:
			t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}}
	_, err := y.serve(t).UpdateIssueLinks(t.Context(), "2-9", ytwrite.IssueLinksSnapshot{Desired: ytwrite.IssueLinks{Related: []string{}, Duplicates: []string{}}})
	var refused *ytwrite.RefusedError
	if !errors.As(err, &refused) || refused.Reason != ytwrite.ReasonIssueLinksConflict {
		t.Fatalf("update error %v, want issue link conflict", err)
	}
	if y.count() != 2 {
		t.Fatalf("made requests after detecting unmanaged link: %v", y.requests)
	}
}

func targetBodyIs(t *testing.T, y *recordedYouTrack, want string) {
	t.Helper()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(y.bodies[len(y.bodies)-1]), &body); err != nil || body.ID != want {
		t.Errorf("mutation body %s, want target %q (err=%v)", y.bodies[len(y.bodies)-1], want, err)
	}
}

func linkTypeFixture() []byte {
	return []byte(`[{"id":"105-0","name":"Relates","directed":false},{"id":"105-1","name":"Duplicate","directed":true},{"id":"105-3","name":"Subtask","directed":true}]`)
}

func issueLinkFixture(links ytwrite.IssueLinks) []byte {
	type issueRef struct {
		ID string `json:"id"`
	}
	type row struct {
		ID       string `json:"id"`
		LinkType struct {
			Name string `json:"name"`
		} `json:"linkType"`
		Issues []issueRef `json:"issues"`
	}
	makeRow := func(id, name string, targets []string) row {
		out := row{ID: id, Issues: []issueRef{}}
		out.LinkType.Name = name
		for _, target := range targets {
			out.Issues = append(out.Issues, issueRef{ID: target})
		}
		return out
	}
	rows := []row{}
	if links.Parent != "" {
		rows = append(rows, makeRow("105-3t", "Subtask", []string{links.Parent}))
	}
	if len(links.Related) > 0 {
		rows = append(rows, makeRow("105-0", "Relates", links.Related))
	}
	if len(links.Duplicates) > 0 {
		rows = append(rows, makeRow("105-1s", "Duplicate", links.Duplicates))
	}
	data, _ := json.Marshal(rows)
	return data
}
