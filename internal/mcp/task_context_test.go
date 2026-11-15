package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type taskContextOut struct {
	Task struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		State string `json:"state"`
		Refs  struct {
			PR string `json:"pr"`
		} `json:"refs"`
		ExternalLinks []struct {
			Kind   string `json:"kind"`
			Access string `json:"access"`
		} `json:"external_links"`
	} `json:"task"`
	OpenDecisions     []store.DecisionRecord  `json:"open_decisions"`
	AnsweredDecisions []store.DecisionRecord  `json:"answered_decisions"`
	DecisionAttempts  []store.DecisionAttempt `json:"decision_attempts"`
	Documents         []store.Document        `json:"documents"`
	Changes           []store.Change          `json:"changes"`
	ChangesMore       bool                    `json:"changes_more"`
	AsOf              time.Time               `json:"as_of"`
	FieldSources      map[string]string       `json:"field_sources"`
}

func TestTaskContextBundle(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	if _, err := st.AddTaskProject(ctx, "mcp-ctx-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "mcp-ctx-project"
	create := func(title string) store.Task {
		t.Helper()
		task, err := st.CreateTask(ctx, store.Task{
			Lane: "mcp-ctx-lane", Title: title, Kind: "implement", CreatedBy: "tester", Project: &project,
			Refs: store.TaskRefs{PR: "https://github.com/mgh3326/handoffkeep/pull/41"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	subject := create("MCP context subject")
	other := create("MCP context other")

	// An answered request on the subject: record it first because a newer
	// request supersedes the one before it.
	second, err := st.RecordDecisionRequest(ctx, subject.ID, "tester", store.DecisionRequestInput{
		Question: "pick a colour", DefaultAction: "blue",
		Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{
			{Key: "A", Label: "red"}, {Key: "B", Label: "blue"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveDecisionRequest(ctx, subject.ID, "tester", store.DecisionResolveInput{
		RequestID: second.Request.ID, Kind: store.DecisionRequestAnswered, Option: "B", Responder: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	// An open structured request on the subject and one on another task; only
	// the subject's may appear.
	if _, err := st.RecordDecisionRequest(ctx, subject.ID, "tester", store.DecisionRequestInput{
		Question: "ship it?", DefaultAction: "hold",
		Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{
			{Key: "A", Label: "ship", Recommended: true}, {Key: "B", Label: "hold"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordDecisionRequest(ctx, other.ID, "tester", store.DecisionRequestInput{
		Question: "other task question", DefaultAction: "hold",
		Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{
			{Key: "A", Label: "yes"}, {Key: "B", Label: "no"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// A document bound to the subject.
	if _, _, err := st.PutDocument(ctx, store.Document{
		Key: "task-ctx/note", Kind: "note", Body: "bound", CreatedBy: "tester", TaskID: &subject.ID,
	}); err != nil {
		t.Fatal(err)
	}
	// A mergewake change linked to the subject.
	if _, err := st.ApplyTaskSourceEvent(ctx, store.TaskSourceEvent{
		Source: store.SourceMergewakePR, SourceRef: "https://github.com/mgh3326/handoffkeep/pull/41", TaskID: subject.ID,
	}); err != nil {
		t.Fatal(err)
	}

	srv := newOperatorServer(t, st, api.Tokens{"tester": "bearer-secret"})
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	out := callTool[taskContextOut](t, session, "task_context", map[string]any{"id": subject.ID})
	if out.Task.ID != subject.ID || out.Task.Title != subject.Title {
		t.Fatalf("task=%+v", out.Task)
	}
	if len(out.Task.ExternalLinks) == 0 {
		t.Fatal("no external_links")
	}
	for _, link := range out.Task.ExternalLinks {
		if link.Access != "stored" {
			t.Fatalf("link %s access=%q want stored", link.Kind, link.Access)
		}
	}
	if len(out.OpenDecisions) != 1 || out.OpenDecisions[0].Question != "ship it?" {
		t.Fatalf("open_decisions=%+v", out.OpenDecisions)
	}
	if out.OpenDecisions[0].TaskID == nil || *out.OpenDecisions[0].TaskID != subject.ID {
		t.Fatalf("open decision task_id=%v", out.OpenDecisions[0].TaskID)
	}
	if len(out.AnsweredDecisions) != 1 || out.AnsweredDecisions[0].Question != "pick a colour" || out.AnsweredDecisions[0].Answer == "" {
		t.Fatalf("answered_decisions=%+v", out.AnsweredDecisions)
	}
	if len(out.DecisionAttempts) == 0 {
		t.Fatal("the resolve wrote no decision_attempts row")
	}
	if len(out.Documents) != 1 || out.Documents[0].Key != "task-ctx/note" {
		t.Fatalf("documents=%+v", out.Documents)
	}
	if len(out.Changes) != 1 || out.Changes[0].PR != "https://github.com/mgh3326/handoffkeep/pull/41" {
		t.Fatalf("changes=%+v", out.Changes)
	}
	for _, section := range []string{"open_decisions", "answered_decisions", "decision_attempts", "documents", "changes"} {
		if out.FieldSources[section] != "hk" {
			t.Fatalf("field_sources[%s]=%q", section, out.FieldSources[section])
		}
	}
	if out.AsOf.IsZero() {
		t.Fatal("as_of unset")
	}
}
