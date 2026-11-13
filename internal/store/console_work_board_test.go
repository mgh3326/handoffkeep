package store

import (
	"fmt"
	"testing"
	"time"
)

func workBoardTask(t *testing.T, st *Store, refs TaskRefs) Task {
	t.Helper()
	task, err := st.CreateTask(t.Context(), Task{Lane: fmt.Sprintf("work-board-%d", time.Now().UnixNano()), Title: "work board", Kind: "implement", Refs: refs, CreatedBy: "work-board-test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// The overlay is derived from the task, placement_decisions and jobs only
// (hk 4425): sessions come from placed decisions, a terminal job ends its
// session, PR and head come from the task's refs.
func TestReadTaskWorkBoards(t *testing.T) {
	st := schemaStore(t, "console_work_board")
	owner := workBoardTask(t, st, TaskRefs{PR: "https://github.com/example/project/pull/42", HeadSHA: "abc1234"})
	reviewTask := workBoardTask(t, st, TaskRefs{})
	at := time.Now().UTC().Add(-time.Minute)
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
	for _, state := range []string{"spawned", "completed"} {
		if _, err := st.UpdateJob(t.Context(), "example-review", JobUpdate{State: &state}); err != nil {
			t.Fatal(err)
		}
	}
	boards, err := st.ReadTaskWorkBoards(t.Context(), []Task{owner, reviewTask})
	if err != nil {
		t.Fatal(err)
	}
	board := boards[owner.ID]
	if len(board.Sessions) != 2 || board.PrimaryJob() != "example-lead" || len(boards[reviewTask.ID].Sessions) != 0 {
		t.Fatal(boards)
	}
	if board.PR != owner.Refs.PR || board.HeadSHA != "abc1234" {
		t.Fatalf("refs %+v", board)
	}
	for _, session := range board.Sessions {
		switch session.JobID {
		case "example-review":
			if session.Active || session.EndedAt.IsZero() {
				t.Fatal("completed reviewer still active", session)
			}
		case "example-lead":
			if !session.Active || session.Host != "example-a" || session.Model != "example-builder" || session.Role != "lead" {
				t.Fatal("lead session", session)
			}
		}
	}
	// A task with no placement shows only its own refs.
	if b := boards[reviewTask.ID]; b.PR != "" || b.PrimaryJob() != "" {
		t.Fatal(b)
	}
	if empty, err := st.ReadTaskWorkBoards(t.Context(), nil); err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
}

func TestTaskWorkBoardPlacementOrder(t *testing.T) {
	host := "h"
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var b TaskWorkBoard
	b.applyPlacement(PlacementDecision{JobID: "j", ChosenHost: &host, Role: "worker", Model: "m1", DecidedAt: at})
	b.applyJob(Job{JobID: "j", State: "completed", UpdatedAt: at.Add(time.Minute)})
	// An older placement of the same job does not replace the newer one.
	b.applyPlacement(PlacementDecision{JobID: "j", ChosenHost: &host, Role: "worker", Model: "m0", DecidedAt: at.Add(-time.Minute)})
	if len(b.Sessions) != 1 || b.Sessions[0].Model != "m1" || b.Sessions[0].Active || b.PrimaryJob() != "j" {
		t.Fatalf("%+v", b)
	}
	// A running job keeps its session active.
	b.applyPlacement(PlacementDecision{JobID: "k", ChosenHost: &host, Role: "lead", DecidedAt: at.Add(2 * time.Minute)})
	b.applyJob(Job{JobID: "k", State: "spawned", UpdatedAt: at.Add(3 * time.Minute)})
	if !b.Sessions[1].Active || b.PrimaryJob() != "k" {
		t.Fatalf("%+v", b)
	}
}
