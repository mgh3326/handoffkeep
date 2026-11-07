package mcp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func newOperatorServer(t *testing.T, st *store.Store, tokens api.Tokens) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, nil))
	t.Cleanup(srv.Close)
	return srv
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
	YouTrack   *struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"youtrack"`
	ConsoleURL   string            `json:"console_url"`
	FieldSources map[string]string `json:"field_sources"`
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
		"state": "hk", "priority": "hk", "lane": "hk", "refs": "hk",
		"comments_by_agents": "hk", "operator_comments": "youtrack", "korean_display_text": "youtrack",
	} {
		if out.FieldSources[field] != want {
			t.Errorf("field_sources[%q]=%q want %q", field, out.FieldSources[field], want)
		}
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
		SyncID int64     `json:"sync_id"`
		Cursor string    `json:"cursor"`
		Kind   string    `json:"kind"`
		Model  string    `json:"model"`
		Action string    `json:"action"`
		TaskID *int64    `json:"task_id"`
		At     time.Time `json:"at"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
	MaxCursor  string `json:"max_cursor"`
	HasMore    bool   `json:"has_more"`
	Reset      bool   `json:"reset"`
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
