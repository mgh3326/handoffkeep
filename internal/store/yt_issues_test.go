package store

import (
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
		b.BoundBy != YTIssuePrincipal || !b.BoundAt.Equal(op.ResolvedAt) {
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
	now := time.Now().UTC()
	claim := func(op ytwrite.Op) ytwrite.ClaimOutcome {
		t.Helper()
		_, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), now, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	free := ytiPrepare(t, s, ytcTask(t, s, ytbProject), ytcTenant)
	if out := claim(free); out != ytwrite.ClaimHeld {
		t.Fatalf("mode off: %s", out)
	}
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)

	elsewhere := ytiPrepare(t, s, ytcTask(t, s, ytbProject), "other.youtrack.cloud")
	if out := claim(elsewhere); out != ytwrite.ClaimHeld {
		t.Fatalf("other tenant: %s", out)
	}
	boundTask := ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(boundTask))
	if out := claim(ytiPrepare(t, s, boundTask, ytcTenant)); out != ytwrite.ClaimHeld {
		t.Fatalf("foreign binding: %s", out)
	}
	ownTask := ytcTask(t, s, ytbProject)
	own := ytiPrepare(t, s, ownTask, ytcTenant)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at, origin, op_id)
		VALUES ($1,$2,'2-own',$3,'test',now(),'op_applied',$4)`, ownTask, ytcTenant, ytbProjectID, own.ID); err != nil {
		t.Fatal(err)
	}
	if out := claim(own); out != ytwrite.ClaimClaimed {
		t.Fatalf("own binding: %s", out)
	}
	if out := claim(free); out != ytwrite.ClaimClaimed {
		t.Fatalf("no binding: %s", out)
	}
	if due, err := s.YTTenantLedger(ytcTenant).Due(t.Context(), now, 100); err != nil || len(due) != 0 {
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
