package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type admissionFake struct {
	board     map[string]bool
	awaiting  map[string]bool
	mu        sync.Mutex
	pending   map[string]bool
	excluded  bool
	task      *store.Task
	holdLimit int
	held      bool
}

func newAdmissionFake() *admissionFake { return &admissionFake{pending: map[string]bool{}} }
func (f *admissionFake) SerializeLinearCreate(ctx context.Context, run func(store.LinearCreateLease) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return run(f)
}
func (f *admissionFake) Task(_ context.Context, id int64) (store.Task, bool, error) {
	if f.task != nil {
		return *f.task, true, nil
	}
	priority := 2
	if f.excluded {
		priority = 3
	}
	return store.Task{ID: id, Priority: priority}, true, nil
}
func (f *admissionFake) Pending(context.Context) ([]string, error) {
	out := []string{}
	for id := range f.pending {
		if f.awaiting[id] {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
func (f *admissionFake) Reserve(_ context.Context, id string) error { f.pending[id] = true; return nil }
func (f *admissionFake) Reject(_ context.Context, id string) error  { f.pending[id] = false; return nil }
func (f *admissionFake) RejectionKnown(_ context.Context, id string) (bool, error) {
	uncertain, found := f.pending[id]
	return found && !uncertain, nil
}

func (f *admissionFake) CapacityHold(context.Context) (int, bool, error) {
	return f.holdLimit, f.held, nil
}
func (f *admissionFake) HoldCapacity(_ context.Context, limit int) error {
	f.holdLimit = limit
	f.held = true
	return nil
}
func (f *admissionFake) ClearCapacityHold(context.Context) error { f.held = false; return nil }

func (f *admissionFake) Complete(_ context.Context, id string) error {
	delete(f.pending, id)
	delete(f.board, id)
	delete(f.awaiting, id)
	return nil
}

type admissionRemote struct {
	mu                  sync.Mutex
	count               int
	creates             int
	requests            int
	countReply          string
	existingReply       string
	loseResponse        bool
	rejectCreate        bool
	rejectStatus        int
	rejectGraphQL       bool
	rejectMessage       string
	acceptThenReject    bool
	rejectThenReadFails bool
	issues              map[string]bool
}

func (f *admissionRemote) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	var in struct {
		Query     string
		Variables map[string]json.RawMessage
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		w.WriteHeader(400)
		return
	}
	switch {
	case strings.Contains(in.Query, "HkAdmissionExisting"):
		if f.existingReply != "" {
			fmt.Fprint(w, f.existingReply)
			return
		}
		var id string
		_ = json.Unmarshal(in.Variables["id"], &id)
		nodes := []map[string]string{}
		if f.issues[id] {
			nodes = append(nodes, map[string]string{"id": id, "identifier": "ROB-1", "url": "https://linear.example/1"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]bool{"hasNextPage": false}}}})
	case strings.Contains(in.Query, "HkAdmissionCount"):
		if f.countReply != "" {
			fmt.Fprint(w, f.countReply)
			return
		}

		var after string
		_ = json.Unmarshal(in.Variables["after"], &after)
		offset, _ := strconv.Atoi(after)
		nodes := []map[string]string{}
		end := min(offset+100, f.count)
		for i := offset; i < end; i++ {
			nodes = append(nodes, map[string]string{"id": fmt.Sprint(i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": end < f.count, "endCursor": strconv.Itoa(end)}}}})

	case strings.Contains(in.Query, "issueCreate"):
		f.creates++
		if f.rejectCreate {
			if f.rejectThenReadFails {
				f.existingReply = `{"errors":[{"message":"read unavailable"}]}`
			}
			if f.rejectGraphQL {
				message := f.rejectMessage
				if message == "" {
					message = "argument rejected"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": message, "extensions": map[string]string{"type": "UserError"}}}})
				return
			}
			status := f.rejectStatus
			if status == 0 {
				status = 503
			}
			w.WriteHeader(status)
			return
		}
		var input struct{ ID string }
		_ = json.Unmarshal(in.Variables["input"], &input)
		f.issues[input.ID] = true
		f.count++
		if f.acceptThenReject {
			f.acceptThenReject = false
			fmt.Fprint(w, `{"errors":[{"message":"response field failed"}]}`)
			return
		}
		if f.loseResponse {
			f.loseResponse = false
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"ROB-1","url":"https://linear.example/1"}}}}`)
	default:
		w.WriteHeader(400)
	}
}
func admissionClient(t *testing.T, f *admissionRemote) (LinearAPI, *admissionFake) {
	t.Helper()
	if f.issues == nil {
		f.issues = map[string]bool{}
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	gate := newAdmissionFake()
	return LinearAPI{Key: "fake", URL: srv.URL, HTTP: srv.Client(), Admission: gate, CapacityScopeVerified: true}, gate
}

func TestLinearAdmissionCountAndScopeFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		count       int
	}{
		{name: "below", count: 269}, {name: "at watermark", count: 270}, {name: "above", count: 275},
		{name: "null count", reply: `{"data":{"issues":null}}`},
		{name: "missing page info", reply: `{"data":{"issues":{"nodes":[]}}}`},
		{name: "duplicate ids", reply: `{"data":{"issues":{"nodes":[{"id":"x"},{"id":"x"}],"pageInfo":{"hasNextPage":false}}}}`},
		{name: "unknown", reply: `{"errors":[{"message":"unavailable"}]}`},
		{name: "missing count cursor", reply: `{"data":{"issues":{"nodes":[{"id":"one"}],"pageInfo":{"hasNextPage":true}}}}`},
		{name: "empty partial page", reply: `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &admissionRemote{count: tc.count, countReply: tc.reply}
			api, gate := admissionClient(t, remote)
			_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "new"})
			if tc.name == "below" {
				if err != nil || remote.creates != 1 || len(gate.pending) != 0 {
					t.Fatal(remote.creates, gate.pending, err)
				}
			} else if !errors.Is(err, ErrLinearCapacity) || remote.creates != 0 {
				t.Fatal(remote.creates, err)
			}
		})
	}
	remote := &admissionRemote{}
	api, _ := admissionClient(t, remote)
	api.CapacityScopeVerified = false
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "new"}); !errors.Is(err, ErrLinearCapacity) || remote.requests != 0 {
		t.Fatal(remote.requests, err)
	}
	api.CapacityScopeVerified = true
	api.Admission = nil
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "new"}); !errors.Is(err, ErrLinearCapacity) || remote.requests != 0 {
		t.Fatal(remote.requests, err)
	}
}

func TestLinearAdmissionConcurrentLastSlot(t *testing.T) {
	remote := &admissionRemote{count: 269}
	api, _ := admissionClient(t, remote)
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		go func() {
			_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: id})
			results <- err
		}()
	}
	admitted, pending := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			admitted++
		} else if errors.Is(err, ErrLinearCapacity) {
			pending++
		} else {
			t.Fatal(err)
		}
	}
	if admitted != 1 || pending != 1 || remote.creates != 1 || remote.count != 270 {
		t.Fatal(admitted, pending, remote.creates, remote.count)
	}
}

func TestLinearAdmissionRecoversLostResponseAtWatermark(t *testing.T) {
	remote := &admissionRemote{count: 269, loseResponse: true}
	api, gate := admissionClient(t, remote)
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "stable"}); err != nil || len(gate.pending) != 0 {
		t.Fatal(gate.pending, err)
	}
	issue, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "stable"})
	if err != nil || issue.Identifier != "ROB-1" || remote.creates != 1 || len(gate.pending) != 0 {
		t.Fatal(issue, remote.creates, gate.pending, err)
	}
}

func TestLinearAdmissionUncertainCreateBooksSlotForNewIdentity(t *testing.T) {
	remote := &admissionRemote{rejectCreate: true}
	api, gate := admissionClient(t, remote)
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "uncertain"}); err == nil {
		t.Fatal("failure accepted")
	}
	remote.rejectCreate = false
	_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "different"})
	if err != nil || remote.creates != 2 || !gate.pending["uncertain"] {
		t.Fatal(remote.creates, gate.pending, err)
	}
}

func TestWorkBoardMirrorAdmissionRechecksCurrentTask(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		priority                  int
		reason                    string
		terminal, linked, missing bool
		wantCreates, wantUpdates  int
		wantState                 string
	}{
		{name: "old eligible payload now excluded", priority: 3, wantState: "skipped"},
		{name: "priority now eligible", priority: 2, wantCreates: 1, wantState: "pending"},
		{name: "unset", priority: 0, wantState: "skipped"},
		{name: "dots", priority: 3, reason: store.LinearMirrorDots, wantCreates: 1, wantState: "pending"},
		{name: "operator visible", priority: 3, reason: store.LinearMirrorOperatorVisible, wantCreates: 1, wantState: "pending"},
		{name: "terminal never freshly mirrored", priority: 1, terminal: true, wantState: "skipped"},
		{name: "existing ineligible link maintained", priority: 3, linked: true, wantUpdates: 1, wantState: "sent"},
		{name: "missing task", missing: true, wantState: "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := newBoardFake()
			q := &boardQueue{links: map[int64]store.LinearIssue{}}
			b := boardFixture()
			q.add(b, store.LinearBoardEvent{})
			task := b.Task
			task.Priority = tc.priority
			task.Refs.Linear = &store.TaskLinear{MirrorReason: tc.reason}
			if tc.terminal {
				task.State = "merged"
			}
			q.tasks[42] = task
			if tc.missing {
				delete(q.tasks, 42)
			}
			if tc.linked {
				issue := BoardIssue{ID: "linked", Identifier: "ROB-1"}
				issue.Team.ID = "team"
				remote.issues["linked"] = issue
				q.links[42] = store.LinearIssue{IssueID: "linked"}
			}
			LinearBoardWorker{Store: q, Linear: remote}.Drain(t.Context())
			if remote.creates != tc.wantCreates || remote.updates != tc.wantUpdates || q.rows[0].State != tc.wantState {
				t.Fatal(remote.creates, remote.updates, q.rows[0])
			}
		})
	}
}

func TestWorkBoardMirrorOpenedIssueReusesExactLink(t *testing.T) {
	remote := newBoardFake()
	issue := BoardIssue{ID: "original", Identifier: "ROB-9"}
	issue.Team.ID = "team"
	remote.issues["original"] = issue
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	b := boardFixture()
	b.Task.Priority = 3
	b.Task.Refs.Linear = &store.TaskLinear{MirrorReason: store.LinearMirrorOpened, IssueID: "original"}
	q.add(b, store.LinearBoardEvent{})
	LinearBoardWorker{Store: q, Linear: remote}.Drain(t.Context())
	if remote.creates != 0 || q.owners[42].IssueID != "original" || q.rows[0].State != "sent" {
		t.Fatal(remote.creates, q.links, q.rows)
	}
}

func TestLinearAdmissionRechecksPolicyInsideCreateLock(t *testing.T) {
	remote := &admissionRemote{}
	api, gate := admissionClient(t, remote)
	gate.excluded = true
	_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "excluded"})
	if !errors.Is(err, ErrLinearMirrorExcluded) || remote.creates != 0 || len(gate.pending) != 0 {
		t.Fatal(remote.creates, gate.pending, err)
	}
}

func TestLinearAdmissionDecisionIdentityMustStillBeOpen(t *testing.T) {
	for _, request := range []*store.DecisionRequest{nil, {ID: "dr-other", Status: store.DecisionRequestOpen}, {ID: "dr-1", Status: "answered"}, {ID: "dr-1", Status: store.DecisionRequestOpen}} {
		remote := &admissionRemote{}
		api, gate := admissionClient(t, remote)
		gate.task = &store.Task{ID: 1, Priority: 2, Refs: store.TaskRefs{DecisionRequest: request}}
		_, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, RequestID: "dr-1", ID: "decision"})
		expected := request != nil && request.ID == "dr-1" && request.Status == store.DecisionRequestOpen
		if expected {
			if err != nil || remote.creates != 1 {
				t.Fatal(remote.creates, err)
			}
		} else if !errors.Is(err, ErrLinearMirrorExcluded) || remote.creates != 0 {
			t.Fatal(remote.creates, err)
		}
	}
}

func TestWorkBoardMirrorOwnershipBeforeROBReuse(t *testing.T) {
	remote := newBoardFake()
	issue := BoardIssue{ID: "shared", Identifier: "ROB-77"}
	issue.Team.ID = "team"
	remote.issues["ROB-77"] = issue
	remote.issues["shared"] = issue
	q := &boardQueue{links: map[int64]store.LinearIssue{99: {TaskID: 99, IssueID: "shared", Identifier: "ROB-77"}}}
	b := boardFixture()
	b.Task.Title = "Operate ROB-77"
	q.add(b, store.LinearBoardEvent{})
	LinearBoardWorker{Store: q, Linear: remote}.Drain(t.Context())
	if remote.creates != 0 || remote.updates != 0 || q.rows[0].State != "failed" || q.links[99].IssueID != "shared" {
		t.Fatal(remote.creates, remote.updates, q.rows, q.links)
	}
}

func TestWorkBoardMirrorOldSnapshotsUseCurrentVersion(t *testing.T) {
	remote := newBoardFake()
	q := &boardQueue{links: map[int64]store.LinearIssue{}}
	old := boardFixture()
	q.add(old, store.LinearBoardEvent{ID: "old", Kind: "READY", At: time.Now()})
	newer := old
	newer.State = "In Review"
	newer.Task.Title = "Current title"
	q.add(newer, store.LinearBoardEvent{ID: "new", Kind: "READY", At: time.Now()})
	// This task was already mirrored before completion.
	issue := BoardIssue{ID: "owned", Identifier: "ROB-1"}
	issue.Team.ID = "team"
	remote.issues["owned"] = issue
	q.links[42] = store.LinearIssue{IssueID: "owned"}
	LinearBoardWorker{Store: q, Linear: remote}.Drain(t.Context())
	// Redeliver the oldest snapshot after the newer version was delivered.
	late := q.rows[0]
	late.State = "pending"
	q.rows = append(q.rows, late)
	LinearBoardWorker{Store: q, Linear: remote}.Drain(t.Context())
	for _, state := range remote.states {
		if state != "In Review" {
			t.Fatalf("old version rolled state back: %v", remote.states)
		}
	}
	if len(remote.states) != 3 || len(remote.comments) != 2 || q.boards[42].Version != 2 {
		t.Fatal(remote.states, remote.comments, q.boards)
	}
}

func TestLinearAdmissionDefiniteRejectionReleasesWithoutReadback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		graphql bool
	}{{"validation", 400, false}, {"rate limit", 429, false}, {"graphql", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &admissionRemote{rejectCreate: true, rejectStatus: tc.status, rejectGraphQL: tc.graphql}
			api, gate := admissionClient(t, remote)
			if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "retry"}); err == nil || len(gate.pending) != 0 {
				t.Fatal("rejected reservation stuck", gate.pending, err)
			}
			remote.mu.Lock()
			remote.rejectCreate = false
			remote.mu.Unlock()
			if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "retry"}); err != nil || remote.count != 1 || remote.creates != 2 {
				t.Fatal(remote.count, remote.creates, err)
			}
		})
	}
	remote := &admissionRemote{rejectCreate: true, rejectStatus: 429, rejectThenReadFails: true}
	api, gate := admissionClient(t, remote)
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "uncertain"}); err == nil || len(gate.pending) != 0 {
		t.Fatal("known rate rejection retained reservation", gate.pending, err)
	}
	remote.mu.Lock()
	remote.rejectCreate = false
	remote.existingReply = ""
	remote.mu.Unlock()
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "uncertain"}); err != nil || len(gate.pending) != 0 || remote.count != 1 {
		t.Fatal("known rejection failed to recover", gate.pending, err)
	}
}

func TestLinearAdmissionGraphQLResponseErrorRecoversAcceptedCreate(t *testing.T) {
	remote := &admissionRemote{acceptThenReject: true}
	api, gate := admissionClient(t, remote)
	result, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "accepted"})
	if err != nil || result.Identifier != "ROB-1" || remote.creates != 1 || len(gate.pending) != 0 {
		t.Fatal(result, remote.creates, gate.pending, err)
	}
}

func TestWorkBoardMirrorWaitsForWebhookAfterTerminalTransition(t *testing.T) {
	remote := newBoardFake()
	stable := store.LinearDecisionUUID("work-board:task:42")
	q := &boardQueue{links: map[int64]store.LinearIssue{}, reservations: map[string]bool{stable: true}}
	b := boardFixture()
	b.ApplyTask(store.Task{ID: 42, State: "merged", Priority: 2})
	q.add(b, store.LinearBoardEvent{})
	LinearBoardWorker{Store: q, Linear: remote}.Drain(t.Context())
	if remote.creates != 0 || remote.updates != 0 || len(q.links) != 0 || q.rows[0].State != "pending" {
		t.Fatal(remote, q.rows)
	}
}

func TestLinearAdmissionCapacityRejectionHoldsUntilKnownSpace(t *testing.T) {
	remote := &admissionRemote{count: 250, rejectCreate: true, rejectGraphQL: true, rejectMessage: "Workspace issue limit reached"}
	api, gate := admissionClient(t, remote)
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "at-limit"}); err == nil || !gate.held || len(gate.pending) != 0 {
		t.Fatal(gate.held, gate.pending, err)
	}
	remote.mu.Lock()
	remote.rejectCreate = false
	remote.mu.Unlock()
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "different"}); !errors.Is(err, ErrLinearCapacity) || remote.creates != 1 {
		t.Fatal("vendor hold bypassed", remote.creates, err)
	}
	policy, err := NewLinearCapacityPolicy(250, 230, 200)
	if err != nil {
		t.Fatal(err)
	}
	api.CapacityPolicy = policy
	remote.mu.Lock()
	remote.count = 244
	remote.mu.Unlock()
	if _, err := api.CreateLinearIssue(t.Context(), LinearDecisionIssue{TaskID: 1, ID: "same-policy-lower"}); err != nil || gate.held || remote.count != 245 {
		t.Fatal("lower configured limit failed to release known space", gate.held, remote.count, err)
	}
}

func TestLinearCapacityPolicyValidation(t *testing.T) {
	for _, values := range [][3]int{{275, 230, 200}, {250, 230, 200}, {250, 210, 180}} {
		p, err := NewLinearCapacityPolicy(values[0], values[1], values[2])
		if err != nil || p.CreateCeiling() != values[0]-5 {
			t.Fatal(p, err)
		}
	}
	for _, values := range [][3]int{{5, 3, 1}, {250, 245, 200}, {250, 200, 200}, {250, 230, -1}} {
		if _, err := NewLinearCapacityPolicy(values[0], values[1], values[2]); err == nil {
			t.Fatal("invalid policy accepted", values)
		}
	}
}

func TestLinearCapacityRejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		message, kind, code string
		want                bool
	}{
		{"Workspace issue limit reached", "", "", true}, {"", "ISSUE_LIMIT_EXCEEDED", "", true}, {"", "", "PLAN_LIMIT_EXCEEDED", true},
		{"Maximum number of issues reached", "", "", true}, {"Issue request rate limit exceeded", "", "", false}, {"", "RATELIMITED", "", false},
		{"Issue title character limit exceeded", "", "", false}, {"permission denied", "", "", false},
	} {
		if got := linearCapacityRejected(tc.message, tc.kind, tc.code); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}

func (f *admissionFake) ReserveBoard(ctx context.Context, id string, taskID int64) (bool, error) {
	for pending := range f.pending {
		if pending == id {
			return false, nil
		}
	}
	if f.board == nil {
		f.board = map[string]bool{}
	}
	f.board[id] = true
	return true, f.Reserve(ctx, id)
}
func (f *admissionFake) AwaitBoardWebhook(_ context.Context, id string) error {
	if f.awaiting == nil {
		f.awaiting = map[string]bool{}
	}
	f.awaiting[id] = true
	return nil
}
func (f *admissionFake) LinearBoardReservation(_ context.Context, id string) (store.LinearBoardReservationState, error) {
	if !f.board[id] {
		return store.LinearBoardReservationNone, nil
	}
	if f.awaiting[id] {
		return store.LinearBoardReservationAwaitingWebhook, nil
	}
	return store.LinearBoardReservationPending, nil
}

func (f *admissionFake) ReservedCreates(context.Context) (int, error) { return len(f.pending), nil }
func (f *admissionFake) ReleaseAbsent(ctx context.Context, id string) error {
	return f.Complete(ctx, id)
}
