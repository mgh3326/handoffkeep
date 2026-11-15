package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// `youtrack links status` (hk 4640, design §9 S5): the read model behind
// GET /v1/yt/links/status. Each test names the operator question it answers.

// The status shows a prepared links op that the mode holds, with its reason,
// keeps the tenant's owned and unowned rows visible while the mode is off,
// and narrows both lists to one task.
func TestYTLinksStatusShowsHeldOpsAndRows(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_status", YTLinksOn, 3)
	ctx := t.Context()
	lo, hi, other := ids[0], ids[1], ids[2]
	if changed, err := s.AddTaskRelation(ctx, hi, RelationRelated, lo, "tester"); err != nil || !changed {
		t.Fatalf("add: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s)
	ops := ytlfOps(t, s, lo)
	if len(ops) == 0 || ops[len(ops)-1].State != ytwrite.StatePrepared {
		t.Fatalf("ops %+v, want a prepared links op for %d", ops, lo)
	}
	opID := ops[len(ops)-1].ID

	// Mode on and the target bound: the op is admitted, so nothing is held.
	st, err := s.YTLinksStatusOf(ctx, lo, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Open || st.Mode != YTLinksOn || st.Tenant != ytcTenant || st.TaskID != lo {
		t.Fatalf("gate in status %+v", st)
	}
	if st.HeldCount != 0 || len(st.Held) != 0 {
		t.Fatalf("admitted op reported held: %+v", st.Held)
	}

	// A row this writer owns and one unowned row seen from its issue.
	ytFeedExec(t, s, `INSERT INTO yt_link_owned(tenant, link_type, from_issue, to_issue, writer_task_id, target_task_id, kind, intended_op_id, intended_at)
		VALUES ($1, 'relates', $2, $3, $4, $5, 'related', $6, now())`, ytcTenant, ytfIssue(lo), ytfIssue(hi), lo, hi, opID)
	ytFeedExec(t, s, `INSERT INTO yt_link_unowned(tenant, issue_id, link_type, side, other_issue, first_seen_op_id, last_seen_op_id, first_seen_at, last_seen_at)
		VALUES ($1, $2, 'relates', 'relates to', 'PERSON-9', $3, $3, now(), now())`, ytcTenant, ytfIssue(lo), opID)

	// Mode off: the prepared op is held as mode_off, and the rows stay visible.
	s.SetYTLinksMode(YTLinksOff, ytcTenant)
	st, err = s.YTLinksStatusOf(ctx, lo, 100)
	if err != nil {
		t.Fatal(err)
	}
	if st.Open || st.Mode != YTLinksOff || st.Tenant != ytcTenant {
		t.Fatalf("gate with mode off in status %+v", st)
	}
	wantHeld := []YTLinkHeldOp{{OpID: opID, TaskID: lo, YTIssueID: ytfIssue(lo), State: string(ytwrite.StatePrepared), Reason: YTLinksHeldModeOff}}
	if st.HeldCount != 1 || !reflect.DeepEqual(heldKeys(st.Held), heldKeys(wantHeld)) {
		t.Fatalf("held with mode off = %+v (count %d), want %+v", st.Held, st.HeldCount, wantHeld)
	}
	if st.OwnedOpen != 1 || len(st.Owned) != 1 || st.Owned[0].Edge != "related" || st.Owned[0].WriterTaskID != lo || st.Owned[0].TargetTaskID != hi {
		t.Fatalf("owned rows with mode off = %+v (count %d)", st.Owned, st.OwnedOpen)
	}
	if st.UnownedOpen != 1 || len(st.Unowned) != 1 || st.Unowned[0].OtherIssue != "PERSON-9" {
		t.Fatalf("unowned rows = %+v (count %d)", st.Unowned, st.UnownedOpen)
	}

	// The other task has no rows, and the all-tasks view counts the same op.
	st, err = s.YTLinksStatusOf(ctx, other, 100)
	if err != nil {
		t.Fatal(err)
	}
	if st.OwnedOpen != 0 || st.UnownedOpen != 0 || st.HeldCount != 0 {
		t.Fatalf("status of task %d = %+v, want nothing", other, st)
	}
	all, err := s.YTLinksStatusOf(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The higher endpoint's first snapshot (empty desired set) is a prepared op
	// too, so every prepared links op is held while the mode is off.
	prepared := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind='issue_links' AND state='prepared' AND tenant=$1`, ytcTenant)
	if all.TaskID != 0 || all.HeldCount != prepared || prepared < 1 || len(all.Held) != 1 || all.OwnedOpen != 1 {
		t.Fatalf("all-tasks status %+v: counts must cover every row, lists stop at the limit", all)
	}
	if all.Sweep.Complete != (all.Sweep.DoneGeneration >= all.Sweep.Generation) {
		t.Fatalf("sweep complete flag disagrees with the generations: %+v", all.Sweep)
	}
}

// An unknown task is ErrTaskNotFound, which the API answers 404 not_found.
func TestYTLinksStatusUnknownTask(t *testing.T) {
	s := schemaStore(t, "yt_links_status_unknown")
	if _, err := s.YTLinksStatusOf(t.Context(), 987654321, 100); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task: err=%v, want ErrTaskNotFound", err)
	}
}

// heldKeys drops the timestamps, which differ between reads.
func heldKeys(held []YTLinkHeldOp) []YTLinkHeldOp {
	out := make([]YTLinkHeldOp, 0, len(held))
	for _, h := range held {
		out = append(out, YTLinkHeldOp{OpID: h.OpID, TaskID: h.TaskID, YTIssueID: h.YTIssueID, State: h.State, Reason: h.Reason})
	}
	return out
}
