package store

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Unbind and the binding history (hk 4490, design/2026-11-13/yt-comment-binding-lifecycle
// §6.2, §6.9; invariants I6, I7, I11, I12).

// ybhOp prepares a raw op on the task's lane that targets the task's bound
// issue, the way the comment, fields and move writers do.
func ybhOp(t *testing.T, s *Store, principal, kind string, task int64, key string) ytwrite.Op {
	t.Helper()
	id := strconv.FormatInt(task, 10)
	return ybhOpAt(t, s, principal, kind, task, key, ytcTenant, "2-"+id)
}

// ybhOpAt is ybhOp for an op aimed at another tenant or issue.
func ybhOpAt(t *testing.T, s *Store, principal, kind string, task int64, key, tenant, target string) ytwrite.Op {
	t.Helper()
	id := strconv.FormatInt(task, 10)
	op, _, err := s.YTWriteLedger().Prepare(t.Context(), time.Now().UTC(), ytwrite.NewOp{Principal: principal, Kind: kind, Key: key + "-" + id,
		PayloadSHA: "sha-" + key, Payload: []byte(`{}`), EntityKind: "task", EntityID: id, YTTargetID: target, Tenant: tenant, Actor: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

type ybhRow struct{ Action, Origin, Actor, Reason, Issue string }

// ybhHistory is the task's history, oldest first.
func ybhHistory(t *testing.T, s *Store, task int64) []ybhRow {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `SELECT action, origin, actor, reason, issue_id FROM yt_binding_history WHERE task_id=$1 ORDER BY id`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []ybhRow
	for rows.Next() {
		var r ybhRow
		if err := rows.Scan(&r.Action, &r.Origin, &r.Actor, &r.Reason, &r.Issue); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func ybhState(t *testing.T, s *Store, op ytwrite.Op) ytwrite.Op {
	t.Helper()
	got, err := s.YTWriteLedger().ByID(t.Context(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// ybhAnchor is the YouTrack issue the task's yt_entity_map row names, "" when
// it has none.
func ybhAnchor(t *testing.T, s *Store, task int64) string {
	t.Helper()
	var issue string
	if err := s.pool.QueryRow(t.Context(), `SELECT coalesce(max(yt_id), '') FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$1`,
		strconv.FormatInt(task, 10)).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	return issue
}

// ybhWritten is every row an unbind could write for the task (its binding,
// history, map row and ops), so a refusal is checked by comparing two
// snapshots.
func ybhWritten(t *testing.T, s *Store, task int64) string {
	t.Helper()
	id := strconv.FormatInt(task, 10)
	var out string
	err := s.pool.QueryRow(t.Context(), `SELECT concat_ws('|',
		(SELECT count(*) FROM yt_task_bindings WHERE task_id=$1), (SELECT count(*) FROM yt_binding_history WHERE task_id=$1),
		coalesce((SELECT string_agg(tenant || '/' || yt_id, ',') FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$2), ''),
		coalesce((SELECT string_agg(id || ':' || state || ':' || reason, ',' ORDER BY id) FROM yt_write_ops WHERE entity_id=$2), ''))`, task, id).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ybhSetCommentsAuthority(t *testing.T, s *Store, side YTAuthoritySide) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE yt_authority SET authority=$1 WHERE kind='comments'`, string(side)); err != nil {
		t.Fatal(err)
	}
}

func TestAddYTTaskBindingAppendsHistory(t *testing.T) {
	s := schemaStore(t, "ybh_add")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	b := ytbBinding(task)
	ytbBind(t, s, b)
	want := []ybhRow{{Action: "bound", Origin: YTBindingImportVerified, Actor: "operator", Issue: b.IssueID}}
	if got := ybhHistory(t, s, task); !slices.Equal(got, want) {
		t.Fatalf("history after a bind = %+v, want %+v", got, want)
	}
	// The same binding again publishes nothing, so it records nothing.
	ytbBind(t, s, b)
	if got := ybhHistory(t, s, task); !slices.Equal(got, want) {
		t.Fatalf("history after a repeat = %+v, want %+v", got, want)
	}
	// A refused binding records nothing either.
	other := ytbBinding(task)
	other.IssueID = "2-other"
	if _, err := ytbAdd(t, s, other); !errors.Is(err, ErrYTBindingConflict) {
		t.Fatalf("second issue: %v", err)
	}
	if got := ybhHistory(t, s, task); !slices.Equal(got, want) {
		t.Fatalf("history after a refusal = %+v, want %+v", got, want)
	}
}

func TestUnbindYTTaskBinding(t *testing.T) {
	s := schemaStore(t, "ybh_unbind")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task, neighbour := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	b := ytbBinding(task)
	ytbBind(t, s, b)
	ytbBind(t, s, ytbBinding(neighbour))
	c := ytbComment(t, s, task, "prepared before the unbind")
	comment, _ := ytcOpFor(t, s, c.ID)
	fields := ybhOp(t, s, YTFieldsPrincipal, ytwrite.KindIssueFields, task, "f")
	move := ybhOp(t, s, YTFieldsPrincipal, ytwrite.KindIssueMove, task, "m")
	// Not gated by the binding: another kind of the comment principal, a
	// document comment's copy and the same ops of another task.
	kept := ybhOp(t, s, YTCommentPrincipal, "issue_create_test", task, "k")
	doc := ybhOp(t, s, ytDocCommentPrincipal, ytwrite.KindCommentAppend, task, "d")
	// Ops this binding never admitted: another tenant's, another issue's.
	foreign := ybhOpAt(t, s, YTCommentPrincipal, ytwrite.KindCommentAppend, task, "x", "other.youtrack.cloud", b.IssueID)
	stale := ybhOpAt(t, s, YTFieldsPrincipal, ytwrite.KindIssueFields, task, "s", ytcTenant, "2-elsewhere")
	// A comment queued behind an issue_create has no target yet; the binding
	// is its gate too.
	queued := ybhOpAt(t, s, YTCommentPrincipal, ytwrite.KindCommentAppend, task, "q", ytcTenant, "")
	cn := ybhComment(t, s, neighbour)
	neighbourOp, _ := ytcOpFor(t, s, cn.ID)

	got, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "operator:me", Reason: "wrong issue"})
	if err != nil {
		t.Fatal(err)
	}
	wantRejected := []YTUnbindOp{
		{OpID: comment.ID, Kind: ytwrite.KindCommentAppend, Reason: ytwrite.ReasonBindingRemoved},
		{OpID: fields.ID, Kind: ytwrite.KindIssueFields, Reason: ytwrite.ReasonBindingRemoved},
		{OpID: move.ID, Kind: ytwrite.KindIssueMove, Reason: ytwrite.ReasonBindingRemoved},
		{OpID: queued.ID, Kind: ytwrite.KindCommentAppend, Reason: ytwrite.ReasonBindingRemoved},
	}
	if !got.Removed || got.Binding == nil || got.Binding.IssueID != b.IssueID || got.Binding.Tenant != b.Tenant ||
		!slices.Equal(got.Rejected, wantRejected) || !slices.Equal(got.Released, []int64{c.ID}) || len(got.Stranded) != 0 || !got.AnchorReleased {
		t.Fatalf("unbind = %+v, want removed with %+v, comment %d released and the anchor released", got, wantRejected, c.ID)
	}
	for _, op := range []ytwrite.Op{comment, fields, move, queued} {
		if now := ybhState(t, s, op); now.State != ytwrite.StateRejected || now.Reason != ytwrite.ReasonBindingRemoved || now.ResolvedAt.IsZero() {
			t.Errorf("op %d (%s) = %+v, want rejected binding_removed", op.ID, op.Kind, now)
		}
	}
	for name, op := range map[string]ytwrite.Op{"other kind": kept, "document comment": doc, "other task": neighbourOp, "other tenant": foreign, "other issue": stale} {
		if now := ybhState(t, s, op); now.State != ytwrite.StatePrepared {
			t.Errorf("%s op %d = %+v, want prepared", name, op.ID, now)
		}
	}
	if _, found, err := s.YTTaskBinding(ctx, task); err != nil || found {
		t.Fatalf("binding after unbind: %v %v", found, err)
	}
	if _, found, _ := s.YTTaskBinding(ctx, neighbour); !found {
		t.Fatal("the neighbour's binding was removed")
	}
	// I7: the latest row says the operator unbound it.
	hist := ybhHistory(t, s, task)
	if len(hist) != 2 || hist[1] != (ybhRow{Action: "unbound", Origin: YTBindingImportVerified, Actor: "operator:me", Reason: "wrong issue", Issue: b.IssueID}) {
		t.Fatalf("history = %+v", hist)
	}
	if hist := ybhHistory(t, s, neighbour); len(hist) != 1 {
		t.Fatalf("neighbour history = %+v", hist)
	}

	// A later comment gets no op, and the unbind is idempotent.
	late := ybhComment(t, s, task)
	if _, ok := ytcOpFor(t, s, late.ID); ok {
		t.Fatalf("comment %d after the unbind got an op", late.ID)
	}
	before := ybhWritten(t, s, task)
	again, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "operator:me"})
	if err != nil || again.Removed || again.Binding != nil || len(again.Rejected) != 0 || again.AnchorReleased {
		t.Fatalf("second unbind = %+v %v", again, err)
	}
	if after := ybhWritten(t, s, task); after != before {
		t.Fatalf("second unbind wrote: %s -> %s", before, after)
	}

	// I10: a rebind is a new publication, and the history keeps all three.
	// The same issue binds again: the unbind released the task's
	// yt_entity_map row, which the first op had anchored on it (hk 4536), and
	// the next op anchors it again.
	ytbBind(t, s, b)
	if hist := ybhHistory(t, s, task); len(hist) != 3 || hist[2].Action != "bound" || hist[2].Issue != b.IssueID {
		t.Fatalf("history after the rebind = %+v", hist)
	}
}

func ybhComment(t *testing.T, s *Store, task int64) TaskComment {
	t.Helper()
	return ytbComment(t, s, task, "comment "+strconv.FormatInt(time.Now().UnixNano(), 10)).TaskComment
}

// The lane is claimable after an unbind: nothing the binding admitted is
// left prepared in front of the task's next op.
func TestUnbindLeavesLaneClaimable(t *testing.T) {
	s := schemaStore(t, "ybh_lane")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	first := ybhComment(t, s, task)
	firstOp, _ := ytcOpFor(t, s, first.ID)
	ledger := s.YTTenantLedger(ytcTenant)
	later := time.Now().UTC().Add(time.Minute)

	if _, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op"}); err != nil {
		t.Fatal(err)
	}
	ytbBind(t, s, ytbBinding(task))
	second := ybhComment(t, s, task)
	secondOp, ok := ytcOpFor(t, s, second.ID)
	if !ok {
		t.Fatal("the comment after the rebind got no op")
	}
	if got := ybhState(t, s, firstOp); got.State != ytwrite.StateRejected {
		t.Fatalf("first op = %+v", got)
	}
	if got, out, err := ledger.Claim(ctx, later, secondOp.ID); err != nil || out != ytwrite.ClaimClaimed || got.State != ytwrite.StateDispatched {
		t.Fatalf("claim after unbind and rebind: %s %+v %v", out, got, err)
	}
}

// An op that may have reached the issue keeps the binding: dispatched, then
// remote_unknown, until yt ops resolve closes it.
func TestUnbindRefusesUncertainOps(t *testing.T) {
	s := schemaStore(t, "ybh_uncertain")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	c := ybhComment(t, s, task)
	op, _ := ytcOpFor(t, s, c.ID)
	waiting := ybhOp(t, s, YTFieldsPrincipal, ytwrite.KindIssueFields, task, "f")
	ledger := s.YTTenantLedger(ytcTenant)
	now := time.Now().UTC().Add(time.Minute)
	if _, out, err := ledger.Claim(ctx, now, op.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim: %s %v", out, err)
	}
	refused := func(name string) {
		t.Helper()
		before := ybhWritten(t, s, task)
		_, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op", Force: true})
		if !errors.Is(err, ErrYTUnbindRefused) || !strings.Contains(err.Error(), strconv.FormatInt(op.ID, 10)+" ("+ytwrite.KindCommentAppend+")") {
			t.Fatalf("%s: err %v, want a refusal naming op %d", name, err, op.ID)
		}
		if after := ybhWritten(t, s, task); after != before {
			t.Fatalf("%s: a refusal wrote: %s -> %s", name, before, after)
		}
		if got := ybhState(t, s, waiting); got.State != ytwrite.StatePrepared {
			t.Fatalf("%s: the prepared fields op = %+v", name, got)
		}
	}
	refused("dispatched")
	if _, err := ledger.MarkUnknown(ctx, now, op.ID, now, ytwrite.ReasonTransport, "lost"); err != nil {
		t.Fatal(err)
	}
	refused("remote_unknown")

	// Resolved, the unbind goes through.
	if _, err := s.YTWriteLedger().Apply(ctx, now, op.ID, "4-1", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op"})
	if err != nil || !got.Removed || len(got.Rejected) != 1 || got.Rejected[0].OpID != waiting.ID {
		t.Fatalf("unbind after the op resolved: %+v %v", got, err)
	}
}

func TestUnbindRefusesPendingCreate(t *testing.T) {
	s := schemaStore(t, "ybh_create")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	create := ytiPrepare(t, s, task, ytcTenant)
	before := ybhWritten(t, s, task)
	_, err := s.UnbindYTTaskBinding(t.Context(), task, YTUnbindOptions{By: "op"})
	if !errors.Is(err, ErrYTUnbindRefused) || !strings.Contains(err.Error(), "issue_create") {
		t.Fatalf("pending create: %v", err)
	}
	if after := ybhWritten(t, s, task); after != before {
		t.Fatalf("a refusal wrote: %s -> %s (op %d)", before, after, create.ID)
	}
}

// I6: while comments are canonical in YouTrack, the unbind keeps a prepared
// comment op, which is the only delivery of its comment, unless --force.
func TestUnbindUnderYouTrackCommentAuthority(t *testing.T) {
	s := schemaStore(t, "ybh_authority")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	c := ybhComment(t, s, task)
	op, _ := ytcOpFor(t, s, c.ID)
	fields := ybhOp(t, s, YTFieldsPrincipal, ytwrite.KindIssueFields, task, "f")
	ybhSetCommentsAuthority(t, s, YTAuthorityYouTrack)

	before := ybhWritten(t, s, task)
	_, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op"})
	if !errors.Is(err, ErrYTUnbindRefused) || !strings.Contains(err.Error(), strconv.FormatInt(op.ID, 10)) ||
		!strings.Contains(err.Error(), "comments "+strconv.FormatInt(c.ID, 10)) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("unbind with a pending comment op: %v", err)
	}
	if after := ybhWritten(t, s, task); after != before {
		t.Fatalf("a refusal wrote: %s -> %s", before, after)
	}

	got, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op", Reason: "forced", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Removed || !slices.Equal(got.Stranded, []int64{c.ID}) || len(got.Released) != 0 || len(got.Rejected) != 2 {
		t.Fatalf("forced unbind = %+v, want comment %d stranded and two ops rejected", got, c.ID)
	}
	for _, r := range got.Rejected {
		if r.Reason != ytwrite.ReasonUnbindForced {
			t.Errorf("rejected %+v, want reason %s", r, ytwrite.ReasonUnbindForced)
		}
	}
	for _, o := range []ytwrite.Op{op, fields} {
		if now := ybhState(t, s, o); now.State != ytwrite.StateRejected || now.Reason != ytwrite.ReasonUnbindForced {
			t.Errorf("op %d = %+v", o.ID, now)
		}
	}
}

// Under comments authority youtrack, ops that are not hk comments do not
// need --force.
func TestUnbindUnderYouTrackAuthorityClosesFieldsOps(t *testing.T) {
	s := schemaStore(t, "ybh_authority_fields")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	fields := ybhOp(t, s, YTFieldsPrincipal, ytwrite.KindIssueFields, task, "f")
	ybhSetCommentsAuthority(t, s, YTAuthorityYouTrack)
	got, err := s.UnbindYTTaskBinding(t.Context(), task, YTUnbindOptions{By: "op"})
	if err != nil || !got.Removed || len(got.Rejected) != 1 || got.Rejected[0].OpID != fields.ID || got.Rejected[0].Reason != ytwrite.ReasonBindingRemoved ||
		len(got.Stranded) != 0 || len(got.Released) != 0 {
		t.Fatalf("unbind = %+v %v", got, err)
	}
}

func TestUnbindArguments(t *testing.T) {
	s := schemaStore(t, "ybh_args")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	for name, o := range map[string]YTUnbindOptions{
		"no actor":       {},
		"actor NUL":      {By: "a\x00b"},
		"actor too big":  {By: strings.Repeat("a", 201)},
		"reason too big": {By: "op", Reason: strings.Repeat("a", 2001)},
	} {
		if _, err := s.UnbindYTTaskBinding(t.Context(), task, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := s.UnbindYTTaskBinding(t.Context(), 0, YTUnbindOptions{By: "op"}); err == nil {
		t.Error("task 0 accepted")
	}
	if _, err := s.UnbindYTTaskBinding(t.Context(), 987654321, YTUnbindOptions{By: "op"}); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("missing task: %v", err)
	}
	if _, found, _ := s.YTTaskBinding(t.Context(), task); !found {
		t.Error("a refused unbind removed the binding")
	}
	// A task with no binding answers removed false and writes nothing.
	bare := ytcTask(t, s, ytbProject)
	res, err := s.UnbindYTTaskBinding(t.Context(), bare, YTUnbindOptions{By: "op"})
	if err != nil || res.Removed || len(ybhHistory(t, s, bare)) != 0 {
		t.Fatalf("unbound task: %+v %v", res, err)
	}
}

// I11, I12: the migration is registered after the table and the column it
// alters, widens origin and leaves the previous release's binding reads and
// inserts working; the history is append-only.
func TestYTBindingHistoryMigration(t *testing.T) {
	var reg *migration
	for i := range migrationRegistry {
		if migrationRegistry[i].ID == YTBindingHistoryMigration {
			reg = &migrationRegistry[i]
		}
	}
	if reg == nil || !slices.Contains(reg.After, YTTaskBindingsMigration) || !slices.Contains(reg.After, YTTaskBindingOriginMigration) {
		t.Fatalf("registered migration %+v", reg)
	}
	s := schemaStore(t, "ybh_mig")
	ctx := t.Context()
	if n := ytcCount(t, s, `SELECT count(*) FROM schema_migrations WHERE id=$1`, YTBindingHistoryMigration); n != 1 {
		t.Fatalf("migration recorded %d times", n)
	}
	// The previous release's insert names no origin; its read scans origin
	// into a string, whatever it holds.
	prevInsert := `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at, comments_from)
		VALUES ($1,'t','i-1','HK-1','0-2','prev',now(),0)`
	if _, err := s.pool.Exec(ctx, prevInsert, 1); err != nil {
		t.Fatalf("previous release's binding insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, bound_by, bound_at, origin) VALUES (2,'t','i-2','x',now(),'auto_verified')`); err != nil {
		t.Fatalf("origin auto_verified: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, bound_by, bound_at, origin) VALUES (3,'t','i-3','x',now(),'bogus')`); err == nil {
		t.Fatal("origin bogus accepted")
	}
	for task, want := range map[int64]string{1: YTBindingImportVerified, 2: "auto_verified"} {
		b, found, err := s.YTTaskBinding(ctx, task)
		if err != nil || !found || b.Origin != want {
			t.Fatalf("task %d: %+v %v %v, want origin %s", task, b, found, err, want)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_binding_history(task_id, tenant, issue_id, origin, action, actor, at) VALUES (1,'t','i-1','import_verified','bound','x',now())`); err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"update":   `UPDATE yt_binding_history SET reason='x'`,
		"delete":   `DELETE FROM yt_binding_history`,
		"truncate": `TRUNCATE yt_binding_history`,
		"action":   `INSERT INTO yt_binding_history(task_id, tenant, issue_id, origin, action, actor, at) VALUES (1,'t','i','import_verified','rebound','x',now())`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// In mode canary the enrollment routes a task's comments without the binding,
// so an unbind that left it would keep delivering to the issue: it refuses
// until the canary row is removed.
func TestUnbindRefusesCanaryEnrollment(t *testing.T) {
	s := schemaStore(t, "ybh_canary")
	ctx := t.Context()
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytcEnroll(t, s, task)
	ytbBind(t, s, ytbBinding(task))
	before := ybhWritten(t, s, task)
	_, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op", Force: true})
	if !errors.Is(err, ErrYTUnbindRefused) || !strings.Contains(err.Error(), "canary remove") {
		t.Fatalf("enrolled task: %v", err)
	}
	if after := ybhWritten(t, s, task); after != before {
		t.Fatalf("a refusal wrote: %s -> %s", before, after)
	}
	if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || !removed {
		t.Fatalf("remove canary: %v %v", removed, err)
	}
	if got, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op"}); err != nil || !got.Removed {
		t.Fatalf("unbind after the canary was removed: %+v %v", got, err)
	}
}

// A pending issue intent would let the issue worker create or adopt an issue
// for the task after the unbind and publish a binding without the operator:
// the unbind drops it.
func TestUnbindDropsPendingIssueIntent(t *testing.T) {
	s := schemaStore(t, "ybh_intent")
	ctx := t.Context()
	ytbMapped(t, s)
	task, other := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	for _, id := range []int64{task, other} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO yt_issue_intents(task_id, tenant, recorded_at) VALUES ($1,$2,now())`, id, ytcTenant); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "op"}); err != nil || !got.Removed {
		t.Fatalf("unbind: %+v %v", got, err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_issue_intents WHERE task_id=$1`, task); n != 0 {
		t.Fatalf("the unbound task keeps %d issue intents", n)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_issue_intents WHERE task_id=$1`, other); n != 1 {
		t.Fatalf("another task's intent: %d rows, want 1", n)
	}
}

// An unbound task is the operator's until they bind it again: a later project
// change prepares no issue_create op for it, as it would for a task that was
// never bound (the same move, as a control).
func TestUnboundTaskGetsNoAutomaticIssueOnProjectChange(t *testing.T) {
	s := schemaStore(t, "ybh_issue")
	ctx := t.Context()
	ytbMapped(t, s)
	ytrProject(t, s, "ybh-other", "0-9")
	unbound, never := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(unbound))
	if got, err := s.UnbindYTTaskBinding(ctx, unbound, YTUnbindOptions{By: "op"}); err != nil || !got.Removed {
		t.Fatalf("unbind: %+v %v", got, err)
	}
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	ytrMove(t, s, unbound, "ybh-other")
	ytrMove(t, s, never, "ybh-other")
	creates := func(task int64) int {
		return ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind=$1 AND entity_kind='task' AND entity_id=$2`, ytwrite.KindIssueCreate, strconv.FormatInt(task, 10))
	}
	if n := creates(never); n != 1 {
		t.Fatalf("control: a never-bound task got %d issue_create ops after the move, want 1", n)
	}
	if n := creates(unbound); n != 0 {
		t.Fatalf("the unbound task got %d issue_create ops after the move, want 0", n)
	}
	// Bound again by the operator (to the YouTrack project the task is in now),
	// the history's latest row is 'bound'.
	again := ytbBinding(unbound)
	again.ProjectID = "0-9"
	ytbBind(t, s, again)
	if hist := ybhHistory(t, s, unbound); hist[len(hist)-1].Action != "bound" {
		t.Fatalf("history after the rebind = %+v", hist)
	}
}

// hk 4536: the first op of a task anchors its yt_entity_map row on the bound
// issue, and every later binding is checked against that row, by the CLI's
// verification (the candidate's MappedIssueID) and again at publication. The
// unbind releases the row with the binding, so an operator can bind a
// verified different issue, and the comment after it goes there.
func TestUnbindReleasesTheAnchorSoADifferentIssueBinds(t *testing.T) {
	s := schemaStore(t, "ybh_anchor")
	ctx := t.Context()
	ytbMapped(t, s)
	s.SetYTCommentsMode(YTCommentsOn)
	task := ytcTask(t, s, ytbProject)
	b := ytbBinding(task)
	ytbBind(t, s, b)
	first := ybhComment(t, s, task)
	firstOp, ok := ytcOpFor(t, s, first.ID)
	if !ok {
		t.Fatal("the first comment got no op")
	}
	if got := ybhAnchor(t, s, task); got != b.IssueID {
		t.Fatalf("anchor after the first comment = %q, want %q", got, b.IssueID)
	}
	// While the task is bound, another issue is refused (I10).
	other := b
	other.IssueID, other.IssueReadable = "2-other", "HK-other"
	if _, err := ytbAdd(t, s, other); !errors.Is(err, ErrYTBindingConflict) {
		t.Fatalf("a second issue while bound: %v", err)
	}

	res, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "operator:me", Reason: "wrong issue"})
	if err != nil || !res.Removed || !res.AnchorReleased {
		t.Fatalf("unbind = %+v %v, want removed with the anchor released", res, err)
	}
	if got := ybhAnchor(t, s, task); got != "" {
		t.Fatalf("anchor after the unbind = %q, want none", got)
	}
	// What the CLI verifies against no longer names the old issue, and the
	// old op is closed where it was aimed: it was never retargeted (I4).
	if cand, err := s.YTCanaryCandidate(ctx, task); err != nil || cand.MappedIssueID != "" {
		t.Fatalf("candidate after the unbind = %+v %v, want no mapped issue", cand, err)
	}
	if got := ybhState(t, s, firstOp); got.State != ytwrite.StateRejected || got.YTTargetID != b.IssueID {
		t.Fatalf("first op after the unbind = %+v", got)
	}

	ytbBind(t, s, other)
	second := ybhComment(t, s, task)
	op, ok := ytcOpFor(t, s, second.ID)
	if !ok || op.YTTargetID != other.IssueID || op.State != ytwrite.StatePrepared {
		t.Fatalf("comment after the rebind = %+v (ok %v), want a prepared op aimed at %s", op, ok, other.IssueID)
	}
	if got := ybhAnchor(t, s, task); got != other.IssueID {
		t.Fatalf("anchor after the rebind = %q, want %q", got, other.IssueID)
	}
	if got, out, err := s.YTTenantLedger(ytcTenant).Claim(ctx, time.Now().UTC().Add(time.Minute), op.ID); err != nil || out != ytwrite.ClaimClaimed || got.YTTargetID != other.IssueID {
		t.Fatalf("claim after the rebind: %s %+v %v", out, got, err)
	}
}

// The unbind releases only the row that anchors the binding it removes: the
// binding's issue on the binding's tenant. A row that names another tenant or
// another issue is somebody else's record and stays.
func TestUnbindKeepsAnAnchorItDoesNotName(t *testing.T) {
	s := schemaStore(t, "ybh_anchor_kept")
	ctx := t.Context()
	ytbMapped(t, s)
	for _, tc := range []struct{ name, tenant, issue string }{
		{"another tenant", "other.youtrack.cloud", ""},
		{"another issue", ytcTenant, "2-elsewhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := ytcTask(t, s, ytbProject)
			b := ytbBinding(task)
			ytbBind(t, s, b)
			issue := tc.issue
			if issue == "" {
				issue = b.IssueID
			}
			op := ybhOp(t, s, YTFieldsPrincipal, ytwrite.KindIssueFields, task, "anchor")
			if _, err := s.pool.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind, tenant, hk_key, yt_id, yt_parent_id, op_id, created_at)
				VALUES ('task',$1,$2,$3,'',$4,now())`, tc.tenant, strconv.FormatInt(task, 10), issue, op.ID); err != nil {
				t.Fatal(err)
			}
			res, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "operator:me"})
			if err != nil || !res.Removed || res.AnchorReleased {
				t.Fatalf("unbind = %+v %v, want removed and no anchor released", res, err)
			}
			if got := ybhAnchor(t, s, task); got != issue {
				t.Fatalf("anchor after the unbind = %q, want %q kept", got, issue)
			}
		})
	}
}

// The map row an applied issue_create wrote is the same routing anchor, and
// it is released with the binding that create published (hk 4536): the old
// issue stays on record in the create op, its result and the history.
func TestUnbindReleasesTheAnchorAnIssueCreateWrote(t *testing.T) {
	s := schemaStore(t, "ybh_anchor_create")
	ctx := t.Context()
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	create := ytiPrepare(t, s, task, ytcTenant)
	ytiDispatch(t, s, create)
	issue := "2-" + strconv.FormatInt(task, 10)
	if _, err := s.YTWriteLedger().Apply(ctx, time.Now().UTC(), create.ID, issue, ytiMaps(create, issue)); err != nil {
		t.Fatal(err)
	}
	if got := ybhAnchor(t, s, task); got != issue {
		t.Fatalf("anchor after the applied create = %q, want %q", got, issue)
	}

	res, err := s.UnbindYTTaskBinding(ctx, task, YTUnbindOptions{By: "operator:me", Reason: "created in the wrong project"})
	if err != nil || !res.Removed || !res.AnchorReleased || res.Binding == nil || res.Binding.Origin != YTBindingOpApplied {
		t.Fatalf("unbind = %+v %v, want the create's binding removed and its anchor released", res, err)
	}
	if got := ybhAnchor(t, s, task); got != "" {
		t.Fatalf("anchor after the unbind = %q, want none", got)
	}
	if got := ybhState(t, s, create); got.State != ytwrite.StateApplied || got.YTResultID != issue {
		t.Fatalf("the create op after the unbind = %+v, want it applied to %s", got, issue)
	}
	other := ytbBinding(task)
	other.IssueID, other.IssueReadable = "2-other", "HK-other"
	ytbBind(t, s, other)
}
