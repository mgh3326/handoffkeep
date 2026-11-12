package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func ytEnroll(t *testing.T, s *store.Store, task int64) {
	t.Helper()
	id := strconv.FormatInt(task, 10)
	verified, err := s.YTCanaryCandidate(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYTCommentCanary(t.Context(), store.YTCommentCanary{TaskID: task, Tenant: "tests.youtrack.cloud", IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", AddedBy: "tests"}, verified); err != nil {
		t.Fatal(err)
	}
}

func ytOps(t *testing.T, s *store.Store, task int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.ListYTWriteOps(t.Context(), store.YTOpFilter{State: "all", TaskID: task})
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// REST (and the CLI, which posts through it) and the console both enqueue one
// op per comment of an enrolled task; the MCP path is covered in internal/mcp.
// The REST answer keeps every old key and type and adds one optional object,
// youtrack_projection: the op that will copy the comment, never a claim that
// YouTrack has it.
func TestAllAuthorizedCommentEntrypointsEnqueue(t *testing.T) {
	s := taskTestStore(t)
	s.SetYTCommentsMode(store.YTCommentsCanary)
	task := newTask(t, s, taskLane(t), "yt canary entry points", 0)
	ytEnroll(t, s, task.ID)
	h := taskHTTP(s)
	defer h.Close()

	status, body := postComment(t, h.Client(), h.URL, "node-token", task.ID, map[string]any{"body": "via REST"})
	if status != http.StatusCreated {
		t.Fatalf("REST status %d %v", status, body)
	}
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if fmt.Sprint(keys) != "[author body created_at id task_id youtrack_projection]" {
		t.Fatalf("REST comment answer changed shape: %v", keys)
	}
	if _, ok := body["id"].(float64); !ok || body["body"] != "via REST" || body["author"] != "node" || body["task_id"] != float64(task.ID) {
		t.Fatalf("REST comment fields changed: %v", body)
	}
	if _, ok := body["created_at"].(string); !ok {
		t.Fatalf("created_at is no longer a string: %v", body)
	}
	client := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}
	x, err := client.CreateTaskComment(t.Context(), task.ID, "via the CLI client")
	if err != nil || x.ID == 0 || x.Author != "node" || x.Body != "via the CLI client" {
		t.Fatalf("CLI client decode: %+v %v", x, err)
	}
	ops := ytOps(t, s, task.ID)
	if len(ops) != 2 || ops[0].Actor != "node" || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("REST/CLI ops = %+v", ops)
	}
	projection, _ := body["youtrack_projection"].(map[string]any)
	if fmt.Sprint(projection) != fmt.Sprint(map[string]any{"status": "pending", "op_id": float64(ops[0].ID)}) {
		t.Fatalf("REST projection = %v, want pending with op %d", body["youtrack_projection"], ops[0].ID)
	}
	if p := x.YouTrackProjection; p == nil || p.Status != "pending" || p.OpID != ops[1].ID || p.Reason != "" {
		t.Fatalf("CLI client projection = %+v, want pending with op %d", p, ops[1].ID)
	}

	// Refused locally (the body quotes the op marker): the comment commits,
	// its projection is rejected with the reason code and nothing else.
	status, body = postComment(t, h.Client(), h.URL, "node-token", task.ID, map[string]any{"body": "quotes <!-- hk-op:1 --> in prose"})
	projection, _ = body["youtrack_projection"].(map[string]any)
	if status != http.StatusCreated || projection["status"] != "rejected" || projection["reason"] != ytwrite.ReasonInvalidComment || projection["op_id"] == nil || len(projection) != 3 {
		t.Fatalf("rejected projection = %d %v", status, body)
	}
	// A task outside the canary: no op, projection none.
	plain := newTask(t, s, taskLane(t), "yt canary outside", 0)
	status, body = postComment(t, h.Client(), h.URL, "node-token", plain.ID, map[string]any{"body": "not enrolled"})
	if projection, _ := body["youtrack_projection"].(map[string]any); status != http.StatusCreated || fmt.Sprint(projection) != fmt.Sprint(map[string]any{"status": "none"}) {
		t.Fatalf("projection outside the canary = %d %v", status, body)
	}

	us := uiStore(t)
	us.SetYTCommentsMode(store.YTCommentsCanary)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	ui := newUITestServer(t, us, fixture, hub.server.URL, "hub-test-token", 0)
	defer ui.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	uiTask := createUITask(t, us, uiLane(t, "yt-canary"), "console canary")
	ytEnroll(t, us, uiTask.ID)
	cookie, csrf := boardCSRF(t, ui.Client(), fmt.Sprintf("%s/ui/tasks/%d", ui.URL, uiTask.ID), assertion)
	response := uiPostForm(t, ui.Client(), uiCommentURL(ui.URL, uiTask.ID), assertion, cookie, url.Values{"body": {"via the console"}, "csrf": {csrf}}, ui.URL, false)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("console status %d %q", response.StatusCode, responseText(t, response))
	}
	if ops := ytOps(t, us, uiTask.ID); len(ops) != 1 || ops[0].Actor != "operator:admin@example.com" {
		t.Fatalf("console ops = %+v", ops)
	}
}

// The op view shows what the operator needs and nothing else: no body, no raw
// error, no key; every unresolved state reads as pending.
func TestOpsViewUsesSafeDTO(t *testing.T) {
	s := taskTestStore(t)
	s.SetYTCommentsMode(store.YTCommentsCanary)
	task := newTask(t, s, taskLane(t), "yt ops view", 0)
	ytEnroll(t, s, task.ID)
	h := taskHTTP(s)
	defer h.Close()
	for _, body := range []string{"secret-looking body one", "second body"} {
		if status, out := postComment(t, h.Client(), h.URL, "node-token", task.ID, map[string]any{"body": body}); status != http.StatusCreated {
			t.Fatalf("status %d %v", status, out)
		}
	}
	ops := ytOps(t, s, task.ID)
	ctx := context.Background()
	ledger := s.YTWriteLedger()
	now := time.Now().UTC()
	if _, out, err := ledger.Claim(ctx, now, ops[0].ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim %s %v", out, err)
	}
	if _, err := ledger.MarkUnknown(ctx, now, ops[0].ID, now.Add(time.Minute), ytwrite.ReasonTransport, "read tcp 10.0.0.1: connection reset; token=abc"); err != nil {
		t.Fatal(err)
	}

	get := func(query string) (int, map[string]any) {
		resp := request(t, h.Client(), http.MethodGet, h.URL+"/v1/yt/ops?"+query, "node-token", nil)
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status, out := get("task=" + strconv.FormatInt(task.ID, 10))
	list, _ := out["ops"].([]any)
	if status != http.StatusOK || len(list) != 2 {
		t.Fatalf("GET /v1/yt/ops = %d %v", status, out)
	}
	first := list[0].(map[string]any)
	if first["state"] != "remote_unknown" || first["status"] != "pending" || first["entity_id"] != strconv.FormatInt(task.ID, 10) {
		t.Fatalf("remote_unknown op shown as %v", first)
	}
	if list[1].(map[string]any)["status"] != "pending" {
		t.Fatalf("prepared op shown as %v", list[1])
	}
	for _, op := range list {
		for _, forbidden := range []string{"payload", "body", "last_error", "key", "idem_key", "principal", "payload_sha"} {
			if _, ok := op.(map[string]any)[forbidden]; ok {
				t.Fatalf("op view carries %q: %v", forbidden, op)
			}
		}
	}
	raw, _ := json.Marshal(out)
	for _, leak := range []string{"secret-looking body", "connection reset", "token=abc"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("op view leaks %q: %s", leak, raw)
		}
	}
	if status, out := get("task=" + strconv.FormatInt(task.ID, 10) + "&limit=1"); status != http.StatusOK || len(out["ops"].([]any)) != 1 || out["next_after_id"] == nil {
		t.Fatalf("paged view = %d %v", status, out)
	}
	if status, out := get("state=done"); status != http.StatusBadRequest || out["error"] != "invalid_context" {
		t.Fatalf("bad state = %d %v", status, out)
	}
	if status, _ := get("state=pending"); status != http.StatusOK {
		t.Fatalf("pending filter = %d", status)
	}
	client := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}
	views, next, err := client.ListYTOps(ctx, "all", "", task.ID, 0, 10)
	if err != nil || len(views) != 2 || next != 0 || views[0].Status != ytwrite.StatusPending {
		t.Fatalf("remote ListYTOps = %+v %d %v", views, next, err)
	}
}
