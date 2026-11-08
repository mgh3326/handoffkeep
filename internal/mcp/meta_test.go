package mcp

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func newMetaStore(t *testing.T) (*store.Store, *pgx.Conn) {
	t.Helper()
	_, url := testdb.Schema(t, "mcp_meta")
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return st, conn
}

func newMetaSession(t *testing.T, st *store.Store) *gmcp.ClientSession {
	t.Helper()
	tokens := api.Tokens{"tester": "bearer-secret"}
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, tokens, nil))
	t.Cleanup(srv.Close)
	return connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
}

type projectView struct {
	Name           string         `json:"name"`
	DesignLane     string         `json:"design_lane"`
	EffectiveOwner string         `json:"effective_owner"`
	UpdatedAt      *time.Time     `json:"updated_at"`
	Status         string         `json:"status"`
	StartAt        *time.Time     `json:"start_at"`
	TargetAt       *time.Time     `json:"target_at"`
	OpenTaskCounts map[string]int `json:"open_task_counts"`
	OpenTotal      int            `json:"open_total"`
}

type projectsOut struct {
	Projects []projectView `json:"projects"`
}

func TestProjectsListOwnersAndCounts(t *testing.T) {
	st, _ := newMetaStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "meta-project", "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetProjectDesignLane(ctx, "meta-project", "meta-project-design", "tester", "seed"); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	target := start.AddDate(0, 1, 0)
	if _, err := st.SetProjectMetadata(ctx, store.ProjectMetadata{
		Name: "meta-project", Status: "active", StartAt: &start, TargetAt: &target,
	}); err != nil {
		t.Fatal(err)
	}
	project := "meta-project"
	create := func(title string) store.Task {
		t.Helper()
		task, err := st.CreateTask(ctx, store.Task{
			Lane: "meta-lane", Title: title, Kind: "implement", Project: &project, CreatedBy: "tester",
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	create("meta backlog one")
	create("meta backlog two")
	unstarted := create("meta unstarted")
	if _, err := st.TransitionTask(ctx, unstarted.ID, store.StateUnstarted, "tester", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	dropped := create("meta dropped")
	if _, err := st.TransitionTask(ctx, dropped.ID, store.StateDropped, "tester", "not needed", nil, ""); err != nil {
		t.Fatal(err)
	}

	session := newMetaSession(t, st)
	out := callTool[projectsOut](t, session, "projects_list", map[string]any{})
	byName := map[string]projectView{}
	for _, p := range out.Projects {
		byName[p.Name] = p
	}
	p, ok := byName["meta-project"]
	if !ok {
		t.Fatalf("meta-project missing from %+v", out.Projects)
	}
	if p.DesignLane != "meta-project-design" || p.EffectiveOwner != "meta-project-design" || p.UpdatedAt == nil {
		t.Fatalf("owner=%+v", p)
	}
	if p.Status != "active" || p.StartAt == nil || !p.StartAt.Equal(start) || p.TargetAt == nil || !p.TargetAt.Equal(target) {
		t.Fatalf("metadata=%+v", p)
	}
	if p.OpenTaskCounts[store.StateBacklog] != 2 || p.OpenTaskCounts[store.StateUnstarted] != 1 || p.OpenTotal != 3 {
		t.Fatalf("open counts=%+v total=%d", p.OpenTaskCounts, p.OpenTotal)
	}
	if _, counted := p.OpenTaskCounts[store.StateDropped]; counted {
		t.Fatalf("settled state counted as open: %+v", p.OpenTaskCounts)
	}
	unowned, ok := byName["handoffkeep"]
	if !ok || unowned.DesignLane != "" || unowned.EffectiveOwner != store.OperatorDeskLane {
		t.Fatalf("unowned project=%+v", unowned)
	}
}

type projectHealthView struct {
	Name   string `json:"name"`
	Health struct {
		InProgress int `json:"in_progress"`
		Waiting    int `json:"waiting"`
		Blocked    int `json:"blocked"`
		Done       int `json:"done"`
		Overdue    int `json:"overdue"`
		Assignees  []struct {
			Name string `json:"name"`
			Open int    `json:"open"`
		} `json:"assignees"`
		TopBlocked []struct {
			TaskID    int64   `json:"task_id"`
			BlockedBy []int64 `json:"blocked_by"`
		} `json:"top_blocked"`
		TopOverdue []struct {
			TaskID int64     `json:"task_id"`
			DueAt  time.Time `json:"due_at"`
		} `json:"top_overdue"`
	} `json:"health"`
}

func TestProjectsListHealthCountsReasonsAndAssignees(t *testing.T) {
	st, _ := newMetaStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "health-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "health-project"
	create := func(title string) store.Task {
		t.Helper()
		task, err := st.CreateTask(ctx, store.Task{Lane: "health-lane", Title: title, Kind: "implement", Project: &project, CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	create("health waiting")
	claimed := create("health claimed")
	if _, err := st.ClaimTask(ctx, claimed.ID, "alice", "job-health-alice", ""); err != nil {
		t.Fatal(err)
	}
	blocker := create("health blocker")
	blocked := create("health blocked")
	if _, err := st.AddTaskRelation(ctx, blocker.ID, store.RelationBlocks, blocked.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	overdue := create("health overdue")
	past := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
	if _, _, err := st.SetTaskDue(ctx, overdue.ID, &past, "tester"); err != nil {
		t.Fatal(err)
	}

	before := time.Now().UTC()
	session := newMetaSession(t, st)
	raw := callTool[struct {
		AsOf     time.Time           `json:"as_of"`
		Projects []projectHealthView `json:"projects"`
	}](t, session, "projects_list", map[string]any{})
	if raw.AsOf.Before(before.Add(-time.Minute)) || raw.AsOf.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("as_of=%v not near now", raw.AsOf)
	}
	var h *projectHealthView
	for i := range raw.Projects {
		if raw.Projects[i].Name == "health-project" {
			h = &raw.Projects[i]
		}
	}
	if h == nil {
		t.Fatal("health-project missing")
	}
	got := h.Health
	// Waiting: triage/backlog/unstarted. The claimed task is developing, so it is in progress.
	if got.Waiting != 4 || got.InProgress != 1 || got.Blocked != 1 || got.Overdue != 1 || got.Done != 0 {
		t.Fatalf("health counts=%+v", got)
	}
	if len(got.Assignees) != 1 || got.Assignees[0].Name != "alice" || got.Assignees[0].Open != 1 {
		t.Fatalf("assignees=%+v", got.Assignees)
	}
	if len(got.TopBlocked) != 1 || got.TopBlocked[0].TaskID != blocked.ID || len(got.TopBlocked[0].BlockedBy) != 1 || got.TopBlocked[0].BlockedBy[0] != blocker.ID {
		t.Fatalf("top_blocked=%+v", got.TopBlocked)
	}
	if len(got.TopOverdue) != 1 || got.TopOverdue[0].TaskID != overdue.ID || !got.TopOverdue[0].DueAt.Equal(past) {
		t.Fatalf("top_overdue=%+v", got.TopOverdue)
	}
}

type stateClosureView struct {
	Types             []string `json:"types"`
	DefaultType       string   `json:"default_type"`
	EvidenceMaxBytes  int      `json:"evidence_max_bytes"`
	RejectsSecretLike bool     `json:"rejects_secret_like"`
	ResultLink        bool     `json:"result_link"`
	DuplicateOf       bool     `json:"duplicate_of"`
}

type stateView struct {
	Name               string            `json:"name"`
	Category           string            `json:"category"`
	Terminal           bool              `json:"terminal"`
	Claimable          bool              `json:"claimable"`
	AllowedTransitions []string          `json:"allowed_transitions"`
	Closure            *stateClosureView `json:"closure"`
}

type statesOut struct {
	States               []stateView `json:"states"`
	Categories           []string    `json:"categories"`
	AcceptedStateNames   []string    `json:"accepted_state_names"`
	LegacyStateNames     []string    `json:"legacy_state_names"`
	AwaitingDecisionFlag string      `json:"awaiting_decision_flag"`
}

func containsName(names []string, name string) bool {
	for _, value := range names {
		if value == name {
			return true
		}
	}
	return false
}

func TestTaskStatesMatchesStoreTables(t *testing.T) {
	st, _ := newMetaStore(t)
	session := newMetaSession(t, st)
	out := callTool[statesOut](t, session, "task_states", map[string]any{})
	if len(out.States) != len(store.TaskStates()) {
		t.Fatalf("states=%d want %d", len(out.States), len(store.TaskStates()))
	}
	byName := map[string]stateView{}
	for _, state := range out.States {
		byName[state.Name] = state
		if state.Category != store.TaskCategory(state.Name) {
			t.Errorf("%s category=%q want %q", state.Name, state.Category, store.TaskCategory(state.Name))
		}
		if state.Terminal != store.IsSettledState(state.Name) {
			t.Errorf("%s terminal=%v want %v", state.Name, state.Terminal, store.IsSettledState(state.Name))
		}
		if state.Claimable != store.IsClaimableState(state.Name) {
			t.Errorf("%s claimable=%v want %v", state.Name, state.Claimable, store.IsClaimableState(state.Name))
		}
		want := store.TaskTransitions[state.Name]
		if len(state.AllowedTransitions) != len(want) {
			t.Errorf("%s transitions=%v want %v", state.Name, state.AllowedTransitions, want)
		}
		for _, to := range state.AllowedTransitions {
			if !want[to] {
				t.Errorf("%s reports forbidden transition to %s", state.Name, to)
			}
		}
		for to := range want {
			if !containsName(state.AllowedTransitions, to) {
				t.Errorf("%s missing transition to %s", state.Name, to)
			}
		}
		if store.IsSettledState(state.Name) && state.Closure == nil {
			t.Errorf("%s is terminal but has no closure rule", state.Name)
		}
		if !store.IsSettledState(state.Name) && state.Closure != nil {
			t.Errorf("%s is not terminal but carries a closure rule", state.Name)
		}
	}
	if completed := byName[store.StateCompleted].Closure; completed == nil || !completed.ResultLink || completed.EvidenceMaxBytes != 1024 {
		t.Fatalf("completed closure=%+v", completed)
	}
	if duplicate := byName[store.StateDuplicate].Closure; duplicate == nil || !duplicate.DuplicateOf || !reflect.DeepEqual(duplicate.Types, []string{"superseded"}) {
		t.Fatalf("duplicate closure=%+v", duplicate)
	}
	if dropped := byName[store.StateDropped].Closure; dropped == nil || len(dropped.Types) != 3 {
		t.Fatalf("dropped closure=%+v", dropped)
	}
	if !reflect.DeepEqual(out.Categories, store.TaskCategories()) {
		t.Fatalf("categories=%v want %v", out.Categories, store.TaskCategories())
	}
	if !reflect.DeepEqual(out.LegacyStateNames, store.LegacyTaskStates()) {
		t.Fatalf("legacy names=%v want %v", out.LegacyStateNames, store.LegacyTaskStates())
	}
	if out.AwaitingDecisionFlag != store.AwaitingDecisionKey {
		t.Fatalf("awaiting flag=%q", out.AwaitingDecisionFlag)
	}
	for _, name := range store.TaskStates() {
		if !containsName(out.AcceptedStateNames, name) {
			t.Errorf("accepted names omit %s", name)
		}
	}
}

type syncOut struct {
	Provider string `json:"provider"`
	Cursor   *struct {
		Cursor         string    `json:"cursor"`
		LastActivityID string    `json:"last_activity_id"`
		UpdatedAt      time.Time `json:"updated_at"`
	} `json:"cursor"`
	LastRun           string     `json:"last_run"`
	LastRunAt         *time.Time `json:"last_run_at"`
	LastRunAgeSeconds *int64     `json:"last_run_age_seconds"`
	Recent            bool       `json:"recent"`
	Outbox            struct {
		Pending                 int        `json:"pending"`
		OldestPendingAt         *time.Time `json:"oldest_pending_at"`
		OldestPendingAgeSeconds *int64     `json:"oldest_pending_age_seconds"`
		MaxAttempts             int        `json:"max_attempts"`
		LastError               string     `json:"last_error"`
	} `json:"outbox"`
	Processed24h      map[string]int `json:"processed_24h"`
	Processed24hTotal int            `json:"processed_24h_total"`
	MirroredTasks     int            `json:"mirrored_tasks"`
	PendingReverts    int            `json:"pending_reverts"`
	WriterNote        string         `json:"writer_note"`
}

func TestSyncStatusEmptyAndSeeded(t *testing.T) {
	st, conn := newMetaStore(t)
	ctx := context.Background()
	session := newMetaSession(t, st)

	empty := callTool[syncOut](t, session, "sync_status", map[string]any{})
	if empty.LastRun != "never" || empty.LastRunAt != nil || empty.LastRunAgeSeconds != nil || empty.Recent {
		t.Fatalf("empty last run=%+v", empty)
	}
	if empty.Outbox.Pending != 0 || empty.Outbox.MaxAttempts != 0 || empty.Outbox.LastError != "" {
		t.Fatalf("empty outbox=%+v", empty.Outbox)
	}
	if empty.Processed24hTotal != 0 || empty.MirroredTasks != 0 || empty.PendingReverts != 0 {
		t.Fatalf("empty counts=%+v", empty)
	}
	for _, outcome := range []string{store.YouTrackOutcomeApplied, store.YouTrackOutcomeSkipped, store.YouTrackOutcomeRejected, store.YouTrackOutcomeDuplicate} {
		if empty.Processed24h[outcome] != 0 {
			t.Fatalf("empty outcome %s=%d", outcome, empty.Processed24h[outcome])
		}
	}
	if empty.Cursor == nil || empty.Cursor.Cursor != "" || empty.Cursor.LastActivityID != "" {
		t.Fatalf("empty cursor=%+v", empty.Cursor)
	}

	if _, err := st.AddTaskProject(ctx, "meta-sync", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "meta-sync"
	task, err := st.CreateTask(ctx, store.Task{
		Lane: "meta-sync-lane", Title: "sync fixture", Kind: "implement", Project: &project, CreatedBy: "tester",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueYouTrack(ctx, task.ID, store.YouTrackOpPriority, "2"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = conn.Exec(ctx, `UPDATE youtrack_outbox SET created_at=$2, attempts=$3, last_error=$4 WHERE task_id=$1 AND done_at IS NULL`,
		task.ID, now.Add(-90*time.Minute), 3, "upstream 401 Bearer abcdefghijklmnopqrstuvwxyz012345"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `UPDATE youtrack_cursor SET cursor='c1', last_activity_id='a1', updated_at=$1 WHERE singleton`,
		now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	activities := []struct {
		id         string
		outcome    string
		at         time.Time
		revertDone bool
	}{
		{"a-applied-1", store.YouTrackOutcomeApplied, now.Add(-2 * time.Hour), true},
		{"a-applied-2", store.YouTrackOutcomeApplied, now.Add(-3 * time.Hour), true},
		{"a-skipped", store.YouTrackOutcomeSkipped, now.Add(-4 * time.Hour), true},
		{"a-rejected", store.YouTrackOutcomeRejected, now.Add(-5 * time.Hour), false},
		{"a-duplicate", store.YouTrackOutcomeDuplicate, now.Add(-6 * time.Hour), true},
		{"a-old", store.YouTrackOutcomeApplied, now.Add(-30 * time.Hour), true},
	}
	for _, activity := range activities {
		if _, err = conn.Exec(ctx, `INSERT INTO youtrack_processed_activities(activity_id,outcome,revert_done,processed_at) VALUES($1,$2,$3,$4)`,
			activity.id, activity.outcome, activity.revertDone, activity.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{
		TaskID: task.ID, IssueID: "yt-meta-1", DisplayRef: "HK-1", Tenant: "test", Role: "mirror",
	}); err != nil {
		t.Fatal(err)
	}

	seeded := callTool[syncOut](t, session, "sync_status", map[string]any{})
	if seeded.LastRun == "never" || seeded.LastRunAt == nil || seeded.LastRunAgeSeconds == nil {
		t.Fatalf("seeded last run=%+v", seeded)
	}
	if age := *seeded.LastRunAgeSeconds; age < 550 || age > 700 {
		t.Fatalf("last run age=%d, want about 600", age)
	}
	if !seeded.Recent {
		t.Fatalf("a cursor ten minutes old is not recent")
	}
	if seeded.Outbox.Pending != 1 || seeded.Outbox.MaxAttempts != 3 {
		t.Fatalf("outbox=%+v", seeded.Outbox)
	}
	if seeded.Outbox.OldestPendingAgeSeconds == nil {
		t.Fatalf("outbox has no oldest age: %+v", seeded.Outbox)
	}
	if age := *seeded.Outbox.OldestPendingAgeSeconds; age < 5300 || age > 5500 {
		t.Fatalf("outbox age=%d, want about 5400", age)
	}
	if !strings.HasPrefix(seeded.Outbox.LastError, "[redacted: ") {
		t.Fatalf("last error leaked: %q", seeded.Outbox.LastError)
	}
	if seeded.Processed24h[store.YouTrackOutcomeApplied] != 2 || seeded.Processed24h[store.YouTrackOutcomeSkipped] != 1 ||
		seeded.Processed24h[store.YouTrackOutcomeRejected] != 1 || seeded.Processed24h[store.YouTrackOutcomeDuplicate] != 1 {
		t.Fatalf("processed outcomes=%+v", seeded.Processed24h)
	}
	if seeded.Processed24hTotal != 5 {
		t.Fatalf("processed total=%d, want 5 (the 30h row is outside the window)", seeded.Processed24hTotal)
	}
	if seeded.MirroredTasks != 1 || seeded.PendingReverts != 1 {
		t.Fatalf("mirrors=%d reverts=%d", seeded.MirroredTasks, seeded.PendingReverts)
	}
	if seeded.Cursor == nil || seeded.Cursor.Cursor != "c1" || seeded.Cursor.LastActivityID != "a1" {
		t.Fatalf("cursor=%+v", seeded.Cursor)
	}
	if seeded.WriterNote == "" {
		t.Fatal("writer note is missing")
	}
}
