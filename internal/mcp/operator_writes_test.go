package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpTaskUpdateOut struct {
	Changed      []string  `json:"changed"`
	Revision     time.Time `json:"revision"`
	Replayed     bool      `json:"replayed"`
	Actor        string    `json:"actor"`
	Channel      string    `json:"channel"`
	Author       string    `json:"author"`
	YouTrackSync string    `json:"youtrack_sync"`
}

type mcpDecisionAskOut struct {
	RequestID string `json:"request_id"`
	Duplicate bool   `json:"duplicate"`
	Replayed  bool   `json:"replayed"`
}

// writesSession serves the operator tools to an Access (ChatGPT-channel)
// principal, the path dots writes through.
func writesSession(t *testing.T, st *store.Store) *gmcp.ClientSession {
	t.Helper()
	fixture := newAccessFixture(t)
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, fixture.verifier))
	t.Cleanup(srv.Close)
	assertion := fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})
	return connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": assertion})
}

func TestTaskUpdateRevisionReplayAndAuthor(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "task update fixture")
	session := writesSession(t, st)
	const operator = "operator:operator@example.com"

	read := func() store.Task {
		t.Helper()
		x, found, err := st.GetTask(ctx, task.ID)
		if err != nil || !found {
			t.Fatalf("read task: found=%v err=%v", found, err)
		}
		return x
	}
	before := read()

	// A revision the caller did not read is a conflict: nothing is written.
	stale := before.UpdatedAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	text, isError := toolText(t, session, "task_update", map[string]any{
		"id": task.ID, "expected_updated_at": stale, "idempotency_key": "upd-stale", "priority": 1,
	})
	if !isError || !strings.Contains(text, "revision_conflict") {
		t.Fatalf("stale revision: isError=%v text=%q", isError, text)
	}
	if read().Priority != before.Priority {
		t.Fatal("a conflicting task_update wrote the priority")
	}

	expected := before.UpdatedAt.UTC().Format(time.RFC3339Nano)
	args := map[string]any{
		"id": task.ID, "expected_updated_at": expected, "idempotency_key": "upd-1", "priority": 1, "note": "raise",
	}
	first := callTool[mcpTaskUpdateOut](t, session, "task_update", args)
	if first.Replayed || len(first.Changed) != 1 || first.Changed[0] != "priority" {
		t.Fatalf("first update=%+v", first)
	}
	if first.Actor != operator || first.Channel != ChannelChatGPT || first.Author != "dots" {
		t.Fatalf("first update attribution=%+v", first)
	}
	if first.YouTrackSync != "applied" {
		t.Fatalf("youtrack_sync=%q for an unmirrored task", first.YouTrackSync)
	}
	after := read()
	if after.Priority != 1 {
		t.Fatalf("priority=%d after update", after.Priority)
	}
	// The event carries the author label, so the console shows dots.
	foundDots := false
	for _, event := range after.Events {
		if event.Field == "priority" && event.By == "dots" {
			foundDots = true
		}
	}
	if !foundDots {
		t.Fatalf("no priority event by dots in %+v", after.Events)
	}

	// The same key replays the first result even though the revision is now
	// stale, and writes nothing.
	replay := callTool[mcpTaskUpdateOut](t, session, "task_update", args)
	if !replay.Replayed || !replay.Revision.Equal(first.Revision) {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	if !read().UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("a replayed task_update changed the task")
	}
}

func TestTaskUpdateRefusesIncompleteInput(t *testing.T) {
	st := newOperatorStore(t)
	task := newOperatorTask(t, st, "task update refusal fixture")
	session := writesSession(t, st)
	expected := task.UpdatedAt.UTC().Format(time.RFC3339Nano)

	text, isError := toolText(t, session, "task_update", map[string]any{
		"id": task.ID, "expected_updated_at": expected, "priority": 1,
	})
	if !isError || !strings.Contains(text, "idempotency_key is required") {
		t.Fatalf("missing key: isError=%v text=%q", isError, text)
	}
	text, isError = toolText(t, session, "task_update", map[string]any{
		"id": task.ID, "expected_updated_at": expected, "idempotency_key": "upd-empty",
	})
	if !isError || !strings.Contains(text, "invalid_task_update") {
		t.Fatalf("no field: isError=%v text=%q", isError, text)
	}
}

func TestTaskCommentReplaysByKey(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "comment replay fixture")
	session := writesSession(t, st)
	args := map[string]any{"id": task.ID, "body": "dots note", "idempotency_key": "cmt-1"}

	first := callTool[commentOut](t, session, "task_comment", args)
	if first.Comment.Author != "dots" {
		t.Fatalf("author=%q", first.Comment.Author)
	}
	again := callTool[commentOut](t, session, "task_comment", args)
	if again.Comment.ID != first.Comment.ID {
		t.Fatalf("replay wrote comment %d, first was %d", again.Comment.ID, first.Comment.ID)
	}
	comments, err := st.ListTaskComments(ctx, task.ID, 0, store.TaskCommentListMax)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("comments=%d after a replayed key, want 1", len(comments))
	}
}

func TestTaskIntakeKeyNamesTheFiledTask(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-project", "tester"); err != nil {
		t.Fatal(err)
	}
	session := writesSession(t, st)
	first := callTool[intakeOut](t, session, "task_intake", map[string]any{
		"title": "keyed intake first title", "body": "filed once", "project": "mcp-project", "idempotency_key": "intake-1",
	})
	if !first.Created {
		t.Fatalf("first intake not created: %+v", first)
	}
	// A changed title with the same key is still the same filing.
	again := callTool[intakeOut](t, session, "task_intake", map[string]any{
		"title": "keyed intake retitled", "body": "different body", "project": "mcp-project", "idempotency_key": "intake-1",
	})
	if again.Created || again.Task.ID != first.Task.ID {
		t.Fatalf("repeat key filed again: created=%v task=%d first=%d", again.Created, again.Task.ID, first.Task.ID)
	}
	recorded, found, err := st.GetTask(ctx, first.Task.ID)
	if err != nil || !found {
		t.Fatalf("read intake: found=%v err=%v", found, err)
	}
	if recorded.CreatedBy != "dots" {
		t.Fatalf("created_by=%q", recorded.CreatedBy)
	}
}

func TestDecisionAskReplayAndOperatorOnlyRefusal(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "decision ask fixture")
	session := writesSession(t, st)
	options := []map[string]any{{"key": "A", "label": "yes"}, {"key": "B", "label": "no"}}

	operatorOnly := callTool[mcpDecisionAskOut](t, session, "decision_ask", map[string]any{
		"task_id": task.ID, "idempotency_key": "ask-1", "question": "Merge it?", "options": options,
		"default_action": "hold", "human_only": true, "answer_policy": "operator_direct",
	})
	if operatorOnly.RequestID == "" || operatorOnly.Replayed {
		t.Fatalf("decision_ask=%+v", operatorOnly)
	}
	replay := callTool[mcpDecisionAskOut](t, session, "decision_ask", map[string]any{
		"task_id": task.ID, "idempotency_key": "ask-1", "question": "Merge it?", "options": options,
		"default_action": "hold", "human_only": true, "answer_policy": "operator_direct",
	})
	if !replay.Replayed || replay.RequestID != operatorOnly.RequestID {
		t.Fatalf("replay=%+v first=%+v", replay, operatorOnly)
	}

	// dots cannot answer an operator-only request; the refusal names its code and
	// records no attempt.
	text, isError := toolText(t, session, "decision_answer", map[string]any{
		"request_id": operatorOnly.RequestID, "option": "A",
	})
	if !isError || !strings.Contains(text, "operator_only_decision") {
		t.Fatalf("operator-only answer: isError=%v text=%q", isError, text)
	}
	attempts, err := st.ListDecisionAttempts(ctx, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("refused answer recorded %d attempts", len(attempts))
	}

	// A request without the operator-only policy is answered as before.
	open := callTool[mcpDecisionAskOut](t, session, "decision_ask", map[string]any{
		"task_id": task.ID, "idempotency_key": "ask-2", "question": "Ship docs?", "options": options,
		"default_action": "hold",
	})
	answer := callTool[answerOut](t, session, "decision_answer", map[string]any{"request_id": open.RequestID, "option": "A"})
	if answer.Result != store.DecisionResultAnswered {
		t.Fatalf("answer=%+v", answer)
	}
}

func TestTaskUpdateLabelsByName(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "task update labels fixture")
	label, err := st.SaveLabel(ctx, store.Label{Name: "dots-review", EditLevel: "anyone"})
	if err != nil {
		t.Fatal(err)
	}
	session := writesSession(t, st)
	read := func() store.Task {
		t.Helper()
		x, found, err := st.GetTask(ctx, task.ID)
		if err != nil || !found {
			t.Fatalf("read task: found=%v err=%v", found, err)
		}
		return x
	}

	// An unknown label name refuses the whole call and writes nothing.
	before := read()
	text, isError := toolText(t, session, "task_update", map[string]any{
		"id": task.ID, "expected_updated_at": before.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"idempotency_key": "lbl-unknown", "add_labels": []string{"no-such-label"},
	})
	if !isError || !strings.Contains(text, store.ErrLabelNotFound.Error()) {
		t.Fatalf("unknown label: isError=%v text=%q", isError, text)
	}
	if !read().UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("a refused label update changed the task")
	}

	added := callTool[mcpTaskUpdateOut](t, session, "task_update", map[string]any{
		"id": task.ID, "expected_updated_at": before.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"idempotency_key": "lbl-1", "add_labels": []string{" dots-review "},
	})
	if len(added.Changed) != 1 || added.Changed[0] != "labels" {
		t.Fatalf("add label=%+v", added)
	}
	if !containsLabel(read().Labels, label.ID) {
		t.Fatalf("label %d not on the task: %+v", label.ID, read().Labels)
	}
}

func containsLabel(ids []int64, id int64) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// A lane change with no note is valid: the store supplies the event note.
func TestTaskUpdateLaneWithoutNote(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "task update lane fixture")
	project := "mcp-project"
	if _, err := st.CreateTask(ctx, store.Task{
		Lane: "mcp-other", Title: "lane target fixture", Kind: "implement", Project: &project, CreatedBy: "tester",
	}); err != nil {
		t.Fatal(err)
	}
	session := writesSession(t, st)
	before, found, err := st.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("read task: found=%v err=%v", found, err)
	}
	out := callTool[mcpTaskUpdateOut](t, session, "task_update", map[string]any{
		"id": task.ID, "expected_updated_at": before.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"idempotency_key": "lane-no-note", "lane": "mcp-other",
	})
	if len(out.Changed) != 1 || out.Changed[0] != "lane" {
		t.Fatalf("lane update=%+v", out)
	}
	after, _, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lane != "mcp-other" {
		t.Fatalf("lane=%q", after.Lane)
	}
}
