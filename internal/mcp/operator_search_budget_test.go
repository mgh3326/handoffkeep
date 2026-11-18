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

// growTo returns a grow function that adds fixtures from build until the
// fixture holds n of them.
func growTo(build func(i int)) func(n int) {
	have := 0
	return func(n int) {
		for ; have < n; have++ {
			build(have)
		}
	}
}

// TestMCPReadStatementsFlat is the N+1 guard for the MCP reads that run every
// turn (hk 5058): each runs over one row and over thirty, and its statement
// count must not grow with the rows. After the guard, the reads must still
// return the thirty rows, so the guard cannot pass on empty results.
func TestMCPReadStatementsFlat(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	o := operatorTools{service: api.Service{Store: st}}
	const rows = 30
	options := store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "A", Recommended: true}, {Key: "B", Label: "B"}}}

	target := newOperatorTask(t, st, "budget task_get fixture")
	blockers := growTo(func(i int) {
		b := newOperatorTask(t, st, fmt.Sprintf("budget blocker %d", i))
		if _, err := st.AddTaskRelation(ctx, b.ID, store.RelationBlocks, target.ID, "tester"); err != nil {
			t.Fatal(err)
		}
	})
	testdb.AssertStatementsFlat(t, "task_get", rows, 0, blockers, func(ctx context.Context) error {
		_, err := o.taskGetView(ctx, target.ID)
		return err
	})

	decisions := growTo(func(i int) {
		task := newOperatorTask(t, st, fmt.Sprintf("budget decision fixture %d", i))
		if _, err := st.RecordDecisionRequest(ctx, task.ID, "tester", store.DecisionRequestInput{
			Question: fmt.Sprintf("Choose %d?", i), Options: options, DefaultAction: "hold", Block: true,
		}); err != nil {
			t.Fatal(err)
		}
	})
	testdb.AssertStatementsFlat(t, "decisions_pending", rows, 0, decisions, func(ctx context.Context) error {
		_, _, err := o.decisionsPending(ctx, nil, struct{}{})
		return err
	})
	testdb.AssertStatementsFlat(t, "operator_attention", rows, 0, decisions, func(ctx context.Context) error {
		_, _, err := o.operatorAttention(ctx, nil, struct{}{})
		return err
	})

	// The reads return the seeded rows, so the guard measured real work.
	view, err := o.taskGetView(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(view["blockers"].([]blockerView)); got != rows {
		t.Fatalf("task_get returned %d blockers, want %d", got, rows)
	}
	pendingRes, _, err := o.decisionsPending(ctx, nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		Decisions []json.RawMessage `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(pendingRes.Content[0].(*gmcp.TextContent).Text), &pending); err != nil {
		t.Fatal(err)
	}
	if len(pending.Decisions) != rows {
		t.Fatalf("decisions_pending returned %d decisions, want %d", len(pending.Decisions), rows)
	}
	attentionRes, _, err := o.operatorAttention(ctx, nil, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var attention struct {
		Decisions struct {
			Count int `json:"count"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(attentionRes.Content[0].(*gmcp.TextContent).Text), &attention); err != nil {
		t.Fatal(err)
	}
	if attention.Decisions.Count != rows {
		t.Fatalf("operator_attention decisions count=%d, want %d", attention.Decisions.Count, rows)
	}
}
