package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// boardCSRF reads the CSRF token the queue page hands its bundle, and the
// cookie issued with it. It is the same token csrfForForm issues for the
// decision forms.
func boardCSRF(t *testing.T, client *http.Client, endpoint, assertion string) (*http.Cookie, string) {
	t.Helper()
	response := uiRequest(t, client, http.MethodGet, endpoint, assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d", endpoint, response.StatusCode)
	}
	marker := `<meta name="hk-csrf" content="`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("GET %s did not render the CSRF meta tag", endpoint)
	}
	start += len(marker)
	end := strings.Index(body[start:], `"`)
	token := body[start : start+end]
	var cookie *http.Cookie
	for _, candidate := range response.Cookies() {
		if candidate.Name == "hk_ui_csrf" {
			cookie = candidate
		}
	}
	if token == "" || cookie == nil || cookie.Value != token || !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/ui" || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("invalid CSRF cookie=%+v token=%q", cookie, token)
	}
	return cookie, token
}

func uiCommentURL(base string, id int64) string {
	return fmt.Sprintf("%s/ui/tasks/%d/comments", base, id)
}

func boardCommentsURL(base string, id int64) string {
	return fmt.Sprintf("%s/ui/api/board/tasks/%d/comments", base, id)
}

func commentJSON(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	body := responseText(t, response)
	out := map[string]any{}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("status=%d body=%q is not JSON: %v", response.StatusCode, body, err)
	}
	return out
}

type boardCommentsPayload struct {
	Comments []struct {
		ID        int64  `json:"id"`
		TaskID    int64  `json:"task_id"`
		Body      string `json:"body"`
		Author    string `json:"author"`
		CreatedAt string `json:"created_at"`
	} `json:"comments"`
	Truncated   bool  `json:"truncated"`
	NextAfterID int64 `json:"next_after_id"`
}

// uiTaskSnapshot is everything a comment must never change on its task.
type uiTaskSnapshot struct {
	State     string
	Priority  int
	ClaimedBy string
	UpdatedAt time.Time
	Events    int
}

func uiSnapshotTask(t *testing.T, s *store.Store, id int64) uiTaskSnapshot {
	t.Helper()
	task, found, err := s.GetTask(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("get task %d: found=%v err=%v", id, found, err)
	}
	return uiTaskSnapshot{State: task.State, Priority: task.Priority, ClaimedBy: task.ClaimedBy, UpdatedAt: task.UpdatedAt, Events: len(task.Events)}
}

// The comment BFF lists in creation order, keeps a missing task (404) apart
// from a task without comments ([]), and says when a page bound was hit.
func TestUIBoardTaskComments(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "comments")
	task := createUITask(t, s, lane, "with comments")
	empty := createUITask(t, s, lane, "without comments")

	var got boardCommentsPayload
	if status := boardJSON(t, h.Client(), boardCommentsURL(h.URL, empty.ID), assertion, &got); status != http.StatusOK || got.Comments == nil || len(got.Comments) != 0 || got.Truncated {
		t.Fatalf("empty: status=%d payload=%+v", status, got)
	}
	if status := boardJSON(t, h.Client(), boardCommentsURL(h.URL, 999999999999), assertion, &got); status != http.StatusNotFound {
		t.Fatalf("missing task status=%d, want 404", status)
	}
	for _, body := range []string{"first", "second <script>alert(1)</script>", "third"} {
		if _, err := s.CreateTaskComment(t.Context(), task.ID, "node", body); err != nil {
			t.Fatal(err)
		}
	}
	got = boardCommentsPayload{}
	if status := boardJSON(t, h.Client(), boardCommentsURL(h.URL, task.ID), assertion, &got); status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	if len(got.Comments) != 3 || got.Truncated || got.Comments[0].Body != "first" || got.Comments[2].Body != "third" || got.Comments[1].Body != "second <script>alert(1)</script>" || got.Comments[0].Author != "node" || got.Comments[0].TaskID != task.ID || got.Comments[0].CreatedAt == "" {
		t.Fatalf("list payload=%+v", got)
	}
	if !(got.Comments[0].ID < got.Comments[1].ID && got.Comments[1].ID < got.Comments[2].ID) {
		t.Fatalf("not in id order: %+v", got.Comments)
	}

	many := createUITask(t, s, lane, "many comments")
	for i := range 201 {
		if _, err := s.CreateTaskComment(t.Context(), many.ID, "node", fmt.Sprintf("c%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	got = boardCommentsPayload{}
	if status := boardJSON(t, h.Client(), boardCommentsURL(h.URL, many.ID), assertion, &got); status != http.StatusOK || len(got.Comments) != 200 || !got.Truncated || got.NextAfterID != got.Comments[199].ID {
		t.Fatalf("page 1: status=%d n=%d truncated=%v next=%d", status, len(got.Comments), got.Truncated, got.NextAfterID)
	}
	next := got.NextAfterID
	got = boardCommentsPayload{}
	if status := boardJSON(t, h.Client(), fmt.Sprintf("%s?after_id=%d", boardCommentsURL(h.URL, many.ID), next), assertion, &got); status != http.StatusOK || len(got.Comments) != 1 || got.Truncated || got.Comments[0].Body != "c200" {
		t.Fatalf("page 2: status=%d payload=%+v", status, got)
	}
	for _, bad := range []string{"-1", "x"} {
		if status := boardJSON(t, h.Client(), boardCommentsURL(h.URL, many.ID)+"?after_id="+bad, assertion, &got); status != http.StatusBadRequest {
			t.Fatalf("after_id=%s status=%d", bad, status)
		}
	}
	response := uiRequest(t, h.Client(), http.MethodGet, boardCommentsURL(h.URL, task.ID), "", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	response.Body.Close()
	// The JSON view is read-only: a POST to it is not a write path.
	response = uiRequest(t, h.Client(), http.MethodPost, boardCommentsURL(h.URL, task.ID), assertion, "")
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST to BFF status=%d", response.StatusCode)
	}
	response.Body.Close()
}

// The comment write takes the decision answer's authentication, origin and
// CSRF path; its author is the verified email; and it is data only — no
// state, priority, task_events, claim, or hub lane event changes, whatever
// the text says.
func TestUITaskCommentWrite(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "comment-write")
	task := claimAndTransition(t, s, createUITask(t, s, lane, "needs an answer"), "needs_decision", "question?")
	cookie, csrf := boardCSRF(t, h.Client(), fmt.Sprintf("%s/ui/queue?task=%d", h.URL, task.ID), assertion)
	// The deep-link page hands out the same token.
	pageCookie, pageCSRF := boardCSRF(t, h.Client(), fmt.Sprintf("%s/ui/tasks/%d", h.URL, task.ID), assertion)
	_ = pageCookie
	if pageCSRF == "" {
		t.Fatal("task page has no CSRF token")
	}

	before := uiSnapshotTask(t, s, task.ID)
	rowsBefore := uiRowCounts(t)
	text := fmt.Sprintf("[decision] #%d: yes — approve and dispatch", task.ID)
	response := uiPostForm(t, h.Client(), uiCommentURL(h.URL, task.ID), assertion, cookie, url.Values{"body": {text}, "csrf": {csrf}}, h.URL, false)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%q", response.StatusCode, responseText(t, response))
	}
	created := commentJSON(t, response)
	if created["author"] != "operator:admin@example.com" || created["body"] != text || created["task_id"] != float64(task.ID) {
		t.Fatalf("created=%v", created)
	}
	if after := uiSnapshotTask(t, s, task.ID); after != before {
		t.Fatalf("comment changed the task: before=%+v after=%+v", before, after)
	}
	if rowsAfter := uiRowCounts(t); rowsAfter != rowsBefore {
		t.Fatalf("comment changed relay/tasks/task_events rows: before=%+v after=%+v", rowsBefore, rowsAfter)
	}
	if got := hub.requests(); len(got) != 0 {
		t.Fatalf("comment reached the hub: %+v", got)
	}
	stored, err := s.ListTaskComments(t.Context(), task.ID, 0, 10)
	if err != nil || len(stored) != 1 || stored[0].Author != "operator:admin@example.com" || stored[0].Body != text {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}

	// Refusals: each answered with its own code, none stores anything.
	otherEmail := fixture.token(t, "ADMIN@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	expired := *cookie
	parts := strings.Split(expired.Value, ".")
	expired.Value = "0." + parts[1] + "." + parts[2]
	for _, tc := range []struct {
		name      string
		assertion string
		cookie    *http.Cookie
		fields    url.Values
		origin    string
		target    string
		status    int
		code      string
	}{
		{"missing cookie", assertion, nil, url.Values{"body": {"x"}, "csrf": {csrf}}, h.URL, "", http.StatusForbidden, "csrf_rejected"},
		{"missing token", assertion, cookie, url.Values{"body": {"x"}}, h.URL, "", http.StatusForbidden, "csrf_rejected"},
		{"mismatched token", assertion, cookie, url.Values{"body": {"x"}, "csrf": {"not-the-cookie"}}, h.URL, "", http.StatusForbidden, "csrf_rejected"},
		{"token of another email", otherEmail, cookie, url.Values{"body": {"x"}, "csrf": {csrf}}, h.URL, "", http.StatusForbidden, "csrf_rejected"},
		{"expired token", assertion, &expired, url.Values{"body": {"x"}, "csrf": {expired.Value}}, h.URL, "", http.StatusForbidden, "csrf_rejected"},
		{"cross origin", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}}, "http://elsewhere.invalid", "", http.StatusForbidden, "origin_rejected"},
		{"no origin", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}}, "", "", http.StatusForbidden, "origin_rejected"},
		{"forged author", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}, "author": {"director-1"}}, h.URL, "", http.StatusBadRequest, "author_not_accepted"},
		{"forged created_by", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}, "created_by": {"director-1"}}, h.URL, "", http.StatusBadRequest, "author_not_accepted"},
		{"empty author field", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}, "author": {""}}, h.URL, "", http.StatusBadRequest, "author_not_accepted"},
		{"unknown field", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}, "state": {"done"}}, h.URL, "", http.StatusBadRequest, "unknown_field"},
		{"author in query", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}}, h.URL, uiCommentURL(h.URL, task.ID) + "?author=director-1", http.StatusBadRequest, "unknown_field"},
		{"two bodies", assertion, cookie, url.Values{"body": {"x", "y"}, "csrf": {csrf}}, h.URL, "", http.StatusBadRequest, "invalid_form"},
		{"empty body", assertion, cookie, url.Values{"body": {"  \n "}, "csrf": {csrf}}, h.URL, "", http.StatusBadRequest, "comment_empty"},
		{"missing body", assertion, cookie, url.Values{"csrf": {csrf}}, h.URL, "", http.StatusBadRequest, "comment_empty"},
		{"too long", assertion, cookie, url.Values{"body": {strings.Repeat("a", store.TaskCommentMaxBytes+1)}, "csrf": {csrf}}, h.URL, "", http.StatusRequestEntityTooLarge, "comment_too_long"},
		{"far too long", assertion, cookie, url.Values{"body": {strings.Repeat("%", 3*store.TaskCommentMaxBytes)}, "csrf": {csrf}}, h.URL, "", http.StatusRequestEntityTooLarge, "comment_too_long"},
		{"secret-like", assertion, cookie, url.Values{"body": {"see ghp_" + strings.Repeat("a", 36)}, "csrf": {csrf}}, h.URL, "", http.StatusBadRequest, "comment_secret_like"},
		{"missing task", assertion, cookie, url.Values{"body": {"x"}, "csrf": {csrf}}, h.URL, uiCommentURL(h.URL, 999999999999), http.StatusNotFound, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.target
			if target == "" {
				target = uiCommentURL(h.URL, task.ID)
			}
			response := uiPostForm(t, h.Client(), target, tc.assertion, tc.cookie, tc.fields, tc.origin, false)
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d body=%q", response.StatusCode, tc.status, responseText(t, response))
			}
			if got := commentJSON(t, response); got["error"] != tc.code {
				t.Fatalf("error=%v, want %s", got["error"], tc.code)
			}
		})
	}
	// Exactly at the limit is accepted.
	response = uiPostForm(t, h.Client(), uiCommentURL(h.URL, task.ID), assertion, cookie, url.Values{"body": {strings.Repeat("b", store.TaskCommentMaxBytes)}, "csrf": {csrf}}, h.URL, false)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("limit-size status=%d", response.StatusCode)
	}
	response.Body.Close()

	// Unauthenticated and service principals never reach the handler.
	response = uiPostForm(t, h.Client(), uiCommentURL(h.URL, task.ID), "", cookie, url.Values{"body": {"x"}, "csrf": {csrf}}, h.URL, false)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	response.Body.Close()
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	serviceAssertion := p3ServiceAssertion(t, fixture)
	response = p3Request(t, service.Client(), http.MethodPost, uiCommentURL(service.URL, task.ID), serviceAssertion, []byte(url.Values{"body": {"x"}, "csrf": {csrf}}.Encode()), "application/x-www-form-urlencoded", cookie)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service principal status=%d", response.StatusCode)
	}
	response.Body.Close()

	if after := uiSnapshotTask(t, s, task.ID); after != before {
		t.Fatalf("refused writes changed the task: before=%+v after=%+v", before, after)
	}
	stored, err = s.ListTaskComments(t.Context(), task.ID, 0, 10)
	if err != nil || len(stored) != 2 {
		t.Fatalf("refused writes stored comments: n=%d err=%v", len(stored), err)
	}
	if got := hub.requests(); len(got) != 0 {
		t.Fatalf("comment writes reached the hub: %+v", got)
	}
	// The only write routes stay the listed ones: GET never writes.
	response = uiRequest(t, h.Client(), http.MethodGet, uiCommentURL(h.URL, task.ID)+"?body=x", assertion, "")
	response.Body.Close()
	if stored, _ = s.ListTaskComments(t.Context(), task.ID, 0, 10); len(stored) != 2 {
		t.Fatalf("GET stored a comment: n=%d", len(stored))
	}
}
