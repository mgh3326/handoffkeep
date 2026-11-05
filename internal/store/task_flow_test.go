package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// flowTask files a task through CreateTask (priority and project set, so it
// passes the triage gate) and forces its state in SQL. withLinkage also gives
// it a claimant and job link, so moves among started states are unguarded and
// an entry into started passes the linkage guard.
func flowTask(t *testing.T, s *Store, pool *pgxpool.Pool, kind, state string, withLinkage bool) Task {
	t.Helper()
	ctx := context.Background()
	x, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "flow"), Title: "flow " + state, Kind: kind, Priority: 5, CreatedBy: "tester", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if withLinkage {
		_, err = pool.Exec(ctx, `UPDATE tasks SET state=$2, claimed_by='seed-worker', refs=refs||'{"job_id":"seed-job"}'::jsonb WHERE id=$1`, x.ID, state)
	} else {
		_, err = pool.Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, x.ID, state)
	}
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found {
		t.Fatalf("seed found=%t err=%v", found, err)
	}
	return got
}

func lastEvent(t *testing.T, s *Store, id int64) TaskEvent {
	t.Helper()
	x, found, err := s.GetTask(context.Background(), id)
	if err != nil || !found || len(x.Events) == 0 {
		t.Fatalf("events found=%t n=%d err=%v", found, len(x.Events), err)
	}
	return x.Events[len(x.Events)-1]
}

func stateOf(t *testing.T, s *Store, id int64) Task {
	t.Helper()
	x, found, err := s.GetTask(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("get %d found=%t err=%v", id, found, err)
	}
	return x
}

// TestEveryManualTransitionEdge runs every ordered pair of states through
// TransitionTaskWith: an edge in TaskTransitions succeeds from a non-settled
// state, every other pair is a conflict that leaves the task untouched, and a
// settled task leaves only through reopen.
func TestEveryManualTransitionEdge(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	other := flowTask(t, s, pool, "implement", "backlog", false)
	for _, from := range TaskStates() {
		for _, to := range TaskStates() {
			if from == to {
				continue
			}
			from, to := from, to
			t.Run(from+"_to_"+to, func(t *testing.T) {
				x := flowTask(t, s, pool, "implement", from, true)
				in := TransitionInput{To: to, By: "tester", Note: "why"}
				if to == StateDuplicate {
					in.DuplicateOf = other.ID
				}
				got, err := s.TransitionTaskWith(ctx, x.ID, in)
				allowed := TaskTransitions[from][to] && !IsSettledState(from) && to != StateCompleted
				if allowed {
					if err != nil {
						t.Fatalf("%s -> %s refused: %v", from, to, err)
					}
					if got.State != to || got.Category != TaskCategory(to) {
						t.Fatalf("state=%q category=%q, want %q", got.State, got.Category, to)
					}
					if ev := lastEvent(t, s, x.ID); ev.From != from || ev.To != to || ev.Kind != TaskEventTransition {
						t.Fatalf("event=%+v", ev)
					}
					return
				}
				if !errors.Is(err, ErrTaskConflict) {
					t.Fatalf("%s -> %s: err=%v, want ErrTaskConflict", from, to, err)
				}
				if after := stateOf(t, s, x.ID); after.State != from || len(after.Events) != len(x.Events) {
					t.Fatalf("refused transition changed the task: %+v", after)
				}
			})
		}
	}
}

func TestReopenReturnsToUnstarted(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for _, from := range []string{"merged", "deployed", "dropped", "icebox"} {
		x := flowTask(t, s, pool, "fix", from, true)
		got, err := s.ReopenTask(ctx, x.ID, "tester", "reverted")
		if err != nil {
			t.Fatalf("reopen %s: %v", from, err)
		}
		if got.State != StateUnstarted || got.ClaimedBy != "" || got.Refs.JobID != "" || got.CompletedAt != nil || got.CanceledAt != nil {
			t.Fatalf("reopen %s: %+v", from, got)
		}
	}
	for _, from := range []string{"backlog", "unstarted", "developing", "triage"} {
		x := flowTask(t, s, pool, "fix", from, true)
		if _, err := s.ReopenTask(ctx, x.ID, "tester", "reason"); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("reopen %s: err=%v, want ErrTaskConflict", from, err)
		}
	}
	// A duplicate reopens too, and leaving duplicate archives its relation.
	target := flowTask(t, s, pool, "fix", "backlog", false)
	dup := flowTask(t, s, pool, "fix", "backlog", false)
	if _, err := s.TransitionTaskWith(ctx, dup.ID, TransitionInput{To: "duplicate", By: "tester", Note: "same", DuplicateOf: target.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenTask(ctx, dup.ID, "tester", "not a duplicate after all"); err != nil {
		t.Fatal(err)
	}
	if live, err := hasLiveRelationTx(ctx, pool, dup.ID, RelationDuplicateOf); err != nil || live {
		t.Fatalf("duplicate_of still live after reopen: live=%t err=%v", live, err)
	}
}

func TestTriageGate(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	// Priority above 0 is required.
	x := flowTask(t, s, pool, "implement", "triage", false)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET priority=0 WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"backlog", "unstarted"} {
		if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "", nil, ""); !errors.Is(err, ErrTriageGate) {
			t.Fatalf("priority 0 -> %s: err=%v, want ErrTriageGate", to, err)
		}
	}
	// A project is required.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET priority=3, project=NULL WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, x.ID, "backlog", "tester", "", nil, ""); !errors.Is(err, ErrTriageGate) {
		t.Fatalf("no project: err=%v, want ErrTriageGate", err)
	}
	// With both, the exit works and records triaged_at.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET project=$2 WHERE id=$1`, x.ID, testProjectName); err != nil {
		t.Fatal(err)
	}
	got, err := s.TransitionTask(ctx, x.ID, "unstarted", "tester", "accepted", nil, "")
	if err != nil || got.State != StateUnstarted || got.TriagedAt == nil {
		t.Fatalf("gate open: %+v err=%v", got, err)
	}
	// Declining needs only a reason.
	y := flowTask(t, s, pool, "implement", "triage", false)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET priority=0, project=NULL WHERE id=$1`, y.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, y.ID, "dropped", "tester", "", nil, ""); err == nil {
		t.Fatal("declining without a reason must be refused")
	}
	if got, err := s.TransitionTask(ctx, y.ID, "dropped", "tester", "not ours", nil, ""); err != nil || got.State != StateDropped {
		t.Fatalf("decline: %+v err=%v", got, err)
	}
	// There is no edge from triage to a started state.
	z := flowTask(t, s, pool, "implement", "triage", true)
	for _, to := range []string{"researching", "developing", "reviewing", "merged"} {
		if _, err := s.TransitionTask(ctx, z.ID, to, "tester", "x", nil, "no job"); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("triage -> %s: err=%v, want ErrTaskConflict", to, err)
		}
	}
}

func TestEnteringStartedNeedsLinkage(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for _, to := range []string{"researching", "developing"} {
		x := flowTask(t, s, pool, "implement", "unstarted", false)
		if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "start", nil, ""); !errors.Is(err, ErrTaskJobRequired) {
			t.Fatalf("%s without linkage: err=%v, want ErrTaskJobRequired", to, err)
		}
		// A job id alone is not enough without a claimant.
		if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "start", &TaskRefs{JobID: "job-x"}, ""); !errors.Is(err, ErrTaskJobRequired) {
			t.Fatalf("%s with job but no claimant: err=%v, want ErrTaskJobRequired", to, err)
		}
		got, err := s.TransitionTask(ctx, x.ID, to, "tester", "start", nil, "operator started it by hand")
		if err != nil || got.State != to || got.StartedAt == nil {
			t.Fatalf("%s with a no-job reason: %+v err=%v", to, got, err)
		}
		if ev := lastEvent(t, s, x.ID); ev.NoJob != "operator started it by hand" {
			t.Fatalf("no-job reason not recorded: %+v", ev)
		}
	}
	// With claimant and job already on the row the entry is free, and so are
	// moves within started.
	linked := flowTask(t, s, pool, "implement", "backlog", true)
	if got, err := s.TransitionTask(ctx, linked.ID, "developing", "tester", "go", nil, ""); err != nil || got.State != StateDeveloping {
		t.Fatalf("linked entry: %+v err=%v", got, err)
	}
	if got, err := s.TransitionTask(ctx, linked.ID, "reviewing", "tester", "pr", nil, ""); err != nil || got.State != StateReviewing {
		t.Fatalf("developing -> reviewing: %+v err=%v", got, err)
	}
}

func TestNoManualEdgeEntersDeployed(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for _, from := range TaskStates() {
		if from == "deployed" {
			continue
		}
		x := flowTask(t, s, pool, "implement", from, true)
		if _, err := s.TransitionTask(ctx, x.ID, "deployed", "tester", "ship it", nil, "x"); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("%s -> deployed: err=%v, want ErrTaskConflict", from, err)
		}
	}
}

func TestReviewingToMergedStoresPR(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	x := flowTask(t, s, pool, "implement", "reviewing", true)
	pr := "https://github.com/example/project/pull/42"
	got, err := s.TransitionTask(ctx, x.ID, "merged", "tester", "merged by hand", &TaskRefs{PR: pr}, "")
	if err != nil || got.State != StateMerged || got.Refs.PR != pr || got.CompletedAt == nil {
		t.Fatalf("merge: %+v err=%v", got, err)
	}
	if reread := stateOf(t, s, x.ID); reread.Refs.PR != pr || reread.Category != "completed" {
		t.Fatalf("persisted: %+v", reread)
	}
	// A task with no PR can still complete.
	y := flowTask(t, s, pool, "implement", "reviewing", true)
	if got, err := s.TransitionTask(ctx, y.ID, "merged", "tester", "no PR", nil, ""); err != nil || got.State != StateMerged || got.Refs.PR != "" {
		t.Fatalf("merge without PR: %+v err=%v", got, err)
	}
	// merged is not reachable from researching or developing.
	for _, from := range []string{"researching", "developing"} {
		z := flowTask(t, s, pool, "implement", from, true)
		if _, err := s.TransitionTask(ctx, z.ID, "merged", "tester", "skip", nil, ""); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("%s -> merged: err=%v, want ErrTaskConflict", from, err)
		}
	}
}

func TestClaimSetsSubstateFromKind(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for _, state := range []string{"backlog", "unstarted"} {
		for kind, want := range map[string]string{"implement": "developing", "fix": "developing", "ops": "developing", "verify": "researching", "decide": "researching"} {
			x := flowTask(t, s, pool, kind, state, false)
			got, err := s.ClaimTask(ctx, x.ID, "worker-1", "job-"+kind+"-"+state, "")
			if err != nil {
				t.Fatalf("claim %s/%s: %v", state, kind, err)
			}
			if got.State != want || got.ClaimedBy != "worker-1" || got.Refs.JobID != "job-"+kind+"-"+state || got.StartedAt == nil {
				t.Fatalf("claim %s/%s: %+v, want state %s", state, kind, got, want)
			}
			if ev := lastEvent(t, s, x.ID); ev.From != state || ev.To != want {
				t.Fatalf("claim event: %+v", ev)
			}
			// The identical claim again is a no-op: same row, no second event.
			n := len(stateOf(t, s, x.ID).Events)
			if again, err := s.ClaimTask(ctx, x.ID, "worker-1", "job-"+kind+"-"+state, ""); err != nil || again.State != want || len(stateOf(t, s, x.ID).Events) != n {
				t.Fatalf("replayed claim: %+v err=%v", again, err)
			}
		}
	}
	// A bare claim still needs a job id or a no-job reason.
	bare := flowTask(t, s, pool, "implement", "backlog", false)
	if _, err := s.ClaimTask(ctx, bare.ID, "worker-1", "", ""); !errors.Is(err, ErrTaskJobRequired) {
		t.Fatalf("bare claim: err=%v, want ErrTaskJobRequired", err)
	}
	// Only backlog and unstarted are claimable.
	for _, state := range []string{"triage", "researching", "developing", "reviewing", "merged", "deployed", "dropped", "duplicate", "icebox"} {
		x := flowTask(t, s, pool, "implement", state, false)
		if _, err := s.ClaimTask(ctx, x.ID, "worker-2", "job-other-"+state, ""); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("claim of %s: err=%v, want ErrTaskConflict", state, err)
		}
	}
}

func TestNextTaskServesBacklogAndUnstarted(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	lane := relaneLane(t, "next")
	mk := func(state string, priority int) Task {
		t.Helper()
		x, err := s.CreateTask(ctx, Task{Lane: lane, Title: "next " + state, Kind: "implement", Priority: priority, CreatedBy: "tester", Project: projectPtr(testProjectName)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, x.ID, state); err != nil {
			t.Fatal(err)
		}
		return x
	}
	backlog, unstarted := mk("backlog", 1), mk("unstarted", 9)
	for _, state := range []string{"triage", "icebox", "reviewing", "merged"} {
		mk(state, 100)
	}
	flagged := mk("backlog", 50)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET awaiting_decision=true WHERE id=$1`, flagged.ID); err != nil {
		t.Fatal(err)
	}
	first, err := s.NextTask(ctx, lane, "worker-1", "job-next-1", "")
	if err != nil || first.ID != unstarted.ID || first.State != StateDeveloping {
		t.Fatalf("first next: %+v err=%v, want task %d developing", first, err, unstarted.ID)
	}
	second, err := s.NextTask(ctx, lane, "worker-1", "job-next-2", "")
	if err != nil || second.ID != backlog.ID {
		t.Fatalf("second next: %+v err=%v, want task %d", second, err, backlog.ID)
	}
	if _, err := s.NextTask(ctx, lane, "worker-1", "job-next-3", ""); !errors.Is(err, ErrQueueEmpty) {
		t.Fatalf("third next: err=%v, want ErrQueueEmpty (flagged, triage and icebox are not served)", err)
	}
}

func TestAwaitingDecisionFlag(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	x := flowTask(t, s, pool, "implement", "developing", true)
	// Setting it requires a question.
	if _, err := s.TransitionTask(ctx, x.ID, "needs_decision", "tester", "  ", nil, ""); err == nil {
		t.Fatal("needs_decision without a question must be refused")
	}
	got, err := s.TransitionTask(ctx, x.ID, "needs_decision", "tester", "Which interface?", nil, "")
	if err != nil || !got.AwaitingDecision || got.State != StateDeveloping {
		t.Fatalf("set flag: %+v err=%v", got, err)
	}
	if ev := lastEvent(t, s, x.ID); ev.Kind != TaskEventAwaiting || ev.ToValue != "true" || ev.Note != "Which interface?" || !ev.SetsAwaiting() {
		t.Fatalf("awaiting event: %+v", ev)
	}
	if _, err := s.TransitionTask(ctx, x.ID, "needs_decision", "tester", "again", nil, ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("setting an already set flag: err=%v, want ErrTaskConflict", err)
	}
	// A flagged task cannot be claimed or moved on; it can be parked or dropped.
	if _, err := s.TransitionTask(ctx, x.ID, "reviewing", "tester", "pr", nil, ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("flagged developing -> reviewing: err=%v, want ErrTaskConflict", err)
	}
	backlog := flowTask(t, s, pool, "implement", "backlog", false)
	if _, err := s.TransitionTask(ctx, backlog.ID, "needs_decision", "tester", "which?", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, backlog.ID, "worker-1", "job-flagged", ""); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "decision") {
		t.Fatalf("claim of a flagged task: err=%v", err)
	}
	// Answering clears it and keeps the work state.
	cleared, err := s.ClearAwaitingDecision(ctx, x.ID, "operator:o", "use A")
	if err != nil || cleared.AwaitingDecision || cleared.State != StateDeveloping {
		t.Fatalf("clear: %+v err=%v", cleared, err)
	}
	if ev := lastEvent(t, s, x.ID); ev.Kind != TaskEventAwaiting || ev.ToValue != "false" {
		t.Fatalf("clear event: %+v", ev)
	}
	if _, err := s.ClearAwaitingDecision(ctx, x.ID, "operator:o", "again"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("clearing an unset flag: err=%v, want ErrTaskConflict", err)
	}
	// The backlog task, now claimable after being answered through the old name.
	if got, err := s.TransitionTask(ctx, backlog.ID, "claimed", "operator:o", "answered", nil, "answered"); err != nil || got.AwaitingDecision || got.State != StateBacklog {
		t.Fatalf("old-name answer: %+v err=%v", got, err)
	}
	if got, err := s.ClaimTask(ctx, backlog.ID, "worker-1", "job-flagged", ""); err != nil || got.State != StateDeveloping {
		t.Fatalf("claim after the answer: %+v err=%v", got, err)
	}
	// Parking a flagged task clears the flag.
	parked := flowTask(t, s, pool, "implement", "unstarted", true)
	if _, err := s.TransitionTask(ctx, parked.ID, "needs_decision", "tester", "park?", nil, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TransitionTask(ctx, parked.ID, "icebox", "tester", "parked", nil, ""); err != nil || got.AwaitingDecision || got.State != StateIcebox {
		t.Fatalf("park: %+v err=%v", got, err)
	}
	// A settled task cannot start waiting.
	merged := flowTask(t, s, pool, "implement", "merged", true)
	if _, err := s.TransitionTask(ctx, merged.ID, "needs_decision", "tester", "q", nil, ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("flag on a merged task: err=%v, want ErrTaskConflict", err)
	}
}

// TestOldNamesOnInput is the "Old names on input" table at the store.
func TestOldNamesOnInput(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	// in_progress and claimed: the spawn rule's sub-state.
	for kind, want := range map[string]string{"implement": "developing", "verify": "researching"} {
		for _, old := range []string{"in_progress", "claimed"} {
			x := flowTask(t, s, pool, kind, "backlog", false)
			got, err := s.TransitionTask(ctx, x.ID, old, "tester", "start", nil, "by hand")
			if err != nil || got.State != want {
				t.Fatalf("%s on %s task: %+v err=%v, want %s", old, kind, got, err, want)
			}
			// Already started: the old name is an idempotent no-op, not a conflict.
			n := len(stateOf(t, s, x.ID).Events)
			if again, err := s.TransitionTask(ctx, x.ID, old, "tester", "again", nil, ""); err != nil || again.State != want || len(stateOf(t, s, x.ID).Events) != n {
				t.Fatalf("%s when started: %+v err=%v", old, again, err)
			}
		}
	}
	// in_progress out of review is the fix-needed edge.
	rv := flowTask(t, s, pool, "implement", "reviewing", true)
	if got, err := s.TransitionTask(ctx, rv.ID, "in_progress", "tester", "fix needed", nil, ""); err != nil || got.State != StateDeveloping {
		t.Fatalf("in_progress from reviewing: %+v err=%v", got, err)
	}
	// verifying and join: reviewing.
	for _, old := range []string{"verifying", "join"} {
		x := flowTask(t, s, pool, "implement", "developing", true)
		if got, err := s.TransitionTask(ctx, x.ID, old, "tester", "pr", nil, ""); err != nil || got.State != StateReviewing {
			t.Fatalf("%s: %+v err=%v", old, got, err)
		}
	}
	// hold: icebox.
	hold := flowTask(t, s, pool, "implement", "backlog", false)
	if got, err := s.TransitionTask(ctx, hold.ID, "hold", "tester", "park", nil, ""); err != nil || got.State != StateIcebox {
		t.Fatalf("hold: %+v err=%v", got, err)
	}
	// reopened: the reopen path (reason required, settled source).
	done := flowTask(t, s, pool, "implement", "merged", true)
	if _, err := s.TransitionTask(ctx, done.ID, "reopened", "tester", "", nil, ""); err == nil {
		t.Fatal("reopened without a reason must be refused")
	}
	if got, err := s.TransitionTask(ctx, done.ID, "reopened", "tester", "regressed", nil, ""); err != nil || got.State != StateUnstarted {
		t.Fatalf("reopened: %+v err=%v", got, err)
	}
	// Unknown names stay invalid.
	if _, err := s.TransitionTask(ctx, hold.ID, "nonsense", "tester", "x", nil, ""); err == nil {
		t.Fatal("an unknown state must be refused")
	}
}

// TestOldNamesInFilters covers list and export: old names select what they map to.
func TestOldNamesInFilters(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	lane := relaneLane(t, "filter")
	mk := func(state string, flagged bool) int64 {
		t.Helper()
		x, err := s.CreateTask(ctx, Task{Lane: lane, Title: "filter " + state, Kind: "implement", Priority: 1, CreatedBy: "tester", Project: projectPtr(testProjectName)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE tasks SET state=$2, awaiting_decision=$3 WHERE id=$1`, x.ID, state, flagged); err != nil {
			t.Fatal(err)
		}
		return x.ID
	}
	ids := map[string]int64{"researching": mk("researching", false), "developing": mk("developing", false), "reviewing": mk("reviewing", false), "icebox": mk("icebox", false), "unstarted": mk("unstarted", false), "flagged": mk("backlog", true)}
	want := map[string][]int64{
		"in_progress":    {ids["researching"], ids["developing"]},
		"claimed":        {ids["researching"], ids["developing"]},
		"verifying":      {ids["reviewing"]},
		"join":           {ids["reviewing"]},
		"hold":           {ids["icebox"]},
		"reopened":       {ids["unstarted"]},
		"needs_decision": {ids["flagged"]},
		"icebox":         {ids["icebox"]},
	}
	for name, wantIDs := range want {
		got, err := s.ListTasks(ctx, lane, name, "", nil, nil, 100)
		if err != nil {
			t.Fatalf("list %s: %v", name, err)
		}
		have := map[int64]bool{}
		for _, x := range got {
			have[x.ID] = true
		}
		if len(have) != len(wantIDs) {
			t.Fatalf("list state=%s: got %v, want %v", name, have, wantIDs)
		}
		for _, id := range wantIDs {
			if !have[id] {
				t.Fatalf("list state=%s: got %v, want %v", name, have, wantIDs)
			}
		}
		exp, err := s.ExportTasks(ctx, lane, name, "", nil, 100)
		if err != nil || exp.Counts.Total != len(wantIDs) {
			t.Fatalf("export state=%s: total=%d err=%v, want %d", name, exp.Counts.Total, err, len(wantIDs))
		}
	}
	exp, err := s.ExportTasks(ctx, lane, "", "", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range TaskStates() {
		if _, ok := exp.Counts.ByState[state]; !ok {
			t.Errorf("by_state lacks %q", state)
		}
	}
	for _, old := range LegacyTaskStates() {
		if _, ok := exp.Counts.ByState[old]; ok {
			t.Errorf("by_state carries the old name %q", old)
		}
	}
	if _, err := s.ListTasks(ctx, lane, "nonsense", "", nil, nil, 10); err == nil {
		t.Error("an unknown state filter must be refused")
	}
}

func TestLifecycleTimestamps(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	x := flowTask(t, s, pool, "implement", "backlog", false)
	if x.StartedAt != nil || x.CompletedAt != nil || x.CanceledAt != nil {
		t.Fatalf("fresh task: %+v", x)
	}
	got, err := s.ClaimTask(ctx, x.ID, "worker-1", "job-life", "")
	if err != nil || got.StartedAt == nil {
		t.Fatalf("claim: %+v err=%v", got, err)
	}
	startedAt := *got.StartedAt
	for _, to := range []string{"reviewing", "merged"} {
		if got, err = s.TransitionTask(ctx, x.ID, to, "tester", "x", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if got.CompletedAt == nil || got.CanceledAt != nil || got.StartedAt == nil || !got.StartedAt.Equal(startedAt) {
		t.Fatalf("merged: %+v", got)
	}
	if got, err = s.ReopenTask(ctx, x.ID, "tester", "again"); err != nil || got.CompletedAt != nil {
		t.Fatalf("reopen clears completed_at: %+v err=%v", got, err)
	}
	if got, err = s.TransitionTask(ctx, x.ID, "dropped", "tester", "never mind", nil, ""); err != nil || got.CanceledAt == nil || got.CompletedAt != nil {
		t.Fatalf("dropped: %+v err=%v", got, err)
	}
}
