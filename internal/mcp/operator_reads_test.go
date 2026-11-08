package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func newOperatorServer(t *testing.T, st *store.Store, tokens api.Tokens) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, nil))
	t.Cleanup(srv.Close)
	return srv
}

// newOperatorStoreWithPool is newOperatorStore plus a pool on the same
// throwaway schema, for tests that must age or prune sync_log rows directly.
func newOperatorStoreWithPool(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	_, url := testdb.Schema(t, "mcp_operator_reset")
	st, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return st, pool
}

type taskGetOut struct {
	ID        int64   `json:"id"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	BodyDoc   string  `json:"body_doc"`
	State     string  `json:"state"`
	Priority  int     `json:"priority"`
	Kind      string  `json:"kind"`
	Lane      string  `json:"lane"`
	Project   *string `json:"project"`
	Area      *string `json:"area"`
	Labels    []int64 `json:"labels"`
	ClaimedBy string  `json:"claimed_by"`
	Refs      struct {
		PR    string `json:"pr"`
		JobID string `json:"job_id"`
	} `json:"refs"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
	ClosedAt  *time.Time           `json:"closed_at"`
	Parent    *store.TaskChild     `json:"parent"`
	Children  []store.TaskChild    `json:"children"`
	Relations []store.TaskRelation `json:"relations"`
	Blockers  []struct {
		RelationID int64            `json:"relation_id"`
		Task       *store.TaskChild `json:"task"`
	} `json:"blockers"`
	ProjectOwner *store.ProjectOwner `json:"project_owner"`
	Translations map[string]struct {
		Text  string `json:"text"`
		Stale bool   `json:"stale"`
	} `json:"translations"`
	CommentsPreview []store.TaskComment `json:"comments_preview"`
	CommentsMore    bool                `json:"comments_more"`
	EventsPreview   []struct {
		Kind string `json:"kind"`
	} `json:"events_preview"`
	EventsMore bool `json:"events_more"`
	Jobs       []struct {
		JobID string       `json:"job_id"`
		Model string       `json:"model"`
		Run   store.JobRun `json:"run"`
	} `json:"jobs"`
	JobsMore bool `json:"jobs_more"`
	YouTrack *struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"youtrack"`
	ConsoleURL   string            `json:"console_url"`
	FieldSources map[string]string `json:"field_sources"`
	Attachments  []struct {
		SHA256       string `json:"sha256"`
		OriginalName string `json:"original_name"`
	} `json:"attachments"`
	AttachmentsMore bool `json:"attachments_more"`
	ExternalLinks   []struct {
		Kind   string  `json:"kind"`
		Source string  `json:"source"`
		Ref    string  `json:"ref"`
		URL    *string `json:"url"`
	} `json:"external_links"`
	FieldSync map[string]struct {
		Source       string     `json:"source"`
		Status       string     `json:"status"`
		LastSyncedAt *time.Time `json:"last_synced_at"`
		SourceURL    *string    `json:"source_url"`
	} `json:"field_sync"`
}

func TestTaskGetCompleteFields(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-project"
	if _, _, err := st.PutDocument(ctx, store.Document{
		Key: "task-body/get-fixture", Kind: "brief", Body: "the fixture body", CreatedBy: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	create := func(title string, bodyDoc string) store.Task {
		t.Helper()
		task, err := st.CreateTask(ctx, store.Task{
			Lane: "mcp-get-lane", Title: title, Kind: "implement", Project: &project,
			CreatedBy: "tester", BodyDoc: bodyDoc,
			Refs: store.TaskRefs{PR: "https://github.com/mgh3326/handoffkeep/pull/9", JobID: "job-9"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	parent := create("MCP get parent", "")
	subject := create("MCP get subject", "task-body/get-fixture")
	child := create("MCP get child", "")
	blocker := create("MCP get blocker", "")

	if _, _, err := st.SetTaskParent(ctx, subject.ID, &parent.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetTaskParent(ctx, child.ID, &subject.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskRelation(ctx, blocker.ID, store.RelationBlocks, subject.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	owner, err := st.SetProjectDesignLane(ctx, "mcp-project", "mcp-design", "tester", "fixture owner")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 6; i++ {
		label, err := st.SaveLabel(ctx, store.Label{
			Name: fmt.Sprintf("mcp-get-label-%d", i), Color: "#123456", EditLevel: "anyone",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.SetTaskLabel(ctx, subject.ID, label.ID, "tester", true); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.PutTranslation(ctx, store.Translation{
		Entity: "task", EntityID: fmt.Sprint(subject.ID), Field: "title", Lang: "ko",
		SourceSHA256: store.SourceSHA256(subject.Title), Text: "MCP 조회 제목", Translator: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTranslation(ctx, store.Translation{
		Entity: "task", EntityID: fmt.Sprint(subject.ID), Field: "body", Lang: "ko",
		SourceSHA256: store.SourceSHA256("the fixture body"), Text: "MCP 조회 본문", Translator: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{
		TaskID: subject.ID, IssueID: "yt-get-1", DisplayRef: "HK-501", URL: "https://youtrack.example/issue/HK-501",
		Tenant: "test", Role: "mirror",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := st.CreateTaskComment(ctx, subject.ID, "tester", fmt.Sprintf("fixture comment %d", i)); err != nil {
			t.Fatal(err)
		}
	}

	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := newOperatorServer(t, st, tokens)
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	out := callTool[taskGetOut](t, session, "task_get", map[string]any{"id": subject.ID})
	if out.ID != subject.ID || out.Title != subject.Title || out.State != store.StateBacklog {
		t.Fatalf("task_get identity=%+v", out)
	}
	if out.Body != "the fixture body" || out.BodyDoc != "task-body/get-fixture" {
		t.Fatalf("body=%q body_doc=%q", out.Body, out.BodyDoc)
	}
	if out.Kind != "implement" || out.Lane != "mcp-get-lane" || out.Project == nil || *out.Project != "mcp-project" {
		t.Fatalf("kind/lane/project=%q/%q/%v", out.Kind, out.Lane, out.Project)
	}
	if len(out.Labels) != 6 {
		t.Fatalf("labels=%v", out.Labels)
	}
	if out.Refs.PR != "https://github.com/mgh3326/handoffkeep/pull/9" || out.Refs.JobID != "job-9" {
		t.Fatalf("refs=%+v", out.Refs)
	}
	if out.CreatedAt.IsZero() || out.UpdatedAt.IsZero() || out.ClosedAt != nil {
		t.Fatalf("timestamps created=%v updated=%v closed=%v", out.CreatedAt, out.UpdatedAt, out.ClosedAt)
	}
	if out.Parent == nil || out.Parent.ID != parent.ID {
		t.Fatalf("parent=%+v", out.Parent)
	}
	linkURL := map[string]string{}
	for _, link := range out.ExternalLinks {
		if link.URL != nil {
			linkURL[link.Kind] = *link.URL
		}
	}
	if linkURL["pr"] != "https://github.com/mgh3326/handoffkeep/pull/9" || linkURL["youtrack"] != "https://youtrack.example/issue/HK-501" {
		t.Fatalf("external_links=%+v", out.ExternalLinks)
	}
	// The mirror has no sync watermark yet: the field is shown as unknown, not omitted.
	for _, field := range []string{"operator_comments", "korean_display_text", "translations", "youtrack"} {
		sync, ok := out.FieldSync[field]
		if !ok || sync.Source != "youtrack" || sync.Status != "unknown" || sync.LastSyncedAt != nil {
			t.Fatalf("field_sync[%s]=%+v ok=%v", field, sync, ok)
		}
	}
	if out.FieldSources["field_sync"] != "hk" || out.FieldSources["external_links"] != "hk" || out.FieldSources["attachments"] != "hk" {
		t.Fatalf("field_sources missing new keys: %+v", out.FieldSources)
	}
	if len(out.Children) != 1 || out.Children[0].ID != child.ID {
		t.Fatalf("children=%+v", out.Children)
	}
	if len(out.Blockers) != 1 || out.Blockers[0].Task == nil || out.Blockers[0].Task.ID != blocker.ID {
		t.Fatalf("blockers=%+v", out.Blockers)
	}
	if out.ProjectOwner == nil || out.ProjectOwner.DesignLane != owner.DesignLane || out.ProjectOwner.EffectiveOwner != owner.EffectiveOwner {
		t.Fatalf("project_owner=%+v", out.ProjectOwner)
	}
	if out.Translations["title"].Text != "MCP 조회 제목" || out.Translations["body"].Text != "MCP 조회 본문" {
		t.Fatalf("translations=%+v", out.Translations)
	}
	if out.YouTrack == nil || out.YouTrack.ID != "HK-501" {
		t.Fatalf("youtrack=%+v", out.YouTrack)
	}
	if out.ConsoleURL != "https://console.robinco.dev/ui/tasks/"+fmt.Sprint(subject.ID) {
		t.Fatalf("console_url=%q", out.ConsoleURL)
	}
	if !out.CommentsMore || len(out.CommentsPreview) != taskGetPreviewLimit || out.CommentsPreview[len(out.CommentsPreview)-1].Body != "fixture comment 5" {
		t.Fatalf("comments preview=%+v more=%v", out.CommentsPreview, out.CommentsMore)
	}
	if !out.EventsMore || len(out.EventsPreview) != taskGetPreviewLimit {
		t.Fatalf("events preview=%+v more=%v", out.EventsPreview, out.EventsMore)
	}
	for field, want := range map[string]string{
		"id": "hk", "title": "hk", "body": "hk", "body_doc": "hk",
		"state": "hk", "priority": "hk", "kind": "hk", "lane": "hk",
		"project": "hk", "area": "hk", "labels": "hk", "claimed_by": "hk",
		"created_by": "hk", "refs": "hk", "created_at": "hk", "updated_at": "hk",
		"started_at": "hk", "completed_at": "hk", "canceled_at": "hk", "closed_at": "hk",
		"parent": "hk", "children": "hk", "relations": "hk", "blockers": "hk",
		"project_owner": "hk", "comments_preview": "hk", "events_preview": "hk",
		"console_url": "hk", "jobs": "hk",
		"comments_by_agents": "hk", "operator_comments": "youtrack",
		"korean_display_text": "youtrack", "translations": "youtrack", "youtrack": "youtrack",
	} {
		if out.FieldSources[field] != want {
			t.Errorf("field_sources[%q]=%q want %q", field, out.FieldSources[field], want)
		}
	}
}

func TestTaskGetJobs(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-jobs-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-jobs-project"
	task, err := st.CreateTask(ctx, store.Task{Lane: "mcp-jobs-lane", Title: "task with jobs", Kind: "implement", Project: &project, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	taskID := task.ID
	for _, id := range []string{"mcp-jobs-a", "mcp-jobs-b"} {
		if _, _, err := st.ClaimJob(ctx, store.Job{JobID: id, TaskID: &taskID, Host: "mac-dev", CreatedBy: "tester"}); err != nil {
			t.Fatal(err)
		}
	}
	spawned, model, selection := "spawned", "gpt-6.1-terra", `{"chosen":{"grade":"A+","tier":"waste","projected_pct":75.5,"why":"waste_risk tier=waste grade=A+ for task B projected=76%"},"candidates":[{"seat":"claude-opus","outcome":"dropped:role_filtered"}]}`
	if _, err := st.UpdateJob(ctx, "mcp-jobs-a", store.JobUpdate{State: &spawned, Model: &model, Selection: json.RawMessage(selection)}); err != nil {
		t.Fatal(err)
	}
	srv := newOperatorServer(t, st, api.Tokens{"tester": "bearer-secret"})
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	out := callTool[taskGetOut](t, session, "task_get", map[string]any{"id": task.ID})
	if len(out.Jobs) != 2 || out.JobsMore || out.Jobs[0].JobID != "mcp-jobs-a" || out.Jobs[1].JobID != "mcp-jobs-b" {
		t.Fatalf("jobs=%+v more=%v", out.Jobs, out.JobsMore)
	}
	run := out.Jobs[0].Run
	if !run.Recorded || !strings.Contains(run.Why, "작업 등급 B에 모델 등급 A+") || len(run.Others) != 1 || !strings.HasPrefix(run.What, "모델 gpt-6.1-terra") || run.Result != "상태 실행 중" {
		t.Fatalf("run=%+v", run)
	}
	if bare := out.Jobs[1].Run; bare.Why != store.NoSelectionText || bare.Recorded {
		t.Fatalf("job without selection: %+v", bare)
	}
}

type listPageOut struct {
	Tasks      []taskSummaryOut `json:"tasks"`
	Truncated  bool             `json:"truncated"`
	NextCursor *string          `json:"next_cursor"`
}

func TestTaskListPagingReturnsEveryTaskOnce(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-project"
	created := make([]store.Task, 0, 5)
	for i := 0; i < 5; i++ {
		task, err := st.CreateTask(ctx, store.Task{
			Lane: "mcp-page-lane", Title: fmt.Sprintf("paged task %d", i), Kind: "implement",
			Project: &project, CreatedBy: "tester",
		})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, task)
	}
	want := make([]int64, 0, len(created))
	sort.Slice(created, func(i, j int) bool {
		if !created[i].UpdatedAt.Equal(created[j].UpdatedAt) {
			return created[i].UpdatedAt.Before(created[j].UpdatedAt)
		}
		return created[i].ID < created[j].ID
	})
	for _, task := range created {
		want = append(want, task.ID)
	}

	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := newOperatorServer(t, st, tokens)
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	cursor := ""
	got := []int64{}
	for page := 0; page < 10; page++ {
		out := callTool[listPageOut](t, session, "task_list", map[string]any{
			"lane": "mcp-page-lane", "limit": 2, "cursor": cursor,
		})
		if out.NextCursor == nil {
			t.Fatal("paged task_list returned no next_cursor field")
		}
		for _, task := range out.Tasks {
			got = append(got, task.ID)
		}
		if *out.NextCursor == "" {
			if out.Truncated {
				t.Fatal("last page reports truncated")
			}
			break
		}
		cursor = *out.NextCursor
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paged ids=%v want=%v", got, want)
	}

	// updated_since starts the same ordering at the given time.
	since := created[2].UpdatedAt
	out := callTool[listPageOut](t, session, "task_list", map[string]any{
		"lane": "mcp-page-lane", "limit": 10, "updated_since": since.Format(time.RFC3339Nano),
	})
	if len(out.Tasks) != 3 {
		t.Fatalf("updated_since tasks=%d want 3", len(out.Tasks))
	}
	for i, task := range out.Tasks {
		if task.ID != created[2+i].ID {
			t.Fatalf("updated_since[%d]=%d want %d", i, task.ID, created[2+i].ID)
		}
	}

	// Without a cursor or updated_since the previous unpaged answer is kept.
	legacy := callTool[listPageOut](t, session, "task_list", map[string]any{"lane": "mcp-page-lane", "limit": 2})
	if legacy.NextCursor != nil {
		t.Fatalf("unpaged task_list gained next_cursor=%q", *legacy.NextCursor)
	}
	if len(legacy.Tasks) != 2 || !legacy.Truncated {
		t.Fatalf("unpaged task_list=%+v", legacy)
	}
}

type commentsPageOut struct {
	Comments   []store.TaskComment `json:"comments"`
	NextCursor string              `json:"next_cursor"`
	HasMore    bool                `json:"has_more"`
}

type eventsPageOut struct {
	Events     []store.TaskEvent `json:"events"`
	NextCursor string            `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

func TestTaskCommentsAndEventsPaging(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "MCP paging fixture")
	for i := 0; i < 7; i++ {
		if _, err := st.CreateTaskComment(ctx, task.ID, "tester", fmt.Sprintf("paged comment %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		label, err := st.SaveLabel(ctx, store.Label{
			Name: fmt.Sprintf("mcp-page-label-%d", i), Color: "#654321", EditLevel: "anyone",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.SetTaskLabel(ctx, task.ID, label.ID, "tester", true); err != nil {
			t.Fatal(err)
		}
	}

	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := newOperatorServer(t, st, tokens)
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	cursor := ""
	commentIDs := []int64{}
	for page := 0; page < 10; page++ {
		out := callTool[commentsPageOut](t, session, "task_comments", map[string]any{
			"id": task.ID, "limit": 3, "cursor": cursor,
		})
		for _, comment := range out.Comments {
			commentIDs = append(commentIDs, comment.ID)
		}
		if out.HasMore != (out.NextCursor != "") {
			t.Fatalf("comments has_more=%v next_cursor=%q", out.HasMore, out.NextCursor)
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if len(commentIDs) != 7 {
		t.Fatalf("comments=%v", commentIDs)
	}
	for i := 1; i < len(commentIDs); i++ {
		if commentIDs[i] <= commentIDs[i-1] {
			t.Fatalf("comments not oldest first: %v", commentIDs)
		}
	}

	full, found, err := st.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("task read: found=%v err=%v", found, err)
	}
	cursor = ""
	eventIDs := []int64{}
	for page := 0; page < 10; page++ {
		out := callTool[eventsPageOut](t, session, "task_events", map[string]any{
			"id": task.ID, "limit": 2, "cursor": cursor,
		})
		for _, event := range out.Events {
			eventIDs = append(eventIDs, event.ID)
		}
		if out.HasMore != (out.NextCursor != "") {
			t.Fatalf("events has_more=%v next_cursor=%q", out.HasMore, out.NextCursor)
		}
		if out.NextCursor == "" {
			break
		}
		cursor = out.NextCursor
	}
	if len(eventIDs) != len(full.Events) {
		t.Fatalf("events=%v want %d", eventIDs, len(full.Events))
	}
	for i := 1; i < len(eventIDs); i++ {
		if eventIDs[i] <= eventIDs[i-1] {
			t.Fatalf("events not oldest first: %v", eventIDs)
		}
	}
}

type changeFeedOut struct {
	Items []struct {
		SyncID      int64  `json:"sync_id"`
		Cursor      string `json:"cursor"`
		Kind        string `json:"kind"`
		Model       string `json:"model"`
		Action      string `json:"action"`
		TaskID      *int64 `json:"task_id"`
		TaskCurrent *struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
		} `json:"task_current"`
		At time.Time `json:"at"`
	} `json:"items"`
	NextCursor           string `json:"next_cursor"`
	MinCursor            string `json:"min_cursor"`
	MaxCursor            string `json:"max_cursor"`
	OldestRetainedSyncID int64  `json:"oldest_retained_sync_id"`
	HasMore              bool   `json:"has_more"`
	Reset                bool   `json:"reset"`
}

func TestChangesSinceReturnsEachChangeOnce(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	first := newOperatorTask(t, st, "MCP feed first")
	if _, err := st.TransitionTask(ctx, first.ID, store.StateUnstarted, "tester", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	second := newOperatorTask(t, st, "MCP feed second")
	if _, err := st.CreateTaskComment(ctx, first.ID, "tester", "feed comment"); err != nil {
		t.Fatal(err)
	}

	expected, _, err := st.SyncDelta(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) < 4 {
		t.Fatalf("fixture wrote only %d sync rows", len(expected))
	}

	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := newOperatorServer(t, st, tokens)
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	cursor := ""
	got := []int64{}
	kinds := map[string]bool{}
	taskCommentTask := map[int64]bool{}
	taskCurrentState := map[int64]string{}
	for page := 0; page < 20; page++ {
		out := callTool[changeFeedOut](t, session, "changes_since", map[string]any{"limit": 3, "cursor": cursor})
		if out.Reset {
			t.Fatal("fresh cursor reported a reset")
		}
		for _, item := range out.Items {
			if item.Cursor == "" {
				t.Fatal("item without a cursor")
			}
			got = append(got, item.SyncID)
			kinds[item.Kind] = true
			if item.TaskCurrent != nil {
				taskCurrentState[item.TaskCurrent.ID] = item.TaskCurrent.State
			}
			if item.Kind == "task_commented" {
				if item.TaskID == nil || *item.TaskID != first.ID {
					t.Fatalf("comment item task_id=%v want %d", item.TaskID, first.ID)
				}
				taskCommentTask[*item.TaskID] = true
			}
		}
		if out.HasMore {
			cursor = out.NextCursor
			continue
		}
		cursor = out.NextCursor
		break
	}
	if len(got) != len(expected) {
		t.Fatalf("feed ids=%v want %d rows", got, len(expected))
	}
	for i, change := range expected {
		if got[i] != change.SyncID {
			t.Fatalf("feed[%d]=%d want %d", i, got[i], change.SyncID)
		}
	}
	for _, kind := range []string{"task_created", "task_updated", "task_commented"} {
		if !kinds[kind] {
			t.Errorf("feed lacks kind %q", kind)
		}
	}
	if !taskCommentTask[first.ID] {
		t.Fatal("comment row did not name its task")
	}
	if second.ID <= 0 {
		t.Fatal("second fixture task missing")
	}
	// task_current is the state when the feed is read, not the state at each
	// historical row: both rows for the transitioned task report unstarted.
	if taskCurrentState[first.ID] != store.StateUnstarted {
		t.Fatalf("task_current[%d]=%q want %q", first.ID, taskCurrentState[first.ID], store.StateUnstarted)
	}
	if taskCurrentState[second.ID] != store.StateBacklog {
		t.Fatalf("task_current[%d]=%q want %q", second.ID, taskCurrentState[second.ID], store.StateBacklog)
	}

	// An idle call after the last cursor returns nothing and holds the cursor.
	idle := callTool[changeFeedOut](t, session, "changes_since", map[string]any{"cursor": cursor})
	if len(idle.Items) != 0 || idle.HasMore || idle.Reset {
		t.Fatalf("idle feed=%+v", idle)
	}
	if idle.NextCursor != cursor {
		t.Fatalf("idle cursor moved from %q to %q", cursor, idle.NextCursor)
	}
	if idle.MaxCursor == "" {
		t.Fatal("feed did not report max_cursor")
	}
}

// TestChangesSinceResetResumesAtOldestRetained pins the reset contract: the
// resume cursor sits immediately before the oldest retained row, so the next
// call returns that row instead of skipping it.
func TestChangesSinceResetResumesAtOldestRetained(t *testing.T) {
	st, pool := newOperatorStoreWithPool(t)
	ctx := context.Background()
	newOperatorTask(t, st, "MCP reset pruned one")
	newOperatorTask(t, st, "MCP reset pruned two")
	if _, err := pool.Exec(ctx, `UPDATE sync_log SET at=now()-interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PruneSync(ctx, 30*24*time.Hour); err != nil || n < 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	retained := newOperatorTask(t, st, "MCP reset retained")

	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := newOperatorServer(t, st, tokens)
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	reset := callTool[changeFeedOut](t, session, "changes_since", map[string]any{})
	if !reset.Reset || len(reset.Items) != 0 || reset.MinCursor == "" {
		t.Fatalf("reset answer=%+v", reset)
	}
	var resume mcpCursor
	if err := decodeCursor(reset.MinCursor, cursorChanges, &resume); err != nil {
		t.Fatalf("min_cursor %q: %v", reset.MinCursor, err)
	}
	if reset.OldestRetainedSyncID != resume.Sync+1 {
		t.Fatalf("oldest_retained_sync_id=%d resume=%d", reset.OldestRetainedSyncID, resume.Sync)
	}
	if reset.NextCursor != reset.MinCursor {
		t.Fatalf("next_cursor=%q min_cursor=%q", reset.NextCursor, reset.MinCursor)
	}

	next := callTool[changeFeedOut](t, session, "changes_since", map[string]any{"cursor": reset.NextCursor})
	if next.Reset || len(next.Items) == 0 {
		t.Fatalf("resume answer=%+v", next)
	}
	if next.Items[0].SyncID != reset.OldestRetainedSyncID {
		t.Fatalf("first resumed sync_id=%d want the oldest retained row %d", next.Items[0].SyncID, reset.OldestRetainedSyncID)
	}
	if next.Items[0].TaskCurrent == nil || next.Items[0].TaskCurrent.ID != retained.ID {
		t.Fatalf("retained item task_current=%+v, want task %d", next.Items[0].TaskCurrent, retained.ID)
	}
}

func TestTaskGetAttachmentsAndSyncWatermark(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-attach-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-attach-project"
	if _, _, err := st.PutDocument(ctx, store.Document{
		Key: "task-body/attach-fixture", Kind: "brief", Body: "attachment body", CreatedBy: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, store.Task{
		Lane: "mcp-attach-lane", Title: "task with attachment", Kind: "implement", Project: &project,
		CreatedBy: "tester", BodyDoc: "task-body/attach-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	const sha = "0000000000000000000000000000000000000000000000000000000000000abc"
	if _, _, err := st.PutAttachment(ctx, store.Attachment{
		SHA256: sha, SizeBytes: 3, MIME: "text/plain", OriginalName: "notes.txt",
		CreatedBy: "tester", RefKind: "document", RefID: "task-body/attach-fixture",
	}, 1000, 10); err != nil {
		t.Fatal(err)
	}
	synced := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)
	if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{
		TaskID: task.ID, IssueID: "yt-attach-1", DisplayRef: "HK-777", URL: "https://youtrack.example/issue/HK-777",
		Tenant: "test", Role: "mirror", SyncedAt: synced,
	}); err != nil {
		t.Fatal(err)
	}

	srv := newOperatorServer(t, st, api.Tokens{"tester": "bearer-secret"})
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	out := callTool[taskGetOut](t, session, "task_get", map[string]any{"id": task.ID})
	if len(out.Attachments) != 1 || out.Attachments[0].SHA256 != sha || out.Attachments[0].OriginalName != "notes.txt" || out.AttachmentsMore {
		t.Fatalf("attachments=%+v more=%v", out.Attachments, out.AttachmentsMore)
	}
	sync := out.FieldSync["youtrack"]
	if sync.Status != "synced" || sync.LastSyncedAt == nil || !sync.LastSyncedAt.Equal(synced) || sync.SourceURL == nil || *sync.SourceURL != "https://youtrack.example/issue/HK-777" {
		t.Fatalf("field_sync[youtrack]=%+v", sync)
	}
}
