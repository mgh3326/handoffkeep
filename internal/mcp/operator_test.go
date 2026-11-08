package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	for key, value := range t.headers {
		clone.Header.Set(key, value)
	}
	return t.base.RoundTrip(clone)
}

func connectMCP(t *testing.T, endpoint string, headers map[string]string) *gmcp.ClientSession {
	t.Helper()
	transport := &gmcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: headers}},
		DisableStandaloneSSE: true,
	}
	session, err := gmcp.NewClient(&gmcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func toolText(t *testing.T, session *gmcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &gmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, content := range result.Content {
		if value, ok := content.(*gmcp.TextContent); ok {
			text += value.Text
		}
	}
	return text, result.IsError
}

func callTool[T any](t *testing.T, session *gmcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	text, isError := toolText(t, session, name, args)
	if isError {
		t.Fatalf("%s returned an error: %s", name, text)
	}
	var out T
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s result is not JSON: %v: %s", name, err, text)
	}
	return out
}

func newOperatorStore(t *testing.T) *store.Store {
	t.Helper()
	_, url := testdb.Schema(t, "mcp_operator")
	st, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func newOperatorTask(t *testing.T, st *store.Store, title string) store.Task {
	t.Helper()
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-project"
	task, err := st.CreateTask(ctx, store.Task{
		Lane: "mcp-lane", Title: title, Kind: "implement", Project: &project, CreatedBy: "tester",
		Refs: store.TaskRefs{PR: "https://github.com/mgh3326/handoffkeep/pull/123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

type taskSummaryOut struct {
	ID         int64   `json:"id"`
	Title      string  `json:"title"`
	Snippet    string  `json:"snippet"`
	State      string  `json:"state"`
	Priority   int     `json:"priority"`
	Lane       string  `json:"lane"`
	Project    *string `json:"project"`
	PR         string  `json:"pr"`
	ConsoleURL string  `json:"console_url"`
	YouTrack   *struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"youtrack"`
}

type searchOut struct {
	Results   []taskSummaryOut `json:"results"`
	Truncated bool             `json:"truncated"`
}

type listOut struct {
	Tasks     []taskSummaryOut `json:"tasks"`
	Truncated bool             `json:"truncated"`
}

type showOut struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	TitleKO  string `json:"title_ko"`
	State    string `json:"state"`
	Priority int    `json:"priority"`
	Lane     string `json:"lane"`
	Project  *string
	Refs     struct {
		PR string `json:"pr"`
	} `json:"refs"`
	ConsoleURL string `json:"console_url"`
	YouTrack   *struct {
		ID string `json:"id"`
	} `json:"youtrack"`
	Comments []store.TaskComment `json:"comments"`
	Events   []struct {
		Kind string `json:"kind"`
		By   string `json:"by"`
	} `json:"events"`
}

type decisionsOut struct {
	Decisions []struct {
		RequestID  string `json:"request_id"`
		TaskID     int64  `json:"task_id"`
		TaskTitle  string `json:"task_title"`
		Question   string `json:"question"`
		QuestionKO string `json:"question_ko"`
		ConsoleURL string `json:"console_url"`
		Options    []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"options"`
	} `json:"decisions"`
}

type answerOut struct {
	Result     string `json:"result"`
	Actor      string `json:"actor"`
	Channel    string `json:"channel"`
	ConsoleURL string `json:"console_url"`
}

type commentOut struct {
	Comment    store.TaskComment `json:"comment"`
	Actor      string            `json:"actor"`
	Channel    string            `json:"channel"`
	ConsoleURL string            `json:"console_url"`
}

type intakeOut struct {
	Created             bool             `json:"created"`
	Task                taskSummaryOut   `json:"task"`
	DuplicateCandidates []taskSummaryOut `json:"duplicate_candidates"`
	Actor               string           `json:"actor"`
	Channel             string           `json:"channel"`
}

func TestOperatorToolsRejectInvalidInputOverHTTP(t *testing.T) {
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{}, tokens, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	text, isError := toolText(t, session, "task_search", map[string]any{"query": ""})
	if !isError || !strings.Contains(text, "query is required") {
		t.Fatalf("isError=%v text=%q", isError, text)
	}
}

func TestToolListAnnotations(t *testing.T) {
	st := newOperatorStore(t)
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	annotations := map[string]*gmcp.ToolAnnotations{}
	for _, tool := range result.Tools {
		annotations[tool.Name] = tool.Annotations
	}
	for _, name := range []string{"task_search", "task_show", "task_get", "task_list", "task_comments", "task_events", "changes_since", "decisions_pending", "decisions_history", "projects_list", "task_states", "sync_status"} {
		a := annotations[name]
		if a == nil || !a.ReadOnlyHint {
			t.Errorf("%s is not marked read-only: %+v", name, a)
		}
	}
	for _, name := range []string{"decision_answer", "task_comment", "task_intake", "task_update", "decision_ask"} {
		a := annotations[name]
		if a == nil || a.ReadOnlyHint {
			t.Errorf("%s is not marked as a write: %+v", name, a)
		}
		if a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s is not marked non-destructive: %+v", name, a)
		}
	}
}

func TestOperatorToolsOverHTTP(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "MCP operator fixture task")
	for i := 0; i < 3; i++ {
		if _, err := st.CreateTaskComment(ctx, task.ID, "tester", fmt.Sprintf("fixture comment %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{
		TaskID: task.ID, IssueID: "yt-1", DisplayRef: "HK-91", URL: "https://youtrack.example/issue/HK-91",
		Tenant: "test", Role: "mirror",
	}); err != nil {
		t.Fatal(err)
	}
	decision, err := st.RecordDecisionRequest(ctx, task.ID, "desk", store.DecisionRequestInput{
		Question:      "Which storage?",
		Options:       store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "local"}, {Key: "B", Label: "central"}}, AllowFree: true},
		DefaultAction: "hold",
		Block:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutTranslation(ctx, store.Translation{
		Entity: "task", EntityID: fmt.Sprint(task.ID), Field: "title", Lang: "ko",
		SourceSHA256: store.SourceSHA256(task.Title), Text: "MCP 운영자 테스트", Translator: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTranslation(ctx, store.Translation{
		Entity: "decision_request", EntityID: decision.Request.ID, Field: "question", Lang: "ko",
		SourceSHA256: store.SourceSHA256(decision.Request.Question), Text: "어떤 저장소?", Translator: "test",
	}); err != nil {
		t.Fatal(err)
	}
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	search := callTool[searchOut](t, session, "task_search", map[string]any{"query": "MCP operator fixture"})
	if len(search.Results) != 1 || search.Results[0].ID != task.ID {
		t.Fatalf("search results=%+v", search.Results)
	}
	if search.Results[0].ConsoleURL != "https://console.robinco.dev/ui/tasks/"+fmt.Sprint(task.ID) {
		t.Fatalf("console_url=%q", search.Results[0].ConsoleURL)
	}
	if search.Results[0].YouTrack == nil || search.Results[0].YouTrack.ID != "HK-91" {
		t.Fatalf("youtrack=%+v", search.Results[0].YouTrack)
	}
	korean := callTool[searchOut](t, session, "task_search", map[string]any{"query": "운영자"})
	if len(korean.Results) != 1 || korean.Results[0].ID != task.ID {
		t.Fatalf("Korean search results=%+v", korean.Results)
	}

	show := callTool[showOut](t, session, "task_show", map[string]any{"id": task.ID})
	if show.Title != task.Title || show.State != store.StateBacklog || show.Lane != "mcp-lane" {
		t.Fatalf("show=%+v", show)
	}
	if show.TitleKO != "MCP 운영자 테스트" {
		t.Fatalf("title_ko=%q", show.TitleKO)
	}
	if show.Refs.PR != task.Refs.PR {
		t.Fatalf("refs.pr=%q", show.Refs.PR)
	}
	if len(show.Comments) != 3 || show.Comments[2].Body != "fixture comment 2" {
		t.Fatalf("comments=%+v", show.Comments)
	}
	if len(show.Events) == 0 {
		t.Fatal("show has no events")
	}
	if show.YouTrack == nil || show.YouTrack.ID != "HK-91" {
		t.Fatalf("show youtrack=%+v", show.YouTrack)
	}

	list := callTool[listOut](t, session, "task_list", map[string]any{"state": "backlog", "lane": "mcp-lane"})
	if len(list.Tasks) != 1 || list.Tasks[0].ID != task.ID {
		t.Fatalf("list=%+v", list.Tasks)
	}

	pending := callTool[decisionsOut](t, session, "decisions_pending", map[string]any{})
	if len(pending.Decisions) != 1 || pending.Decisions[0].RequestID != decision.Request.ID {
		t.Fatalf("pending=%+v", pending.Decisions)
	}
	if got := pending.Decisions[0].Question; got != "Which storage?" {
		t.Fatalf("question=%q", got)
	}
	if got := pending.Decisions[0].QuestionKO; got != "어떤 저장소?" {
		t.Fatalf("question_ko=%q", got)
	}
	if got := len(pending.Decisions[0].Options); got != 2 {
		t.Fatalf("options=%d", got)
	}

	answer := callTool[answerOut](t, session, "decision_answer", map[string]any{"request_id": decision.Request.ID, "option": "A"})
	if answer.Result != store.DecisionResultAnswered || answer.Actor != "tester" || answer.Channel != ChannelBearer {
		t.Fatalf("answer=%+v", answer)
	}
	text, isError := toolText(t, session, "decision_answer", map[string]any{"request_id": decision.Request.ID, "text": "a different answer"})
	if !isError || !strings.Contains(text, store.ErrDecisionRequestResolved.Error()) {
		t.Fatalf("second answer: isError=%v text=%q", isError, text)
	}

	comment := callTool[commentOut](t, session, "task_comment", map[string]any{"id": task.ID, "body": "from mcp"})
	if comment.Comment.Author != "tester" || comment.Comment.Body != "from mcp" {
		t.Fatalf("comment=%+v", comment.Comment)
	}

	intake := callTool[intakeOut](t, session, "task_intake", map[string]any{
		"title": "MCP operator fixture task duplicate", "body": "filed by chatgpt", "project": "mcp-project",
	})
	if !intake.Created || intake.Task.State != store.StateTriage {
		t.Fatalf("intake=%+v", intake)
	}
	if len(intake.DuplicateCandidates) == 0 || intake.DuplicateCandidates[0].ID != task.ID {
		t.Fatalf("duplicate candidates=%+v", intake.DuplicateCandidates)
	}
	recorded, found, err := st.GetTask(ctx, intake.Task.ID)
	if err != nil || !found {
		t.Fatalf("intake task read: found=%v err=%v", found, err)
	}
	if recorded.Source != "chatgpt" || recorded.CreatedBy != "tester" || !strings.HasPrefix(recorded.SourceRef, "chatgpt-") {
		t.Fatalf("intake row source=%q created_by=%q source_ref=%q", recorded.Source, recorded.CreatedBy, recorded.SourceRef)
	}
	again := callTool[intakeOut](t, session, "task_intake", map[string]any{
		"title": "MCP operator fixture task duplicate", "body": "filed by chatgpt", "project": "mcp-project",
	})
	if again.Created || again.Task.ID != intake.Task.ID {
		t.Fatalf("repeated intake=%+v", again)
	}
}

func TestTaskSearchFiltersBeforeLimit(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-project"
	target, err := st.CreateTask(ctx, store.Task{
		Lane: "mcp-search-lane", Title: "ranking filter target", Kind: "implement", Project: &project, CreatedBy: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(ctx, target.ID, store.StateUnstarted, "tester", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	// Newer, higher-ranked hits that do not match the state filter must not
	// consume the single result slot before the filter runs.
	for i := 0; i < 5; i++ {
		if _, err := st.CreateTask(ctx, store.Task{
			Lane: "mcp-search-lane", Title: fmt.Sprintf("ranking filter noise %d", i), Kind: "implement", Project: &project, CreatedBy: "tester",
		}); err != nil {
			t.Fatal(err)
		}
	}
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	out := callTool[searchOut](t, session, "task_search", map[string]any{"query": "ranking filter", "state": "unstarted", "limit": 1})
	if len(out.Results) != 1 || out.Results[0].ID != target.ID {
		t.Fatalf("filtered search=%+v", out.Results)
	}
	if out.Truncated {
		t.Fatal("filtered search reported truncation with a complete search window")
	}
}

func TestTaskSearchCutWindowStaysTruncated(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-project"
	create := func(title string) int64 {
		task, err := st.CreateTask(ctx, store.Task{
			Lane: "mcp-window-lane", Title: title, Kind: "implement", Project: &project, CreatedBy: "tester",
		})
		if err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	// The oldest hit falls outside the 50-row window; the target stays inside
	// it, so filtering leaves exactly the caller's limit while the window is
	// still cut.
	create("window cutoff noise oldest")
	target := create("window cutoff target")
	if _, err := st.TransitionTask(ctx, target, store.StateUnstarted, "tester", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 49; i++ {
		create(fmt.Sprintf("window cutoff noise %d", i))
	}
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	out := callTool[searchOut](t, session, "task_search", map[string]any{"query": "window cutoff", "state": "unstarted", "limit": 1})
	if len(out.Results) != 1 || out.Results[0].ID != target {
		t.Fatalf("filtered search=%+v", out.Results)
	}
	if !out.Truncated {
		t.Fatal("a cut search window with a full filtered page was reported as complete")
	}
}

func TestAccessIdentityWritesRecordOperator(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "MCP access fixture task")
	decision, err := st.RecordDecisionRequest(ctx, task.ID, "desk", store.DecisionRequestInput{
		Question:      "Ship it?",
		Options:       store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "yes"}, {Key: "B", Label: "no"}}, AllowFree: true},
		DefaultAction: "hold",
		Block:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newAccessFixture(t)
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, fixture.verifier))
	defer srv.Close()
	assertion := fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": assertion})

	const operator = "operator:operator@example.com"
	comment := callTool[commentOut](t, session, "task_comment", map[string]any{"id": task.ID, "body": "access comment"})
	// A ChatGPT-channel write is labelled dots; the operator stays in actor.
	if comment.Comment.Author != "dots" {
		t.Fatalf("comment author=%q", comment.Comment.Author)
	}
	if comment.Actor != operator || comment.Channel != ChannelChatGPT {
		t.Fatalf("comment attribution=%+v", comment)
	}

	answer := callTool[answerOut](t, session, "decision_answer", map[string]any{"request_id": decision.Request.ID, "option": "A"})
	if answer.Result != store.DecisionResultAnswered {
		t.Fatalf("answer=%+v", answer)
	}
	attempts, err := st.ListDecisionAttempts(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Channel != store.DecisionChannelConsole || attempts[0].Actor != operator || attempts[0].Provenance != store.DecisionProvenanceOperatorDirect {
		t.Fatalf("attempts=%+v", attempts)
	}

	intake := callTool[intakeOut](t, session, "task_intake", map[string]any{
		"title": "Access intake fixture", "body": "filed from chatgpt", "project": "mcp-project",
	})
	recorded, found, err := st.GetTask(ctx, intake.Task.ID)
	if err != nil || !found {
		t.Fatalf("intake read: found=%v err=%v", found, err)
	}
	if recorded.CreatedBy != "dots" {
		t.Fatalf("intake created_by=%q", recorded.CreatedBy)
	}
	if intake.Actor != operator || intake.Channel != ChannelChatGPT {
		t.Fatalf("intake attribution=%+v", intake)
	}
}

func TestDecisionsPendingTemplate(t *testing.T) {
	st := newOperatorStore(t)
	task := newOperatorTask(t, st, "template fixture")
	q := `Approve deployment {"identity":{"environment_known":true,"config":"x","payload":"y"},"host":"mbp","repo":"mgh3326/handoffkeep","previous":"unknown","release":"d4b91c069","paths":[],"reasons":["missing or invalid previous deployment receipt"]}`
	result, err := st.RecordDecisionRequest(t.Context(), task.ID, "mergewake-design", store.DecisionRequestInput{Question: q, Options: store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "Approve this deployment"}, {Key: "B", Label: "Keep the current version"}}}, DefaultAction: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	out := callTool[struct {
		Decisions []struct {
			RequestID  string `json:"request_id"`
			Question   string `json:"question"`
			QuestionKO string `json:"question_ko"`
			Options    []struct {
				Recommended bool `json:"recommended"`
			} `json:"options"`
			OptionsKO []struct {
				Label string `json:"label"`
			} `json:"options_ko"`
		} `json:"decisions"`
	}](t, session, "decisions_pending", map[string]any{})
	for _, d := range out.Decisions {
		if d.RequestID == result.Request.ID {
			if d.Question != q || !strings.Contains(d.QuestionKO, "대상 호스트") || len(d.Options) != 2 || !d.Options[0].Recommended || len(d.OptionsKO) != 2 || d.OptionsKO[0].Label != "이 배포 승인" {
				t.Fatalf("decision=%+v", d)
			}
			return
		}
	}
	t.Fatalf("decision missing: %+v", out)
}
