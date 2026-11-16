package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDotsToolsRegistered(t *testing.T) {
	srv := httptest.NewServer(HTTPHandler(api.Service{}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]bool{}
	descriptions := map[string]string{}
	for _, tool := range list.Tools {
		if tool.Name == "dots_inbox" || tool.Name == "dots_reply" {
			tools[tool.Name] = true
			descriptions[tool.Name] = tool.Description
		}
	}
	if !tools["dots_inbox"] || !tools["dots_reply"] {
		t.Fatalf("dots tools: %v", tools)
	}
	// The inbox must tell dots to read the files rather than ask for a copy.
	if want := "attachments lists files on the request; read them before asking for a copy"; !strings.Contains(descriptions["dots_inbox"], want) {
		t.Fatalf("dots_inbox description omits %q: %s", want, descriptions["dots_inbox"])
	}
	// A request that stops short must be visible as stopped: a partial result
	// goes out as RESULT saying what is missing, and only a real obstacle or a
	// real question uses BLOCKED or QUESTION (operator decision 360).
	for _, want := range []string{
		"When you stop without going on, send RESULT and say in its body what the request asked for that is missing, why, and who is next",
		"a partial result is never QUESTION or PROGRESS",
		"Send BLOCKED when someone else must clear something before you can go on, and QUESTION only when you need an answer to go on",
	} {
		if !strings.Contains(descriptions["dots_reply"], want) {
			t.Fatalf("dots_reply description omits %q: %s", want, descriptions["dots_reply"])
		}
	}
}

func TestDotsReplyWritesSwitch(t *testing.T) {
	o := operatorTools{service: api.Service{Store: &store.Store{}}}
	for _, value := range []string{"", "off", "invalid"} {
		t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", value)
		_, _, err := o.dotsReply(context.Background(), nil, store.DotsReplyInput{})
		if err == nil || err.Error() != "dots_reply_writes_disabled" {
			t.Fatalf("setting %q: err=%v", value, err)
		}
	}
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "on")
	_, _, err := o.dotsReply(context.Background(), nil, store.DotsReplyInput{})
	if err == nil || !strings.Contains(err.Error(), "invalid dots reply") {
		t.Fatalf("on: err=%v", err)
	}
}

func TestDotsInboxAvailableWhenWritesOff(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "off")
	st := newOperatorStore(t)
	if _, err := st.CreateDotsRequest(context.Background(), "caller", store.DotsRequestInput{Title: "Research", Body: "Investigate"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	text, failed := toolText(t, session, "dots_inbox", map[string]any{})
	if failed || !strings.Contains(text, "Investigate") {
		t.Fatalf("inbox failed=%v text=%s", failed, text)
	}
	text, failed = toolText(t, session, "dots_reply", map[string]any{"task_id": 1, "kind": "RESULT", "body": "done", "idempotency_key": "key"})
	if !failed || !strings.Contains(text, "dots_reply_writes_disabled") {
		t.Fatalf("reply failed=%v text=%s", failed, text)
	}
}

func TestDotsReplyExplainsMissingYouTrackIssue(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "on")
	st := newOperatorStore(t)
	task, err := st.CreateDotsRequest(context.Background(), "caller", store.DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.ReplyDots(context.Background(), "operator:test@example.com", "chatgpt", store.DotsReplyInput{
		TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: "start", ExpectedUpdatedAt: &task.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	st.SetYTCommentsMode(store.YTCommentsOn)
	if _, _, err := st.SetYTAuthority(context.Background(), []store.YTAuthorityKind{store.YTAuthorityComments}, store.YTAuthorityYouTrack, "operator", "test"); err != nil {
		t.Fatal(err)
	}
	access := newAccessFixture(t)
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, nil, access.verifier))
	defer srv.Close()
	assertion := access.token(t, jwt.MapClaims{"email": "operator@example.com"})
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": assertion})
	text, failed := toolText(t, session, "dots_reply", map[string]any{
		"task_id": task.ID, "kind": "PROGRESS", "body": "No issue yet", "idempotency_key": "progress",
		"run_token": started.RunToken,
	})
	if !failed || !strings.Contains(text, "this task has no YouTrack issue yet; retry in a minute") {
		t.Fatalf("dots_reply failed=%v text=%s", failed, text)
	}
}

func TestDotsNamedTaskIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		self int64
		max  int
		want []int64
	}{
		{name: "case insensitive", body: "full package on HK 4715", self: 9, max: 5, want: []int64{4715}},
		{name: "body order and distinct", body: "hk 7 then hk 3 then hk 7", self: 9, max: 5, want: []int64{7, 3}},
		{name: "never self", body: "this request hk 9 and hk 12", self: 9, max: 5, want: []int64{12}},
		{name: "at most max", body: "hk 1 hk 2 hk 3 hk 4 hk 5 hk 6 hk 7", self: 9, max: 5, want: []int64{1, 2, 3, 4, 5}},
		{name: "no id joined to the word", body: "hk4715 and hk 0", self: 9, max: 5, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dotsNamedTaskIDs(tc.body, tc.self, tc.max); !slices.Equal(got, tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

type dotsInboxAttachmentOut struct {
	SHA256       string    `json:"sha256"`
	SizeBytes    int64     `json:"size_bytes"`
	MIME         string    `json:"mime"`
	OriginalName string    `json:"original_name"`
	CreatedAt    time.Time `json:"created_at"`
	FromTask     int64     `json:"from_task"`
}

type dotsInboxOut struct {
	Requests []struct {
		Task struct {
			ID int64 `json:"id"`
		} `json:"task"`
		Body                           string                   `json:"body"`
		Attachments                    []dotsInboxAttachmentOut `json:"attachments"`
		AttachmentsTruncated           bool                     `json:"attachments_truncated"`
		ReferencedAttachments          []dotsInboxAttachmentOut `json:"referenced_attachments"`
		ReferencedAttachmentsTruncated bool                     `json:"referenced_attachments_truncated"`
		Options                        []store.DecisionOption   `json:"options"`
	} `json:"requests"`
}

// dotsInboxText runs the tool directly so a test can carry its own context
// (the query-count test needs a recording span in it).
func dotsInboxText(t *testing.T, o operatorTools, ctx context.Context, in dotsInboxInput) string {
	t.Helper()
	result, _, err := o.dotsInbox(ctx, nil, in)
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, content := range result.Content {
		if value, ok := content.(*gmcp.TextContent); ok {
			text += value.Text
		}
	}
	return text
}

func decodeDotsInbox(t *testing.T, text string) dotsInboxOut {
	t.Helper()
	var out dotsInboxOut
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("inbox result is not JSON: %v: %s", err, text)
	}
	return out
}

// dotsAttachment inserts one attachment bound to a task ref at a fixed time,
// so ordering does not depend on when the test runs.
func dotsAttachment(t *testing.T, pool *pgxpool.Pool, n int, taskID int64, createdAt time.Time, name string) store.Attachment {
	t.Helper()
	sha := fmt.Sprintf("%064x", n)
	if _, err := pool.Exec(t.Context(), `INSERT INTO attachments(sha256,size_bytes,mime,original_name,created_by,created_at) VALUES($1,$2,'image/png',$3,'desktop',$4)`, sha, n, name, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO attachment_refs(sha256,ref_kind,ref_id) VALUES($1,'task',$2)`, sha, strconv.FormatInt(taskID, 10)); err != nil {
		t.Fatal(err)
	}
	return store.Attachment{SHA256: sha, SizeBytes: int64(n), MIME: "image/png", OriginalName: name, CreatedBy: "desktop", CreatedAt: createdAt}
}

func TestDotsInboxListsRequestAndNamedTaskAttachments(t *testing.T) {
	st, pool := newOperatorStoreWithPool(t)
	ctx := context.Background()
	packageTask := newOperatorTask(t, st, "package task")
	request, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Review", Body: "Review the pair. The full package is on HK " + strconv.FormatInt(packageTask.ID, 10) + "."})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Nothing", Body: "No files on this one."})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	second := dotsAttachment(t, pool, 1, request.ID, base.Add(2*time.Second), "second.png")
	first := dotsAttachment(t, pool, 2, request.ID, base, "first.png")
	packaged := dotsAttachment(t, pool, 3, packageTask.ID, base.Add(time.Second), "package.png")

	o := operatorTools{service: api.Service{Store: st}}
	out := decodeDotsInbox(t, dotsInboxText(t, o, ctx, dotsInboxInput{}))
	if len(out.Requests) != 2 || out.Requests[0].Task.ID != request.ID || out.Requests[1].Task.ID != empty.ID {
		t.Fatalf("requests = %+v", out.Requests)
	}
	got := out.Requests[0]
	if len(got.Attachments) != 2 || got.Attachments[0].SHA256 != first.SHA256 || got.Attachments[1].SHA256 != second.SHA256 {
		t.Fatalf("own attachments order = %+v", got.Attachments)
	}
	if got.Attachments[0].OriginalName != "first.png" || got.Attachments[0].SizeBytes != first.SizeBytes || got.Attachments[0].MIME != "image/png" || !got.Attachments[0].CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("own attachment fields = %+v", got.Attachments[0])
	}
	if got.AttachmentsTruncated || got.ReferencedAttachmentsTruncated {
		t.Fatalf("truncated = %v/%v with two and one attachments", got.AttachmentsTruncated, got.ReferencedAttachmentsTruncated)
	}
	if len(got.ReferencedAttachments) != 1 || got.ReferencedAttachments[0].SHA256 != packaged.SHA256 || got.ReferencedAttachments[0].OriginalName != "package.png" {
		t.Fatalf("referenced attachments = %+v", got.ReferencedAttachments)
	}
	// The reference names the task the body named, not the request.
	if got.ReferencedAttachments[0].FromTask != packageTask.ID {
		t.Fatalf("referenced from_task = %d, want %d", got.ReferencedAttachments[0].FromTask, packageTask.ID)
	}
	// A request with no files carries empty lists, never null.
	text := dotsInboxText(t, o, ctx, dotsInboxInput{})
	if !strings.Contains(text, `"attachments":[]`) || !strings.Contains(text, `"referenced_attachments":[]`) {
		t.Fatalf("empty lists are not []: %s", text)
	}
}

func TestDotsInboxCapsAttachmentLists(t *testing.T) {
	st, pool := newOperatorStoreWithPool(t)
	ctx := context.Background()
	packageTask := newOperatorTask(t, st, "package task")
	request, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Review", Body: "Everything is on hk " + strconv.FormatInt(packageTask.ID, 10) + "."})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	for i := 0; i < dotsInboxAttachmentLimit+5; i++ {
		dotsAttachment(t, pool, 100+i, request.ID, base.Add(time.Duration(i)*time.Second), fmt.Sprintf("own-%02d.png", i))
		dotsAttachment(t, pool, 200+i, packageTask.ID, base.Add(time.Duration(i)*time.Second), fmt.Sprintf("package-%02d.png", i))
	}
	o := operatorTools{service: api.Service{Store: st}}
	got := decodeDotsInbox(t, dotsInboxText(t, o, ctx, dotsInboxInput{})).Requests[0]
	if len(got.Attachments) != dotsInboxAttachmentLimit || !got.AttachmentsTruncated {
		t.Fatalf("own attachments = %d truncated=%v", len(got.Attachments), got.AttachmentsTruncated)
	}
	if got.Attachments[0].OriginalName != "own-00.png" || got.Attachments[dotsInboxAttachmentLimit-1].OriginalName != "own-19.png" {
		t.Fatalf("own attachments keep the first 20 by created_at: %s .. %s", got.Attachments[0].OriginalName, got.Attachments[dotsInboxAttachmentLimit-1].OriginalName)
	}
	if len(got.ReferencedAttachments) != dotsInboxAttachmentLimit || !got.ReferencedAttachmentsTruncated {
		t.Fatalf("referenced attachments = %d truncated=%v", len(got.ReferencedAttachments), got.ReferencedAttachmentsTruncated)
	}
}

func TestDotsInboxReadsAtMostFiveNamedTasks(t *testing.T) {
	st, pool := newOperatorStoreWithPool(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	body := "The package is spread over"
	ids := make([]int64, 0, 7)
	for i := 0; i < 7; i++ {
		task := newOperatorTask(t, st, fmt.Sprintf("package %d", i))
		ids = append(ids, task.ID)
		body += " hk " + strconv.FormatInt(task.ID, 10)
		dotsAttachment(t, pool, 300+i, task.ID, base.Add(time.Duration(i)*time.Second), fmt.Sprintf("package-%d.png", i))
	}
	if _, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Review", Body: body + "."}); err != nil {
		t.Fatal(err)
	}
	o := operatorTools{service: api.Service{Store: st}}
	got := decodeDotsInbox(t, dotsInboxText(t, o, ctx, dotsInboxInput{})).Requests[0]
	if len(got.ReferencedAttachments) != dotsInboxNamedTaskLimit {
		t.Fatalf("referenced attachments = %d, want %d", len(got.ReferencedAttachments), dotsInboxNamedTaskLimit)
	}
	for i, attachment := range got.ReferencedAttachments {
		if attachment.OriginalName != fmt.Sprintf("package-%d.png", i) {
			t.Fatalf("referenced %d = %s, want the first five named tasks in body order", i, attachment.OriginalName)
		}
	}
	if got.ReferencedAttachmentsTruncated {
		t.Fatal("five items from five tasks reported truncated")
	}
}

// One read serves the whole page: three requests with files must still make a
// single statement that touches attachment_refs.
func TestDotsInboxReadsAttachmentsInOneQuery(t *testing.T) {
	st, pool := newOperatorStoreWithPool(t)
	base := time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		request, err := st.CreateDotsRequest(context.Background(), "caller", store.DotsRequestInput{Title: fmt.Sprintf("Review %d", i), Body: "One file."})
		if err != nil {
			t.Fatal(err)
		}
		dotsAttachment(t, pool, 400+i, request.ID, base.Add(time.Duration(i)*time.Second), fmt.Sprintf("file-%d.png", i))
	}
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	ctx, span := provider.Tracer("dots-test").Start(context.Background(), "dots_inbox")
	o := operatorTools{service: api.Service{Store: st}}
	decodeDotsInbox(t, dotsInboxText(t, o, ctx, dotsInboxInput{}))
	span.End()
	queries := 0
	for _, ended := range exporter.GetSpans() {
		for _, attr := range ended.Attributes {
			if attr.Key == "db.query.text" && strings.Contains(attr.Value.AsString(), "attachment_refs") {
				queries++
			}
		}
	}
	if queries != 1 {
		t.Fatalf("attachment queries = %d, want 1 for the whole page", queries)
	}
}

// The inbox carries a request's declared options, so dots sees that the
// request ends in the operator's choice and what the choices are (hk 4789).
func TestDotsInboxListsDeclaredOptions(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	declared, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{
		Title: "Review", Body: "Pick one.", RequesterLane: "requester",
		Options: []store.DecisionOption{{Key: "A", Label: "시안 A"}, {Key: "B", Label: "시안 B"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{Title: "Nothing", Body: "No choice here."})
	if err != nil {
		t.Fatal(err)
	}
	o := operatorTools{service: api.Service{Store: st}}
	out := decodeDotsInbox(t, dotsInboxText(t, o, ctx, dotsInboxInput{}))
	if len(out.Requests) != 2 || out.Requests[0].Task.ID != declared.ID || out.Requests[1].Task.ID != plain.ID {
		t.Fatalf("requests = %+v", out.Requests)
	}
	got := out.Requests[0].Options
	if len(got) != 2 || got[0].Key != "A" || got[0].Label != "시안 A" || got[1].Key != "B" || got[1].Label != "시안 B" {
		t.Fatalf("options = %+v", got)
	}
	if out.Requests[1].Options != nil {
		t.Fatalf("undeclared request lists options: %+v", out.Requests[1].Options)
	}
}

// dots_reply carries the RESULT-only annotations into the operator's decision,
// and refuses them on another kind or naming no declared key (hk 4789).
func TestDotsReplyResultAnnotations(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DOTS_REPLY_WRITES", "on")
	st := newOperatorStore(t)
	ctx := context.Background()
	task, err := st.CreateDotsRequest(ctx, "caller", store.DotsRequestInput{
		Title: "Review", Body: "Pick one.", RequesterLane: "requester",
		Options: []store.DecisionOption{{Key: "A", Label: "시안 A"}, {Key: "B", Label: "시안 B"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.ReplyDots(ctx, "operator:test@example.com", "chatgpt", store.DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "starting", IdempotencyKey: "start", ExpectedUpdatedAt: &task.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	access := newAccessFixture(t)
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, nil, access.verifier))
	defer srv.Close()
	// dots connects through Access, so the principal's channel is chatgpt --
	// the only channel ReplyDots accepts.
	assertion := access.token(t, jwt.MapClaims{"email": "operator@example.com"})
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": assertion})

	text, failed := toolText(t, session, "dots_reply", map[string]any{
		"task_id": task.ID, "kind": "PROGRESS", "body": "half", "idempotency_key": "progress",
		"run_token": started.RunToken, "one_fix": "fix it",
	})
	if !failed || !strings.Contains(text, "apply to RESULT only") {
		t.Fatalf("one_fix on PROGRESS failed=%v text=%s", failed, text)
	}
	text, failed = toolText(t, session, "dots_reply", map[string]any{
		"task_id": task.ID, "kind": "RESULT", "body": "done", "idempotency_key": "result-bad",
		"run_token": started.RunToken, "recommended": "C",
	})
	if !failed || !strings.Contains(text, "is not a declared option key") {
		t.Fatalf("unknown recommended failed=%v text=%s", failed, text)
	}
	if _, failed := toolText(t, session, "dots_reply", map[string]any{
		"task_id": task.ID, "kind": "RESULT", "body": "두 시안 완성", "idempotency_key": "result",
		"run_token": started.RunToken, "recommended": "B", "recommendation_reason": "B가 대비가 좋습니다",
		"unverified": []string{"모바일 실기기 미확인"}, "one_fix": "결제 버튼 대비를 올리세요",
	}); failed {
		t.Fatalf("annotated RESULT failed")
	}
	current, found, err := st.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	request := current.Refs.DecisionRequest
	if request == nil || request.Status != store.DecisionRequestOpen || current.State == store.StateCompleted {
		t.Fatalf("task=%+v request=%+v", current, request)
	}
	if request.RecommendationReason != "B가 대비가 좋습니다" || request.OneFix != "결제 버튼 대비를 올리세요" || len(request.Unverified) != 1 {
		t.Fatalf("request material = %+v", request)
	}
	options := current.Refs.DecisionOptions
	if options == nil || len(options.Options) != 2 || options.Options[0].Recommended || !options.Options[1].Recommended {
		t.Fatalf("options = %+v", options)
	}
}
