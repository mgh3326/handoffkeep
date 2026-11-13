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

func TestReadTaskWorkBoardsEndsReplacedJobsFromTaskEvents(t *testing.T) {
	st := schemaStore(t, "console_work_board_rebind")
	rebound := workBoardTask(t, st, TaskRefs{})
	if _, err := st.ClaimTask(t.Context(), rebound.ID, "old-worker", "old-job", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	place := func(job string, at time.Time) {
		t.Helper()
		host := "example-host"
		if _, _, err := st.RecordPlacement(t.Context(), PlacementDecision{JobID: job, Attempt: 1, TaskID: &rebound.ID, DecidedAt: at, DecidedOn: "example-reporter", Source: "forced", Ranking: "forced", StateSource: "hub", ChosenHost: &host, Outcome: "placed", Family: "openai", Model: "example-worker", Role: "worker", Candidates: []PlacementCandidate{}}); err != nil {
			t.Fatal(err)
		}
	}
	place("old-job", now)
	// Client placement times may run backward relative to an earlier session.
	place("new-job", now.Add(-time.Second))
	if _, err := st.RebindTaskJob(t.Context(), rebound.ID, "old-job", "new-job", "new-worker", "take over"); err != nil {
		t.Fatal(err)
	}

	unplaced := workBoardTask(t, st, TaskRefs{})
	if _, err := st.ClaimTask(t.Context(), unplaced.ID, "old-worker", "unplaced-old", ""); err != nil {
		t.Fatal(err)
	}
	placeForTask := func(task Task, job string) {
		t.Helper()
		host := "example-host"
		if _, _, err := st.RecordPlacement(t.Context(), PlacementDecision{JobID: job, Attempt: 1, TaskID: &task.ID, DecidedAt: now, DecidedOn: "example-reporter", Source: "forced", Ranking: "forced", StateSource: "hub", ChosenHost: &host, Outcome: "placed", Family: "openai", Model: "example-worker", Role: "worker", Candidates: []PlacementCandidate{}}); err != nil {
			t.Fatal(err)
		}
	}
	placeForTask(unplaced, "unplaced-old")
	if _, err := st.RebindTaskJob(t.Context(), unplaced.ID, "unplaced-old", "unplaced-new", "new-worker", "take over without placement"); err != nil {
		t.Fatal(err)
	}

	boards, err := st.ReadTaskWorkBoards(t.Context(), []Task{rebound, unplaced})
	if err != nil {
		t.Fatal(err)
	}
	byJob := func(board TaskWorkBoard, job string) (TaskWorkSession, bool) {
		for _, session := range board.Sessions {
			if session.JobID == job {
				return session, true
			}
		}
		return TaskWorkSession{}, false
	}
	old, ok := byJob(boards[rebound.ID], "old-job")
	if !ok || old.Active || old.EndedAt.IsZero() {
		t.Fatalf("rebound old session = %+v, found %t", old, ok)
	}
	newSession, ok := byJob(boards[rebound.ID], "new-job")
	if !ok || !newSession.Active {
		t.Fatalf("pending replacement session = %+v, found %t", newSession, ok)
	}
	unplacedOld, ok := byJob(boards[unplaced.ID], "unplaced-old")
	if !ok || unplacedOld.Active || unplacedOld.EndedAt.IsZero() {
		t.Fatalf("session replaced without a placement = %+v, found %t", unplacedOld, ok)
	}

	reclaimed := workBoardTask(t, st, TaskRefs{})
	if _, err := st.ClaimTask(t.Context(), reclaimed.ID, "first-worker", "first-claim", ""); err != nil {
		t.Fatal(err)
	}
	placeForTask(reclaimed, "first-claim")
	if _, err := st.RebindTaskJob(t.Context(), reclaimed.ID, "first-claim", "second-claim", "second-worker", "take over"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), reclaimed.ID, StateUnstarted, "operator", "release for a fresh claim", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimTask(t.Context(), reclaimed.ID, "first-worker-again", "first-claim", ""); err != nil {
		t.Fatal(err)
	}

	reboundBack := workBoardTask(t, st, TaskRefs{})
	if _, err := st.ClaimTask(t.Context(), reboundBack.ID, "first-worker", "rebound-a", ""); err != nil {
		t.Fatal(err)
	}
	placeForTask(reboundBack, "rebound-a")
	placeForTask(reboundBack, "rebound-b")
	if _, err := st.RebindTaskJob(t.Context(), reboundBack.ID, "rebound-a", "rebound-b", "second-worker", "take over"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RebindTaskJob(t.Context(), reboundBack.ID, "rebound-b", "rebound-a", "first-worker-again", "return to first job"); err != nil {
		t.Fatal(err)
	}

	pending := workBoardTask(t, st, TaskRefs{JobID: "unclaimed-job"})
	placeForTask(pending, "unclaimed-job")
	pendingBoards, err := st.ReadTaskWorkBoards(t.Context(), []Task{pending})
	if err != nil {
		t.Fatal(err)
	}
	pendingSession, ok := byJob(pendingBoards[pending.ID], "unclaimed-job")
	if !ok || !pendingSession.Active {
		t.Fatalf("unreplaced placement = %+v, found %t", pendingSession, ok)
	}

	claimBoards, err := st.ReadTaskWorkBoards(t.Context(), []Task{reclaimed, reboundBack})
	if err != nil {
		t.Fatal(err)
	}
	firstClaim, ok := byJob(claimBoards[reclaimed.ID], "first-claim")
	if !ok || !firstClaim.Active {
		t.Fatalf("session after release and re-claim = %+v, found %t", firstClaim, ok)
	}
	firstAgain, ok := byJob(claimBoards[reboundBack.ID], "rebound-a")
	if !ok || !firstAgain.Active {
		t.Fatalf("session after A→B→A rebind = %+v, found %t", firstAgain, ok)
	}
	second, ok := byJob(claimBoards[reboundBack.ID], "rebound-b")
	if !ok || second.Active || second.EndedAt.IsZero() {
		t.Fatalf("session after replacing B with A = %+v, found %t", second, ok)
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

	if view := (TaskWorkBoard{}); view.PrimaryJob() != "" {
		t.Fatal(view)
	}
}

// endReplacedSessions must end only jobs replaced and not later rebound, each
// at its last unbind time. A job rebound back (A->B->A) or re-claimed after a
// release stays Active.
func TestEndReplacedSessionsFollowsBindingHistory(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	cases := []struct {
		name   string
		events []taskJobBindingEvent
		ended  map[string]time.Time
	}{
		{
			name:   "rebound back A->B->A keeps A active and ends B",
			events: []taskJobBindingEvent{{JobID: "A", At: at(1)}, {JobID: "B", FromJob: "A", At: at(2)}, {JobID: "A", FromJob: "B", At: at(3)}},
			ended:  map[string]time.Time{"B": at(3)},
		},
		{
			name:   "release then re-claim A keeps A active",
			events: []taskJobBindingEvent{{JobID: "A", At: at(1)}, {JobID: "B", FromJob: "A", At: at(2)}, {JobID: "B", At: at(3)}, {JobID: "A", At: at(4)}},
			ended:  map[string]time.Time{"B": at(4)},
		},
		{
			name:   "reopen clears job then re-claim A keeps A active",
			events: []taskJobBindingEvent{{JobID: "A", At: at(1)}, {At: at(2)}, {JobID: "A", At: at(3)}},
			ended:  map[string]time.Time{},
		},
		{
			name:   "A->B->C ends A at its replacement and B at its replacement",
			events: []taskJobBindingEvent{{JobID: "A", At: at(1)}, {JobID: "B", FromJob: "A", At: at(2)}, {JobID: "C", FromJob: "B", At: at(3)}},
			ended:  map[string]time.Time{"A": at(2), "B": at(3)},
		},
		{
			name:   "placed but never claimed stays active",
			events: []taskJobBindingEvent{{JobID: "A", At: at(1)}},
			ended:  map[string]time.Time{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b TaskWorkBoard
			for _, id := range []string{"A", "B", "C"} {
				b.Sessions = append(b.Sessions, TaskWorkSession{JobID: id, Active: true})
			}
			b.endReplacedSessions(tc.events)
			for _, s := range b.Sessions {
				want, wantEnded := tc.ended[s.JobID]
				if s.Active == wantEnded {
					t.Fatalf("%s: active=%v, want ended=%v (%+v)", s.JobID, s.Active, wantEnded, s)
				}
				if wantEnded && !s.EndedAt.Equal(want) {
					t.Fatalf("%s: EndedAt=%v, want %v", s.JobID, s.EndedAt, want)
				}
			}
		})
	}
}
