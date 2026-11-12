package store

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Task fields mode (hk 4003): the task_fields feed consumer prepares one
// issue_fields op per changed snapshot of a bound task.

// ytfSetup is a throwaway store in fields mode on with the test project
// mapped, one bound task and one unbound task, and the consumer's cursor
// already created (history before it is not the feed's).
func ytfSetup(t *testing.T, prefix string) (s *Store, bound, unbound int64) {
	t.Helper()
	s = schemaStore(t, prefix)
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	ytbMapped(t, s)
	bound, unbound = ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at)
		VALUES ($1,$2,$3,$4,'test',now())`, bound, ytcTenant, ytfIssue(bound), ytbProjectID); err != nil {
		t.Fatal(err)
	}
	if pass := ytfPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	return s, bound, unbound
}

func ytfIssue(task int64) string { return "2-" + strconv.FormatInt(task, 10) }

func ytfPass(t *testing.T, s *Store) YTFeedPass {
	t.Helper()
	return ytFeedPassOK(t, s, ytFeedConsumers[ytFieldsConsumer])
}

// ytfOps is the issue_fields ops of a task, oldest first.
func ytfOps(t *testing.T, s *Store, task int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().list(t.Context(), `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE kind=$1 AND entity_kind='task' AND entity_id=$2 ORDER BY id`,
		ytwrite.KindIssueFields, strconv.FormatInt(task, 10))
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func ytfPayload(t *testing.T, op ytwrite.Op) ytwrite.IssueFieldsPayload {
	t.Helper()
	var p ytwrite.IssueFieldsPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func ytfField(p ytwrite.IssueFieldsPayload, name string) string {
	for _, f := range p.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return "<absent>"
}

func TestYTFieldsUpdateMakesOneOp(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytf_update")
	ytFeedExec(t, s, `UPDATE tasks SET state='developing', priority=2, title='renamed task (grade A, T2)' WHERE id=$1`, bound)
	ytFeedExec(t, s, `UPDATE tasks SET lane='ytf-lane' WHERE id=$1`, bound)
	ytfPass(t, s)
	ops := ytfOps(t, s, bound)
	if len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
	op := ops[0]
	if op.State != ytwrite.StatePrepared || op.YTTargetID != ytfIssue(bound) || op.Tenant != ytcTenant || op.Principal != YTFieldsPrincipal ||
		!strings.HasPrefix(op.Key, ytwrite.KindIssueFields+":"+strconv.FormatInt(bound, 10)+":1:") {
		t.Fatalf("op %+v", op)
	}
	p := ytfPayload(t, op)
	if p.ProjectID != ytbProjectID || p.Summary != "#"+strconv.FormatInt(bound, 10)+" renamed task (grade A, T2)" {
		t.Fatalf("payload %+v", p)
	}
	for name, want := range map[string]string{"hk 상태": "개발 중", "hk 우선순위": "2", "레인": "ytf-lane", "등급": "A", "티어": "T2", "hk 프로젝트": ytbProject, "PR": ""} {
		if got := ytfField(p, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if x, ok := ytwrite.ParseFields(p.Description); !ok || x.TaskID != bound || x.Project != ytbProject {
		t.Fatalf("description %q", p.Description)
	}

	// The op rides the task's lane and the worker admits it: mode on, the
	// target is the bound issue.
	if _, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), time.Now().UTC(), op.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim: %s %v", out, err)
	}
}

func TestYTFieldsNoOpUpdateMakesNoOp(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytf_noop")
	ytFeedExec(t, s, `UPDATE tasks SET priority=3 WHERE id=$1`, bound)
	ytfPass(t, s)
	// updated_at is not part of the snapshot: the feed sees the change, the
	// snapshot is the one already prepared.
	ytFeedExec(t, s, `UPDATE tasks SET updated_at=updated_at+interval '1 second' WHERE id=$1`, bound)
	if pass := ytfPass(t, s); pass.Handled != 1 {
		t.Fatalf("pass %+v", pass)
	}
	if ops := ytfOps(t, s, bound); len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
}

func TestYTFieldsABAMakesThreeOps(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytf_aba")
	for _, title := range []string{"A", "B", "A"} {
		ytFeedExec(t, s, `UPDATE tasks SET title=$2 WHERE id=$1`, bound, title)
		ytfPass(t, s)
	}
	ops := ytfOps(t, s, bound)
	if len(ops) != 3 {
		t.Fatalf("ops %+v", ops)
	}
	if ops[0].PayloadSHA != ops[2].PayloadSHA || ops[0].PayloadSHA == ops[1].PayloadSHA || ops[0].Key == ops[2].Key {
		t.Fatalf("A/B/A shas %s %s %s keys %s %s", ops[0].PayloadSHA, ops[1].PayloadSHA, ops[2].PayloadSHA, ops[0].Key, ops[2].Key)
	}
	for i, op := range ops {
		if want := ":" + strconv.Itoa(i+1) + ":"; !strings.Contains(op.Key, want) {
			t.Fatalf("op %d key %q, want seq %d", i, op.Key, i+1)
		}
	}
}

func TestYTFieldsSkipsUnboundAndMovedTasks(t *testing.T) {
	s, bound, unbound := ytfSetup(t, "ytf_skip")
	ytFeedExec(t, s, `UPDATE tasks SET priority=4 WHERE id=$1`, unbound)
	if pass := ytfPass(t, s); pass.Handled != 1 {
		t.Fatalf("pass %+v", pass)
	}
	if ops := ytfOps(t, s, unbound); len(ops) != 0 {
		t.Fatalf("unbound task ops %+v", ops)
	}
	// The bound task's project now maps to another YouTrack project (Y4c).
	ytFeedExec(t, s, `UPDATE tasks SET project='ytf-elsewhere', priority=5 WHERE id=$1`, bound)
	ytfPass(t, s)
	if ops := ytfOps(t, s, bound); len(ops) != 0 {
		t.Fatalf("moved task ops %+v", ops)
	}
	// A binding on another tenant is not this worker's.
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytbProject)
	ytFeedExec(t, s, `UPDATE yt_task_bindings SET tenant='other.youtrack.cloud' WHERE task_id=$1`, bound)
	ytfPass(t, s)
	if ops := ytfOps(t, s, bound); len(ops) != 0 {
		t.Fatalf("other-tenant ops %+v", ops)
	}
}

func TestYTFieldsModeOff(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytf_off")
	ytFeedExec(t, s, `UPDATE tasks SET priority=6 WHERE id=$1`, bound)
	ytfPass(t, s)
	ops := ytfOps(t, s, bound)
	if len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
	// Off: the worker admits nothing and the consumer prepares nothing,
	// while its cursor keeps up.
	s.SetYTFieldsMode(YTFieldsOff, "")
	if _, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), time.Now().UTC(), ops[0].ID); err != nil || out != ytwrite.ClaimHeld {
		t.Fatalf("claim in mode off: %s %v", out, err)
	}
	if due, err := s.YTTenantLedger(ytcTenant).Due(t.Context(), time.Now().UTC().Add(time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("due in mode off: %+v %v", due, err)
	}
	ytFeedExec(t, s, `UPDATE tasks SET priority=7 WHERE id=$1`, bound)
	if pass := ytfPass(t, s); pass.Handled != 1 || pass.Cursor != ytFeedMaxSyncID(t, s) {
		t.Fatalf("pass %+v", pass)
	}
	if ops := ytfOps(t, s, bound); len(ops) != 1 {
		t.Fatalf("mode off prepared %+v", ops)
	}
	// On again, an op whose target is not the bound issue is held.
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	ytFeedExec(t, s, `UPDATE yt_task_bindings SET issue_id='2-rebound' WHERE task_id=$1`, bound)
	if _, out, err := s.YTTenantLedger(ytcTenant).Claim(t.Context(), time.Now().UTC(), ops[0].ID); err != nil || out != ytwrite.ClaimHeld {
		t.Fatalf("claim of a rebound task's op: %s %v", out, err)
	}
}

// A task with no project is bound to the import's bucket, a YouTrack project
// no youtrack_projects row maps: that is no move, and its hk 프로젝트 is
// Unassigned as the import wrote it.
func TestYTFieldsTaskWithoutProject(t *testing.T) {
	s, _, _ := ytfSetup(t, "ytf_noproject")
	task := ytcTask(t, s, ytbProject)
	ytFeedExec(t, s, `UPDATE tasks SET project=NULL WHERE id=$1`, task)
	ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at) VALUES ($1,$2,$3,'0-99','test',now())`,
		task, ytcTenant, ytfIssue(task))
	ytFeedExec(t, s, `UPDATE tasks SET priority=8 WHERE id=$1`, task)
	ytfPass(t, s)
	ops := ytfOps(t, s, task)
	if len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
	if p := ytfPayload(t, ops[0]); p.ProjectID != "0-99" || ytfField(p, "hk 프로젝트") != ytwrite.UnassignedProject {
		t.Fatalf("payload %+v", p)
	}
	// Bound to a mapped project, the same task is a move.
	ytFeedExec(t, s, `UPDATE yt_task_bindings SET project_id=$2 WHERE task_id=$1`, task, ytbProjectID)
	ytFeedExec(t, s, `UPDATE tasks SET priority=9 WHERE id=$1`, task)
	ytfPass(t, s)
	if ops := ytfOps(t, s, task); len(ops) != 1 {
		t.Fatalf("ops after the move %+v", ops)
	}
}

// Applying a task's issue_create publishes its binding, which is no task
// change: the first snapshot is prepared in the same transaction, behind the
// issue_create on the task's lane, with the task as it is then (hk 4003
// review). Mode off prepares none.
func TestYTFieldsSnapshotOnIssueCreateApply(t *testing.T) {
	s, _, _ := ytfSetup(t, "ytf_onbind")
	task := ytcTask(t, s, ytbProject)
	// A change consumed while the task is unbound prepares nothing.
	ytFeedExec(t, s, `UPDATE tasks SET state='developing' WHERE id=$1`, task)
	ytfPass(t, s)
	if ops := ytfOps(t, s, task); len(ops) != 0 {
		t.Fatalf("unbound ops %+v", ops)
	}
	op := ytiPrepare(t, s, task, ytcTenant)
	ytiDispatch(t, s, op)
	if _, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, "2-onbind", ytiMaps(op, "2-onbind")); err != nil {
		t.Fatal(err)
	}
	ops := ytfOps(t, s, task)
	if len(ops) != 1 || ops[0].ID <= op.ID || ops[0].YTTargetID != "2-onbind" || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("ops after the apply %+v (issue_create %d)", ops, op.ID)
	}
	if p := ytfPayload(t, ops[0]); ytfField(p, "hk 상태") != "개발 중" || p.ProjectID != ytbProjectID {
		t.Fatalf("payload %+v", p)
	}
	// The next pass sees no change of the task: nothing more.
	ytfPass(t, s)
	if ops := ytfOps(t, s, task); len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}

	s.SetYTFieldsMode(YTFieldsOff, "")
	other := ytcTask(t, s, ytbProject)
	op = ytiPrepare(t, s, other, ytcTenant)
	ytiDispatch(t, s, op)
	if _, err := s.YTWriteLedger().Apply(t.Context(), time.Now().UTC(), op.ID, "2-onbind-off", ytiMaps(op, "2-onbind-off")); err != nil {
		t.Fatal(err)
	}
	if ops := ytfOps(t, s, other); len(ops) != 0 {
		t.Fatalf("mode off ops %+v", ops)
	}
}

// A binding published by the comments backfill has no issue_create apply to
// trigger the initial snapshot, so AddYTTaskBinding must prepare it itself.
func TestYTFieldsSnapshotOnTaskBinding(t *testing.T) {
	s := schemaStore(t, "ytf_binding")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET state='developing' WHERE id=$1`, task); err != nil {
		t.Fatal(err)
	}
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	if _, err := ytbAdd(t, s, ytbBinding(task)); err != nil {
		t.Fatal(err)
	}
	ops := ytfOps(t, s, task)
	if len(ops) != 1 || ops[0].State != ytwrite.StatePrepared || ops[0].YTTargetID != ytfIssue(task) {
		t.Fatalf("binding ops %+v", ops)
	}
	if got := ytfField(ytfPayload(t, ops[0]), "hk 상태"); got != "개발 중" {
		t.Fatalf("snapshot state = %q, want current task state", got)
	}

	offTask := ytcTask(t, s, ytbProject)
	s.SetYTFieldsMode(YTFieldsOff, "")
	if _, err := ytbAdd(t, s, ytbBinding(offTask)); err != nil {
		t.Fatal(err)
	}
	if ops := ytfOps(t, s, offTask); len(ops) != 0 {
		t.Fatalf("mode off binding ops %+v", ops)
	}
}

func TestYTFieldsSnapshotOnTaskBindingRollsBackWithBinding(t *testing.T) {
	s := schemaStore(t, "ytf_binding_rollback")
	ytbMapped(t, s)
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	task := ytcTask(t, s, ytbProject)
	// The deferred constraint confirms the snapshot was present at commit
	// time, then aborts the transaction so both rows must roll back.
	_, err := s.pool.Exec(t.Context(), `
		CREATE FUNCTION reject_test_binding_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM yt_write_ops WHERE kind='issue_fields' AND entity_kind='task' AND entity_id=NEW.task_id::text) THEN
				RAISE EXCEPTION 'issue_fields snapshot absent at binding commit';
			END IF;
			RAISE EXCEPTION 'test binding rollback';
		END $$;
		CREATE CONSTRAINT TRIGGER reject_test_binding_commit AFTER INSERT ON yt_task_bindings
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_test_binding_commit();`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ytbAdd(t, s, ytbBinding(task)); err == nil || !strings.Contains(err.Error(), "test binding rollback") {
		t.Fatalf("AddYTTaskBinding error = %v, want deferred rollback", err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_task_bindings WHERE task_id=$1`, task); n != 0 {
		t.Fatalf("binding rows after rollback = %d", n)
	}
	if ops := ytfOps(t, s, task); len(ops) != 0 {
		t.Fatalf("ops after binding rollback %+v", ops)
	}
}
