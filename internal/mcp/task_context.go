package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	taskContextDecisionsLimit = 50
	taskContextAttemptsLimit  = 20
	taskContextDocumentsLimit = 100
	taskContextChangesLimit   = 20
)

// taskContextFieldSources names the canonical system for each bundle section
// the task field_sources map does not already cover; every one is the hk
// Postgres record.
var taskContextFieldSources = map[string]string{
	"open_decisions":     "hk",
	"answered_decisions": "hk",
	"decision_attempts":  "hk",
	"documents":          "hk",
	"changes":            "hk",
}

// registerTaskContextTool adds the one-call task context bundle (hk 3475,
// design/2026-11-14/ai-collaboration): what a session needs around a task
// without joining task_get, decisions_history, list_documents and the change
// feed itself.
func registerTaskContextTool(s *gmcp.Server, o operatorTools) {
	gmcp.AddTool(s, &gmcp.Tool{
		Name: "task_context",
		Description: "Read one hk task and its working context in a single call: the full task_get payload under task " +
			"(state, body, blockers, jobs, comments and events previews, external_links each marked access=stored — " +
			"the link is on record, nothing verifies it is reachable), open_decisions and answered_decisions (decision " +
			"records bound to the task — a session decision names its task in grounds instead, read it through " +
			"decisions_history), decision_attempts, documents bound to the task " +
			"(metadata only; fetch bodies with get_document), and changes: the task's mergewake change rows carrying " +
			"each related PR's lifecycle (state, merged_at, reverted_by). Comment and event threads page through " +
			"task_comments and task_events.",
		Annotations: readOnlyAnnotations(),
	}, o.taskContext)
}

type taskContextInput struct {
	ID int64 `json:"id"`
}

func (o operatorTools) taskContext(ctx context.Context, _ *gmcp.CallToolRequest, in taskContextInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 {
		return nil, nil, errors.New("id must be positive")
	}
	now := time.Now().UTC()
	task, err := o.taskGetView(ctx, in.ID)
	if err != nil {
		return nil, nil, err
	}
	openDecisions, err := o.service.Store.ListDecisionRecords(ctx, store.DecisionRecordFilter{
		Status: store.DecisionStatusOpen, TaskID: in.ID, IncludeSnoozed: true, Limit: taskContextDecisionsLimit,
	}, now)
	if err != nil {
		return nil, nil, err
	}
	answeredDecisions, err := o.service.Store.ListDecisionRecords(ctx, store.DecisionRecordFilter{
		Status: store.DecisionStatusAnswered, TaskID: in.ID, Limit: taskContextDecisionsLimit, NewestFirst: true,
	}, now)
	if err != nil {
		return nil, nil, err
	}
	attempts, err := o.service.Store.ListDecisionAttempts(ctx, in.ID, taskContextAttemptsLimit)
	if err != nil {
		return nil, nil, err
	}
	documents, err := o.service.Store.ListDocuments(ctx, "", "", "", in.ID, taskContextDocumentsLimit)
	if err != nil {
		return nil, nil, err
	}
	changes, err := o.service.Store.ListChangesByTask(ctx, in.ID, taskContextChangesLimit+1)
	if err != nil {
		return nil, nil, err
	}
	changesMore := len(changes) > taskContextChangesLimit
	if changesMore {
		changes = changes[:taskContextChangesLimit]
	}
	if openDecisions == nil {
		openDecisions = []store.DecisionRecord{}
	}
	if answeredDecisions == nil {
		answeredDecisions = []store.DecisionRecord{}
	}
	if attempts == nil {
		attempts = []store.DecisionAttempt{}
	}
	if documents == nil {
		documents = []store.Document{}
	}
	if changes == nil {
		changes = []store.Change{}
	}
	return jsonResult(map[string]any{
		"task":               task,
		"open_decisions":     openDecisions,
		"answered_decisions": answeredDecisions,
		"decision_attempts":  attempts,
		"documents":          documents,
		"changes":            changes,
		"changes_more":       changesMore,
		"as_of":              now,
		"field_sources":      taskContextFieldSources,
	})
}
