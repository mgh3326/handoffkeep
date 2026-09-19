package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func testExportStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL export tests")
	}
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func exportLane(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("export-%d", time.Now().UnixNano())
}

func exportTask(t *testing.T, st *Store, lane, title string) Task {
	t.Helper()
	task, err := st.CreateTask(t.Context(), Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "export-test"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

type exportTableCount struct {
	Tasks, TaskEvents, RelayEvents, Documents int64
}

func exportTableCounts(t *testing.T, st *Store) exportTableCount {
	t.Helper()
	var c exportTableCount
	err := st.pool.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM task_events),(SELECT COUNT(*) FROM relay_events),(SELECT COUNT(*) FROM documents)`).Scan(&c.Tasks, &c.TaskEvents, &c.RelayEvents, &c.Documents)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestExportTasksSingleSnapshot commits a task claim (row update plus
// task_event insert, committed atomically by ClaimTask) and a second task
// create from other pool connections while the export transaction is held
// open between its metadata/count reads and its row scan. Every field of the
// export must then describe the one pre-change snapshot — never a mixture —
// and a second export must describe the post-change snapshot just as
// uniformly. This is the deterministic C3/T4 proof.
func TestExportTasksSingleSnapshot(t *testing.T) {
	s := testExportStore(t)
	lane := exportLane(t)
	task := exportTask(t, s, lane, "snapshot probe")
	var baseTaskEventMax int64
	if err := s.pool.QueryRow(t.Context(), `SELECT COALESCE(MAX(id),0) FROM task_events`).Scan(&baseTaskEventMax); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	exportSeam = func(ctx context.Context) {
		close(entered)
		<-release
	}
	defer func() { exportSeam = nil }()

	type exportResult struct {
		out TaskExport
		err error
	}
	done := make(chan exportResult, 1)
	go func() {
		out, err := s.ExportTasks(t.Context(), lane, "", "", 100)
		done <- exportResult{out, err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("export seam not reached")
	}
	// These commits land after the export's repeatable-read snapshot was
	// fixed but before its row scan runs.
	claimed, err := s.ClaimTask(t.Context(), task.ID, "captain")
	if err != nil {
		t.Fatal(err)
	}
	extra := exportTask(t, s, lane, "post-snapshot create")
	close(release)
	var res exportResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("export did not finish")
	}
	exportSeam = nil
	if res.err != nil {
		t.Fatal(res.err)
	}
	shown, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || len(shown.Events) != 1 {
		t.Fatalf("shown=%+v found=%v err=%v", shown, found, err)
	}
	claimEventID := shown.Events[0].ID
	out := res.out

	// The in-flight export is the pre-change snapshot: one backlog row, no
	// claimed task, the pre-claim event watermark.
	if len(out.Tasks) != 1 || out.Tasks[0].ID != task.ID || out.Tasks[0].State != "backlog" {
		t.Fatalf("tasks=%+v", out.Tasks)
	}
	if out.Counts.Total != 1 || out.Counts.ByState["backlog"] != 1 || out.Counts.ByState["claimed"] != 0 {
		t.Fatalf("counts=%+v", out.Counts)
	}
	if out.Watermark.TaskEventMaxID != baseTaskEventMax || out.Watermark.TaskEventMaxID >= claimEventID {
		t.Fatalf("watermark=%+v claimEvent=%d", out.Watermark, claimEventID)
	}
	if !out.Complete || out.Truncated || out.RowsReturned != 1 || out.SnapshotID == "" {
		t.Fatalf("export=%+v", out)
	}
	// The claim's row update and task_event are one atomic commit; the row
	// state and the event watermark can only be observed together.
	rowState, watermark := out.Tasks[0].State, out.Watermark.TaskEventMaxID
	consistent := (rowState == "backlog" && watermark < claimEventID) || (rowState == "claimed" && watermark >= claimEventID)
	if !consistent {
		t.Fatalf("row-state/event-watermark mixture: state=%s watermark=%d claimEvent=%d", rowState, watermark, claimEventID)
	}

	// A second export is an independent post-change snapshot: two rows in
	// ascending ID order, the claim visible with its event watermark.
	after, err := s.ExportTasks(t.Context(), lane, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Tasks) != 2 || after.Tasks[0].ID != task.ID || after.Tasks[0].State != "claimed" || after.Tasks[1].ID != extra.ID || after.Tasks[1].State != "backlog" {
		t.Fatalf("after tasks=%+v", after.Tasks)
	}
	if after.Counts.Total != 2 || after.Counts.ByState["backlog"] != 1 || after.Counts.ByState["claimed"] != 1 {
		t.Fatalf("after counts=%+v", after.Counts)
	}
	if after.Watermark.TaskEventMaxID < claimEventID {
		t.Fatalf("after watermark=%+v claimEvent=%d", after.Watermark, claimEventID)
	}
	rowState, watermark = after.Tasks[0].State, after.Watermark.TaskEventMaxID
	consistent = (rowState == "backlog" && watermark < claimEventID) || (rowState == "claimed" && watermark >= claimEventID)
	if !consistent {
		t.Fatalf("row-state/event-watermark mixture: state=%s watermark=%d claimEvent=%d", rowState, watermark, claimEventID)
	}
	if claimed.State != "claimed" {
		t.Fatalf("claimed=%+v", claimed)
	}
}

// TestExportTasksReadOnly proves the export writes nothing and that the
// transaction options it uses refuse writes.
func TestExportTasksReadOnly(t *testing.T) {
	s := testExportStore(t)
	lane := exportLane(t)
	exportTask(t, s, lane, "read only")

	before := exportTableCounts(t, s)
	if _, err := s.ExportTasks(t.Context(), lane, "", "", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportTasks(t.Context(), "", "", "", ExportLimitMax); err != nil {
		t.Fatal(err)
	}
	if after := exportTableCounts(t, s); after != before {
		t.Fatalf("export changed rows: before=%+v after=%+v", before, after)
	}

	tx, err := s.pool.BeginTx(t.Context(), exportTxOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `UPDATE tasks SET title='mutated' WHERE id=$1`, int64(1)); err == nil {
		t.Fatal("write inside the export transaction options succeeded")
	}
}

func TestExportTasksValidation(t *testing.T) {
	s := testExportStore(t)
	lane := exportLane(t)
	for _, args := range []struct {
		lane, state, parentLane string
		limit                   int
	}{
		{"bad lane", "", "", 10},
		{"", "bogus", "", 10},
		{"", "", "bad lane", 10},
		{"", "", "", 0},
		{"", "", "", -1},
		{"", "", "", ExportLimitMax + 1},
	} {
		if _, err := s.ExportTasks(t.Context(), args.lane, args.state, args.parentLane, args.limit); !errors.Is(err, ErrInvalidExportQuery) {
			t.Fatalf("args=%+v err=%v", args, err)
		}
	}
	out, err := s.ExportTasks(t.Context(), lane, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if out.Tasks == nil || len(out.Tasks) != 0 || out.Counts.Total != 0 || out.RowsReturned != 0 || !out.Complete || out.Truncated {
		t.Fatalf("empty export=%+v", out)
	}
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if out.IDsSHA256 != emptySHA256 || out.RowsSHA256 != emptySHA256 {
		t.Fatalf("digests=%s %s", out.IDsSHA256, out.RowsSHA256)
	}
	if out.Watermark.TaskEventMaxID < 0 || out.Watermark.RelayEventMaxID < 0 {
		t.Fatalf("watermark=%+v", out.Watermark)
	}
	for state := range taskStates {
		if _, ok := out.Counts.ByState[state]; !ok {
			t.Fatalf("by_state missing %q: %+v", state, out.Counts.ByState)
		}
	}
}
