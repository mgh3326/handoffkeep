package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

const fakeLinearSigningValue = "fake-signing-value-for-tests"

type fakeIssues struct {
	issue LinearIssue
	err   error
	calls int
}

func (f *fakeIssues) LinearIssue(context.Context, string) (LinearIssue, error) {
	f.calls++
	return f.issue, f.err
}

type fakeDots struct {
	results   []string
	followups []string
}

func (f *fakeDots) RecordDotsResult(_ context.Context, kind string, issue store.DotsIssue, c store.DotsComment) (string, bool, error) {
	f.results = append(f.results, kind+" "+issue.Identifier+" "+c.ID)
	return "k", true, nil
}

func (f *fakeDots) RecordDotsFollowup(_ context.Context, issue store.DotsIssue, id string) (bool, error) {
	f.followups = append(f.followups, issue.Identifier+" "+id)
	return true, nil
}

func dotsLinearIssue() LinearIssue {
	return LinearIssue{Identifier: "ZZZ-1", Title: "Fake issue", URL: "https://linear.example/zzz/1", Labels: []string{"other", "dots"}, StateType: "started"}
}

func linearPayload(body string, at time.Time) []byte {
	raw, err := json.Marshal(map[string]any{
		"action": "create", "type": "Comment", "webhookTimestamp": at.UnixMilli(), "futureField": 1,
		"data": map[string]any{"id": "0a1b2c3d-0000-4000-8000-000000000001", "body": body, "createdAt": "2026-11-02T01:02:03.000Z", "issueId": "issue-1"},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func linearSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func postLinear(t *testing.T, h http.Handler, body []byte, signature string) (int, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/linear", strings.NewReader(string(body)))
	if signature != "" {
		req.Header.Set("Linear-Signature", signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]string{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestLinearWebhookAuthentication(t *testing.T) {
	result := linearPayload("[dots] RESULT done", time.Now())
	stale := linearPayload("[dots] RESULT done", time.Now().Add(-2*time.Minute))
	future := linearPayload("[dots] RESULT done", time.Now().Add(2*time.Minute))
	cases := []struct {
		name      string
		body      []byte
		signature string
		status    int
		code      string
		writes    int
	}{
		{"valid", result, linearSign(fakeLinearSigningValue, result), http.StatusOK, "", 1},
		{"missing signature", result, "", http.StatusUnauthorized, "unauthorized", 0},
		{"wrong key", result, linearSign("other-fake-value", result), http.StatusUnauthorized, "unauthorized", 0},
		{"not hex", result, "zz", http.StatusUnauthorized, "unauthorized", 0},
		{"signature of another body", stale, linearSign(fakeLinearSigningValue, result), http.StatusUnauthorized, "unauthorized", 0},
		{"stale", stale, linearSign(fakeLinearSigningValue, stale), http.StatusUnauthorized, "stale_timestamp", 0},
		{"future", future, linearSign(fakeLinearSigningValue, future), http.StatusUnauthorized, "stale_timestamp", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dots := &fakeDots{}
			h := LinearWebhook{Secret: fakeLinearSigningValue, Issues: &fakeIssues{issue: dotsLinearIssue()}, Store: dots}
			status, out := postLinear(t, h, tc.body, tc.signature)
			if status != tc.status || out["error"] != tc.code || len(dots.results) != tc.writes {
				t.Fatalf("status=%d out=%v writes=%d", status, out, len(dots.results))
			}
		})
	}
}

func TestLinearWebhookRouting(t *testing.T) {
	closed := dotsLinearIssue()
	closed.StateType, closed.CompletedAt = "completed", "2026-11-01T00:00:00.000Z"
	canceled := dotsLinearIssue()
	canceled.StateType, canceled.CanceledAt = "canceled", "2026-11-01T00:00:00.000Z"
	closedLater := dotsLinearIssue()
	closedLater.StateType, closedLater.CompletedAt = "completed", "2026-11-03T00:00:00.000Z"
	unlabelled := closed
	unlabelled.Labels = []string{"other"}
	// An issue completed, reopened and later canceled can carry both times.
	canceledLater := canceled
	canceledLater.CompletedAt, canceledLater.CanceledAt = "2026-11-01T00:00:00.000Z", "2026-11-03T00:00:00.000Z"
	canceledAfterCompletion := canceled
	canceledAfterCompletion.CompletedAt, canceledAfterCompletion.CanceledAt = "2026-10-30T00:00:00.000Z", "2026-11-01T00:00:00.000Z"
	cases := []struct {
		name      string
		comment   string
		issue     LinearIssue
		status    string
		results   int
		followups int
		fetches   int
	}{
		{"result", "[dots] RESULT done", dotsLinearIssue(), "recorded", 1, 0, 1},
		{"blocked", "[dots] BLOCKED why", dotsLinearIssue(), "recorded", 1, 0, 1},
		{"question", "[dots] QUESTION which", dotsLinearIssue(), "recorded", 1, 0, 1},
		{"start writes nothing and spends no api call", "[dots] START", dotsLinearIssue(), "ignored", 0, 0, 0},
		{"unknown word", "[dots] RESULTS", dotsLinearIssue(), "ignored", 0, 0, 0},
		{"result without the label", "[dots] RESULT done", unlabelled, "ignored", 0, 0, 1},
		{"result on a closed issue is still a result", "[dots] RESULT done", closed, "recorded", 1, 0, 1},
		{"human comment on an open issue", "hello", dotsLinearIssue(), "ignored", 0, 0, 1},
		{"human comment on a completed issue", "hello", closed, "flagged", 0, 1, 1},
		{"human comment on a canceled issue", "hello", canceled, "flagged", 0, 1, 1},
		{"human comment from before the close", "hello", closedLater, "ignored", 0, 0, 1},
		{"human comment between an old completion and the cancel", "hello", canceledLater, "ignored", 0, 0, 1},
		{"human comment after the cancel of a once completed issue", "hello", canceledAfterCompletion, "flagged", 0, 1, 1},
		{"human comment on a closed unlabelled issue", "hello", unlabelled, "ignored", 0, 0, 1},
		{"dots start on a closed issue is not a followup", "[dots] START", closed, "ignored", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dots, issues := &fakeDots{}, &fakeIssues{issue: tc.issue}
			body := linearPayload(tc.comment, time.Now())
			status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: issues, Store: dots}, body, linearSign(fakeLinearSigningValue, body))
			if status != http.StatusOK || out["status"] != tc.status || len(dots.results) != tc.results || len(dots.followups) != tc.followups || issues.calls != tc.fetches {
				t.Fatalf("status=%d out=%v dots=%+v fetches=%d", status, out, dots, issues.calls)
			}
		})
	}
}

func TestLinearWebhookIgnoresOtherEvents(t *testing.T) {
	for name, envelope := range map[string]map[string]any{
		"comment update":      {"action": "update", "type": "Comment", "data": map[string]any{"body": "[dots] RESULT", "issueId": "i"}},
		"issue create":        {"action": "create", "type": "Issue", "data": map[string]any{"body": "[dots] RESULT", "issueId": "i"}},
		"comment on no issue": {"action": "create", "type": "Comment", "data": map[string]any{"body": "[dots] RESULT"}},
	} {
		t.Run(name, func(t *testing.T) {
			envelope["webhookTimestamp"] = time.Now().UnixMilli()
			body, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			dots, issues := &fakeDots{}, &fakeIssues{}
			status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: issues, Store: dots}, body, linearSign(fakeLinearSigningValue, body))
			if status != http.StatusOK || out["status"] != "ignored" || len(dots.results) != 0 || issues.calls != 0 {
				t.Fatalf("status=%d out=%v", status, out)
			}
		})
	}
}

func TestLinearWebhookUsesLabelsInThePayload(t *testing.T) {
	build := func(comment, issue string) []byte {
		return []byte(fmt.Sprintf(`{"action":"create","type":"Comment","webhookTimestamp":%d,"data":{"id":"0a1b2c3d-0000","body":%q,"createdAt":"2026-11-02T01:02:03.000Z","issueId":"i","issue":%s}}`, time.Now().UnixMilli(), comment, issue))
	}
	const (
		named  = `"identifier":"ZZZ-2","title":"t","url":"u"`
		dots   = `"labels":[{"name":"dots"}]`
		result = "[dots] RESULT x"
		human  = "hello"
	)
	cases := []struct {
		name    string
		comment string
		issue   string
		status  string
		fetches int
	}{
		{"complete and open", result, `{` + named + `,` + dots + `,"state":{"type":"started"}}`, "recorded", 0},
		{"complete and open, human comment", human, `{` + named + `,` + dots + `,"state":{"type":"started"}}`, "ignored", 0},
		{"complete and closed, human comment", human, `{` + named + `,` + dots + `,"state":{"type":"completed"},"completedAt":"2026-11-01T00:00:00.000Z"}`, "flagged", 0},
		{"labels without dots settle it", result, `{"identifier":"ZZZ-2","labels":[{"name":"other"}]}`, "ignored", 0},
		{"labels empty settle it", human, `{"identifier":"ZZZ-2","labels":[]}`, "ignored", 0},
		{"labels absent", result, `{"identifier":"ZZZ-2","title":"t"}`, "recorded", 1},
		{"labels in an unknown shape", result, `{"identifier":"ZZZ-2","labels":["dots"]}`, "recorded", 1},
		{"dots without state, human comment", human, `{` + named + `,` + dots + `}`, "flagged", 1},
		{"closed without a close time, human comment", human, `{` + named + `,` + dots + `,"state":{"type":"completed"}}`, "flagged", 1},
		{"dots without a title", result, `{"identifier":"ZZZ-2","url":"u",` + dots + `,"state":{"type":"started"}}`, "recorded", 1},
		{"dots without a url", result, `{"identifier":"ZZZ-2","title":"t",` + dots + `,"state":{"type":"started"}}`, "recorded", 1},
		{"dots without an identifier", result, `{"title":"t","url":"u",` + dots + `,"state":{"type":"started"}}`, "recorded", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fetched issue is closed, so a followup can only come from it
			// or from a payload that says so itself.
			fetched := dotsLinearIssue()
			fetched.StateType, fetched.CompletedAt = "completed", "2026-11-01T00:00:00.000Z"
			issues := &fakeIssues{issue: fetched}
			body := build(tc.comment, tc.issue)
			status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: issues, Store: &fakeDots{}}, body, linearSign(fakeLinearSigningValue, body))
			if status != http.StatusOK || out["status"] != tc.status || issues.calls != tc.fetches {
				t.Fatalf("status=%d out=%v fetches=%d", status, out, issues.calls)
			}
		})
	}
}

func TestLinearWebhookRetriesWhenTheIssueCannotBeRead(t *testing.T) {
	dots := &fakeDots{}
	body := linearPayload("[dots] RESULT done", time.Now())
	status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: &fakeIssues{err: errors.New("boom")}, Store: dots}, body, linearSign(fakeLinearSigningValue, body))
	if status != http.StatusServiceUnavailable || out["error"] != "linear_unavailable" || len(dots.results) != 0 {
		t.Fatalf("status=%d out=%v", status, out)
	}
}

func TestLinearWebhookRejectsMalformedSignedBody(t *testing.T) {
	body := []byte(`{not json`)
	status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: &fakeIssues{}, Store: &fakeDots{}}, body, linearSign(fakeLinearSigningValue, body))
	if status != http.StatusBadRequest || out["error"] != "invalid_context" {
		t.Fatalf("status=%d out=%v", status, out)
	}
}

func TestLinearWebhookRejectsOversizedBody(t *testing.T) {
	body := []byte(strings.Repeat("x", linearWebhookMaxBytes+1))
	status, out := postLinear(t, LinearWebhook{Secret: fakeLinearSigningValue, Issues: &fakeIssues{}, Store: &fakeDots{}}, body, linearSign(fakeLinearSigningValue, body))
	if status != http.StatusRequestEntityTooLarge || out["error"] != "payload_too_large" {
		t.Fatalf("status=%d out=%v", status, out)
	}
}

func TestLinearWebhookRouteRegistration(t *testing.T) {
	body := linearPayload("[dots] START", time.Now())
	signature := linearSign(fakeLinearSigningValue, body)
	if status, _ := postLinear(t, Server{}.Handler(), body, signature); status != http.StatusNotFound {
		t.Fatalf("unconfigured status=%d", status)
	}
	h := Server{LinearWebhook: LinearWebhook{Secret: fakeLinearSigningValue, Issues: &fakeIssues{}, Store: &fakeDots{}}}.Handler()
	if status, _ := postLinear(t, h, body, signature); status != http.StatusOK {
		t.Fatalf("configured status=%d", status)
	}
	if status, _ := postLinear(t, h, body, ""); status != http.StatusUnauthorized {
		t.Fatalf("unsigned status=%d", status)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hooks/linear", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", rec.Code)
	}
}

func TestLinearAPIReadsTheIssue(t *testing.T) {
	var auth, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		query = string(raw)
		io.WriteString(w, `{"data":{"issue":{"identifier":"ZZZ-3","title":"t","url":"u","completedAt":"2026-11-01T00:00:00.000Z","canceledAt":null,"state":{"type":"completed"},"labels":{"nodes":[{"name":"dots"}]}}}}`)
	}))
	defer srv.Close()
	got, err := LinearAPI{Key: "fake-key-value", URL: srv.URL}.LinearIssue(t.Context(), "issue-3")
	if err != nil {
		t.Fatal(err)
	}
	if auth != "fake-key-value" || !strings.Contains(query, "issue-3") {
		t.Fatalf("auth=%q query=%q", auth, query)
	}
	if got.Identifier != "ZZZ-3" || got.StateType != "completed" || got.CompletedAt == "" || got.CanceledAt != "" || len(got.Labels) != 1 || got.Labels[0] != "dots" {
		t.Fatalf("issue=%+v", got)
	}
}

func TestLinearAPIFailures(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter){
		"status":   func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
		"errors":   func(w http.ResponseWriter) { io.WriteString(w, `{"errors":[{"message":"nope"}]}`) },
		"no issue": func(w http.ResponseWriter) { io.WriteString(w, `{"data":{"issue":null}}`) },
		"not json": func(w http.ResponseWriter) { io.WriteString(w, `<html>`) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { respond(w) }))
			defer srv.Close()
			if _, err := (LinearAPI{Key: "fake-key-value", URL: srv.URL}).LinearIssue(t.Context(), "i"); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

// End to end against a real store: one RESULT delivered twice is one document
// and one pending notification, and a START writes neither.
func TestLinearWebhookRecordsAgainstStore(t *testing.T) {
	_, url := testdb.Schema(t, "hkhook")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	closed := dotsLinearIssue()
	closed.Identifier, closed.StateType, closed.CompletedAt = "ZZZ-5", "completed", "2026-11-01T00:00:00.000Z"
	issues := &fakeIssues{issue: dotsLinearIssue()}
	h := Server{Service: Service{Store: st}, LinearWebhook: LinearWebhook{Secret: fakeLinearSigningValue, Issues: issues, Store: st}}.Handler()
	post := func(comment string) string {
		t.Helper()
		body := linearPayload(comment, time.Now())
		status, out := postLinear(t, h, body, linearSign(fakeLinearSigningValue, body))
		if status != http.StatusOK {
			t.Fatalf("status=%d out=%v", status, out)
		}
		return out["status"]
	}
	if got := post("[dots] START"); got != "ignored" {
		t.Fatalf("start=%q", got)
	}
	if docs, err := st.ListDocumentsByPrefix(t.Context(), "dots/results/", 10); err != nil || len(docs) != 0 {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	if got := post("[dots] RESULT done\nsummary line"); got != "recorded" {
		t.Fatalf("first=%q", got)
	}
	if got := post("[dots] RESULT done\nsummary line"); got != "duplicate" {
		t.Fatalf("second=%q", got)
	}
	docs, err := st.ListDocumentsByPrefix(t.Context(), "dots/results/", 10)
	if err != nil || len(docs) != 1 || docs[0].Key != "dots/results/ZZZ-1/2026-11-02-0a1b2c3d" {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	rows, err := st.ListUnsentNotifications(t.Context(), 10)
	if err != nil || len(rows) != 1 || rows[0].Kind != store.OutboxKindDotsReport || rows[0].TargetLane != "operator-desk" {
		t.Fatalf("outbox=%+v err=%v", rows, err)
	}
	issues.issue = closed
	if got := post("a question for a closed issue"); got != "flagged" {
		t.Fatalf("followup=%q", got)
	}
	rows, err = st.ListUnsentNotifications(t.Context(), 10)
	if err != nil || len(rows) != 2 || rows[1].Kind != store.OutboxKindDotsFollowup || !strings.Contains(rows[1].Text, "ZZZ-5 is closed") {
		t.Fatalf("outbox=%+v err=%v", rows, err)
	}
}
