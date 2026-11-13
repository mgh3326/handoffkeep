package store

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Held-op recovery (hk 4491): a task's binding is the admission gate of its
// comment, field and move ops, and an op whose binding is gone must not hold
// the task's lane for ever (invariant I5).

const ytrOtherTenant = "other.youtrack.cloud"

func TestYTHeldClassOf(t *testing.T) {
	op := ytHeldOp{id: 1, tenant: ytcTenant, target: "2-1"}
	bound := func(tenant, issue string) *YTTaskBinding {
		return &YTTaskBinding{TaskID: 1, Tenant: tenant, IssueID: issue}
	}
	for _, tc := range []struct {
		name string
		b    *YTTaskBinding
		side YTAuthoritySide
		want ytHeldClass
	}{
		{"no binding", nil, YTAuthorityHK, ytHeldBinding},
		{"binding to another issue", bound(ytcTenant, "2-9"), YTAuthorityHK, ytHeldBindingChanged},
		{"binding on another tenant", bound(ytrOtherTenant, "2-1"), YTAuthorityHK, ytHeldOtherTenant},
		{"another tenant, another issue", bound(ytrOtherTenant, "2-9"), YTAuthorityHK, ytHeldOtherTenant},
		{"matching binding", bound(ytcTenant, "2-1"), YTAuthorityHK, ytHeldReady},
		{"no binding, youtrack canonical", nil, YTAuthorityYouTrack, ytHeldAuthorityYouTrack},
		{"changed binding, youtrack canonical", bound(ytcTenant, "2-9"), YTAuthorityYouTrack, ytHeldAuthorityYouTrack},
		{"other tenant, youtrack canonical", bound(ytrOtherTenant, "2-1"), YTAuthorityYouTrack, ytHeldOtherTenant},
	} {
		if got := ytHeldClassOf(op, tc.b, tc.side); got != tc.want {
			t.Errorf("%s: class = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// ytrPrepare prepares an op of the given kind and principal for the task's
// lane, created at the given time.
func ytrPrepare(t *testing.T, s *Store, task int64, kind, principal, tenant, target, key string, at time.Time) ytwrite.Op {
	t.Helper()
	op, _, err := s.YTWriteLedger().Prepare(t.Context(), at, ytwrite.NewOp{Principal: principal, Kind: kind, Key: key, PayloadSHA: key,
		Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: strconv.FormatInt(task, 10), YTTargetID: target, Tenant: tenant, Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func ytrComment(t *testing.T, s *Store, task int64, target, key string, at time.Time) ytwrite.Op {
	t.Helper()
	return ytrPrepare(t, s, task, ytwrite.KindCommentAppend, YTCommentPrincipal, ytcTenant, target, key, at)
}

func ytrState(t *testing.T, s *Store, op ytwrite.Op) ytwrite.Op {
	t.Helper()
	got, err := s.YTWriteLedger().ByID(t.Context(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func ytrRecover(t *testing.T, s *Store, at time.Time) YTRecovery {
	t.Helper()
	got, err := s.RecoverHeldYTOps(t.Context(), at, YTHeldRecoveryGrace)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func ytrDeleteBinding(t *testing.T, s *Store, task int64) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `DELETE FROM yt_task_bindings WHERE task_id=$1`, task); err != nil {
		t.Fatal(err)
	}
}

func ytrAlertTexts(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `SELECT text FROM notification_outbox WHERE kind=$1 ORDER BY id`, OutboxKindYTWriteOp)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		out = append(out, text)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A prepared op whose task lost its binding is rejected once it has been held
// for the grace, with a recorded reason, and the op behind it on the task's
// lane is dispatched. Before the grace nothing is touched; a second pass has
// nothing to do.
func TestRecoverHeldYTOpsReleasesTheLane(t *testing.T) {
	s := schemaStore(t, "ytrec_lane")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	first := ytrComment(t, s, task, "2-"+strconv.FormatInt(task, 10), "lane-first", t0)

	// The binding is removed by hand and the task is bound to another issue;
	// the first op is held and the second, admitted, waits behind it.
	ytrDeleteBinding(t, s, task)
	other := ytbBinding(task)
	other.IssueID, other.IssueReadable = "2-9001", "HK-9001"
	ytbBind(t, s, other)
	second := ytrComment(t, s, task, other.IssueID, "lane-second", t0.Add(time.Minute))

	ledger := s.YTTenantLedger(ytcTenant)
	now := time.Now().UTC().Add(time.Minute)
	if got, out, err := ledger.Claim(ctx, now, second.ID); err != nil || out != ytwrite.ClaimBlocked {
		t.Fatalf("claim behind a held op = %s %+v %v, want blocked", out, got, err)
	}

	if got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace-time.Second)); got.Rejected != 0 {
		t.Fatalf("pass before the grace rejected %+v", got)
	}
	got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace))
	if got.Rejected != 1 || !slices.Equal(got.OpIDs, []int64{first.ID}) {
		t.Fatalf("pass after the grace = %+v, want op %d rejected", got, first.ID)
	}
	op := ytrState(t, s, first)
	if op.State != ytwrite.StateRejected || op.Reason != ytwrite.ReasonBindingChanged || op.YTTargetID != first.YTTargetID || op.Tenant != first.Tenant {
		t.Fatalf("rejected op = %+v, want state rejected, reason binding_changed, target and tenant unchanged", op)
	}
	if got, out, err := ledger.Claim(ctx, now, second.ID); err != nil || out != ytwrite.ClaimClaimed || got.State != ytwrite.StateDispatched {
		t.Fatalf("claim after recovery = %s %+v %v, want claimed", out, got, err)
	}
	if got := ytrRecover(t, s, t0.Add(2*YTHeldRecoveryGrace)); got.Rejected != 0 || got.Kept != 0 {
		t.Fatalf("second pass = %+v, want nothing to do", got)
	}
}

// With no binding at all the reason is binding_removed. An op that is
// admitted, an op of another kind or principal and an op whose kind's mode is
// off are not touched.
func TestRecoverHeldYTOpsScope(t *testing.T) {
	s := schemaStore(t, "ytrec_scope")
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	s.SetYTFieldsMode(YTFieldsOff, "")
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	at := t0.Add(YTHeldRecoveryGrace)

	gone := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(gone))
	held := ytrComment(t, s, gone, "2-"+strconv.FormatInt(gone, 10), "scope-held", t0)
	ytrDeleteBinding(t, s, gone)

	kept := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(kept))
	admitted := ytrComment(t, s, kept, "2-"+strconv.FormatInt(kept, 10), "scope-admitted", t0)

	unbound := ytcTask(t, s, ytbProject)
	doc := ytrPrepare(t, s, unbound, ytwrite.KindCommentAppend, ytDocCommentPrincipal, ytcTenant, "2-1", "scope-doc", t0)
	fields := ytrPrepare(t, s, unbound, ytwrite.KindIssueFields, YTFieldsPrincipal, ytcTenant, "2-1", "scope-fields", t0)

	got := ytrRecover(t, s, at)
	if got.Rejected != 1 || !slices.Equal(got.OpIDs, []int64{held.ID}) {
		t.Fatalf("pass = %+v, want only op %d rejected", got, held.ID)
	}
	if op := ytrState(t, s, held); op.State != ytwrite.StateRejected || op.Reason != ytwrite.ReasonBindingRemoved {
		t.Fatalf("op with no binding = %+v, want rejected binding_removed", op)
	}
	for name, op := range map[string]ytwrite.Op{"admitted": admitted, "document comment": doc, "fields, mode off": fields} {
		if got := ytrState(t, s, op); got.State != ytwrite.StatePrepared {
			t.Errorf("%s op = %+v, want still prepared", name, got)
		}
	}

	// Turning fields mode on makes the fields op a candidate, and the task has
	// no binding.
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	if got := ytrRecover(t, s, at); got.Rejected != 1 || !slices.Equal(got.OpIDs, []int64{fields.ID}) {
		t.Fatalf("pass with fields mode on = %+v, want op %d rejected", got, fields.ID)
	}
	if op := ytrState(t, s, fields); op.State != ytwrite.StateRejected || op.Reason != ytwrite.ReasonBindingRemoved {
		t.Fatalf("fields op = %+v, want rejected binding_removed", op)
	}
	if got := ytrState(t, s, doc); got.State != ytwrite.StatePrepared {
		t.Fatalf("document comment = %+v, want still prepared", got)
	}
}

// A stop is not an error: with the comments mode in canary, nothing is closed,
// and the mode on again releases the op by the same rule.
func TestRecoverHeldYTOpsLeavesAStopAlone(t *testing.T) {
	s := schemaStore(t, "ytrec_mode")
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	op := ytrComment(t, s, task, "2-"+strconv.FormatInt(task, 10), "mode-held", t0)
	ytrDeleteBinding(t, s, task)

	s.SetYTCommentsMode(YTCommentsCanary)
	if got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace)); got.Rejected != 0 || got.Kept != 0 {
		t.Fatalf("pass in canary mode = %+v, want it to read nothing", got)
	}
	s.SetYTCommentsMode(YTCommentsOff)
	if got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace)); got.Rejected != 0 {
		t.Fatalf("pass with the mode off = %+v", got)
	}
	if got := ytrState(t, s, op); got.State != ytwrite.StatePrepared {
		t.Fatalf("op during a stop = %+v, want prepared", got)
	}
	s.SetYTCommentsMode(YTCommentsOn)
	if got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace)); got.Rejected != 1 {
		t.Fatalf("pass with the mode back on = %+v, want the op rejected", got)
	}
}

// A binding on another tenant is kept: that tenant's worker may own the op.
func TestRecoverHeldYTOpsKeepsAnotherTenantsBinding(t *testing.T) {
	s := schemaStore(t, "ytrec_tenant")
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	op := ytrComment(t, s, task, "2-"+strconv.FormatInt(task, 10), "tenant-held", t0)
	ytrDeleteBinding(t, s, task)
	elsewhere := ytbBinding(task)
	elsewhere.Tenant = ytrOtherTenant
	ytbBind(t, s, elsewhere)

	got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace))
	if got.Rejected != 0 || got.Kept != 1 {
		t.Fatalf("pass = %+v, want the op kept", got)
	}
	if op := ytrState(t, s, op); op.State != ytwrite.StatePrepared {
		t.Fatalf("op = %+v, want prepared", op)
	}
	if _, err := s.EnqueueYTWriteOpAlerts(t.Context(), t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if texts := ytrAlertTexts(t, s); len(texts) != 1 || !strings.Contains(texts[0], "(held_other_tenant: 1)") {
		t.Fatalf("alerts = %q, want one notice labelled held_other_tenant", texts)
	}
}

// While comments are canonical in YouTrack the op is the only delivery of the
// comment: nothing is rejected and the alert says so. Under hk the same op is
// rejected.
func TestRecoverHeldYTOpsKeepsWhatYouTrackOwns(t *testing.T) {
	s := schemaStore(t, "ytrec_auth")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack, "test", "recovery"); err != nil {
		t.Fatal(err)
	}
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	op := ytrComment(t, s, task, "2-"+strconv.FormatInt(task, 10), "auth-held", t0)
	ytrDeleteBinding(t, s, task)

	got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace))
	if got.Rejected != 0 || got.Kept != 1 {
		t.Fatalf("pass under youtrack authority = %+v, want the op kept", got)
	}
	if op := ytrState(t, s, op); op.State != ytwrite.StatePrepared {
		t.Fatalf("op = %+v, want prepared", op)
	}
	if _, err := s.EnqueueYTWriteOpAlerts(ctx, t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if texts := ytrAlertTexts(t, s); len(texts) != 1 || !strings.Contains(texts[0], "(held_authority_youtrack: 1)") {
		t.Fatalf("alerts = %q, want one notice labelled held_authority_youtrack", texts)
	}

	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "test", "back"); err != nil {
		t.Fatal(err)
	}
	if got := ytrRecover(t, s, t0.Add(YTHeldRecoveryGrace)); got.Rejected != 1 {
		t.Fatalf("pass under hk authority = %+v, want the op rejected", got)
	}
}

// The candidate scan is stale by the time an op is decided. The decision reads
// the op, its binding and the authority again under the task's lane lock, so
// a change between the two is honoured: the real switch to youtrack refuses
// while an op is pending, which makes a raw row update the strongest form of
// it.
func TestRecoverHeldYTOpsDecidesFromTheRowsAsTheyAreNow(t *testing.T) {
	s := schemaStore(t, "ytrec_stale")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	t0 := time.Now().UTC().Add(-3 * time.Hour)
	at := t0.Add(YTHeldRecoveryGrace)
	scan := func(op ytwrite.Op) ytHeldOp {
		t.Helper()
		cands, err := s.heldYTCandidates(ctx, s.ytHeldKindsOn(), at.Add(-YTHeldRecoveryGrace), time.Unix(0, 0), 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cands {
			if c.id == op.ID {
				return c
			}
		}
		t.Fatalf("op %d is not a candidate: %+v", op.ID, cands)
		return ytHeldOp{}
	}
	held := func(name string) (int64, ytwrite.Op) {
		t.Helper()
		task := ytcTask(t, s, ytbProject)
		ytbBind(t, s, ytbBinding(task))
		op := ytrComment(t, s, task, "2-"+strconv.FormatInt(task, 10), name, t0)
		ytrDeleteBinding(t, s, task)
		return task, op
	}

	// Comments became canonical in YouTrack after the scan.
	_, op := held("stale-authority")
	c := scan(op)
	if _, err := s.pool.Exec(ctx, `UPDATE yt_authority SET authority='youtrack' WHERE kind='comments'`); err != nil {
		t.Fatal(err)
	}
	if rejected, err := s.recoverHeldYTOp(ctx, at, c); err != nil || rejected {
		t.Fatalf("decision after the switch = %v, %v, want the op kept", rejected, err)
	}
	if got := ytrState(t, s, op); got.State != ytwrite.StatePrepared {
		t.Fatalf("op = %+v, want prepared", got)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_authority SET authority='hk' WHERE kind='comments'`); err != nil {
		t.Fatal(err)
	}

	// The binding was restored after the scan: the op is admitted again.
	task, op := held("stale-binding")
	c = scan(op)
	ytbBind(t, s, ytbBinding(task))
	if rejected, err := s.recoverHeldYTOp(ctx, at, c); err != nil || rejected {
		t.Fatalf("decision after the binding returned = %v, %v, want the op kept", rejected, err)
	}
	if got := ytrState(t, s, op); got.State != ytwrite.StatePrepared {
		t.Fatalf("op = %+v, want prepared", got)
	}

	// Someone else resolved the op after the scan.
	_, op = held("stale-resolved")
	c = scan(op)
	if _, err := s.YTWriteLedger().Reject(ctx, at, op.ID, ytwrite.ReasonOperatorNotDone, "by hand"); err != nil {
		t.Fatal(err)
	}
	if rejected, err := s.recoverHeldYTOp(ctx, at, c); err != nil || rejected {
		t.Fatalf("decision on a resolved op = %v, %v, want it left alone", rejected, err)
	}
	if got := ytrState(t, s, op); got.Reason != ytwrite.ReasonOperatorNotDone {
		t.Fatalf("op = %+v, want the operator's reason kept", got)
	}

	// A decision that still holds is made.
	_, op = held("stale-held")
	if rejected, err := s.recoverHeldYTOp(ctx, at, scan(op)); err != nil || !rejected {
		t.Fatalf("decision on a held op = %v, %v, want rejected", rejected, err)
	}
}

// The stuck notice counts a held op under why it is held, and an op its gate
// admits, or of a kind this pass does not read, under "prepared".
func TestYTWriteOpAlertLabelsHeldOps(t *testing.T) {
	s := schemaStore(t, "ytrec_label")
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	created := t0.Add(-11 * time.Minute)

	gone := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(gone))
	heldOp := ytrComment(t, s, gone, "2-"+strconv.FormatInt(gone, 10), "label-gone", created)
	ytrDeleteBinding(t, s, gone)

	bound := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(bound))
	ready := ytrComment(t, s, bound, "2-"+strconv.FormatInt(bound, 10), "label-ready", created)

	other := ytcTask(t, s, ytbProject)
	create := ytrPrepare(t, s, other, ytwrite.KindIssueCreate, YTIssuePrincipal, ytcTenant, "", "label-create", created)
	fields := ytrPrepare(t, s, other, ytwrite.KindIssueFields, YTFieldsPrincipal, ytcTenant, "2-1", "label-fields", created)

	if n, err := s.EnqueueYTWriteOpAlerts(t.Context(), t0); err != nil || n != 1 {
		t.Fatalf("alert pass = %d, %v, want one notice", n, err)
	}
	id := func(op ytwrite.Op) string { return strconv.FormatInt(op.ID, 10) }
	want := "[ytwrite] 4 stuck (held_binding: 1, held_mode: 1, prepared: 2) op ids " +
		strings.Join([]string{id(heldOp), id(ready), id(create), id(fields)}, ", ") + " | handoffkeep yt ops"
	if texts := ytrAlertTexts(t, s); len(texts) != 1 || texts[0] != want {
		t.Fatalf("alerts = %q, want %q", texts, want)
	}
}
