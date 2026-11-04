package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func testPR(n int) string { return fmt.Sprintf("https://github.com/example/app/pull/%d", n) }

// testEvent builds a well-formed event of source about PR n.
func testEvent(source string, n int) sourceEvent {
	ev := sourceEvent{Source: source, At: time.Now().UTC()}
	switch source {
	case SourceMergewakePR, SourceMergewakeReview, SourceMergewakeMerge:
		ev.SourceRef, ev.PR = testPR(n), testPR(n)
	case SourceMergewakeRevert:
		ev.SourceRef, ev.RevertPR, ev.RevertedPR, ev.PR = testPR(n+1000), testPR(n+1000), testPR(n), testPR(n)
	case SourceDeploy:
		ev.SourceRef, ev.PR = deployRef("deploy/svc/20261105T000000Z", testPR(n)), testPR(n)
	case SourceSweep:
		ev.SourceRef = fmt.Sprintf("idle-since/%d", n)
	case SourcePanewireJob:
		ev.SourceRef, ev.To = fmt.Sprintf("job-%d/role/builder", n), StateDeveloping
	}
	return ev
}

// applyEvent runs one event through applySourceEventTx the way every
// producer does: under the task's row lock, in one transaction.
func applyEvent(t *testing.T, s *Store, id int64, ev sourceEvent) TaskEventResult {
	t.Helper()
	ctx := t.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		t.Fatal(err)
	}
	res, err := s.applySourceEventTx(ctx, tx, x, ev, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return res
}

func sourceRows(t *testing.T, s *Store, id int64, source string) []TaskEvent {
	t.Helper()
	var out []TaskEvent
	for _, e := range stateOf(t, s, id).Events {
		if e.Source == source {
			out = append(out, e)
		}
	}
	return out
}

func send(t *testing.T, s *Store, in TaskSourceEvent) TaskEventResult {
	t.Helper()
	res, err := s.ApplyTaskSourceEvent(t.Context(), in)
	if err != nil {
		t.Fatalf("event %+v: %v", in, err)
	}
	return res
}

func mergeEvent(id int64, pr string) TaskSourceEvent {
	return TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: id}
}

func revertEvent(id int64, reverted, revert string) TaskSourceEvent {
	return TaskSourceEvent{Source: SourceMergewakeRevert, SourceRef: revert, TaskID: id, RevertedPR: reverted}
}

func putDeploy(t *testing.T, s *Store, key, service, result string, prs any) {
	t.Helper()
	body := fmt.Sprintf(`{"schema":"deploy-record/v0","service":%q,"target":"test","result":%q,"included_prs":%s,"source":"test"}`, service, result, prsJSON(prs))
	if _, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "report", Session: "s2-test", Body: body, CreatedBy: "tester"}); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func prsJSON(prs any) string {
	switch v := prs.(type) {
	case nil:
		return "null"
	case []string:
		out := "["
		for i, pr := range v {
			if i > 0 {
				out += ","
			}
			out += fmt.Sprintf("%q", pr)
		}
		return out + "]"
	}
	panic("prs")
}

func setRefsPR(t *testing.T, s *Store, id int64, pr string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET refs=refs||jsonb_build_object('pr',$2::text) WHERE id=$1`, id, pr); err != nil {
		t.Fatal(err)
	}
}

// TestEventTableEveryRow drives every row of the event table from every
// state it lists (applied: the task moves and the row is applied) and from a
// state outside it (recorded with applied=false, state unchanged).
// hk.auto_close keeps its own tests in hierarchy_test.go; its row is covered
// by TestEventTableAutoCloseRow below.
func TestEventTableEveryRow(t *testing.T) {
	s, pool := searchTestStore(t)
	rows := []struct {
		source  string
		to      string
		from    []string
		outside []string
	}{
		{SourcePanewireJob, StateDeveloping, []string{StateResearching}, []string{StateDeveloping, StateBacklog}},
		{SourceMergewakePR, StateReviewing, []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping}, []string{StateReviewing, StateMerged}},
		{SourceMergewakeReview, StateReviewing, []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping}, []string{StateReviewing, StateDeployed}},
		{SourceMergewakeMerge, StateMerged, []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing}, []string{StateMerged, StateIcebox}},
		{SourceMergewakeRevert, StateDeveloping, []string{StateMerged, StateDeployed}, []string{StateReviewing, StateDropped}},
		{SourceDeploy, StateDeployed, []string{StateMerged}, []string{StateReviewing, StateDeployed}},
		{SourceSweep, StateIcebox, []string{StateBacklog}, []string{StateUnstarted, StateDeveloping}},
	}
	n := 1
	for _, row := range rows {
		for _, from := range row.from {
			t.Run(row.source+"/from_"+from+"/applied", func(t *testing.T) {
				x := flowTask(t, s, pool, "implement", from, true)
				n++
				ev := testEvent(row.source, n)
				res := applyEvent(t, s, x.ID, ev)
				got := stateOf(t, s, x.ID)
				if !res.Applied || got.State != row.to || res.State != row.to {
					t.Fatalf("applied=%t state=%s result=%s, want %s", res.Applied, got.State, res.State, row.to)
				}
				recorded := sourceRows(t, s, x.ID, row.source)
				if len(recorded) != 1 || !recorded[0].Applied || recorded[0].Kind != TaskEventTransition || recorded[0].From != from || recorded[0].To != row.to || recorded[0].SourceRef != ev.SourceRef || !recorded[0].Auto {
					t.Fatalf("recorded %+v", recorded)
				}
			})
		}
		for _, from := range row.outside {
			t.Run(row.source+"/from_"+from+"/recorded", func(t *testing.T) {
				x := flowTask(t, s, pool, "implement", from, true)
				n++
				res := applyEvent(t, s, x.ID, testEvent(row.source, n))
				got := stateOf(t, s, x.ID)
				if res.Applied || res.Reason == "" || got.State != from || !got.UpdatedAt.Equal(x.UpdatedAt) {
					t.Fatalf("applied=%t reason=%q state=%s, want unchanged %s", res.Applied, res.Reason, got.State, from)
				}
				recorded := sourceRows(t, s, x.ID, row.source)
				if len(recorded) != 1 || recorded[0].Applied || recorded[0].Kind != TaskEventSource || recorded[0].From != from || recorded[0].To != from {
					t.Fatalf("recorded %+v", recorded)
				}
			})
		}
	}
}

// TestEventTableAutoCloseRow covers hk.auto_close: the last child closing
// closes a parent in any listed state; a triage parent stays where it is.
func TestEventTableAutoCloseRow(t *testing.T) {
	s, pool := searchTestStore(t)
	for _, from := range []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing, StateTriage} {
		t.Run("from_"+from, func(t *testing.T) {
			parent := flowTask(t, s, pool, "implement", from, true)
			child := flowTask(t, s, pool, "implement", StateReviewing, true)
			setParent(t, s, child, parent)
			send(t, s, mergeEvent(child.ID, testPR(int(child.ID))))
			want := StateMerged
			if from == StateTriage {
				want = StateTriage
			}
			if got := stateOf(t, s, parent.ID).State; got != want {
				t.Fatalf("parent %s -> %s, want %s", from, got, want)
			}
		})
	}
}

func TestEventRule1RecordsSourceAndRefusesUnknown(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	res := send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(11), TaskID: x.ID})
	if !res.Applied || res.State != StateReviewing {
		t.Fatalf("pr event: %+v", res)
	}
	got := sourceRows(t, s, x.ID, SourceMergewakePR)
	if len(got) != 1 || got[0].SourceRef != testPR(11) || got[0].By != SourceMergewakePR {
		t.Fatalf("rows %+v", got)
	}
	for _, bad := range []TaskSourceEvent{
		{Source: "mergewake.closed", SourceRef: testPR(12), TaskID: x.ID},
		{Source: SourceMergewakeMerge, SourceRef: "not a url", TaskID: x.ID},
		{Source: SourceMergewakeMerge, SourceRef: testPR(13), PR: testPR(14), TaskID: x.ID},
		{Source: SourceMergewakeRevert, SourceRef: testPR(15), TaskID: x.ID},
	} {
		if _, err := s.ApplyTaskSourceEvent(t.Context(), bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if _, err := s.ApplyTaskSourceEvent(t.Context(), mergeEvent(999999999, testPR(16))); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestEventRule2OnlyForward(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	send(t, s, mergeEvent(x.ID, testPR(21)))
	// A late PR-opened event after the merge is recorded and moves nothing.
	res := send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(21), TaskID: x.ID})
	if res.Applied || stateOf(t, s, x.ID).State != StateMerged {
		t.Fatalf("late pr event moved the task back: %+v", res)
	}
	if rows := sourceRows(t, s, x.ID, SourceMergewakePR); len(rows) != 1 || rows[0].Applied {
		t.Fatalf("late event rows %+v", rows)
	}
	// reviewing does not move to reviewing again.
	y := flowTask(t, s, pool, "implement", StateReviewing, true)
	if res := send(t, s, TaskSourceEvent{Source: SourceMergewakeReview, SourceRef: testPR(22), TaskID: y.ID}); res.Applied {
		t.Fatalf("same-state event applied: %+v", res)
	}
}

func TestEventRule3TriageIsRecordedOnly(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateTriage, false)
	for i, source := range []string{SourceMergewakePR, SourceMergewakeReview, SourceMergewakeMerge} {
		res := send(t, s, TaskSourceEvent{Source: source, SourceRef: testPR(31 + i), TaskID: x.ID})
		if res.Applied || res.State != StateTriage {
			t.Fatalf("%s on triage: %+v", source, res)
		}
	}
	if got := stateOf(t, s, x.ID); got.State != StateTriage || got.ClaimedBy != "" {
		t.Fatalf("triage task changed: %+v", got)
	}
}

func TestEventRule4CanceledIsNotRevived(t *testing.T) {
	s, pool := searchTestStore(t)
	for _, state := range []string{StateDropped, StateDuplicate, StateIcebox} {
		x := flowTask(t, s, pool, "implement", state, true)
		res := send(t, s, mergeEvent(x.ID, testPR(int(x.ID))))
		if res.Applied || stateOf(t, s, x.ID).State != state {
			t.Fatalf("merge revived %s: %+v", state, res)
		}
		if rows := sourceRows(t, s, x.ID, SourceMergewakeMerge); len(rows) != 1 || rows[0].Applied {
			t.Fatalf("merge on %s not recorded: %+v", state, rows)
		}
	}
}

func TestEventRule5JobEndDoesNotMoveTask(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	jobID := fmt.Sprintf("job-r5-%d", x.ID)
	if _, _, err := s.ClaimJob(ctx, Job{JobID: jobID, TaskID: x.ID, Host: "test-host", CreatedBy: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=refs||jsonb_build_object('job_id',$2::text) WHERE id=$1`, x.ID, jobID); err != nil {
		t.Fatal(err)
	}
	before := len(stateOf(t, s, x.ID).Events)
	lost := "lost"
	if _, err := s.UpdateJob(ctx, jobID, JobUpdate{State: &lost}); err != nil {
		t.Fatal(err)
	}
	got := stateOf(t, s, x.ID)
	if got.State != StateDeveloping || len(got.Events) != before {
		t.Fatalf("job end moved the task: state=%s events %d -> %d", got.State, before, len(got.Events))
	}
	// With no activity past the setting, the task reaches the stalled flag.
	old := time.Now().UTC().Add(-48 * time.Hour)
	ageTask(t, s, x.ID, old)
	if !stateOf(t, s, x.ID).Stalled {
		t.Fatal("a started task with an ended job and no activity is not stalled")
	}
}

func TestEventRule6ClosedUnmergedPRMovesNothing(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateReviewing, true)
	// There is no event for a PR closed without merging: a sender that names
	// one is refused and nothing is recorded.
	if _, err := s.ApplyTaskSourceEvent(t.Context(), TaskSourceEvent{Source: "mergewake.close", SourceRef: testPR(61), TaskID: x.ID}); err == nil {
		t.Fatal("a close event was accepted")
	}
	got := stateOf(t, s, x.ID)
	if got.State != StateReviewing || len(got.Events) != len(x.Events) {
		t.Fatalf("closed PR changed the task: %+v", got)
	}
}

func TestEventRule7ClaimProvenance(t *testing.T) {
	s, pool := searchTestStore(t)
	unclaimed := flowTask(t, s, pool, "implement", StateBacklog, false)
	res := send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(71), TaskID: unclaimed.ID})
	if !res.Applied || res.Task.ClaimedBy != SourceMergewakePR || res.Task.State != StateReviewing {
		t.Fatalf("unclaimed: %+v", res.Task)
	}
	rows := sourceRows(t, s, unclaimed.ID, SourceMergewakePR)
	if len(rows) != 1 || rows[0].NoJob != testPR(71) {
		t.Fatalf("no-job reason: %+v", rows)
	}
	claimed := flowTask(t, s, pool, "implement", StateDeveloping, true)
	res = send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(72), TaskID: claimed.ID})
	if res.Task.ClaimedBy != "seed-worker" || res.Task.Refs.JobID != "seed-job" {
		t.Fatalf("existing claim changed: %+v", res.Task)
	}
	if rows := sourceRows(t, s, claimed.ID, SourceMergewakePR); len(rows) != 1 || rows[0].NoJob != "" {
		t.Fatalf("claimed rows %+v", rows)
	}
}

func TestEventRule8DeployBeforeMerge(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateReviewing, true)
	pr := testPR(81)
	setRefsPR(t, s, x.ID, pr)
	key := "deploy/svc/20261105T010000Z"
	putDeploy(t, s, key, "svc", "success", []string{pr})
	if got := stateOf(t, s, x.ID).State; got != StateReviewing {
		t.Fatalf("deploy moved a reviewing task to %s", got)
	}
	// The record addresses merged tasks only; nothing is recorded yet.
	if early := sourceRows(t, s, x.ID, SourceDeploy); len(early) != 0 {
		t.Fatalf("early deploy rows %+v", early)
	}
	res := send(t, s, mergeEvent(x.ID, pr))
	if !res.Applied || res.State != StateDeployed {
		t.Fatalf("merge after deploy: %+v", res)
	}
	rows := sourceRows(t, s, x.ID, SourceDeploy)
	if len(rows) != 1 || !rows[0].Applied || rows[0].Kind != TaskEventTransition || rows[0].From != StateMerged || rows[0].To != StateDeployed || rows[0].SourceRef != deployRef(key, pr) {
		t.Fatalf("deploy row after merge %+v", rows)
	}
	// A task that had no PR before the merge finds the record too: the merge
	// sets refs.pr.
	y := flowTask(t, s, pool, "implement", StateDeveloping, true)
	pr2 := testPR(82)
	putDeploy(t, s, "deploy/svc/20261105T020000Z", "svc", "success", []string{pr2})
	if res := send(t, s, mergeEvent(y.ID, pr2)); res.State != StateDeployed || res.Task.Refs.PR != pr2 {
		t.Fatalf("merge without prior pr: %+v", res)
	}
	// A manual merge enters merged too.
	z := flowTask(t, s, pool, "implement", StateReviewing, true)
	pr3 := testPR(83)
	putDeploy(t, s, "deploy/svc/20261105T030000Z", "svc", "success", []string{pr3})
	if got, err := s.TransitionTask(t.Context(), z.ID, StateMerged, "tester", "merged by hand", &TaskRefs{PR: pr3}, ""); err != nil || got.State != StateDeployed {
		t.Fatalf("manual merge: state=%s err=%v", got.State, err)
	}
}

func TestEventRule8DeployAfterMerge(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateReviewing, true)
	pr := testPR(84)
	if res := send(t, s, mergeEvent(x.ID, pr)); res.State != StateMerged {
		t.Fatalf("merge: %+v", res)
	}
	key := "deploy/svc/20261105T040000Z"
	putDeploy(t, s, key, "svc", "success", []string{testPR(85), pr})
	got := stateOf(t, s, x.ID)
	if got.State != StateDeployed {
		t.Fatalf("deploy after merge: %s", got.State)
	}
	if rows := sourceRows(t, s, x.ID, SourceDeploy); len(rows) != 1 || !rows[0].Applied || rows[0].SourceRef != deployRef(key, pr) {
		t.Fatalf("deploy rows %+v", rows)
	}
}

func TestEventRule9RevertUndoesMerge(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	pr, revert := testPR(91), testPR(1091)
	send(t, s, mergeEvent(child.ID, pr))
	if got := stateOf(t, s, parent.ID).State; got != StateMerged {
		t.Fatalf("parent did not auto-close: %s", got)
	}
	res := send(t, s, revertEvent(child.ID, pr, revert))
	if !res.Applied || res.State != StateDeveloping || res.Task.Refs.RevertPR != revert {
		t.Fatalf("revert: %+v", res)
	}
	// The seed job is not live, so the revert claims the task for itself.
	if res.Task.ClaimedBy != SourceMergewakeRevert || res.Task.Refs.JobID != "" {
		t.Fatalf("revert claim: %+v", res.Task)
	}
	if rows := sourceRows(t, s, child.ID, SourceMergewakeRevert); len(rows) != 1 || rows[0].NoJob != revert || rows[0].ToValue != pr {
		t.Fatalf("revert rows %+v", rows)
	}
	if got := stateOf(t, s, parent.ID).State; got != StateMerged {
		t.Fatalf("parent reopened: %s", got)
	}
	if rows := sourceRows(t, s, parent.ID, SourceMergewakeRevert); len(rows) != 1 || rows[0].Applied {
		t.Fatalf("revert not recorded on the parent: %+v", rows)
	}
	// A later deploy listing only the reverted PR moves nothing.
	putDeploy(t, s, "deploy/svc/20261105T050000Z", "svc", "success", []string{pr})
	if got := stateOf(t, s, child.ID).State; got != StateDeveloping {
		t.Fatalf("deploy of the reverted PR moved the child to %s", got)
	}
	// A merged task with a live job keeps its claim.
	live := flowTask(t, s, pool, "implement", StateReviewing, true)
	jobID := fmt.Sprintf("job-r9-%d", live.ID)
	if _, _, err := s.ClaimJob(t.Context(), Job{JobID: jobID, TaskID: live.ID, Host: "test-host", CreatedBy: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET refs=refs||jsonb_build_object('job_id',$2::text) WHERE id=$1`, live.ID, jobID); err != nil {
		t.Fatal(err)
	}
	send(t, s, mergeEvent(live.ID, testPR(92)))
	res = send(t, s, revertEvent(live.ID, testPR(92), testPR(1092)))
	if res.Task.ClaimedBy != "seed-worker" || res.Task.Refs.JobID != jobID {
		t.Fatalf("live job claim changed: %+v", res.Task)
	}
}

func TestEventRule10RevertBeforeMerge(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateReviewing, true)
	pr, revert := testPR(101), testPR(1101)
	res := send(t, s, revertEvent(x.ID, pr, revert))
	if res.Applied {
		t.Fatalf("early revert applied: %+v", res)
	}
	if rows := sourceRows(t, s, x.ID, SourceMergewakeRevert); len(rows) != 1 || rows[0].ToValue != pr || rows[0].Field != revertedPRField {
		t.Fatalf("early revert did not record the reverted PR: %+v", rows)
	}
	res = send(t, s, mergeEvent(x.ID, pr))
	if res.Applied || stateOf(t, s, x.ID).State != StateReviewing {
		t.Fatalf("a reverted PR merged the task: %+v", res)
	}
	if rows := sourceRows(t, s, x.ID, SourceMergewakeMerge); len(rows) != 1 || rows[0].Applied {
		t.Fatalf("merge rows %+v", rows)
	}
	// Relanding takes a new PR.
	if res := send(t, s, mergeEvent(x.ID, testPR(102))); !res.Applied || res.State != StateMerged {
		t.Fatalf("reland: %+v", res)
	}
}

func TestEventRule10MergeRedeliveredAfterRevert(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateReviewing, true)
	pr, revert := testPR(103), testPR(1103)
	send(t, s, mergeEvent(x.ID, pr))
	send(t, s, revertEvent(x.ID, pr, revert))
	res := send(t, s, mergeEvent(x.ID, pr))
	if res.Applied || !res.Redelivered || stateOf(t, s, x.ID).State != StateDeveloping {
		t.Fatalf("redelivered merge after revert: %+v", res)
	}
	if rows := sourceRows(t, s, x.ID, SourceMergewakeMerge); len(rows) != 1 {
		t.Fatalf("merge rows %+v", rows)
	}
}

func TestEventRedeliveryChangesNothing(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateReviewing, true)
	first := send(t, s, mergeEvent(x.ID, testPR(111)))
	before := stateOf(t, s, x.ID)
	second := send(t, s, mergeEvent(x.ID, testPR(111)))
	after := stateOf(t, s, x.ID)
	if !first.Applied || second.Applied || !second.Redelivered {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) || len(after.Events) != len(before.Events) {
		t.Fatalf("redelivery changed the task: %+v -> %+v", before, after)
	}
	if rows := sourceRows(t, s, x.ID, SourceMergewakeMerge); len(rows) != 1 || !rows[0].Applied {
		t.Fatalf("rows %+v", rows)
	}
}

func TestDeployRecordMovesOnlyListedSuccess(t *testing.T) {
	s, pool := searchTestStore(t)
	merged := func(n int) (Task, string) {
		x := flowTask(t, s, pool, "implement", StateReviewing, true)
		pr := testPR(n)
		send(t, s, mergeEvent(x.ID, pr))
		return x, pr
	}
	listed, listedPR := merged(121)
	failed, failedPR := merged(122)
	nullPRs, _ := merged(123)
	badKey, badKeyPR := merged(124)
	badBody, badBodyPR := merged(125)
	unlisted, _ := merged(126)

	putDeploy(t, s, "deploy/svc/20261105T060000Z", "svc", "success", []string{listedPR})
	putDeploy(t, s, "deploy/svc/20261105T060100Z", "svc", "failed", []string{failedPR})
	putDeploy(t, s, "deploy/svc/20261105T060200Z", "svc", "success", nil)
	putDeploy(t, s, "deploy/svc/20261105T0603Z", "svc", "success", []string{badKeyPR})
	// The body names another service than its key.
	putDeploy(t, s, "deploy/svc/20261105T060400Z", "other", "success", []string{badBodyPR})
	if _, _, err := s.PutDocument(t.Context(), Document{Key: "deploy/svc/20261105T060500Z", Kind: "report", Session: "s2-test", Body: "not json " + badBodyPR, CreatedBy: "tester"}); err != nil {
		t.Fatal(err)
	}

	if got := stateOf(t, s, listed.ID).State; got != StateDeployed {
		t.Fatalf("listed: %s", got)
	}
	for name, x := range map[string]Task{"failed": failed, "null": nullPRs, "bad key": badKey, "bad body": badBody, "unlisted": unlisted} {
		if got := stateOf(t, s, x.ID).State; got != StateMerged {
			t.Fatalf("%s moved to %s", name, got)
		}
		if rows := sourceRows(t, s, x.ID, SourceDeploy); len(rows) != 0 {
			t.Fatalf("%s recorded %+v", name, rows)
		}
	}
}

func TestJobRoleMovesTaskForwardOnly(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	bind := func(state string) (Task, string) {
		x := flowTask(t, s, pool, "implement", state, true)
		jobID := fmt.Sprintf("job-role-%d", x.ID)
		if _, _, err := s.ClaimJob(ctx, Job{JobID: jobID, TaskID: x.ID, Host: "test-host", CreatedBy: "tester"}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=refs||jsonb_build_object('job_id',$2::text) WHERE id=$1`, x.ID, jobID); err != nil {
			t.Fatal(err)
		}
		return x, jobID
	}
	role := func(r string) *string { return &r }
	researching, job := bind(StateResearching)
	if _, err := s.UpdateJob(ctx, job, JobUpdate{Role: role("builder")}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, s, researching.ID).State; got != StateDeveloping {
		t.Fatalf("builder role on an implement task: %s", got)
	}
	if rows := sourceRows(t, s, researching.ID, SourcePanewireJob); len(rows) != 1 || !rows[0].Applied {
		t.Fatalf("rows %+v", rows)
	}
	developing, job2 := bind(StateDeveloping)
	if _, err := s.UpdateJob(ctx, job2, JobUpdate{Role: role("consult")}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, s, developing.ID).State; got != StateDeveloping {
		t.Fatalf("consult role moved developing back to %s", got)
	}
	// Setting the same role again records nothing new.
	if _, err := s.UpdateJob(ctx, job2, JobUpdate{Role: role("consult")}); err != nil {
		t.Fatal(err)
	}
	if rows := sourceRows(t, s, developing.ID, SourcePanewireJob); len(rows) != 1 || rows[0].Applied {
		t.Fatalf("rows %+v", rows)
	}
}

func TestSourceEventTargetIsForwardOnly(t *testing.T) {
	for source := range sourceEventTable {
		for _, from := range TaskStates() {
			to, reason := sourceEventTarget(source, from, "")
			if reason != "" {
				continue
			}
			if source == SourceMergewakeRevert || source == SourceSweep {
				continue
			}
			if forwardRank[to] <= forwardRank[from] {
				t.Fatalf("%s moves %s back to %s", source, from, to)
			}
		}
	}
	if _, reason := sourceEventTarget("nope", StateBacklog, ""); reason == "" {
		t.Fatal("unknown source applied")
	}
}
