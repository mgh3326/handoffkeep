package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestTaskSearchReadsHitsInOneBatch: the statements a search runs do not grow
// with its hits. Before hk 5058 each hit was read with GetTask, about six
// statements apiece, so a search of 40 hits ran about 240 of them.
func TestTaskSearchReadsHitsInOneBatch(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	project := "mcp-project"
	if _, err := st.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	create := func(title string) {
		t.Helper()
		if _, err := st.CreateTask(ctx, store.Task{
			Lane: "mcp-lane", Title: title, Kind: "implement", Project: &project, CreatedBy: "tester",
		}); err != nil {
			t.Fatal(err)
		}
	}
	create("lonely quokka probe")
	for i := range 40 {
		create(fmt.Sprintf("zanzibar batch probe %d", i))
	}
	o := operatorTools{service: api.Service{Store: st}}
	// statements runs one search and returns how many results it reports and
	// how many statements it ran.
	statements := func(query string) (results, statements int) {
		t.Helper()
		qctx, count := telemetry.WithQueryCount(ctx)
		res, _, err := o.taskSearch(qctx, nil, taskSearchInput{Query: query, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		text := res.Content[0].(*gmcp.TextContent).Text
		var out struct {
			Results []taskSummary `json:"results"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		return len(out.Results), count.Count()
	}
	oneHits, oneStatements := statements("quokka")
	manyHits, manyStatements := statements("zanzibar")
	if oneHits != 1 || manyHits != 40 {
		t.Fatalf("hits: %d for quokka, %d for zanzibar; want 1 and 40", oneHits, manyHits)
	}
	if oneStatements != manyStatements {
		t.Fatalf("statements: %d for 1 hit, %d for 40 hits; a hit must not cost statements", oneStatements, manyStatements)
	}
}

// TestMCPReadToolsWithinQueryBudget: the MCP reads that run every turn stay
// within one request's statement budget over seeded data (hk 5058). Thirty
// blocking decision requests give decisions_pending and operator_attention a
// page of rows, so a per-row loop added to either would exceed the budget. The
// reads must return those rows, so the test cannot pass on empty results.
func TestMCPReadToolsWithinQueryBudget(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	const rows = 30
	options := store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "A", Recommended: true}, {Key: "B", Label: "B"}}}
	for i := range rows {
		task := newOperatorTask(t, st, fmt.Sprintf("budget decision fixture %d", i))
		if _, err := st.RecordDecisionRequest(ctx, task.ID, "tester", store.DecisionRequestInput{
			Question: fmt.Sprintf("Choose %d?", i), Options: options, DefaultAction: "hold", Block: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := newOperatorTask(t, st, "budget task_get fixture")
	o := operatorTools{service: api.Service{Store: st}}
	testdb.AssertQueryBudget(t, "task_get", func(ctx context.Context) error {
		_, err := o.taskGetView(ctx, first.ID)
		return err
	})
	testdb.AssertQueryBudget(t, "operator_attention", func(ctx context.Context) error {
		_, _, err := o.operatorAttention(ctx, nil, struct{}{})
		return err
	})
	testdb.AssertQueryBudget(t, "decisions_pending", func(ctx context.Context) error {
		_, _, err := o.decisionsPending(ctx, nil, struct{}{})
		return err
	})

	res, _, err := o.decisionsPending(ctx, nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		Decisions []json.RawMessage `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*gmcp.TextContent).Text), &pending); err != nil {
		t.Fatal(err)
	}
	if len(pending.Decisions) != rows {
		t.Fatalf("decisions_pending returned %d decisions, want the %d seeded", len(pending.Decisions), rows)
	}
	res, _, err = o.operatorAttention(ctx, nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var attention struct {
		Decisions struct {
			Count int               `json:"count"`
			Items []json.RawMessage `json:"items"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*gmcp.TextContent).Text), &attention); err != nil {
		t.Fatal(err)
	}
	if attention.Decisions.Count != rows || len(attention.Decisions.Items) != store.AttentionItems {
		t.Fatalf("operator_attention decisions count=%d items=%d, want count %d and items capped at %d",
			attention.Decisions.Count, len(attention.Decisions.Items), rows, store.AttentionItems)
	}
}
