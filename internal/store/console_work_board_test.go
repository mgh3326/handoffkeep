package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestReadTaskWorkBoards(t *testing.T) {
	_, url := testdb.Schema(t, "console_work_board")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	owner := linearTask(t, st, TaskRefs{})
	reviewTask := linearTask(t, st, TaskRefs{})
	at := time.Now().UTC().Add(-time.Minute)
	// The reader must work before Linear is enabled or its reconciler runs.
	for _, p := range []struct{ job, host, role, model string }{{"example-lead", "example-a", "lead", "example-builder"}, {"example-review", "example-b", "review", "example-reviewer"}} {
		host := p.host
		_, _, err := st.RecordPlacement(t.Context(), PlacementDecision{JobID: p.job, Attempt: 1, TaskID: &owner.ID, DecidedAt: at, DecidedOn: "example-reporter", Source: "forced", Ranking: "forced", StateSource: "hub", ChosenHost: &host, Outcome: "placed", Family: "openai", Model: p.model, Role: p.role, Candidates: []PlacementCandidate{}})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A reviewer job's own task differs from its placement's explicit owner.
	if _, _, err := st.ClaimJob(t.Context(), Job{JobID: "example-review", TaskID: &reviewTask.ID, Host: "example-b", CreatedBy: "example-node"}); err != nil {
		t.Fatal(err)
	}
	state := "spawned"
	if _, err := st.UpdateJob(t.Context(), "example-review", JobUpdate{State: &state}); err != nil {
		t.Fatal(err)
	}
	state = "completed"
	if _, err := st.UpdateJob(t.Context(), "example-review", JobUpdate{State: &state}); err != nil {
		t.Fatal(err)
	}
	boards, err := st.ReadTaskWorkBoards(t.Context(), []Task{owner, reviewTask})
	if err != nil {
		t.Fatal(err)
	}
	board := boards[owner.ID]
	if len(board.Sessions) != 2 || board.PrimaryJob() != "example-lead" || len(boards[reviewTask.ID].Sessions) != 0 {
		t.Fatal(boards)
	}
	for _, session := range board.Sessions {
		if session.JobID == "example-review" && session.Active {
			t.Fatal("completed reviewer became active", session)
		}
	}
	var count int
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM linear_board_tasks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("read wrote projections: count=%d err=%v", count, err)
	}
	// Keep PR and retained sessions even after the immutable placement ages out.
	board.Task.Refs.PR = "https://github.com/example/project/pull/42"
	board.Sessions = append(board.Sessions, LinearBoardSession{JobID: "example-consult", Host: "example-c", Role: "consult", Model: "example-consultant", Active: true, At: at})
	raw, err := json.Marshal(board)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `INSERT INTO linear_board_tasks(task_id,status) VALUES($1,$2::jsonb)`, owner.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `DELETE FROM placement_decisions`); err != nil {
		t.Fatal(err)
	}
	boards, err = st.ReadTaskWorkBoards(t.Context(), []Task{owner})
	if err != nil || len(boards[owner.ID].Sessions) != 3 || boards[owner.ID].Task.Refs.PR != board.Task.Refs.PR {
		t.Fatalf("boards=%+v err=%v", boards, err)
	}
	if empty, err := st.ReadTaskWorkBoards(t.Context(), nil); err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
}
