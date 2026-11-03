package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryBoardMetadata struct {
	ready   bool
	items   []store.LinearMetadata
	failed  map[string]bool
	fetches int
}

func (m *memoryBoardMetadata) EnsureLinearBoardMetadata(ctx context.Context, fetch func(context.Context) ([]store.LinearMetadata, error)) error {
	if m.ready {
		return nil
	}
	m.fetches++
	items, err := fetch(ctx)
	if err != nil {
		return err
	}
	m.items = items
	m.ready = true
	return nil
}
func (m *memoryBoardMetadata) LinearBoardMetadata(context.Context) ([]store.LinearMetadata, error) {
	return m.items, nil
}
func (m *memoryBoardMetadata) ApplyLinearMetadata(_ context.Context, items []store.LinearMetadata) error {
	m.items = append(m.items, items...)
	return nil
}
func (m *memoryBoardMetadata) ResolveLinearBoardLabel(ctx context.Context, team, name string, create func(context.Context) (store.LinearMetadata, error)) (string, error) {
	for _, item := range m.items {
		if item.Kind == "label" && item.Name == name && !item.Deleted && (item.TeamID == team || item.TeamID == "") {
			return item.ID, nil
		}
	}
	if m.failed[name] {
		return "", store.ErrLinearBoardLabelUnavailable
	}
	item, err := create(ctx)
	if err != nil {
		if errors.Is(err, store.ErrLinearBoardLabelUnavailable) {
			if m.failed == nil {
				m.failed = map[string]bool{}
			}
			m.failed[name] = true
		}
		return "", err
	}
	m.items = append(m.items, item)
	return item.ID, nil
}
func boardMetadataFixture() *memoryBoardMetadata {
	return &memoryBoardMetadata{ready: true, items: []store.LinearMetadata{{Kind: "team", ID: "team", Name: "ROB"}, {Kind: "state", ID: "progress", TeamID: "team", Name: "In Progress"}}}
}

func TestLinearBoardMetadataInitialFetchThenDatabaseReads(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in struct{ Query string }
		json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in.Query, "HkBoardMetadataTeams") {
			fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","key":"ROB","updatedAt":"2026-01-01T00:00:00Z","states":{"nodes":[{"id":"progress","name":"In Progress","updatedAt":"2026-01-01T00:00:00Z"}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}`)
		} else if strings.Contains(in.Query, "HkBoardMetadataLabels") {
			fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[{"id":"fleet","name":"fleet","updatedAt":"2026-01-01T00:00:00Z","team":{"id":"team"}}],"pageInfo":{"hasNextPage":false}}}}`)
		} else {
			t.Error("unexpected metadata request", in.Query)
		}
	}))
	defer srv.Close()
	meta := &memoryBoardMetadata{}
	api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: meta}
	for range 100 {
		target, err := api.BoardTarget(t.Context())
		if err != nil || target.States["In Progress"] != "progress" {
			t.Fatal(target, err)
		}
		label, err := api.BoardLabel(t.Context(), "team", "fleet")
		if err != nil || label != "fleet" {
			t.Fatal(label, err)
		}
	}
	if calls != 2 || meta.fetches != 1 {
		t.Fatal(calls, meta.fetches)
	}
	t.Logf("100 metadata consumers: %d Linear calls total, initialization only", calls)
}
func TestLinearBoardMissingLabelOneCircuitOneError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in struct{ Query string }
		json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in.Query, "HkBoardLabelByID") {
			fmt.Fprint(w, `{"data":{"issueLabel":null}}`)
		} else {
			fmt.Fprint(w, `{"data":{"issueLabelCreate":{"success":false}}}`)
		}
	}))
	defer srv.Close()
	control := &LinearBoardControl{}
	api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: boardMetadataFixture(), BoardControl: control}
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	for i := int64(1); i <= 100; i++ {
		b := boardFixture()
		b.Task.ID = i
		q.add(b, store.LinearBoardEvent{})
	}
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)
	worker := LinearBoardWorker{Store: q, Linear: api, Control: control}
	for range 720 {
		worker.Drain(t.Context())
	}
	if calls != 2 || q.processed != 1 || strings.Count(out.String(), "linear_board.circuit_open") != 1 || strings.Contains(out.String(), "linear_board.sync_failed") {
		t.Fatal(calls, out.String())
	}
	pending := 0
	for _, row := range q.rows {
		if row.State == "pending" {
			pending++
		}
	}
	if pending != 100 {
		t.Fatal("circuit consumed work", q.rows)
	}
	t.Logf("simulated hour (720 five-second drains, 100 queued tasks): calls=%d, errors=1, calls/hour=%d", calls, calls)
}
func TestLinearBoardRetryAfterPausesAllCalls(t *testing.T) {
	for _, header := range []string{"120", time.Date(2026, 11, 3, 0, 2, 0, 0, time.UTC).Format(http.TimeFormat)} {
		t.Run(header, func(t *testing.T) {
			now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
			control := &LinearBoardControl{now: func() time.Time { return now }}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", header)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()
			api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: boardMetadataFixture(), BoardControl: control}
			q := &boardQueue{links: map[int64]store.LinearIssue{}}
			for i := int64(1); i <= 20; i++ {
				b := boardFixture()
				b.Task.ID = i
				q.add(b, store.LinearBoardEvent{})
			}
			worker := LinearBoardWorker{Store: q, Linear: api, Control: control}
			worker.Drain(t.Context())
			if calls != 1 || !control.blocked() {
				t.Fatal(calls, control)
			}
			for range 50 {
				worker.Drain(t.Context())
				var result any
				_ = api.graphql(t.Context(), time.Second, "query{}", nil, &result)
			}
			if calls != 1 {
				t.Fatal("requests during Retry-After", calls)
			}
			now = now.Add(121 * time.Second)
			if control.blocked() {
				t.Fatal("pause never expired")
			}
			var result any
			_ = api.graphql(t.Context(), time.Second, "query{}", nil, &result)
			if calls != 2 {
				t.Fatal(calls)
			}
		})
	}
}
func TestLinearBoardClosedTasksHourlyIdleMakesZeroCalls(t *testing.T) {
	_, url := testdb.Schema(t, "board_idle")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 100; i++ {
		task, err := st.CreateTask(t.Context(), store.Task{Lane: "idle", Title: fmt.Sprintf("closed %d", i), Kind: "implement", Priority: 2, CreatedBy: "test", Project: projectStr("experiment")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.ClaimTask(t.Context(), task.ID, "test", fmt.Sprintf("closed-job-%d", i), ""); err != nil {
			t.Fatal(err)
		}
		if _, err = st.TransitionTask(t.Context(), task.ID, "dropped", "test", "closed", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	control := &LinearBoardControl{}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		t.Error("closed task caused Linear call")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: st, BoardControl: control}
	st.EnableLinearBoard()
	// Explicit one-shot repair may still build local console projections; it
	// queues no remote work for these never-linked terminal tasks.
	if err = st.ReconcileLinearBoard(t.Context()); err != nil {
		t.Fatal(err)
	}
	worker := LinearBoardWorker{Store: st, Linear: api, Control: control}
	for range 720 {
		worker.Drain(t.Context())
	}
	if calls != 0 {
		t.Fatal(calls)
	}
	t.Logf("N=100 closed tasks; one manual repair + 720 drains: Linear calls/hour=%d", calls)
}

func (m *memoryBoardMetadata) RefreshLinearBoardMetadata(ctx context.Context, fetch func(context.Context) ([]store.LinearMetadata, error)) error {
	m.ready = false
	m.failed = nil
	return m.EnsureLinearBoardMetadata(ctx, fetch)
}

type boardWebhookSpy struct {
	events   []store.LinearBoardIssueEvent
	metadata []store.LinearMetadata
}

func (b *boardWebhookSpy) RecordLinearBoardIssue(_ context.Context, e store.LinearBoardIssueEvent) error {
	b.events = append(b.events, e)
	return nil
}
func (b *boardWebhookSpy) ApplyLinearMetadata(_ context.Context, m []store.LinearMetadata) error {
	b.metadata = append(b.metadata, m...)
	return nil
}
func TestLinearBoardSignedWebhookOwnsLinksAndNestedMetadata(t *testing.T) {
	spy := &boardWebhookSpy{}
	control := &LinearBoardControl{missingLabel: "fleet", missingTeam: "team"}
	h := LinearWebhook{Secret: fakeLinearSigningValue, Board: spy, BoardControl: control, Store: &fakeDots{}, Issues: &fakeIssues{}}
	body := []byte(fmt.Sprintf(`{"type":"Issue","action":"create","webhookTimestamp":%d,"data":{"id":"issue","identifier":"ROB-1","description":"<!-- work-board:task:42 -->","updatedAt":"2026-11-03T01:00:00Z","team":{"id":"team","key":"ROB"},"state":{"id":"s","name":"In Progress"}}}`, time.Now().UnixMilli()))
	status, _ := postLinear(t, h, body, "invalid")
	if status != http.StatusUnauthorized || len(spy.events) != 0 || len(spy.metadata) != 0 {
		t.Fatal(status, spy)
	}
	status, _ = postLinear(t, h, body, linearSign(fakeLinearSigningValue, body))
	if status != http.StatusOK || len(spy.events) != 1 || len(spy.metadata) != 2 || spy.events[0].Identifier != "ROB-1" {
		t.Fatal(status, spy)
	}
	label := []byte(fmt.Sprintf(`{"type":"IssueLabel","action":"create","webhookTimestamp":%d,"data":{"id":"label","name":"fleet","teamId":"team","updatedAt":"2026-11-03T01:00:00Z"}}`, time.Now().UnixMilli()))
	status, _ = postLinear(t, h, label, linearSign(fakeLinearSigningValue, label))
	if status != http.StatusOK || len(spy.metadata) != 3 || control.blocked() {
		t.Fatal(status, spy, control)
	}
	labelRemove := []byte(fmt.Sprintf(`{"type":"IssueLabel","action":"remove","createdAt":"2026-11-03T01:10:00Z","webhookTimestamp":%d,"data":{"id":"label","updatedAt":"2026-11-03T01:00:00Z"}}`, time.Now().UnixMilli()))
	status, _ = postLinear(t, h, labelRemove, linearSign(fakeLinearSigningValue, labelRemove))
	if status != http.StatusOK || len(spy.metadata) != 4 || !spy.metadata[3].Deleted || spy.metadata[3].UpdatedAt.Format(time.RFC3339) != "2026-11-03T01:10:00Z" {
		t.Fatal("removal lost to stale entity time", status, spy)
	}
	archive := []byte(fmt.Sprintf(`{"type":"Issue","action":"remove","webhookTimestamp":%d,"data":{"id":"issue"}}`, time.Now().UnixMilli()))
	status, _ = postLinear(t, h, archive, linearSign(fakeLinearSigningValue, archive))
	if status != http.StatusOK || len(spy.events) != 2 || !spy.events[1].Inactive {
		t.Fatal(status, spy)
	}
}

type metadataRefreshSpy struct{ calls int }

func (s *metadataRefreshSpy) RefreshLinearMetadata(context.Context) error { s.calls++; return nil }
func TestLinearMetadataRefreshRequiresBearerAndExplicitCommand(t *testing.T) {
	spy := &metadataRefreshSpy{}
	handler := Server{Tokens: Tokens{"operator": "fake-token"}, LinearMetadata: spy}.Handler()
	for _, tc := range []struct {
		token, body string
		want        int
	}{{"", "{}", http.StatusUnauthorized}, {"fake-token", `{"extra":true}`, http.StatusBadRequest}, {"fake-token", "{}", http.StatusOK}} {
		req := httptest.NewRequest(http.MethodPost, "/v1/linear/metadata/refresh", strings.NewReader(tc.body))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	if spy.calls != 1 {
		t.Fatal(spy.calls)
	}
}
func TestLinearBoardCreateMakesNoIdentitySearchAndWaitsForWebhook(t *testing.T) {
	gate := newAdmissionFake()
	queries := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Query string }
		json.NewDecoder(r.Body).Decode(&in)
		queries = append(queries, in.Query)
		if strings.Contains(in.Query, "HkAdmissionCount") {
			fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
		} else if strings.Contains(in.Query, "issueCreate") {
			fmt.Fprint(w, `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"ROB-1","url":"https://example.test/ROB-1"}}}}`)
		} else {
			t.Error("unexpected identity search", in.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Admission: gate, CapacityScopeVerified: true}
	id := store.LinearDecisionUUID("work-board:task:42")
	_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{Board: true, TaskID: 42, ID: id, TeamID: "team", LabelID: "fleet"})
	if err != nil || len(queries) != 2 || !gate.pending[id] || !gate.awaiting[id] {
		t.Fatal(queries, gate, err)
	}
	// A second attempt cannot issue another create; the durable reservation is
	// retained until the webhook establishes the link.
	_, err = api.CreateLinearIssue(t.Context(), LinearDecisionIssue{Board: true, TaskID: 42, ID: id, TeamID: "team", LabelID: "fleet"})
	if !errors.Is(err, errBoardAwaitWebhook) || len(queries) != 2 {
		t.Fatal(queries, err)
	}
}

func TestLinearBoardManualRepairOnlyStampsIssueForWebhook(t *testing.T) {
	b := boardFixture()
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	q.add(b, store.LinearBoardEvent{})
	gate := newAdmissionFake()
	id := store.LinearDecisionUUID("work-board:task:42")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in struct {
			Query     string
			Variables map[string]json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in.Query, "HkBoardRepair") {
			fmt.Fprintf(w, `{"data":{"issues":{"nodes":[{"id":%q,"identifier":"ROB-42","description":"human notes","team":{"id":"team"}}],"pageInfo":{"hasNextPage":false}}}}`, id)
		} else if strings.Contains(in.Query, "issueUpdate") {
			var input struct{ Description string }
			json.Unmarshal(in.Variables["input"], &input)
			if !strings.Contains(input.Description, "human notes") || !strings.Contains(input.Description, "<!-- work-board:task:42 -->") {
				t.Error(input)
			}
			fmt.Fprint(w, `{"data":{"issueUpdate":{"success":true}}}`)
		} else {
			t.Error("manual reconcile created/searched", in.Query)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Metadata: boardMetadataFixture(), Admission: gate}
	if _, err := api.ReconcileLinearBoardIssue(t.Context(), q, 42); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(q.links) != 0 || !gate.awaiting[id] {
		t.Fatal(calls, q.links, gate)
	}
}

func TestLinearBoardCreateRejectionReleasesOnlyDefiniteReservation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		response   string
		keep, held bool
	}{
		{"http field rejection", http.StatusBadRequest, "", false, false},
		{"capacity", http.StatusOK, `{"errors":[{"message":"Workspace issue limit reached"}]}`, false, true},
		{"ambiguous graphql", http.StatusOK, `{"errors":[{"message":"response lost"}]}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newAdmissionFake()
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var in struct{ Query string }
				json.NewDecoder(r.Body).Decode(&in)
				if strings.Contains(in.Query, "HkAdmissionCount") {
					fmt.Fprint(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`)
					return
				}
				if !strings.Contains(in.Query, "issueCreate") {
					t.Error("rejection caused identity lookup", in.Query)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()
			api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Admission: gate, CapacityScopeVerified: true}
			id := store.LinearDecisionUUID("work-board:task:42")
			_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{Board: true, TaskID: 42, ID: id, TeamID: "team", LabelID: "fleet"})
			_, kept := gate.pending[id]
			if err == nil || kept != tc.keep || gate.held != tc.held || calls != 2 {
				t.Fatal(kept, gate.held, calls, err)
			}
		})
	}
}

func TestLinearBoardAwaitingWebhookBooksCapacitySlot(t *testing.T) {
	gate := newAdmissionFake()
	gate.pending["prior"] = true
	gate.awaiting = map[string]bool{"prior": true}
	gate.board = map[string]bool{"prior": true}
	remote := &admissionRemote{count: 269, issues: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(remote.serve))
	defer srv.Close()
	api := LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Admission: gate, CapacityScopeVerified: true}
	_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{Board: true, TaskID: 42, ID: store.LinearDecisionUUID("work-board:task:42"), TeamID: "team", LabelID: "fleet"})
	if !errors.Is(err, ErrLinearCapacity) || remote.creates != 0 {
		t.Fatal("unacknowledged acceptance lost its slot", remote.creates, err)
	}
}

func (b *boardWebhookSpy) LinearBoardLabelAvailable(_ context.Context, team, name string) (bool, error) {
	for _, m := range b.metadata {
		if m.Kind == "label" && m.Name == name && !m.Deleted && (m.TeamID == team || m.TeamID == "") {
			return true, nil
		}
	}
	return false, nil
}
