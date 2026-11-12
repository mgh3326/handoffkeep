package store

import (
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// Issue mode (hk 3898): CreateTask prepares the task's issue_create op, and
// applying it publishes the task's binding in the same transaction.

// ytiCreate files a task through CreateTask in project.
func ytiCreate(t *testing.T, s *Store, project string) Task {
	t.Helper()
	if _, err := s.AddTaskProject(t.Context(), project, "test"); err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateTask(t.Context(), Task{Lane: "yti-lane", Title: "yti task " + project, Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// ytiOps is the issue_create ops of a task, oldest first.
func ytiOps(t *testing.T, s *Store, task int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().list(t.Context(), `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE kind=$1 AND entity_kind='task' AND entity_id=$2 ORDER BY id`,
		ytwrite.KindIssueCreate, strconv.FormatInt(task, 10))
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// ytiPrepare stores task's issue_create op on tenant through the unscoped
// ledger and returns it.
func ytiPrepare(t *testing.T, s *Store, task int64, tenant string) ytwrite.Op {
	t.Helper()
	n, err := ytwrite.IssueOp(ytwrite.IssueRequest{Principal: YTIssuePrincipal, Actor: "test", TaskID: task, Title: "t",
		Project: ytbProject, YTProjectID: ytbProjectID, YTProjectShort: "HK", Tenant: tenant})
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := s.YTWriteLedger().Prepare(t.Context(), time.Now().UTC(), n)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// ytiDispatch claims op on the unscoped ledger.
func ytiDispatch(t *testing.T, s *Store, op ytwrite.Op) {
	t.Helper()
	if _, out, err := s.YTWriteLedger().Claim(t.Context(), time.Now().UTC(), op.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim %d: %s %v", op.ID, out, err)
	}
}

// ytiClaim claims op on the tenant's worker ledger.
func ytiClaim(t *testing.T, s *Store, op ytwrite.Op) ytwrite.ClaimOutcome {
	t.Helper()
	_, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), time.Now().UTC(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ytiMaps(op ytwrite.Op, issue string) []ytwrite.MapRow {
	return []ytwrite.MapRow{{EntityKind: "task", HKKey: op.EntityID, YTID: issue, YTParentID: op.YTTargetID, OpID: op.ID}}
}

func TestYTIssueCreateTaskPreparesOneOp(t *testing.T) {
	s := schemaStore(t, "yti_create")
	ytbMapped(t, s)
	logs := ytbLog(t)

	off := ytiCreate(t, s, ytbProject)
	if ops := ytiOps(t, s, off.ID); len(ops) != 0 {
		t.Fatalf("mode off prepared %+v", ops)
	}

	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	ops := ytiOps(t, s, x.ID)
	if len(ops) != 1 {
		t.Fatalf("ops %+v, want one", ops)
	}
	op := ops[0]
	if op.State != ytwrite.StatePrepared || op.Principal != YTIssuePrincipal || op.Key != ytwrite.IssueKey(x.ID) || op.Tenant != ytcTenant ||
		op.YTTargetID != ytbProjectID || op.Actor != "test" || !op.CreatedAt.Equal(x.CreatedAt) {
		t.Fatalf("op %+v for task %+v", op, x)
	}

	unmapped := ytiCreate(t, s, "yti-unmapped")
	if ops := ytiOps(t, s, unmapped.ID); len(ops) != 0 {
		t.Fatalf("unmapped project prepared %+v", ops)
	}
	if want := "ytwrite.issue_skipped task_id=" + strconv.FormatInt(unmapped.ID, 10); strings.Count(logs(), want) != 1 {
		t.Fatalf("log %q, want one %q", logs(), want)
	}
}

// The worker creates the issue and the apply commits the op, the task's map
// row and its op_applied binding together.
func TestYTIssueApplyBindsTheTask(t *testing.T) {
	s := schemaStore(t, "yti_apply")
	ytbMapped(t, s)
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	ytcuRawComment(t, s, x.ID, "worker") // written before the issue exists
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	if _, err := ytcProtocol(t, s, yt, &offset).Drive(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	op := ytiOps(t, s, x.ID)[0]
	issues := yt.Issues(ytbProjectID)
	if op.State != ytwrite.StateApplied || op.Reason != "" || len(issues) != 1 || op.YTResultID != issues[0].ID {
		t.Fatalf("op %+v, issues %+v", op, issues)
	}
	m, ok, err := s.YTWriteLedger().MapGet(t.Context(), "task", strconv.FormatInt(x.ID, 10))
	if err != nil || !ok || m.YTID != op.YTResultID || m.OpID != op.ID || m.YTParentID != ytbProjectID {
		t.Fatalf("map %+v %v %v", m, ok, err)
	}
	b, found, err := s.YTTaskBinding(t.Context(), x.ID)
	if err != nil || !found {
		t.Fatalf("binding %v %v", found, err)
	}
	if b.Origin != YTBindingOpApplied || b.OpID != op.ID || b.Tenant != ytcTenant || b.IssueID != op.YTResultID || b.ProjectID != ytbProjectID ||
		b.BoundBy != YTIssuePrincipal || !b.BoundAt.Equal(op.ResolvedAt) || b.CommentsFrom != 0 {
		t.Fatalf("binding %+v for op %+v", b, op)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_task_bindings`); n != 1 {
		t.Fatalf("%d bindings", n)
	}
	// Applying again with the same id is a no-op.
	if again, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, op.YTResultID, ytiMaps(op, op.YTResultID)); err != nil || again.State != ytwrite.StateApplied {
		t.Fatalf("re-apply %+v %v", again, err)
	}
}

// A binding that cannot be written fails the whole apply: no op applied, no
// map row.
func TestYTIssueApplyIsOneTransaction(t *testing.T) {
	s := schemaStore(t, "yti_atomic")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	op := ytiPrepare(t, s, task, "") // yt_task_bindings refuses an empty tenant
	ytiDispatch(t, s, op)
	if _, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, "2-1", ytiMaps(op, "2-1")); err == nil {
		t.Fatal("apply with an unwritable binding succeeded")
	}
	got, err := s.YTWriteLedger().ByID(t.Context(), op.ID)
	if err != nil || got.State != ytwrite.StateDispatched {
		t.Fatalf("op %+v %v", got, err)
	}
	if _, ok, _ := s.YTWriteLedger().MapGet(t.Context(), "task", op.EntityID); ok {
		t.Fatal("map row committed without the op")
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_task_bindings`); n != 0 {
		t.Fatalf("%d bindings", n)
	}
}

// An existing binding is never overwritten: the same issue is a no-op;
// another issue keeps the op applied with reason binding_conflict, logs it,
// and lists and alerts it with the rejected ops.
func TestYTIssueBindingNeverOverwritten(t *testing.T) {
	s := schemaStore(t, "yti_conflict")
	ytbMapped(t, s)
	logs := ytbLog(t)

	same := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(same))
	op := ytiPrepare(t, s, same, ytcTenant)
	ytiDispatch(t, s, op)
	issue := ytbBinding(same).IssueID
	got, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, issue, ytiMaps(op, issue))
	if err != nil || got.State != ytwrite.StateApplied || got.Reason != "" {
		t.Fatalf("same issue: %+v %v", got, err)
	}
	if b, _, _ := s.YTTaskBinding(t.Context(), same); b.Origin != YTBindingImportVerified || b.OpID != 0 {
		t.Fatalf("same issue rewrote the binding: %+v", b)
	}

	other := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(other))
	op = ytiPrepare(t, s, other, ytcTenant)
	ytiDispatch(t, s, op)
	got, err = s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, "2-elsewhere", ytiMaps(op, "2-elsewhere"))
	if err != nil || got.State != ytwrite.StateApplied || got.Reason != ytwrite.ReasonBindingConflict || got.YTResultID != "2-elsewhere" {
		t.Fatalf("another issue: %+v %v", got, err)
	}
	if b, _, _ := s.YTTaskBinding(t.Context(), other); b.IssueID != ytbBinding(other).IssueID || b.Origin != YTBindingImportVerified {
		t.Fatalf("binding overwritten: %+v", b)
	}
	if _, ok, _ := s.YTWriteLedger().MapGet(t.Context(), "task", op.EntityID); ok {
		t.Fatal("a conflicting apply published the task's map row")
	}
	want := "ytwrite.binding_conflict task_id=" + strconv.FormatInt(other, 10) + " op_id=" + strconv.FormatInt(op.ID, 10) + ` issue_id="2-elsewhere"`
	if !strings.Contains(logs(), want) {
		t.Fatalf("log %q, want %q", logs(), want)
	}
	rejected, err := s.ListYTWriteOps(t.Context(), YTOpFilter{State: "rejected"})
	if err != nil || len(rejected) != 1 || rejected[0].ID != op.ID {
		t.Fatalf("rejected view %+v %v", rejected, err)
	}
	if applied, _ := s.ListYTWriteOps(t.Context(), YTOpFilter{State: "applied"}); len(applied) != 2 {
		t.Fatalf("applied view %+v", applied)
	}
	if _, err := s.EnqueueYTWriteOpAlerts(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM notification_outbox WHERE event_id=$1`, "yt-op-"+strconv.FormatInt(op.ID, 10)+"-rejected"); n != 1 {
		t.Fatalf("%d rejected alerts for the conflict", n)
	}

	// The issue bound to another task is a conflict too.
	third := ytcTask(t, s, ytbProject)
	op = ytiPrepare(t, s, third, ytcTenant)
	ytiDispatch(t, s, op)
	taken := ytbBinding(other).IssueID
	if _, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, taken, []ytwrite.MapRow{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.YTWriteLedger().ByID(t.Context(), op.ID); got.Reason != ytwrite.ReasonBindingConflict {
		t.Fatalf("issue of another task: %+v", got)
	}
	if _, found, _ := s.YTTaskBinding(t.Context(), third); found {
		t.Fatal("bound the issue of another task")
	}
}

// The worker admits an issue_create op only in mode on, on its tenant, and
// while the task has no binding or one this op published.
func TestYTIssueGate(t *testing.T) {
	s := schemaStore(t, "yti_gate")
	ytbMapped(t, s)
	free := ytiPrepare(t, s, ytcTask(t, s, ytbProject), ytcTenant)
	if out := ytiClaim(t, s, free); out != ytwrite.ClaimHeld {
		t.Fatalf("mode off: %s", out)
	}
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)

	elsewhere := ytiPrepare(t, s, ytcTask(t, s, ytbProject), "other.youtrack.cloud")
	if out := ytiClaim(t, s, elsewhere); out != ytwrite.ClaimHeld {
		t.Fatalf("other tenant: %s", out)
	}
	boundTask := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(boundTask))
	if out := ytiClaim(t, s, ytiPrepare(t, s, boundTask, ytcTenant)); out != ytwrite.ClaimHeld {
		t.Fatalf("foreign binding: %s", out)
	}
	ownTask := ytcTask(t, s, ytbProject)
	own := ytiPrepare(t, s, ownTask, ytcTenant)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at, origin, op_id)
		VALUES ($1,$2,'2-own',$3,'test',now(),'op_applied',$4)`, ownTask, ytcTenant, ytbProjectID, own.ID); err != nil {
		t.Fatal(err)
	}
	if out := ytiClaim(t, s, own); out != ytwrite.ClaimClaimed {
		t.Fatalf("own binding: %s", out)
	}
	if out := ytiClaim(t, s, free); out != ytwrite.ClaimClaimed {
		t.Fatalf("no binding: %s", out)
	}
	if due, err := s.YTTenantLedger(ytcTenant).Due(t.Context(), time.Now().UTC(), 100); err != nil || len(due) != 0 {
		t.Fatalf("due lists held ops: %+v %v", due, err)
	}
}

func TestYTIssueBackfillIsIdempotent(t *testing.T) {
	s := schemaStore(t, "yti_backfill")
	ytbMapped(t, s)
	unbound := ytcTask(t, s, ytbProject)
	bound := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(bound))
	unmapped := ytcTask(t, s, "yti-unmapped")
	hasOp := ytcTask(t, s, ytbProject)
	ytiPrepare(t, s, hasOp, ytcTenant)

	dry, err := s.BackfillYTIssueOps(t.Context(), ytcTenant, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind=$1`, ytwrite.KindIssueCreate); n != 1 {
		t.Fatalf("dry run wrote ops: %d", n)
	}
	counts := func(r YTIssueBackfillReport) map[string]YTIssueBackfillProject {
		out := map[string]YTIssueBackfillProject{}
		for _, p := range r.Projects {
			out[p.Project] = *p
		}
		return out
	}
	if c := counts(dry); c[ytbProject].Prepared != 1 || c[ytbProject].Existing != 1 || c["yti-unmapped"].Unmapped != 1 {
		t.Fatalf("dry run %+v", c)
	}
	first, err := s.BackfillYTIssueOps(t.Context(), ytcTenant, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if c := counts(first); c[ytbProject].Prepared != 1 || c[ytbProject].Existing != 1 {
		t.Fatalf("first run %+v", c)
	}
	if ops := ytiOps(t, s, unbound); len(ops) != 1 || ops[0].Tenant != ytcTenant || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("unbound task ops %+v", ops)
	}
	for _, task := range []int64{bound, unmapped} {
		if ops := ytiOps(t, s, task); len(ops) != 0 {
			t.Fatalf("task %d got ops %+v", task, ops)
		}
	}
	second, err := s.BackfillYTIssueOps(t.Context(), ytcTenant, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if c := counts(second); c[ytbProject].Prepared != 0 || c[ytbProject].Existing != 2 {
		t.Fatalf("second run %+v", c)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind=$1`, ytwrite.KindIssueCreate); n != 2 {
		t.Fatalf("%d ops after two runs", n)
	}
}

// A database current on every other ID gains origin and op_id when this
// binary opens it; a binding published before reads as import_verified.
func TestYTTaskBindingOriginMigrationUpgrades(t *testing.T) {
	const id = "20261111-yt-task-binding-origin"
	_, url := testdb.Schema(t, "yti_mig")
	var without []migration
	for _, m := range migrationRegistry {
		if m.ID != id {
			without = append(without, m)
		}
	}
	if len(without) != len(migrationRegistry)-1 {
		t.Fatalf("migration %s is not registered", id)
	}
	old := openRegistry(t, url, without)
	ytbMapped(t, old)
	task := ytcTask(t, old, ytbProject)
	if _, err := old.pool.Exec(t.Context(), `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at)
		VALUES ($1,$2,'2-1',$3,'old release',now())`, task, ytcTenant, ytbProjectID); err != nil {
		t.Fatal(err)
	}
	old.Close()
	for i := 0; i < 2; i++ {
		s := openRegistry(t, url, migrationRegistry)
		b, found, err := s.YTTaskBinding(t.Context(), task)
		if err != nil || !found || b.Origin != YTBindingImportVerified || b.OpID != 0 {
			t.Fatalf("open %d: binding %+v %v %v", i, b, found, err)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM schema_migrations WHERE id=$1`, id); n != 1 {
			t.Fatalf("open %d: id recorded %d times", i, n)
		}
		s.Close()
	}
}

// A task map row anchored by an earlier comment op (the canary) is kept: the
// apply of an issue_create op naming another issue records the conflict
// instead of failing, and writes neither map row nor binding.
func TestYTIssueApplyKeepsAnExistingMapRow(t *testing.T) {
	s := schemaStore(t, "yti_anchor")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytbAnchor(t, s, task, "2-canary")
	op := ytiPrepare(t, s, task, ytcTenant)
	ytiDispatch(t, s, op)
	got, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, "2-new", ytiMaps(op, "2-new"))
	if err != nil || got.State != ytwrite.StateApplied || got.Reason != ytwrite.ReasonBindingConflict {
		t.Fatalf("apply %+v %v", got, err)
	}
	if m, _, _ := s.YTWriteLedger().MapGet(t.Context(), "task", op.EntityID); m.YTID != "2-canary" {
		t.Fatalf("map row %+v, want the canary's", m)
	}
	if _, found, _ := s.YTTaskBinding(t.Context(), task); found {
		t.Fatal("bound a task whose map row names another issue")
	}
}

// While a task's issue_create op is unresolved, neither a comments-backfill
// binding nor a canary enrollment can give it an issue another way.
func TestYTIssuePendingOpFencesOtherIssueWriters(t *testing.T) {
	s := schemaStore(t, "yti_fence")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	op := ytiPrepare(t, s, task, ytcTenant)
	if _, err := ytbAdd(t, s, ytbBinding(task)); !errors.Is(err, ErrYTBindingConflict) {
		t.Fatalf("binding with a pending issue op: %v", err)
	}
	verified, err := s.YTCanaryCandidate(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYTCommentCanary(t.Context(), ytcBinding(task), verified); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("canary with a pending issue op: %v", err)
	}
	if _, err := s.YTWriteLedger().Reject(t.Context(), time.Now().UTC(), op.ID, "test", "closed"); err != nil {
		t.Fatal(err)
	}
	ytbBind(t, s, ytbBinding(task)) // a rejected op no longer holds the task
}

// The backfill leaves a task the canary or a map row already ties to an issue.
func TestYTIssueBackfillSkipsOwnedTasks(t *testing.T) {
	s := schemaStore(t, "yti_owned")
	ytbMapped(t, s)
	canary, anchored, origin := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbCanaryRow(t, s, canary, ytcTenant, "2-c")
	ytbAnchor(t, s, anchored, "2-a")
	if _, err := s.UpsertYouTrackMirror(t.Context(), YouTrackMirrorInput{TaskID: origin, IssueID: "2-o", DisplayRef: "HK-9", Tenant: ytcTenant, Role: "mirror"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.BackfillYTIssueOps(t.Context(), ytcTenant, ytbProject, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Projects) != 1 || r.Projects[0].Owned != 3 || r.Projects[0].Prepared != 0 {
		t.Fatalf("report %+v", r.Projects)
	}
	for _, task := range []int64{canary, anchored, origin} {
		if ops := ytiOps(t, s, task); len(ops) != 0 {
			t.Fatalf("task %d got ops %+v", task, ops)
		}
	}
}

// An adopted issue already holds the task's history: its binding starts after
// the task's highest comment, as a verified binding does.
func TestYTIssueAdoptedBindingSkipsHistory(t *testing.T) {
	s := schemaStore(t, "yti_adopt")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytcuRawComment(t, s, task, "worker")
	last := ytcuRawComment(t, s, task, "worker").ID
	op := ytiPrepare(t, s, task, ytcTenant)
	ytiDispatch(t, s, op)
	maps := ytiMaps(op, "2-adopted")
	maps[0].Adopted = true
	if got, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, "2-adopted", maps); err != nil || got.Reason != "" {
		t.Fatalf("apply %+v %v", got, err)
	}
	b, found, err := s.YTTaskBinding(t.Context(), task)
	if err != nil || !found || b.CommentsFrom != last || b.Origin != YTBindingOpApplied {
		t.Fatalf("binding %+v %v %v, want comments_from %d", b, found, err, last)
	}
}

// A resolver that marks an issue_create op applied without its binding (the
// previous release's, which knows no such kind) is refused by the database;
// other kinds are untouched.
func TestYTIssueApplyGuardRefusesABindinglessApply(t *testing.T) {
	s := schemaStore(t, "yti_guard")
	ytbMapped(t, s)
	op := ytiPrepare(t, s, ytcTask(t, s, ytbProject), ytcTenant)
	ytiDispatch(t, s, op)
	oldApply := `UPDATE yt_write_ops SET state='applied', yt_result_id=$2, reason='', last_error='', resolved_at=now() WHERE id=$1`
	if _, err := s.pool.Exec(t.Context(), oldApply, op.ID, "2-9"); err == nil || !strings.Contains(err.Error(), "applied without its binding") {
		t.Fatalf("bindingless apply: %v", err)
	}
	if got, _ := s.YTWriteLedger().ByID(t.Context(), op.ID); got.State != ytwrite.StateDispatched {
		t.Fatalf("op %+v", got)
	}
	comment, _, err := s.YTWriteLedger().Prepare(t.Context(), time.Now().UTC(), ytwrite.NewOp{Principal: "test", Kind: ytwrite.KindCommentAppend,
		Key: "k", PayloadSHA: "x", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "424242", YTTargetID: "2-1", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ytiDispatch(t, s, comment)
	if _, err := s.pool.Exec(t.Context(), oldApply, comment.ID, "c-1"); err != nil {
		t.Fatalf("comment apply: %v", err)
	}
}

// A YouTrack origin naming another issue is a conflict at apply: the op is
// applied with binding_conflict and no binding is written.
func TestYTIssueOriginConflict(t *testing.T) {
	s := schemaStore(t, "yti_origin")
	ytbMapped(t, s)
	raced := ytcTask(t, s, ytbProject)
	racedOp := ytiPrepare(t, s, raced, ytcTenant)
	ytiDispatch(t, s, racedOp)
	if _, err := s.UpsertYouTrackMirror(t.Context(), YouTrackMirrorInput{TaskID: raced, IssueID: "2-other", DisplayRef: "HK-2", Tenant: ytcTenant, Role: "mirror"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), racedOp.ID, "2-new", ytiMaps(racedOp, "2-new"))
	if err != nil || got.Reason != ytwrite.ReasonBindingConflict {
		t.Fatalf("apply against an origin: %+v %v", got, err)
	}
	if _, found, _ := s.YTTaskBinding(t.Context(), raced); found {
		t.Fatal("bound a task whose origin names another issue")
	}
}

// A task bound after the backfill's scan is rechecked under its lane lock.
func TestYTIssueBackfillRechecksTheBinding(t *testing.T) {
	s := schemaStore(t, "yti_recheck")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(task))
	outcome, _, err := s.backfillYTIssue(t.Context(), ytcTenant, task, false)
	if err != nil || outcome != "owned" {
		t.Fatalf("outcome %q %v", outcome, err)
	}
	if ops := ytiOps(t, s, task); len(ops) != 0 {
		t.Fatalf("ops %+v", ops)
	}
}

// The other task creators prepare the op too in mode on (intake here).
func TestYTIssueIntakePreparesOneOp(t *testing.T) {
	s := schemaStore(t, "yti_intake")
	ytbMapped(t, s)
	if _, err := s.AddTaskProject(t.Context(), ytbProject, "test"); err != nil {
		t.Fatal(err)
	}
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	project := ytbProject
	in := IntakeInput{Source: "dots", SourceRef: "yti-1", Title: "intake with an issue", Body: "b", Project: &project, CreatedBy: "tester"}
	x, created, err := s.IntakeTask(t.Context(), in)
	if err != nil || !created {
		t.Fatalf("intake %v %v", created, err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("ops %+v", ops)
	}
	if again, created, err := s.IntakeTask(t.Context(), in); err != nil || created || again.ID != x.ID {
		t.Fatalf("replay %+v %v %v", again, created, err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 {
		t.Fatalf("a replay prepared another op: %+v", ops)
	}
}

// A task moved to another project before its issue op was sent gets the new
// project's op in the move's transaction; the stale op is rejected
// project_changed and never sent (hk 3909). A sent op is left alone.
func TestYTIssueProjectChangeRetargetsAnUnsentOp(t *testing.T) {
	s := schemaStore(t, "yti_retarget")
	ytbMapped(t, s)
	const other, otherID = "yti-other", "0-9"
	if _, err := s.AddTaskProject(t.Context(), other, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTaskProject(t.Context(), "yti-unmapped", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: other, ProjectID: otherID, ShortName: "HO"}); err != nil {
		t.Fatal(err)
	}
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	x := ytiCreate(t, s, ytbProject)
	if _, _, err := s.SetTaskProject(t.Context(), x.ID, other, "test", "moved"); err != nil {
		t.Fatal(err)
	}
	ops := ytiOps(t, s, x.ID)
	if len(ops) != 2 || ops[0].State != ytwrite.StateRejected || ops[0].Reason != ytwrite.ReasonProjectChanged ||
		ops[1].State != ytwrite.StatePrepared || ops[1].YTTargetID != otherID || ops[1].Tenant != ytcTenant ||
		ops[1].Key != ytwrite.IssueKey(x.ID)+":after-"+strconv.FormatInt(ops[0].ID, 10) {
		t.Fatalf("ops after the move %+v", ops)
	}
	if out := ytiClaim(t, s, ops[1]); out != ytwrite.ClaimClaimed {
		t.Fatalf("claim of the new op = %s", out)
	}
	if _, err := s.YTWriteLedger().Defer(t.Context(), time.Now().UTC(), ops[1].ID, time.Now().UTC(), ytwrite.ReasonRateLimited, "429"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetTaskProject(t.Context(), x.ID, "yti-unmapped", "test", "moved again"); err != nil {
		t.Fatal(err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 2 || ops[1].State != ytwrite.StateRejected || ops[1].Reason != ytwrite.ReasonProjectChanged {
		t.Fatalf("a deferred op moved to an unmapped project: %+v", ops)
	}

	sent := ytiCreate(t, s, ytbProject)
	op := ytiOps(t, s, sent.ID)[0]
	ytiDispatch(t, s, op)
	if _, _, err := s.SetTaskProject(t.Context(), sent.ID, other, "test", "moved"); err != nil {
		t.Fatal(err)
	}
	if ops := ytiOps(t, s, sent.ID); len(ops) != 1 || ops[0].State != ytwrite.StateDispatched {
		t.Fatalf("a dispatched op was retargeted: %+v", ops)
	}
}

// A task created (or moved) while its project maps to no YouTrack project
// keeps a durable intent; the worker prepares its op once the mapping exists
// (hk 3942).
func TestYTIssueUnmappedTaskGetsItsOpOnceMapped(t *testing.T) {
	s := schemaStore(t, "yti_intent")
	ytbMapped(t, s)
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	const later = "yti-later"
	x := ytiCreate(t, s, later)
	moved := ytiCreate(t, s, ytbProject)
	if _, _, err := s.SetTaskProject(t.Context(), moved.ID, later, "test", "moved"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 || len(ytiOps(t, s, x.ID)) != 0 {
		t.Fatalf("unmapped pass prepared %d, %v", n, err)
	}
	if err := s.PutYouTrackProject(t.Context(), YouTrackProject{HKProject: later, ProjectID: "0-11", ShortName: "HL"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 2 {
		t.Fatalf("mapped pass prepared %d, %v; want 2", n, err)
	}
	if ops := ytiOps(t, s, x.ID); len(ops) != 1 || ops[0].State != ytwrite.StatePrepared || ops[0].Key != ytwrite.IssueKey(x.ID) || ops[0].YTTargetID != "0-11" {
		t.Fatalf("ops of the created task %+v", ops)
	}
	if ops := ytiOps(t, s, moved.ID); len(ops) != 2 || ops[0].Reason != ytwrite.ReasonProjectChanged || ops[1].State != ytwrite.StatePrepared ||
		ops[1].Key != ytwrite.IssueKey(moved.ID)+":after-"+strconv.FormatInt(ops[0].ID, 10) {
		t.Fatalf("ops of the moved task %+v", ops)
	}
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 {
		t.Fatalf("second pass prepared %d, %v", n, err)
	}
	s.SetYTIssuesMode(YTIssuesOff, "")
	if n, err := s.PrepareMappedYTIssues(t.Context(), 10); err != nil || n != 0 {
		t.Fatalf("mode off prepared %d, %v", n, err)
	}
}
