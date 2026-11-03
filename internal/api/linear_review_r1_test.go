package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type r1Admission struct {
	*admissionFake
	holdFails bool
	released  int
}

func (g *r1Admission) SerializeLinearCreate(ctx context.Context, run func(store.LinearCreateLease) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return run(g)
}
func (g *r1Admission) HoldCapacity(ctx context.Context, n int) error {
	if g.holdFails {
		return errors.New("hold persistence failed")
	}
	return g.admissionFake.HoldCapacity(ctx, n)
}
func (g *r1Admission) ReservedCreates(context.Context) (int, error) { return len(g.pending), nil }
func (g *r1Admission) ReleaseAbsent(ctx context.Context, id string) error {
	g.released++
	return g.Complete(ctx, id)
}
func TestR1DefiniteCreateOutcomesReleaseReservations(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		holdFails  bool
	}{
		{"capacity cleanup despite hold error", `{"errors":[{"message":"Workspace issue limit reached"}]}`, true},
		{"invalid label", `{"errors":[{"message":"invalid labelId","extensions":{"type":"UserError"}}]}`, false},
		{"invalid state", `{"errors":[{"message":"bad state","extensions":{"code":"BAD_USER_INPUT"}}]}`, false},
		{"explicit false", `{"data":{"issueCreate":{"success":false}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &r1Admission{admissionFake: newAdmissionFake(), holdFails: tc.holdFails}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var in struct{ Query string }
				json.NewDecoder(r.Body).Decode(&in)
				if strings.Contains(in.Query, "HkAdmissionCount") {
					fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
				} else {
					fmt.Fprint(w, tc.body)
				}
			}))
			defer srv.Close()
			client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Admission: gate, CapacityScopeVerified: true}
			id := store.LinearDecisionUUID("work-board:task:42")
			_, err := client.CreateLinearIssue(t.Context(), LinearDecisionIssue{Board: true, ID: id, TaskID: 42})
			if err == nil || len(gate.pending) != 0 {
				t.Fatal("definite outcome retained reservation", gate.pending, err)
			}
		})
	}
}
func TestR1AmbiguousBoardReservationDoesNotBlockUnrelatedCreates(t *testing.T) {
	gate := &r1Admission{admissionFake: newAdmissionFake()}
	prior := store.LinearDecisionUUID("work-board:task:1")
	gate.pending[prior] = true
	gate.board = map[string]bool{prior: true}
	remote := &admissionRemote{issues: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(remote.serve))
	defer srv.Close()
	client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Admission: gate, CapacityScopeVerified: true}
	if _, err := client.CreateLinearIssue(t.Context(), LinearDecisionIssue{Board: true, ID: store.LinearDecisionUUID("work-board:task:42"), TaskID: 42}); err != nil {
		t.Fatal("unrelated board blocked", err)
	}
	if _, err := client.CreateLinearIssue(t.Context(), LinearDecisionIssue{ID: store.LinearDecisionUUID("decision:43"), TaskID: 43}); err != nil {
		t.Fatal("unrelated decision blocked", err)
	}
	if remote.creates != 2 || !gate.pending[prior] {
		t.Fatal(remote.creates, gate.pending)
	}
}
func TestR1ManualRepairReleasesConfirmedAbsentReservation(t *testing.T) {
	gate := &r1Admission{admissionFake: newAdmissionFake()}
	id := store.LinearDecisionUUID("work-board:task:42")
	gate.pending[id] = true
	gate.board = map[string]bool{id: true}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
	}))
	defer srv.Close()
	client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Admission: gate, CapacityScopeVerified: true}
	h := Server{Tokens: Tokens{"operator": "fake-token"}, LinearBoard: &client}.Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/linear/reservations/reconcile", strings.NewReader(fmt.Sprintf(`{"issue_id":%q}`, id)))
	req.Header.Set("Authorization", "Bearer fake-token")
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "released") || len(gate.pending) != 0 || gate.released != 1 || requests != 1 {
		t.Fatal(out.Code, out.Body.String(), gate.pending, requests)
	}
}
func TestR1RateBackoffResetsAfterSuccess(t *testing.T) {
	now := time.Now().UTC()
	c := &LinearBoardControl{now: func() time.Time { return now }}
	for range 8 {
		c.rateLimited("")
	}
	now = now.Add(2 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":{"ok":true}}`) }))
	defer srv.Close()
	client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), BoardControl: c}
	var out struct{ OK bool }
	if err := client.graphql(t.Context(), time.Second, "query{ok}", nil, &out); err != nil {
		t.Fatal(err)
	}
	var rate linearRateLimitError
	if !errors.As(c.rateLimited(""), &rate) || rate.Until.Sub(now) != time.Minute {
		t.Fatal("isolated failure retained old backoff", rate.Until.Sub(now))
	}
}
func TestR1AmbiguousLabelFailureIsRetryable(t *testing.T) {
	for _, readStatus := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(fmt.Sprint(readStatus), func(t *testing.T) {
			meta := boardMetadataFixture()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var in struct{ Query string }
				json.NewDecoder(r.Body).Decode(&in)
				if strings.Contains(in.Query, "HkBoardLabelByID") {
					w.WriteHeader(readStatus)
					fmt.Fprint(w, `{"data":{"issueLabel":null}}`)
				} else {
					fmt.Fprint(w, `{"errors":[{"message":"internal failure","extensions":{"type":"InternalError"}}]}`)
				}
			}))
			defer srv.Close()
			client := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: meta}
			_, err := client.BoardLabel(t.Context(), "team", "fleet")
			var missing linearBoardLabelUnavailableError
			if err == nil || errors.As(err, &missing) || meta.failed["fleet"] {
				t.Fatal("ambiguous label fault became durable", err, meta.failed)
			}
		})
	}
}

type r1WebhookMetadata struct{ available bool }

func (*r1WebhookMetadata) ApplyLinearMetadata(context.Context, []store.LinearMetadata) error {
	return nil
}
func (*r1WebhookMetadata) RecordLinearBoardIssue(context.Context, store.LinearBoardIssueEvent) error {
	return nil
}
func (s *r1WebhookMetadata) LinearBoardLabelAvailable(context.Context, string, string) (bool, error) {
	return s.available, nil
}
func TestR1OnlyResolvingLabelClearsCircuit(t *testing.T) {
	c := &LinearBoardControl{}
	c.breakLabel("fleet")
	db := &r1WebhookMetadata{}
	h := LinearWebhook{Board: db, BoardControl: c}
	for _, tc := range []struct{ kind, action, name string }{{"IssueLabel", "create", "other"}, {"IssueLabel", "remove", "fleet"}, {"WorkflowState", "update", "fleet"}, {"IssueLabel", "update", "fleet"}} {
		var p linearWebhookPayload
		p.Type = tc.kind
		p.Action = tc.action
		p.Data.ID = "id"
		p.Data.Name = tc.name
		p.WebhookTimestamp = time.Now().UnixMilli()
		if err := h.handleBoardWebhook(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		if !c.blocked() {
			t.Fatal("unrelated, deleted or unapplied label cleared circuit", tc)
		}
	}
	db.available = true
	var p linearWebhookPayload
	p.Type = "IssueLabel"
	p.Action = "create"
	p.Data.ID = "id"
	p.Data.Name = "fleet"
	p.WebhookTimestamp = time.Now().UnixMilli()
	if err := h.handleBoardWebhook(t.Context(), p); err != nil || c.blocked() {
		t.Fatal("resolving label did not recover", err)
	}
}
