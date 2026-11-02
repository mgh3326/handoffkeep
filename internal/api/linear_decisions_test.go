package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

const fakeOperator = "operator-user-id"

// fakeLinear stands in for Linear. Like the real API it refuses a second create
// under an id it already holds.
type fakeLinear struct {
	fakeIssues
	target    LinearDecisionTarget
	targetErr error
	created   map[string]LinearDecisionIssue
	creates   int
	loseReply bool
	createErr error
	comments  []string
	closes    []string
	opErr     error
}

func newFakeLinear() *fakeLinear {
	return &fakeLinear{target: LinearDecisionTarget{TeamID: "team", LabelID: "label", CancelStateID: "canceled"}, created: map[string]LinearDecisionIssue{}}
}

func (f *fakeLinear) LinearDecisionTarget(context.Context, string, string) (LinearDecisionTarget, error) {
	return f.target, f.targetErr
}

func (f *fakeLinear) CreateLinearIssue(_ context.Context, in LinearDecisionIssue) (LinearIssue, error) {
	f.creates++
	if f.createErr != nil {
		return LinearIssue{}, f.createErr
	}
	if _, dup := f.created[in.ID]; dup {
		return LinearIssue{}, errors.New("entity already exists")
	}
	f.created[in.ID] = in
	if f.loseReply {
		f.loseReply = false
		return LinearIssue{}, errors.New("response lost")
	}
	return f.issueFor(in.ID), nil
}

func (f *fakeLinear) issueFor(id string) LinearIssue {
	return LinearIssue{Identifier: "ROB-" + id[:4], Title: f.created[id].Title, URL: "https://linear.example/" + id[:4], Labels: []string{store.LinearDecisionLabel}, StateType: "unstarted"}
}

func (f *fakeLinear) LinearIssue(_ context.Context, id string) (LinearIssue, error) {
	if _, ok := f.created[id]; ok {
		return f.issueFor(id), nil
	}
	return f.fakeIssues.LinearIssue(context.Background(), id)
}

func (f *fakeLinear) CreateLinearComment(_ context.Context, _, issueID, body string) error {
	if f.opErr != nil {
		return f.opErr
	}
	f.comments = append(f.comments, issueID+": "+body)
	return nil
}

func (f *fakeLinear) UpdateLinearIssueState(_ context.Context, issueID, stateID string) error {
	if f.opErr != nil {
		return f.opErr
	}
	f.closes = append(f.closes, issueID+": "+stateID)
	return nil
}

// fakeAskStore holds what asking writes, with the store's duplicate rule.
type fakeAskStore struct {
	result    store.DecisionRequestResult
	recordErr error
	records   int
	links     map[string]store.LinearDecisionLink
	announced []string
}

func (f *fakeAskStore) RecordDecisionRequest(_ context.Context, taskID int64, by string, in store.DecisionRequestInput) (store.DecisionRequestResult, error) {
	if f.recordErr != nil {
		return store.DecisionRequestResult{}, f.recordErr
	}
	if f.result.Request.ID != "" && f.result.Request.Question == in.Question {
		out := f.result
		out.Duplicate = true
		return out, nil
	}
	f.records++
	options := in.Options
	f.result = store.DecisionRequestResult{
		Task: store.Task{ID: taskID, Refs: store.TaskRefs{DecisionOptions: &options}},
		Request: store.DecisionRequest{ID: fmt.Sprintf("dr-%d-%d", taskID, f.records), Revision: f.records, Status: store.DecisionRequestOpen, Question: in.Question,
			DefaultAction: in.DefaultAction, DefaultOption: in.DefaultOption, DueAt: in.DueAt, Doc: in.Doc, Refs: in.Refs, Supersedes: in.Supersedes, RequestedBy: by},
	}
	return f.result, nil
}

func (f *fakeAskStore) LinearDecisionLinkByRequest(_ context.Context, id string) (store.LinearDecisionLink, bool, error) {
	link, ok := f.links[id]
	return link, ok, nil
}

func (f *fakeAskStore) RecordLinearDecisionIssue(_ context.Context, link store.LinearDecisionLink, _, _, announce string) (store.LinearDecisionLink, bool, error) {
	if existing, ok := f.links[link.RequestID]; ok {
		return existing, false, nil
	}
	if f.links == nil {
		f.links = map[string]store.LinearDecisionLink{}
	}
	f.links[link.RequestID] = link
	f.announced = append(f.announced, announce)
	return link, true, nil
}

func askInput() store.DecisionRequestInput {
	due := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	return store.DecisionRequestInput{
		Question: "Ship the migration?",
		Options:  store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "ship now", Recommended: true}, {Key: "B", Label: "wait a week"}}},
		Reason:   "low risk", DefaultAction: "hold", DefaultOption: "B", DueAt: &due, Doc: "design/x", Refs: []string{"handoffkeep#107"},
	}
}

func TestDecisionAskCreatesOneIssueAndRerunCreatesNothing(t *testing.T) {
	linear, st := newFakeLinear(), &fakeAskStore{}
	l := LinearDecisions{Team: "ROB", Linear: linear}
	first, err := l.Ask(t.Context(), st, 7, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	if !first.IssueCreated || first.Duplicate || first.Issue == nil || first.DecisionRef != "D-7-1" || st.records != 1 || linear.creates != 1 {
		t.Fatalf("first=%+v records=%d creates=%d", first, st.records, linear.creates)
	}
	wantVer := store.DecisionRequestVer("Ship the migration?", askInput().Options, "design/x", []string{"handoffkeep#107"})
	if first.Ver != wantVer || first.Issue.Ver != wantVer {
		t.Fatalf("ver=%q link=%q want %q", first.Ver, first.Issue.Ver, wantVer)
	}
	issue := linear.created[first.Issue.IssueID]
	if issue.TeamID != "team" || issue.LabelID != "label" || issue.Title != "[D-7-1] Ship the migration?" {
		t.Fatalf("issue=%+v", issue)
	}
	for _, want := range []string{"Ship the migration?", "1. A: ship now (recommended)", "2. B: wait a week", "**Default:** hold (option B)", "2026-11-02 18:00 KST", "hk:doc design/x", "handoffkeep#107", "ver=" + wantVer, "`D-7-1 1`"} {
		if !strings.Contains(issue.Description, want) {
			t.Errorf("body lacks %q:\n%s", want, issue.Description)
		}
	}
	if len(st.announced) != 1 {
		t.Fatalf("announced=%q", st.announced)
	}
	for _, want := range []string{"**D-7-1**", "ver=" + wantVer, "2026-11-02 18:00 KST", "Ship the migration?", "1. A: ship now", "2. B: wait a week", first.Issue.URL} {
		if !strings.Contains(st.announced[0], want) {
			t.Errorf("announcement lacks %q:\n%s", want, st.announced[0])
		}
	}
	second, err := l.Ask(t.Context(), st, 7, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	if second.IssueCreated || !second.Duplicate || second.Issue == nil || second.Issue.IssueID != first.Issue.IssueID || st.records != 1 || linear.creates != 1 || len(st.announced) != 1 {
		t.Fatalf("second=%+v records=%d creates=%d announced=%d", second, st.records, linear.creates, len(st.announced))
	}
}

// The create reached Linear but its response was lost, so no link was stored.
// The re-run must adopt that issue, not make another.
func TestDecisionAskAdoptsTheIssueALostResponseLeftBehind(t *testing.T) {
	linear, st := newFakeLinear(), &fakeAskStore{}
	linear.loseReply = true
	l := LinearDecisions{Team: "ROB", Linear: linear}
	out, err := l.Ask(t.Context(), st, 7, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	if out.Issue == nil || out.Issue.Identifier == "" || len(linear.created) != 1 {
		t.Fatalf("out=%+v created=%d", out, len(linear.created))
	}
}

func TestDecisionAskLinearDownRetriesOnTheSameRequest(t *testing.T) {
	linear, st := newFakeLinear(), &fakeAskStore{}
	linear.createErr = errors.New("boom")
	linear.err = errors.New("not found")
	l := LinearDecisions{Team: "ROB", Linear: linear}
	_, err := l.Ask(t.Context(), st, 7, "desk", askInput())
	var fetch linearFetchError
	if !errors.As(err, &fetch) || len(st.links) != 0 || st.records != 1 {
		t.Fatalf("err=%v links=%v records=%d", err, st.links, st.records)
	}
	linear.createErr = nil
	out, err := l.Ask(t.Context(), st, 7, "desk", askInput())
	if err != nil || !out.IssueCreated || st.records != 1 || len(linear.created) != 1 {
		t.Fatalf("out=%+v err=%v records=%d created=%d", out, err, st.records, len(linear.created))
	}
}

func TestDecisionAskMissingTargetWritesNothing(t *testing.T) {
	linear, st := newFakeLinear(), &fakeAskStore{}
	linear.targetErr = fmt.Errorf("%w: label decision not found", errLinearDecisionTarget)
	if _, err := (LinearDecisions{Team: "ROB", Linear: linear}).Ask(t.Context(), st, 7, "desk", askInput()); !errors.Is(err, errLinearDecisionTarget) {
		t.Fatalf("err=%v", err)
	}
	if st.records != 0 || linear.creates != 0 {
		t.Fatalf("records=%d creates=%d", st.records, linear.creates)
	}
}

func TestDecisionAskClosedRequestGetsNoIssue(t *testing.T) {
	linear, st := newFakeLinear(), &fakeAskStore{}
	input := askInput()
	options := input.Options
	st.result = store.DecisionRequestResult{Task: store.Task{ID: 7, Refs: store.TaskRefs{DecisionOptions: &options}}, Request: store.DecisionRequest{ID: "dr-7-1", Status: store.DecisionRequestAnswered, Question: input.Question}}
	out, err := (LinearDecisions{Team: "ROB", Linear: linear}).Ask(t.Context(), st, 7, "desk", input)
	if err != nil || out.Issue != nil || out.IssueCreated || linear.creates != 0 {
		t.Fatalf("out=%+v err=%v creates=%d", out, err, linear.creates)
	}
}

func TestDecisionAnnouncementStaysWithinDiscordLimit(t *testing.T) {
	input := askInput()
	request := store.DecisionRequest{ID: "dr-7-1", Question: strings.Repeat("가", 5000)}
	got := decisionAnnouncement(request, input.Options, "ab12cd3e", "https://linear.example/x")
	if n := utf8.RuneCountInString(got); n > discordContentMaxRunes {
		t.Fatalf("%d runes", n)
	}
	for _, want := range []string{"ver=ab12cd3e", "2. B: wait a week", "https://linear.example/x", "…", "no deadline"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q", want)
		}
	}
}

func TestLinearDecisionTitleUsesTheFirstLine(t *testing.T) {
	got := linearDecisionTitle(store.DecisionRequest{ID: "dr-7-2", Question: "line one\nline two"})
	if got != "[D-7-2] line one" {
		t.Fatalf("title=%q", got)
	}
	long := linearDecisionTitle(store.DecisionRequest{ID: "dr-7-2", Question: strings.Repeat("가", 300)})
	if n := utf8.RuneCountInString(long); n > linearTitleMaxRunes+len("[D-7-2] …") {
		t.Fatalf("title has %d runes", n)
	}
}

// fakeDecisions records what the webhook passes on.
type fakeDecisions struct {
	linked  bool
	outcome string
	answers []store.LinearDecisionAnswer
	err     error
}

func (f *fakeDecisions) LinearDecisionLinkByIssue(context.Context, string) (store.LinearDecisionLink, bool, error) {
	return store.LinearDecisionLink{RequestID: "dr-7-1"}, f.linked, f.err
}

func (f *fakeDecisions) AnswerLinearDecision(_ context.Context, in store.LinearDecisionAnswer) (string, error) {
	f.answers = append(f.answers, in)
	return f.outcome, nil
}

func decisionLinearIssue() LinearIssue {
	return LinearIssue{Identifier: "ROB-1", Title: "t", URL: "u", Labels: []string{store.LinearDecisionLabel}, StateType: "unstarted"}
}

func decisionWebhook(d LinearDecisionRecorder, issues LinearIssueFetcher, dots DotsRecorder) LinearWebhook {
	return LinearWebhook{Secret: fakeLinearSigningValue, Issues: issues, Store: dots, Decisions: d, Operator: fakeOperator, StateOptions: map[string]int{"Approved": 1, "Rejected": 2}}
}

func signedPost(t *testing.T, h http.Handler, envelope map[string]any) (int, map[string]string) {
	t.Helper()
	envelope["webhookTimestamp"] = time.Now().UnixMilli()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return postLinear(t, h, body, linearSign(fakeLinearSigningValue, body))
}

func commentEnvelope(body, user string, actor map[string]any) map[string]any {
	out := map[string]any{"action": "create", "type": "Comment", "data": map[string]any{"id": "comment-1", "body": body, "userId": user, "createdAt": "2026-11-02T01:02:03.000Z", "issueId": "issue-1"}}
	if actor != nil {
		out["actor"] = actor
	}
	return out
}

func operatorActor() map[string]any { return map[string]any{"id": fakeOperator, "type": "user"} }

func TestLinearWebhookDecisionComment(t *testing.T) {
	closed := decisionLinearIssue()
	closed.StateType, closed.CompletedAt = "completed", "2026-11-01T00:00:00.000Z"
	unlabelled := decisionLinearIssue()
	unlabelled.Labels = []string{"other"}
	withBot := commentEnvelope("1", fakeOperator, operatorActor())
	withBot["data"].(map[string]any)["botActor"] = map[string]any{"type": "integration"}
	nestedUser := commentEnvelope("1", "", operatorActor())
	nestedUser["data"].(map[string]any)["user"] = map[string]any{"id": fakeOperator}
	cases := []struct {
		name     string
		envelope map[string]any
		issue    LinearIssue
		fetchErr error
		status   int
		out      string
		answers  int
		closed   bool
	}{
		{name: "operator", envelope: commentEnvelope("1", fakeOperator, operatorActor()), issue: decisionLinearIssue(), status: http.StatusOK, out: "recorded", answers: 1},
		{name: "operator with ref", envelope: commentEnvelope("D-7-1 2\nnote", fakeOperator, operatorActor()), issue: decisionLinearIssue(), status: http.StatusOK, out: "recorded", answers: 1},
		{name: "operator, actor null", envelope: commentEnvelope("1", fakeOperator, nil), issue: decisionLinearIssue(), status: http.StatusOK, out: "recorded", answers: 1},
		{name: "author only in data.user", envelope: nestedUser, issue: decisionLinearIssue(), status: http.StatusOK, out: "recorded", answers: 1},
		{name: "another user", envelope: commentEnvelope("1", "someone-else", map[string]any{"id": "someone-else", "type": "user"}), issue: decisionLinearIssue(), status: http.StatusOK, out: "ignored"},
		{name: "no author", envelope: commentEnvelope("1", "", nil), issue: decisionLinearIssue(), status: http.StatusOK, out: "ignored"},
		{name: "operator as author, another actor", envelope: commentEnvelope("1", fakeOperator, map[string]any{"id": "someone-else", "type": "user"}), issue: decisionLinearIssue(), status: http.StatusOK, out: "ignored"},
		{name: "oauth client acting as the operator", envelope: commentEnvelope("1", fakeOperator, map[string]any{"id": fakeOperator, "type": "oauthClient"}), issue: decisionLinearIssue(), status: http.StatusOK, out: "ignored"},
		{name: "bot comment", envelope: withBot, issue: decisionLinearIssue(), status: http.StatusOK, out: "ignored"},
		{name: "ordinary comment", envelope: commentEnvelope("I think we should wait", fakeOperator, operatorActor()), issue: decisionLinearIssue(), status: http.StatusOK, out: "ignored"},
		{name: "label removed", envelope: commentEnvelope("1", fakeOperator, operatorActor()), issue: unlabelled, status: http.StatusOK, out: "ignored"},
		{name: "closed issue is passed on as closed", envelope: commentEnvelope("1", fakeOperator, operatorActor()), issue: closed, status: http.StatusOK, out: "recorded", answers: 1, closed: true},
		{name: "linear down", envelope: commentEnvelope("1", fakeOperator, operatorActor()), fetchErr: errors.New("boom"), status: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decisions := &fakeDecisions{linked: true, outcome: "recorded"}
			dots := &fakeDots{}
			status, out := signedPost(t, decisionWebhook(decisions, &fakeIssues{issue: tc.issue, err: tc.fetchErr}, dots), tc.envelope)
			if status != tc.status || out["status"] != tc.out || len(decisions.answers) != tc.answers || len(dots.results)+len(dots.followups) != 0 {
				t.Fatalf("status=%d out=%v answers=%+v dots=%+v", status, out, decisions.answers, dots)
			}
			if tc.answers == 1 && (decisions.answers[0].IssueClosed != tc.closed || decisions.answers[0].Source != store.LinearDecisionSourceComment || decisions.answers[0].UserID != fakeOperator || decisions.answers[0].CommentID != "comment-1") {
				t.Fatalf("answer=%+v", decisions.answers[0])
			}
		})
	}
}

func TestLinearWebhookDecisionCommentCarriesTheAuditFields(t *testing.T) {
	decisions := &fakeDecisions{linked: true, outcome: "recorded"}
	envelope := commentEnvelope("D-7-1 ab12cd3e 2\nbecause", fakeOperator, operatorActor())
	envelope["webhookTimestamp"] = time.Now().UnixMilli()
	body, _ := json.Marshal(envelope)
	req := httptest.NewRequest(http.MethodPost, "/hooks/linear", strings.NewReader(string(body)))
	req.Header.Set("Linear-Signature", linearSign(fakeLinearSigningValue, body))
	req.Header.Set("Linear-Delivery", "delivery-1")
	rec := httptest.NewRecorder()
	decisionWebhook(decisions, &fakeIssues{issue: decisionLinearIssue()}, &fakeDots{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(decisions.answers) != 1 {
		t.Fatalf("code=%d answers=%+v", rec.Code, decisions.answers)
	}
	got := decisions.answers[0]
	if got.Ref != "D-7-1" || got.Ver != "ab12cd3e" || got.Choice != "2" || got.RawText != "D-7-1 ab12cd3e 2\nbecause" || got.DeliveryID != "delivery-1" || got.IssueID != "issue-1" || got.EventAt != "2026-11-02T01:02:03.000Z" {
		t.Fatalf("answer=%+v", got)
	}
}

// A comment on an issue with no request still takes the dots path.
func TestLinearWebhookUnlinkedCommentKeepsDotsBehaviour(t *testing.T) {
	decisions, dots := &fakeDecisions{}, &fakeDots{}
	status, out := signedPost(t, decisionWebhook(decisions, &fakeIssues{issue: dotsLinearIssue()}, dots), commentEnvelope("[dots] RESULT done", "dots-user", nil))
	if status != http.StatusOK || out["status"] != "recorded" || len(dots.results) != 1 || len(decisions.answers) != 0 {
		t.Fatalf("status=%d out=%v dots=%+v answers=%+v", status, out, dots, decisions.answers)
	}
}

func issueUpdateEnvelope(state string, actor map[string]any, labels []string, updatedFrom map[string]any) map[string]any {
	data := map[string]any{"id": "issue-1", "state": map[string]any{"name": state, "type": "completed"}, "updatedAt": "2026-11-02T02:00:00.000Z"}
	if labels != nil {
		named := []map[string]any{}
		for _, label := range labels {
			named = append(named, map[string]any{"name": label})
		}
		data["labels"] = named
	}
	out := map[string]any{"action": "update", "type": "Issue", "data": data, "updatedFrom": updatedFrom}
	if actor != nil {
		out["actor"] = actor
	}
	return out
}

func TestLinearWebhookDecisionStateChange(t *testing.T) {
	moved := map[string]any{"stateId": "old-state"}
	labelled := []string{store.LinearDecisionLabel}
	cases := []struct {
		name     string
		envelope map[string]any
		linked   bool
		fetched  LinearIssue
		out      string
		answers  int
		choice   string
	}{
		{name: "approved", envelope: issueUpdateEnvelope("Approved", operatorActor(), labelled, moved), linked: true, out: "recorded", answers: 1, choice: "1"},
		{name: "rejected", envelope: issueUpdateEnvelope("Rejected", operatorActor(), labelled, moved), linked: true, out: "recorded", answers: 1, choice: "2"},
		{name: "labels absent are fetched", envelope: issueUpdateEnvelope("Approved", operatorActor(), nil, moved), linked: true, fetched: decisionLinearIssue(), out: "recorded", answers: 1, choice: "1"},
		{name: "unmapped state", envelope: issueUpdateEnvelope("In Review", operatorActor(), labelled, moved), linked: true, out: "ignored"},
		{name: "edit while resting in a mapped state", envelope: issueUpdateEnvelope("Approved", operatorActor(), labelled, map[string]any{"title": "old"}), linked: true, out: "ignored"},
		{name: "another user", envelope: issueUpdateEnvelope("Approved", map[string]any{"id": "someone-else", "type": "user"}, labelled, moved), linked: true, out: "ignored"},
		{name: "no actor", envelope: issueUpdateEnvelope("Approved", nil, labelled, moved), linked: true, out: "ignored"},
		{name: "oauth client", envelope: issueUpdateEnvelope("Approved", map[string]any{"id": fakeOperator, "type": "oauthClient"}, labelled, moved), linked: true, out: "ignored"},
		{name: "label removed", envelope: issueUpdateEnvelope("Approved", operatorActor(), []string{"other"}, moved), linked: true, out: "ignored"},
		{name: "issue with no request", envelope: issueUpdateEnvelope("Approved", operatorActor(), labelled, moved), out: "ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decisions := &fakeDecisions{linked: tc.linked, outcome: "recorded"}
			status, out := signedPost(t, decisionWebhook(decisions, &fakeIssues{issue: tc.fetched}, &fakeDots{}), tc.envelope)
			if status != http.StatusOK || out["status"] != tc.out || len(decisions.answers) != tc.answers {
				t.Fatalf("status=%d out=%v answers=%+v", status, out, decisions.answers)
			}
			if tc.answers == 1 {
				got := decisions.answers[0]
				if got.Source != store.LinearDecisionSourceState || got.Choice != tc.choice || !got.StateOptions[1] || !got.StateOptions[2] || got.EventAt == "" || got.IssueID != "issue-1" {
					t.Fatalf("answer=%+v", got)
				}
			}
		})
	}
}

func TestLinearWebhookIssueEventsIgnoredWithoutDecisions(t *testing.T) {
	h := LinearWebhook{Secret: fakeLinearSigningValue, Issues: &fakeIssues{}, Store: &fakeDots{}}
	status, out := signedPost(t, h, issueUpdateEnvelope("Approved", operatorActor(), []string{store.LinearDecisionLabel}, map[string]any{"stateId": "x"}))
	if status != http.StatusOK || out["status"] != "ignored" {
		t.Fatalf("status=%d out=%v", status, out)
	}
}

func TestParseLinearDecisionStates(t *testing.T) {
	got, err := ParseLinearDecisionStates(" Approved = 1 , Needs work=2")
	if err != nil || got["Approved"] != 1 || got["Needs work"] != 2 || len(got) != 2 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got, err := ParseLinearDecisionStates("  "); err != nil || len(got) != 0 {
		t.Fatalf("empty got=%v err=%v", got, err)
	}
	for _, bad := range []string{"Approved", "Approved=x", "=1", "Approved=0", "Approved=7", "Approved=1,Approved=2"} {
		if _, err := ParseLinearDecisionStates(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDecisionAskRouteAnswers(t *testing.T) {
	h := Server{Tokens: Tokens{"desk": "t"}}.Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/7/decision-request/ask", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "linear_decisions_disabled") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/v1/tasks/7/decision-request/ask", strings.NewReader(`{}`)))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code=%d", unauth.Code)
	}
}

func TestLinearAPIDecisionTarget(t *testing.T) {
	respond := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }))
	}
	const teams = `"teams":{"nodes":[{"id":"team","states":{"nodes":[{"id":"done","type":"completed","position":1},{"id":"late","type":"canceled","position":9},{"id":"early","type":"canceled","position":2}]}}]}`
	cases := []struct {
		name  string
		body  string
		label string
		state string
		miss  bool
	}{
		{"team label wins", `{"data":{` + teams + `,"issueLabels":{"nodes":[{"id":"ws","team":null},{"id":"own","team":{"id":"team"}}]}}}`, "own", "early", false},
		{"workspace label", `{"data":{` + teams + `,"issueLabels":{"nodes":[{"id":"ws","team":null}]}}}`, "ws", "early", false},
		{"another team's label", `{"data":{` + teams + `,"issueLabels":{"nodes":[{"id":"x","team":{"id":"other"}}]}}}`, "", "", true},
		{"no label", `{"data":{` + teams + `,"issueLabels":{"nodes":[]}}}`, "", "", true},
		{"no team", `{"data":{"teams":{"nodes":[]},"issueLabels":{"nodes":[{"id":"ws","team":null}]}}}`, "", "", true},
		{"no canceled state", `{"data":{"teams":{"nodes":[{"id":"team","states":{"nodes":[]}}]},"issueLabels":{"nodes":[{"id":"ws","team":null}]}}}`, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := respond(tc.body)
			defer srv.Close()
			got, err := LinearAPI{Key: "fake-key-value", URL: srv.URL}.LinearDecisionTarget(t.Context(), "ROB", "decision")
			if tc.miss {
				if !errors.Is(err, errLinearDecisionTarget) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil || got.LabelID != tc.label || got.CancelStateID != tc.state || got.TeamID != "team" {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	_, err := LinearAPI{Key: "k", URL: srv.URL}.LinearDecisionTarget(t.Context(), "ROB", "decision")
	var fetch linearFetchError
	if !errors.As(err, &fetch) || errors.Is(err, errLinearDecisionTarget) {
		t.Fatalf("outage must read as unavailable, got %v", err)
	}
}

func TestLinearAPIWrites(t *testing.T) {
	var seen []map[string]any
	reply := `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"ROB-9","url":"https://l/9"}},"commentCreate":{"success":true},"issueUpdate":{"success":true}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	a := LinearAPI{Key: "fake-key-value", URL: srv.URL}
	issue, err := a.createLinearIssue(t.Context(), LinearDecisionIssue{ID: "id-1", TeamID: "team", LabelID: "label", Title: "t", Description: "d"})
	if err != nil || issue.Identifier != "ROB-9" || issue.URL != "https://l/9" {
		t.Fatalf("issue=%+v err=%v", issue, err)
	}
	input := seen[0]["variables"].(map[string]any)["input"].(map[string]any)
	if input["id"] != "id-1" || input["teamId"] != "team" || input["labelIds"].([]any)[0] != "label" {
		t.Fatalf("input=%v", input)
	}
	if err := a.CreateLinearComment(t.Context(), "c-1", "i-1", "hello"); err != nil {
		t.Fatal(err)
	}
	input = seen[1]["variables"].(map[string]any)["input"].(map[string]any)
	if input["id"] != "c-1" || input["issueId"] != "i-1" || input["body"] != "hello" {
		t.Fatalf("input=%v", input)
	}
	if err := a.UpdateLinearIssueState(t.Context(), "i-1", "s-1"); err != nil {
		t.Fatal(err)
	}
	vars := seen[2]["variables"].(map[string]any)
	if vars["id"] != "i-1" || vars["input"].(map[string]any)["stateId"] != "s-1" {
		t.Fatalf("vars=%v", vars)
	}
	reply = `{"data":{"issueCreate":{"success":false},"commentCreate":{"success":false},"issueUpdate":{"success":false}}}`
	if _, err := a.createLinearIssue(t.Context(), LinearDecisionIssue{}); err == nil {
		t.Error("issue success=false accepted")
	}
	if err := a.CreateLinearComment(t.Context(), "c", "i", "b"); err == nil {
		t.Error("comment success=false accepted")
	}
	if err := a.UpdateLinearIssueState(t.Context(), "i", "s"); err == nil {
		t.Error("update success=false accepted")
	}
}

func TestDiscordWebhookPosts(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := (DiscordWebhook{URL: srv.URL}).Post(t.Context(), "@everyone hello"); err != nil {
		t.Fatal(err)
	}
	if body["content"] != "@everyone hello" {
		t.Fatalf("body=%v", body)
	}
	if parse, ok := body["allowed_mentions"].(map[string]any)["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("mentions not disabled: %v", body)
	}
}

// The webhook URL is a credential, and op errors are logged and stored.
func TestDiscordWebhookErrorsNeverCarryTheURL(t *testing.T) {
	const secretPath = "/api/webhooks/123/fake-webhook-credential"
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer failing.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for name, target := range map[string]string{"status": failing.URL + secretPath, "unreachable": closedURL + secretPath, "unbuildable": "https://exa mple/" + secretPath} {
		err := (DiscordWebhook{URL: target}).Post(t.Context(), "x")
		if err == nil || strings.Contains(err.Error(), "fake-webhook-credential") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, err := ParseDiscordWebhookURL("http://discord.example/api/webhooks/1/fake-webhook-credential"); err == nil || strings.Contains(err.Error(), "fake-webhook-credential") {
		t.Errorf("plain http: err=%v", err)
	}
	if got, err := ParseDiscordWebhookURL("https://discord.example/api/webhooks/1/x"); err != nil || got == "" {
		t.Errorf("https refused: %v", err)
	}
}

// opsQueue feeds ops to the worker and records how each ended.
type opsQueue struct {
	ops  []store.LinearDecisionOp
	done map[string]error
}

func (q *opsQueue) ProcessNextLinearDecisionOp(ctx context.Context, process func(context.Context, store.LinearDecisionOp) error) (bool, error, error) {
	if len(q.ops) == 0 {
		return false, nil, nil
	}
	op := q.ops[0]
	q.ops = q.ops[1:]
	err := process(ctx, op)
	if q.done == nil {
		q.done = map[string]error{}
	}
	q.done[op.DedupeKey] = err
	return true, err, nil
}

func TestLinearDecisionOpsWorkerSendsEachKind(t *testing.T) {
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body.Content)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	linear := newFakeLinear()
	queue := &opsQueue{ops: []store.LinearDecisionOp{
		{DedupeKey: "a", Op: store.LinearDecisionOpComment, IssueID: "i", Body: "✅ recorded"},
		{DedupeKey: "b", Op: store.LinearDecisionOpClose, IssueID: "i", StateID: "canceled"},
		{DedupeKey: "c", Op: store.LinearDecisionOpDiscord, IssueID: "i", Body: "hello channel"},
		{DedupeKey: "d", Op: "nonsense", IssueID: "i"},
	}}
	LinearDecisionOpsWorker{Store: queue, Linear: linear, Discord: &DiscordWebhook{URL: srv.URL}}.Drain(t.Context())
	if len(linear.comments) != 1 || linear.comments[0] != "i: ✅ recorded" || len(linear.closes) != 1 || linear.closes[0] != "i: canceled" || len(posted) != 1 || posted[0] != "hello channel" {
		t.Fatalf("comments=%v closes=%v posted=%v", linear.comments, linear.closes, posted)
	}
	if queue.done["a"] != nil || queue.done["b"] != nil || queue.done["c"] != nil || queue.done["d"] == nil {
		t.Fatalf("done=%v", queue.done)
	}
}

func TestLinearDecisionOpsWorkerSkipsDiscordWhenUnset(t *testing.T) {
	queue := &opsQueue{ops: []store.LinearDecisionOp{{DedupeKey: "c", Op: store.LinearDecisionOpDiscord, IssueID: "i", Body: "x"}}}
	LinearDecisionOpsWorker{Store: queue, Linear: newFakeLinear()}.Drain(t.Context())
	if !errors.Is(queue.done["c"], store.ErrLinearDecisionOpSkipped) {
		t.Fatalf("done=%v", queue.done)
	}
}

// decisionStoreFixture is a real store with one task, in a throwaway schema.
func decisionStoreFixture(t *testing.T) (*store.Store, store.Task) {
	t.Helper()
	_, url := testdb.Schema(t, "hkdecide")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "decision-project"
	if _, err := st.AddTaskProject(t.Context(), project, "t"); err != nil {
		t.Fatal(err)
	}
	x, err := st.CreateTask(t.Context(), store.Task{Lane: "decision-lane", Title: "decide", Kind: "implement", CreatedBy: "t", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return st, x
}

func opsOf(t *testing.T, st *store.Store, issueID, op string) []store.LinearDecisionOp {
	t.Helper()
	ops, err := st.ListLinearDecisionOps(t.Context(), issueID)
	if err != nil {
		t.Fatal(err)
	}
	out := []store.LinearDecisionOp{}
	for _, x := range ops {
		if x.Op == op {
			out = append(out, x)
		}
	}
	return out
}

// Ask, answer, re-deliver and answer again against a real store.
func TestDecisionFlowAgainstStore(t *testing.T) {
	st, task := decisionStoreFixture(t)
	linear := newFakeLinear()
	asker := LinearDecisions{Team: "ROB", Linear: linear}
	asked, err := asker.Ask(t.Context(), st, task.ID, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	wantRef := store.DecisionRef(fmt.Sprintf("dr-%d-1", task.ID))
	if !asked.IssueCreated || asked.DecisionRef != wantRef || asked.Request.Status != store.DecisionRequestOpen {
		t.Fatalf("asked=%+v", asked)
	}
	again, err := asker.Ask(t.Context(), st, task.ID, "desk", askInput())
	if err != nil || again.IssueCreated || !again.Duplicate || again.Issue.IssueID != asked.Issue.IssueID || linear.creates != 1 {
		t.Fatalf("again=%+v err=%v creates=%d", again, err, linear.creates)
	}
	issueID := asked.Issue.IssueID
	if posts := opsOf(t, st, issueID, store.LinearDecisionOpDiscord); len(posts) != 1 || !strings.Contains(posts[0].Body, asked.Ver) {
		t.Fatalf("posts=%+v", posts)
	}
	h := decisionWebhook(st, linear, &fakeDots{})
	comment := func(id, body, user string) string {
		t.Helper()
		envelope := commentEnvelope(body, user, map[string]any{"id": user, "type": "user"})
		data := envelope["data"].(map[string]any)
		data["id"], data["issueId"] = id, issueID
		status, out := signedPost(t, h, envelope)
		if status != http.StatusOK {
			t.Fatalf("status=%d out=%v", status, out)
		}
		return out["status"]
	}
	requestStatus := func() store.DecisionRequest {
		t.Helper()
		x, _, err := st.GetTask(t.Context(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return *x.Refs.DecisionRequest
	}
	if got := comment("c-other", "1", "someone-else"); got != "ignored" {
		t.Fatalf("other user=%q", got)
	}
	if got := comment("c-wrongver", "1 ver=00000000", fakeOperator); got != store.LinearDecisionRejected {
		t.Fatalf("wrong ver=%q", got)
	}
	if requestStatus().Status != store.DecisionRequestOpen {
		t.Fatal("a refused answer resolved the request")
	}
	if got := comment("c-win", wantRef+" 2\nbecause it is safer", fakeOperator); got != store.LinearDecisionRecorded {
		t.Fatalf("first=%q", got)
	}
	if got := comment("c-win", wantRef+" 2\nbecause it is safer", fakeOperator); got != store.LinearDecisionDuplicate {
		t.Fatalf("redelivery=%q", got)
	}
	if got := comment("c-late", "1", fakeOperator); got != store.LinearDecisionAlreadyResolved {
		t.Fatalf("later=%q", got)
	}
	if got := comment("c-late", "1", fakeOperator); got != store.LinearDecisionAlreadyResolved {
		t.Fatalf("later redelivered=%q", got)
	}
	resolved := requestStatus()
	audit := resolved.Resolution.Linear
	if resolved.Status != store.DecisionRequestAnswered || resolved.Resolution.Option != "B" || resolved.Resolution.Responder != "operator" || audit == nil {
		t.Fatalf("resolved=%+v", resolved)
	}
	if audit.DecisionID != wantRef || audit.Ver != asked.Ver || audit.Choice != "2" || audit.RawText != wantRef+" 2\nbecause it is safer" || audit.UserID != fakeOperator || audit.CommentID != "c-win" || audit.IssueID != issueID || audit.EventAt == "" || resolved.Resolution.At.IsZero() {
		t.Fatalf("audit=%+v", audit)
	}
	x, _, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if x.State != task.State {
		t.Fatalf("state changed: %q", x.State)
	}
	replies := opsOf(t, st, issueID, store.LinearDecisionOpComment)
	if len(replies) != 3 || !strings.Contains(replies[0].Body, "not recorded") || !strings.HasPrefix(replies[1].Body, "✅ recorded") || !strings.HasPrefix(replies[2].Body, "already resolved") {
		t.Fatalf("replies=%+v", replies)
	}
	posts := opsOf(t, st, issueID, store.LinearDecisionOpDiscord)
	if len(posts) != 2 || !strings.HasPrefix(posts[1].Body, "✅ "+wantRef) || !strings.Contains(posts[1].Body, "wait a week") {
		t.Fatalf("posts=%+v", posts)
	}
	rows, err := st.ListUnsentNotifications(t.Context(), 10)
	if err != nil || len(rows) != 1 || rows[0].Kind != store.OutboxKindDecisionAnswered || rows[0].TargetLane != store.LinearDecisionLane || !strings.Contains(rows[0].Text, wantRef) {
		t.Fatalf("outbox=%+v err=%v", rows, err)
	}
	worker := LinearDecisionOpsWorker{Store: st, Linear: linear}
	worker.Drain(t.Context())
	if len(linear.comments) != 3 {
		t.Fatalf("comments=%v", linear.comments)
	}
	for _, op := range opsOf(t, st, issueID, store.LinearDecisionOpDiscord) {
		if op.State != "skipped" {
			t.Fatalf("op=%+v", op)
		}
	}
	for _, op := range opsOf(t, st, issueID, store.LinearDecisionOpComment) {
		if op.State != "sent" {
			t.Fatalf("op=%+v", op)
		}
	}
}

// Linear retries a delivery it saw fail even if hk committed it. If the verdict
// changed in between, the operator must get the new reply, not only the old one.
func TestDecisionRedeliveryWithAChangedVerdictRepliesAgain(t *testing.T) {
	st, task := decisionStoreFixture(t)
	linear := newFakeLinear()
	asked, err := LinearDecisions{Team: "ROB", Linear: linear}.Ask(t.Context(), st, task.ID, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	h := decisionWebhook(st, linear, &fakeDots{})
	deliver := func(state string) string {
		t.Helper()
		envelope := commentEnvelope("1", fakeOperator, operatorActor())
		data := envelope["data"].(map[string]any)
		data["id"], data["issueId"] = "c-1", asked.Issue.IssueID
		data["issue"] = map[string]any{"labels": []map[string]any{{"name": store.LinearDecisionLabel}}, "state": map[string]any{"type": state}, "completedAt": "2026-11-01T00:00:00.000Z"}
		status, out := signedPost(t, h, envelope)
		if status != http.StatusOK {
			t.Fatalf("status=%d out=%v", status, out)
		}
		return out["status"]
	}
	if got := deliver("completed"); got != store.LinearDecisionRejected {
		t.Fatalf("closed issue=%q", got)
	}
	if got := deliver("completed"); got != store.LinearDecisionRejected {
		t.Fatalf("closed issue again=%q", got)
	}
	if replies := opsOf(t, st, asked.Issue.IssueID, store.LinearDecisionOpComment); len(replies) != 1 {
		t.Fatalf("a repeat with the same verdict replied again: %+v", replies)
	}
	if got := deliver("started"); got != store.LinearDecisionRecorded {
		t.Fatalf("reopened issue=%q", got)
	}
	replies := opsOf(t, st, asked.Issue.IssueID, store.LinearDecisionOpComment)
	if len(replies) != 2 || !strings.HasPrefix(replies[1].Body, "✅ recorded") {
		t.Fatalf("replies=%+v", replies)
	}
}

func TestDecisionSupersedeAgainstStore(t *testing.T) {
	st, task := decisionStoreFixture(t)
	linear := newFakeLinear()
	asker := LinearDecisions{Team: "ROB", Linear: linear}
	first, err := asker.Ask(t.Context(), st, task.ID, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	changed := askInput()
	changed.Question = "Ship the migration this week?"
	if _, err := asker.Ask(t.Context(), st, task.ID, "desk", changed); !errors.Is(err, store.ErrDecisionRequestOpen) {
		t.Fatalf("change without naming the open request: %v", err)
	}
	changed.Supersedes = first.Request.ID
	second, err := asker.Ask(t.Context(), st, task.ID, "desk", changed)
	if err != nil {
		t.Fatal(err)
	}
	if !second.IssueCreated || second.Ver == first.Ver || second.Issue.IssueID == first.Issue.IssueID || second.Request.Revision != 2 {
		t.Fatalf("second=%+v", second)
	}
	retried, err := asker.Ask(t.Context(), st, task.ID, "desk", changed)
	if err != nil || retried.IssueCreated || linear.creates != 2 {
		t.Fatalf("retried=%+v err=%v creates=%d", retried, err, linear.creates)
	}
	oldID := first.Issue.IssueID
	old, found, err := st.LinearDecisionLinkByIssue(t.Context(), oldID)
	if err != nil || !found || old.SupersededBy != second.Request.ID {
		t.Fatalf("old=%+v found=%v err=%v", old, found, err)
	}
	if closes := opsOf(t, st, oldID, store.LinearDecisionOpClose); len(closes) != 1 || closes[0].StateID != "canceled" {
		t.Fatalf("closes=%+v", closes)
	}
	if posts := opsOf(t, st, oldID, store.LinearDecisionOpDiscord); len(posts) != 2 || !strings.HasPrefix(posts[1].Body, "♻️") {
		t.Fatalf("posts=%+v", posts)
	}
	h := decisionWebhook(st, linear, &fakeDots{})
	post := func(issueID, commentID string) string {
		t.Helper()
		envelope := commentEnvelope("1", fakeOperator, operatorActor())
		data := envelope["data"].(map[string]any)
		data["id"], data["issueId"] = commentID, issueID
		status, out := signedPost(t, h, envelope)
		if status != http.StatusOK {
			t.Fatalf("status=%d out=%v", status, out)
		}
		return out["status"]
	}
	if got := post(oldID, "c-old"); got != store.LinearDecisionSuperseded {
		t.Fatalf("old issue=%q", got)
	}
	x, _, err := st.GetTask(t.Context(), task.ID)
	if err != nil || x.Refs.DecisionRequest.Status != store.DecisionRequestOpen {
		t.Fatalf("request=%+v err=%v", x.Refs.DecisionRequest, err)
	}
	if got := post(second.Issue.IssueID, "c-new"); got != store.LinearDecisionRecorded {
		t.Fatalf("new issue=%q", got)
	}
}

func TestDecisionStateChangeAgainstStore(t *testing.T) {
	st, task := decisionStoreFixture(t)
	linear := newFakeLinear()
	asked, err := LinearDecisions{Team: "ROB", Linear: linear}.Ask(t.Context(), st, task.ID, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	h := decisionWebhook(st, linear, &fakeDots{})
	envelope := func(updatedAt string) map[string]any {
		out := issueUpdateEnvelope("Approved", operatorActor(), []string{store.LinearDecisionLabel}, map[string]any{"stateId": "old"})
		data := out["data"].(map[string]any)
		data["id"], data["updatedAt"] = asked.Issue.IssueID, updatedAt
		return out
	}
	if _, out := signedPost(t, h, envelope("2026-11-02T02:00:00.000Z")); out["status"] != store.LinearDecisionRecorded {
		t.Fatalf("first out=%v", out)
	}
	if _, out := signedPost(t, h, envelope("2026-11-02T02:00:00.000Z")); out["status"] != store.LinearDecisionDuplicate {
		t.Fatalf("redelivery out=%v", out)
	}
	if _, out := signedPost(t, h, envelope("2026-11-02T03:00:00.000Z")); out["status"] != store.LinearDecisionAlreadyResolved {
		t.Fatalf("later out=%v", out)
	}
	x, _, err := st.GetTask(t.Context(), task.ID)
	if err != nil || x.Refs.DecisionRequest.Resolution.Option != "A" || x.Refs.DecisionRequest.Resolution.Linear.Source != store.LinearDecisionSourceState {
		t.Fatalf("request=%+v err=%v", x.Refs.DecisionRequest, err)
	}
}

// A decision closed in the console still reaches the channel.
func TestDecisionResolvedElsewherePostsToChannel(t *testing.T) {
	st, task := decisionStoreFixture(t)
	asked, err := LinearDecisions{Team: "ROB", Linear: newFakeLinear()}.Ask(t.Context(), st, task.ID, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveDecisionRequest(t.Context(), task.ID, "desk", store.DecisionResolveInput{RequestID: asked.Request.ID, Kind: store.DecisionRequestWithdrawn, Text: "moot"}); err != nil {
		t.Fatal(err)
	}
	posts := opsOf(t, st, asked.Issue.IssueID, store.LinearDecisionOpDiscord)
	if len(posts) != 2 || !strings.HasPrefix(posts[1].Body, "⏹️") || !strings.Contains(posts[1].Body, "withdrawn") {
		t.Fatalf("posts=%+v", posts)
	}
}

func TestLinearDecisionOpBacksOffThenFails(t *testing.T) {
	st, task := decisionStoreFixture(t)
	asked, err := LinearDecisions{Team: "ROB", Linear: newFakeLinear()}.Ask(t.Context(), st, task.ID, "desk", askInput())
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("linear down")
	found, opErr, err := st.ProcessNextLinearDecisionOp(t.Context(), func(context.Context, store.LinearDecisionOp) error { return boom })
	if !found || !errors.Is(opErr, boom) || err != nil {
		t.Fatalf("found=%v opErr=%v err=%v", found, opErr, err)
	}
	ops := opsOf(t, st, asked.Issue.IssueID, store.LinearDecisionOpDiscord)
	if len(ops) != 1 || ops[0].State != "pending" || ops[0].Attempts != 1 || ops[0].LastError != "linear down" {
		t.Fatalf("ops=%+v", ops)
	}
	found, _, err = st.ProcessNextLinearDecisionOp(t.Context(), func(context.Context, store.LinearDecisionOp) error { return nil })
	if found || err != nil {
		t.Fatalf("op ran inside its backoff: found=%v err=%v", found, err)
	}
}
