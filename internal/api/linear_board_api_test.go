package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestWorkBoardGraphQLAdapter(t *testing.T) {
	labels := map[string]string{}
	updated := false
	loseLabelResponse := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-key" {
			t.Error("missing authentication")
		}
		var in struct {
			Query     string
			Variables map[string]json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case strings.Contains(in.Query, "FleetBoardTarget"):
			fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","states":{"nodes":[{"id":"progress","name":"In Progress"}],"pageInfo":{"hasNextPage":false}}}]}}}`)
		case strings.Contains(in.Query, "FleetBoardIssue"):
			fmt.Fprint(w, `{"data":{"issue":{"id":"issue","identifier":"ROB-1234","description":"Human notes","team":{"id":"team"},"labels":{"nodes":[{"id":"custom","name":"custom"},{"id":"old-host","name":"host:old"}],"pageInfo":{"hasNextPage":false}}}}}`)
		case strings.Contains(in.Query, "HkBoardLabelByID"):
			var id string
			json.Unmarshal(in.Variables["id"], &id)
			for name, labelID := range labels {
				if id == labelID {
					fmt.Fprintf(w, `{"data":{"issueLabel":{"id":%q,"name":%q,"updatedAt":"2026-01-01T00:00:00Z","team":{"id":"team"}}}}`, id, name)
					return
				}
			}
			fmt.Fprint(w, `{"data":{"issueLabel":null}}`)
		case strings.Contains(in.Query, "FleetBoardLabels"):
			var name string
			json.Unmarshal(in.Variables["name"], &name)
			if id := labels[name]; id != "" {
				fmt.Fprintf(w, `{"data":{"issueLabels":{"nodes":[{"id":%q,"team":{"id":"team"}}]}}}`, id)
			} else {
				fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[]}}}`)
			}
		case strings.Contains(in.Query, "FleetBoardLabel"):
			var input struct{ ID, Name, TeamID string }
			json.Unmarshal(in.Variables["input"], &input)
			if input.TeamID != "team" || input.ID == "" {
				t.Error("invalid label create")
			}
			labels[input.Name] = input.ID
			if loseLabelResponse {
				loseLabelResponse = false
				fmt.Fprint(w, `{"errors":[{"message":"response lost"}]}`)
				return
			}
			fmt.Fprintf(w, `{"data":{"issueLabelCreate":{"success":true,"issueLabel":{"id":%q,"updatedAt":"2026-01-01T00:00:00Z"}}}}`, input.ID)
		case strings.Contains(in.Query, "issueUpdate"):
			var input struct {
				StateID, Description string
				LabelIDs             []string
			}
			json.Unmarshal(in.Variables["input"], &input)
			if input.StateID != "progress" || !strings.Contains(input.Description, "Human notes") || !strings.Contains(input.Description, boardStart) || strings.Contains(strings.Join(input.LabelIDs, ","), "old-host") || !strings.Contains(strings.Join(input.LabelIDs, ","), "custom") {
				t.Errorf("bad update: %+v", input)
			}
			updated = true
			fmt.Fprint(w, `{"data":{"issueUpdate":{"success":true}}}`)
		case strings.Contains(in.Query, "FleetBoardComment"):
			fmt.Fprint(w, `{"data":{"comment":{"id":"comment","body":"READY","issue":{"id":"issue"}}}}`)
		default:
			t.Errorf("unexpected query %s", in.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	client := LinearAPI{Metadata: boardMetadataFixture(), Key: "test-key", URL: srv.URL, HTTP: srv.Client()}
	q := &boardQueue{links: map[int64]store.LinearIssue{42: {IssueID: "issue"}}}
	q.add(boardFixture(), store.LinearBoardEvent{})
	LinearBoardWorker{Store: q, Linear: client}.Drain(t.Context())
	if !updated || q.rows[0].State != "sent" {
		t.Fatal(q.rows)
	}
	exists, err := client.BoardCommentExists(t.Context(), "comment", "issue", "READY")
	if err != nil || !exists {
		t.Fatal(exists, err)
	}
	exists, err = client.BoardCommentExists(t.Context(), "comment", "other", "READY")
	if err != nil || exists {
		t.Fatal(exists, err)
	}
}
