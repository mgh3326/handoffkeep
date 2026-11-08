package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/client"
)

const (
	fixtureToken    = "fixture-token"
	fixtureCFID     = "cf-access-id.fixture.invalid"
	fixtureCFSecret = "cf-access-secret-fixture"
)

// seen is one request the fake hk server received.
type seen struct {
	method, uri, auth, ua, cfID, cfSecret, contentType string
	header                                             http.Header
	body                                               []byte
}

type reply struct {
	status int
	header map[string]string
	body   string
}

// fakeHK answers each request with the reply registered for its method and
// escaped request URI, and records every request it sees.
type fakeHK struct {
	*httptest.Server
	mu      sync.Mutex
	got     []seen
	replies map[string]reply
}

func newFakeHK(t *testing.T) *fakeHK {
	t.Helper()
	f := &fakeHK{replies: map[string]reply{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, seen{
			method: r.Method, uri: r.URL.RequestURI(), auth: r.Header.Get("Authorization"), ua: r.Header.Get("User-Agent"),
			cfID: r.Header.Get("CF-Access-Client-Id"), cfSecret: r.Header.Get("CF-Access-Client-Secret"),
			contentType: r.Header.Get("Content-Type"), header: r.Header.Clone(), body: b,
		})
		x, ok := f.replies[r.Method+" "+r.URL.RequestURI()]
		f.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"fixture_route_missing"}`)
			return
		}
		if _, set := x.header["Content-Type"]; !set {
			w.Header().Set("Content-Type", "application/json")
		}
		for k, v := range x.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(x.status)
		_, _ = io.WriteString(w, x.body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeHK) on(method, uri string, status int, body string, header map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method+" "+uri] = reply{status: status, body: body, header: header}
}

func (f *fakeHK) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.got...)
}

// taskJSON is a task as the server writes it: every store.Task field the
// fleet reads plus fields the client does not model, which it must ignore.
const taskJSON = `{
  "id": 42, "lane": "panewire-design", "parent_lane": "operator-desk", "title": "ship the client",
  "kind": "feature", "state": "developing", "priority": 2, "category": "started",
  "project": "handoffkeep", "area": null, "expected": null, "awaiting_decision": true,
  "claimed_by": "worker-1", "created_by": "desk", "body_doc": "task/42/body",
  "created_at": "2026-11-08T01:02:03Z", "updated_at": "2026-11-08T04:05:06Z",
  "last_activity_at": "2026-11-08T04:05:06Z", "stalled": false, "labels": [],
  "refs": {
    "job_id": "job-7", "pr": "https://github.com/mgh3326/handoffkeep/pull/9", "head_sha": "abc",
    "closure": {"type": "merged", "evidence": "pr 9"},
    "decision_request": {
      "id": "dr-42-3", "revision": 3, "supersedes": "dr-42-2", "root": "dr-42-1",
      "status": "answered", "question": "deploy?", "default_action": "hold",
      "due_at": "2026-11-09T00:00:00Z", "requested_by": "mergewake", "requested_at": "2026-11-08T00:00:00Z",
      "human_only": true,
      "resolution": {"kind": "answered", "option": "A", "text": "go", "by": "operator", "at": "2026-11-08T05:00:00Z"}
    }
  }
}`

func checkTask(t *testing.T, x client.Task) {
	t.Helper()
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if x.ID != 42 || x.Lane != "panewire-design" || x.Title != "ship the client" || x.Kind != "feature" ||
		x.State != "developing" || x.Category != "started" || x.Priority != 2 || x.ClaimedBy != "worker-1" ||
		!x.AwaitingDecision || x.Project == nil || *x.Project != "handoffkeep" ||
		!x.CreatedAt.Equal(at("2026-11-08T01:02:03Z")) || !x.UpdatedAt.Equal(at("2026-11-08T04:05:06Z")) {
		t.Fatalf("task decoded wrong: %+v", x)
	}
	if x.Refs.JobID != "job-7" || x.Refs.PR != "https://github.com/mgh3326/handoffkeep/pull/9" ||
		x.Refs.Closure == nil || x.Refs.Closure.Type != "merged" || x.Refs.Closure.Evidence != "pr 9" {
		t.Fatalf("task refs decoded wrong: %+v", x.Refs)
	}
	dr := x.Refs.DecisionRequest
	if dr == nil || dr.ID != "dr-42-3" || dr.Revision != 3 || dr.Supersedes != "dr-42-2" || dr.Root != "dr-42-1" ||
		dr.Status != "answered" || dr.Question != "deploy?" || !dr.HumanOnly || dr.DueAt == nil ||
		!dr.DueAt.Equal(at("2026-11-09T00:00:00Z")) || dr.Resolution == nil || dr.Resolution.Kind != "answered" ||
		dr.Resolution.Option != "A" {
		t.Fatalf("decision request decoded wrong: %+v", dr)
	}
}

// claimedTaskJSON is taskJSON as a claim or rebind answers it.
func claimedTaskJSON(claimedBy, jobID string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(taskJSON), &m); err != nil {
		panic(err)
	}
	m["claimed_by"] = claimedBy
	m["refs"].(map[string]any)["job_id"] = jobID
	b, _ := json.Marshal(m)
	return string(b)
}

// fleetCase is one fleet call: the request it must send and the decode it
// must make of a server-shaped answer.
type fleetCase struct {
	name        string
	method, uri string
	status      int
	reply       string
	replyHeader map[string]string
	// wantBody is the exact JSON body; subsetBody only names keys that must
	// carry these values (struct bodies also send the zero fields).
	wantBody   map[string]any
	subsetBody map[string]any
	rawBody    string
	wantHeader map[string]string
	call       func(context.Context, client.Client) error
}

func fleetCases(t *testing.T) []fleetCase {
	due := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	return []fleetCase{
		{
			name: "GetTask", method: "GET", uri: "/v1/tasks/42", status: http.StatusOK, reply: taskJSON,
			call: func(ctx context.Context, c client.Client) error {
				x, found, err := c.GetTask(ctx, 42)
				if err != nil || !found {
					return errors.Join(err, errors.New("GetTask not found"))
				}
				checkTask(t, x)
				return nil
			},
		},
		{
			name: "GetTaskNotFound", method: "GET", uri: "/v1/tasks/43", status: http.StatusNotFound, reply: `{"error":"not_found"}`,
			call: func(ctx context.Context, c client.Client) error {
				_, found, err := c.GetTask(ctx, 43)
				if err != nil || found {
					return errors.Join(err, errors.New("GetTask on a missing task reported found"))
				}
				return nil
			},
		},
		{
			name: "ClaimTask", method: "POST", uri: "/v1/tasks/42/claim", status: http.StatusOK, reply: claimedTaskJSON("worker-1", "job-7"),
			wantBody: map[string]any{"claimed_by": "worker-1", "job_id": "job-7"},
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.ClaimTask(ctx, 42, "worker-1", "job-7", "")
				if err == nil && (x.ClaimedBy != "worker-1" || x.Refs.JobID != "job-7") {
					t.Fatalf("claim decoded wrong: %+v", x)
				}
				return err
			},
		},
		{
			name: "RebindTaskJob", method: "POST", uri: "/v1/tasks/42/rebind", status: http.StatusOK, reply: claimedTaskJSON("worker-2", "job-8"),
			wantBody: map[string]any{"from_job": "job-7", "to_job": "job-8", "by": "worker-2", "reason": "takeover"},
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.RebindTaskJob(ctx, 42, "job-7", "job-8", "worker-2", "takeover")
				if err == nil && (x.ClaimedBy != "worker-2" || x.Refs.JobID != "job-8") {
					t.Fatalf("rebind decoded wrong: %+v", x)
				}
				return err
			},
		},
		{
			name: "ReopenTask", method: "POST", uri: "/v1/tasks/42/reopen", status: http.StatusOK, reply: taskJSON,
			wantBody: map[string]any{"reason": "regressed"},
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.ReopenTask(ctx, 42, "regressed")
				if err == nil {
					checkTask(t, x)
				}
				return err
			},
		},
		{
			name: "CommentTask", method: "POST", uri: "/v1/tasks/42/comments", status: http.StatusCreated,
			reply:    `{"id": 9, "task_id": 42, "body": "merged in #9", "author": "svc-mergewake", "created_at": "2026-11-08T06:00:00Z"}`,
			wantBody: map[string]any{"body": "merged in #9"},
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.CommentTask(ctx, 42, "merged in #9")
				if err == nil && (x.ID != 9 || x.TaskID != 42 || x.Body != "merged in #9" || x.Author != "svc-mergewake" || x.CreatedAt.IsZero()) {
					t.Fatalf("comment decoded wrong: %+v", x)
				}
				return err
			},
		},
		{
			name: "ProjectOwner", method: "GET", uri: "/v1/tasks/projects/handoffkeep/owner", status: http.StatusOK,
			reply: `{"name": "handoffkeep", "design_lane": "handoffkeep-design", "effective_owner": "handoffkeep-design", "updated_at": "2026-11-08T07:00:00Z", "events": []}`,
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.ProjectOwner(ctx, "handoffkeep")
				if err == nil && (x.Name != "handoffkeep" || x.DesignLane != "handoffkeep-design" || x.EffectiveOwner != "handoffkeep-design" || x.UpdatedAt == nil) {
					t.Fatalf("project owner decoded wrong: %+v", x)
				}
				return err
			},
		},
		{
			name: "GetDocument", method: "GET", uri: "/v1/documents/policy%2Fmodel-routing", status: http.StatusOK,
			reply: `{"id": 5, "key": "policy/model-routing", "kind": "policy", "session": "", "job": "", "body": "route: opus", "sha256": "ab", "created_by": "desk", "created_at": "2026-11-08T08:00:00Z", "updated_at": "2026-11-08T08:00:00Z", "task_id": 42, "origin_class": "authored"}`,
			call: func(ctx context.Context, c client.Client) error {
				x, found, err := c.GetDocument(ctx, "policy/model-routing")
				if err == nil && (!found || x.ID != 5 || x.Key != "policy/model-routing" || x.Kind != "policy" || x.Body != "route: opus" || x.SHA256 != "ab" || x.TaskID == nil || *x.TaskID != 42 || x.UpdatedAt.IsZero()) {
					t.Fatalf("document decoded wrong: found=%t %+v", found, x)
				}
				return err
			},
		},
		{
			name: "GetDocumentNotFound", method: "GET", uri: "/v1/documents/policy%2Fmissing", status: http.StatusNotFound, reply: `{"error":"not_found"}`,
			call: func(ctx context.Context, c client.Client) error {
				_, found, err := c.GetDocument(ctx, "policy/missing")
				if err != nil || found {
					return errors.Join(err, errors.New("GetDocument on a missing key reported found"))
				}
				return nil
			},
		},
		{
			name: "PutDocument", method: "PUT", uri: "/v1/documents/review%2F2026-11-08%2Fpr-9", status: http.StatusOK,
			reply:      `{"document": {"id": 6, "key": "review/2026-11-08/pr-9", "kind": "review", "job": "job-7", "body": "lgtm", "sha256": "cd", "created_by": "svc-mergewake", "created_at": "2026-11-08T09:00:00Z", "updated_at": "2026-11-08T09:00:00Z"}, "changed": true}`,
			subsetBody: map[string]any{"key": "review/2026-11-08/pr-9", "kind": "review", "job": "job-7", "body": "lgtm"},
			call: func(ctx context.Context, c client.Client) error {
				x, changed, err := c.PutDocument(ctx, client.Document{Key: "review/2026-11-08/pr-9", Kind: "review", Job: "job-7", Body: "lgtm"})
				if err == nil && (!changed || x.ID != 6 || x.SHA256 != "cd" || x.CreatedBy != "svc-mergewake") {
					t.Fatalf("put document decoded wrong: changed=%t %+v", changed, x)
				}
				return err
			},
		},
		{
			name: "PutAttachment", method: "PUT", uri: "/v1/attachments", status: http.StatusCreated,
			reply:      `{"attachment": {"sha256": "ef", "size_bytes": 5, "mime": "text/plain", "original_name": "log.txt", "created_by": "svc-mergewake", "created_at": "2026-11-08T10:00:00Z", "ref_kind": "task", "ref_id": "42"}, "created": true}`,
			rawBody:    "hello",
			wantHeader: map[string]string{"X-HK-Name": "log.txt", "Content-Type": "text/plain", "X-HK-Ref": "task:42"},
			call: func(ctx context.Context, c client.Client) error {
				x, created, err := c.PutAttachment(ctx, "log.txt", "text/plain", "task:42", []byte("hello"))
				if err == nil && (!created || x.SHA256 != "ef" || x.SizeBytes != 5 || x.MIME != "text/plain" || x.OriginalName != "log.txt" || x.RefKind != "task" || x.RefID != "42") {
					t.Fatalf("attachment decoded wrong: created=%t %+v", created, x)
				}
				return err
			},
		},
		{
			name: "GetAttachment", method: "GET", uri: "/v1/attachments/ef", status: http.StatusOK, reply: "hello",
			replyHeader: map[string]string{"Content-Type": "text/plain", "Content-Disposition": `attachment; filename="log.txt"`},
			call: func(ctx context.Context, c client.Client) error {
				x, body, err := c.GetAttachment(ctx, "ef")
				if err != nil {
					return err
				}
				defer body.Close()
				b, err := io.ReadAll(body)
				if err == nil && (x.SHA256 != "ef" || x.MIME != "text/plain" || string(b) != "hello") {
					t.Fatalf("attachment get decoded wrong: %+v %q", x, b)
				}
				return err
			},
		},
		{
			name: "AskDecision", method: "POST", uri: "/v1/tasks/42/decision-request/ask", status: http.StatusOK,
			reply: `{"task": ` + taskJSON + `, "request": {"id": "dr-42-4", "revision": 4, "supersedes": "dr-42-3", "root": "dr-42-1", "status": "open", "question": "deploy v2?", "default_action": "hold", "requested_by": "svc-mergewake", "requested_at": "2026-11-08T11:00:00Z", "human_only": true}, "decision_ref": "hk-decision:dr-42-4", "ver": "v1-abc", "duplicate": false, "issue_created": true}`,
			wantBody: map[string]any{
				"question": "deploy v2?",
				"options": map[string]any{"options": []any{
					map[string]any{"key": "A", "label": "Approve this deployment", "recommended": true},
					map[string]any{"key": "B", "label": "Keep the current version"},
				}, "allow_free": false},
				"default_action": "Hold; keep the current version",
				"due_at":         "2026-11-09T00:00:00Z",
				"supersedes":     "dr-42-3",
				"human_only":     true,
			},
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.AskDecision(ctx, 42, client.DecisionAskInput{
					Question: "deploy v2?",
					Options: client.DecisionOptions{Options: []client.DecisionOption{
						{Key: "A", Label: "Approve this deployment", Recommended: true},
						{Key: "B", Label: "Keep the current version"},
					}},
					DefaultAction: "Hold; keep the current version",
					DueAt:         &due,
					Supersedes:    "dr-42-3",
					HumanOnly:     true,
				})
				if err != nil {
					return err
				}
				if x.Request.ID != "dr-42-4" || x.Request.Supersedes != "dr-42-3" || x.Request.Status != "open" || !x.Request.HumanOnly ||
					x.DecisionRef != "hk-decision:dr-42-4" || x.Ver != "v1-abc" || x.Duplicate || !x.IssueCreated || x.Task.ID != 42 {
					t.Fatalf("ask decoded wrong: %+v", x)
				}
				return nil
			},
		},
		{
			// The record-only route opens no Linear issue: the answer has no
			// decision_ref, ver or issue_created. 201 means a new request.
			name: "RecordDecisionRequest", method: "POST", uri: "/v1/tasks/42/decision-request", status: http.StatusCreated,
			reply: `{"task": ` + taskJSON + `, "request": {"id": "dr-42-4", "revision": 4, "supersedes": "dr-42-3", "root": "dr-42-1", "status": "open", "question": "deploy v2?", "default_action": "hold", "requested_by": "svc-mergewake", "requested_at": "2026-11-08T11:00:00Z", "human_only": true}, "duplicate": false}`,
			wantBody: map[string]any{
				"question": "deploy v2?",
				"options": map[string]any{"options": []any{
					map[string]any{"key": "A", "label": "Approve this deployment", "recommended": true},
					map[string]any{"key": "B", "label": "Keep the current version"},
				}, "allow_free": false},
				"default_action": "Hold; keep the current version",
				"due_at":         "2026-11-09T00:00:00Z",
				"supersedes":     "dr-42-3",
				"human_only":     true,
			},
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.RecordDecisionRequest(ctx, 42, client.DecisionAskInput{
					Question: "deploy v2?",
					Options: client.DecisionOptions{Options: []client.DecisionOption{
						{Key: "A", Label: "Approve this deployment", Recommended: true},
						{Key: "B", Label: "Keep the current version"},
					}},
					DefaultAction: "Hold; keep the current version",
					DueAt:         &due,
					Supersedes:    "dr-42-3",
					HumanOnly:     true,
				})
				if err != nil {
					return err
				}
				if x.Request.ID != "dr-42-4" || x.Request.Revision != 4 || x.Request.Supersedes != "dr-42-3" ||
					x.Request.Status != "open" || !x.Request.HumanOnly || x.Duplicate {
					t.Fatalf("record decoded wrong: %+v", x)
				}
				checkTask(t, x.Task)
				return nil
			},
		},
		{
			// An identical open request answers 200 with duplicate:true and
			// the request already on record.
			name: "RecordDecisionRequestDuplicate", method: "POST", uri: "/v1/tasks/42/decision-request", status: http.StatusOK,
			reply:      `{"task": ` + taskJSON + `, "request": {"id": "dr-42-3", "revision": 3, "status": "open", "question": "deploy v2?", "requested_by": "svc-mergewake", "requested_at": "2026-11-08T10:00:00Z"}, "duplicate": true}`,
			subsetBody: map[string]any{"question": "deploy v2?", "default_action": "hold", "block": true},
			call: func(ctx context.Context, c client.Client) error {
				x, err := c.RecordDecisionRequest(ctx, 42, client.DecisionAskInput{Question: "deploy v2?", DefaultAction: "hold", Block: true})
				if err != nil {
					return err
				}
				if !x.Duplicate || x.Request.ID != "dr-42-3" || x.Task.ID != 42 {
					t.Fatalf("duplicate record decoded wrong: %+v", x)
				}
				return nil
			},
		},
	}
}

// Every fleet call sends its route with the bearer token and explicit
// User-Agent, the CF Access pair only when both halves are set, the body the
// internal/remote equivalent sends, and decodes a server-shaped answer.
func TestFleetCallsWireShape(t *testing.T) {
	for _, withCF := range []bool{true, false} {
		for _, tc := range fleetCases(t) {
			name := tc.name
			if withCF {
				name += "/cf"
			}
			t.Run(name, func(t *testing.T) {
				hk := newFakeHK(t)
				hk.on(tc.method, tc.uri, tc.status, tc.reply, tc.replyHeader)
				c := client.Client{URL: hk.URL + "/", Token: fixtureToken}
				if withCF {
					c.CFAccessClientID, c.CFAccessClientSecret = fixtureCFID, fixtureCFSecret
				}
				if err := tc.call(context.Background(), c); err != nil {
					t.Fatalf("call: %v", err)
				}
				got := hk.requests()
				if len(got) != 1 {
					t.Fatalf("requests=%d want 1: %+v", len(got), got)
				}
				r := got[0]
				if r.method != tc.method || r.uri != tc.uri {
					t.Fatalf("request %s %s, want %s %s", r.method, r.uri, tc.method, tc.uri)
				}
				if r.auth != "Bearer "+fixtureToken {
					t.Fatalf("Authorization=%q", r.auth)
				}
				if r.ua != client.UserAgent || !strings.HasPrefix(r.ua, "handoffkeep/") {
					t.Fatalf("User-Agent=%q want %q", r.ua, client.UserAgent)
				}
				if withCF && (r.cfID != fixtureCFID || r.cfSecret != fixtureCFSecret) {
					t.Fatalf("CF pair=%q/%q", r.cfID, r.cfSecret)
				}
				if !withCF && (r.header.Values("CF-Access-Client-Id") != nil || r.header.Values("CF-Access-Client-Secret") != nil) {
					t.Fatalf("CF headers sent without a configured pair: %v", r.header)
				}
				for k, v := range tc.wantHeader {
					if r.header.Get(k) != v {
						t.Fatalf("header %s=%q want %q", k, r.header.Get(k), v)
					}
				}
				switch {
				case tc.rawBody != "":
					if string(r.body) != tc.rawBody {
						t.Fatalf("body=%q want %q", r.body, tc.rawBody)
					}
				case tc.wantBody != nil || tc.subsetBody != nil:
					if r.contentType != "application/json" {
						t.Fatalf("Content-Type=%q", r.contentType)
					}
					var body map[string]any
					if err := json.Unmarshal(r.body, &body); err != nil {
						t.Fatalf("body %q: %v", r.body, err)
					}
					if tc.wantBody != nil && !reflect.DeepEqual(body, tc.wantBody) {
						t.Fatalf("body=%v\nwant %v", body, tc.wantBody)
					}
					for k, v := range tc.subsetBody {
						if !reflect.DeepEqual(body[k], v) {
							t.Fatalf("body[%s]=%v want %v (body %v)", k, body[k], v, body)
						}
					}
				default:
					if len(r.body) != 0 {
						t.Fatalf("unexpected body %q", r.body)
					}
				}
			})
		}
	}
}

// A non-2xx answer decodes into *client.HTTPError with the server's code,
// reason and status, and ErrorCode reads it.
func TestFleetCallErrorDecode(t *testing.T) {
	hk := newFakeHK(t)
	hk.on("POST", "/v1/tasks/42/claim", http.StatusConflict, `{"error":"task_claimed","reason":"claimed by worker-9"}`, nil)
	hk.on("PUT", "/v1/documents/note", http.StatusBadRequest, `{"error":"secret_like_content","pattern":"aws_key"}`, nil)
	c := client.Client{URL: hk.URL, Token: fixtureToken}
	_, err := c.ClaimTask(context.Background(), 42, "worker-1", "job-7", "")
	var he *client.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "task_claimed" || he.Reason != "claimed by worker-9" {
		t.Fatalf("claim err=%v (%#v)", err, he)
	}
	if client.ErrorCode(err) != "task_claimed" || err.Error() != "task_claimed: claimed by worker-9" {
		t.Fatalf("ErrorCode=%q text=%q", client.ErrorCode(err), err.Error())
	}
	_, _, err = c.PutDocument(context.Background(), client.Document{Key: "note", Kind: "note", Body: "x"})
	if p, ok := client.SecretPattern(err); !ok || p != "aws_key" {
		t.Fatalf("SecretPattern=%q,%t err=%v", p, ok, err)
	}
}

// A claim the server answers without recording the claimant or job is a
// failure, as in internal/remote: the task would be claimed but unlinked.
func TestFleetClaimRejectsSilentDrop(t *testing.T) {
	hk := newFakeHK(t)
	hk.on("POST", "/v1/tasks/42/claim", http.StatusOK, claimedTaskJSON("worker-1", ""), nil)
	c := client.Client{URL: hk.URL, Token: fixtureToken}
	if _, err := c.ClaimTask(context.Background(), 42, "worker-1", "job-7", ""); err == nil || !strings.HasPrefix(err.Error(), "claim_job_id_not_recorded") {
		t.Fatalf("claim with a dropped job id: err=%v", err)
	}
}

// A redirect away from the configured host is never followed, so neither the
// bearer token nor the CF pair reaches it.
func TestFleetCallRefusesForeignRedirect(t *testing.T) {
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignHits.Add(1) }))
	defer foreign.Close()
	hk := newFakeHK(t)
	hk.on("GET", "/v1/tasks/42", http.StatusFound, "", map[string]string{"Location": foreign.URL + "/v1/tasks/42"})
	c := client.Client{URL: hk.URL, Token: fixtureToken, CFAccessClientID: fixtureCFID, CFAccessClientSecret: fixtureCFSecret}
	_, _, err := c.GetTask(context.Background(), 42)
	if client.ErrorCode(err) != "redirect_foreign_host" {
		t.Fatalf("err=%v want redirect_foreign_host", err)
	}
	if n := foreignHits.Load(); n != 0 {
		t.Fatalf("foreign host received %d requests", n)
	}
}

// The presign answer is surfaced, never followed: the storage host sees no
// hk header, and a presign pointing at the Access login fails loud.
func TestFleetAttachmentURLNeverFollowed(t *testing.T) {
	var storageHits atomic.Int32
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { storageHits.Add(1) }))
	defer storage.Close()
	hk := newFakeHK(t)
	hk.on("GET", "/v1/attachments/ef?presign=1", http.StatusFound, "", map[string]string{"Location": storage.URL + "/blob/ef?sig=1"})
	hk.on("GET", "/v1/attachments/gh?presign=1", http.StatusFound, "", map[string]string{"Location": "https://team.cloudflareaccess.com/cdn-cgi/access/login?sig=secret"})
	c := client.Client{URL: hk.URL, Token: fixtureToken, CFAccessClientID: fixtureCFID, CFAccessClientSecret: fixtureCFSecret}
	u, err := c.AttachmentURL(context.Background(), "ef")
	if err != nil || u != storage.URL+"/blob/ef?sig=1" {
		t.Fatalf("AttachmentURL=%q err=%v", u, err)
	}
	if n := storageHits.Load(); n != 0 {
		t.Fatalf("storage host received %d requests", n)
	}
	_, err = c.AttachmentURL(context.Background(), "gh")
	if client.ErrorCode(err) != "cf_access_login_redirect" || strings.Contains(err.Error(), "sig=secret") {
		t.Fatalf("Access-login presign err=%v", err)
	}
}
