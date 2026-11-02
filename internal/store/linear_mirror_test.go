package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestLinearMirrorAdmissionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want string
	}{
		{"unset priority", Task{}, ""},
		{"negative priority", Task{Priority: -1}, ""},
		{"priority 1", Task{Priority: 1}, "priority"},
		{"priority 2", Task{Priority: 2}, "priority"},
		{"ordinary priority 3", Task{Priority: 3}, ""},
		{"claimed placement title and linked metadata", Task{Priority: 3, State: "claimed", Title: "ROB-123 dots decision", Refs: TaskRefs{JobID: "job", PR: "repo#1", Linear: &TaskLinear{IssueID: "old", Identifier: "ROB-123"}}}, ""},
		{"dots", Task{Priority: 3, Refs: TaskRefs{Linear: &TaskLinear{MirrorReason: LinearMirrorDots}}}, LinearMirrorDots},
		{"operator visible", Task{Refs: TaskRefs{Linear: &TaskLinear{MirrorReason: LinearMirrorOperatorVisible}}}, LinearMirrorOperatorVisible},
		{"opened from Linear", Task{Refs: TaskRefs{Linear: &TaskLinear{MirrorReason: LinearMirrorOpened}}}, LinearMirrorOpened},
		{"explicit legacy opt-in", Task{Refs: TaskRefs{Linear: &TaskLinear{Sync: true}}}, LinearMirrorOperatorVisible},
		{"open decision", Task{Refs: TaskRefs{DecisionRequest: &DecisionRequest{Status: DecisionRequestOpen}}}, "decision"},
		{"closed decision", Task{Refs: TaskRefs{DecisionRequest: &DecisionRequest{Status: "answered"}}}, ""},
		{"terminal priority", Task{Priority: 1, State: "merged"}, ""},
		{"terminal request", Task{State: "dropped", Refs: TaskRefs{Linear: &TaskLinear{MirrorReason: LinearMirrorDots}}}, ""},
		{"unknown reason", Task{Refs: TaskRefs{Linear: &TaskLinear{MirrorReason: "from title"}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LinearMirrorReason(tc.task); got != tc.want {
				t.Fatalf("reason=%q want %q", got, tc.want)
			}
		})
	}
	if validTaskLinear(TaskLinear{MirrorReason: "unrecognized"}) {
		t.Fatal("unrecognized policy accepted")
	}
	old := TaskRefs{Linear: &TaskLinear{MirrorReason: LinearMirrorDots}}
	if got := mergeTaskRefs(old, TaskRefs{Linear: &TaskLinear{Report: "report/a"}}); got.Linear.MirrorReason != LinearMirrorDots {
		t.Fatal("partial patch erased policy")
	}
}

func TestLinearMirrorAdmissionPersistsProjectionWithoutDelivery(t *testing.T) {
	_, url := testdb.Schema(t, "mirror_admission")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	st.EnableLinearBoard()
	task, err := st.CreateTask(t.Context(), Task{Lane: "test-mirror", Title: "Internal task ROB-123", Kind: "implement", Priority: 3, CreatedBy: "test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClaimTask(t.Context(), task.ID, "test", "test-mirror-job", ""); err != nil {
		t.Fatal(err)
	}
	if err = st.ReconcileLinearBoard(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("ineligible outbox=%v err=%v", rows, err)
	}
	var count int
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM linear_board_tasks WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	// Existing links can still be maintained without creating replacement issues.
	if _, err = st.pool.Exec(t.Context(), `INSERT INTO linear_issues VALUES($1,'existing','ROB-123',now(),now())`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask(t.Context(), task.ID, "in_progress", "test", "continue", nil, ""); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}

func TestLinearMirrorAdmissionSerializesAndRetainsReservations(t *testing.T) {
	_, url := testdb.Schema(t, "mirror_serial")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	second, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.SerializeLinearCreate(context.Background(), func(lease LinearCreateLease) error {
			if err := lease.Reserve(context.Background(), "uncertain"); err != nil {
				return err
			}
			close(entered)
			<-release
			return errors.New("lost response")
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("first admission: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err = second.SerializeLinearCreate(ctx, func(LinearCreateLease) error { t.Error("concurrent create entered lock"); return nil })
	cancel()
	if err == nil {
		t.Fatal("second create not serialized")
	}
	close(release)
	if err = <-done; err == nil {
		t.Fatal("lost response missing")
	}
	if err = st.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error {
		ids, err := lease.Pending(t.Context())
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != "uncertain" {
			return fmt.Errorf("reservation lost: %v", ids)
		}
		return lease.Complete(t.Context(), "uncertain")
	}); err != nil {
		t.Fatal(err)
	}
	if err = st.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error {
		if err := lease.Reserve(t.Context(), "rejected"); err != nil {
			return err
		}
		return lease.Reject(t.Context(), "rejected")
	}); err != nil {
		t.Fatal(err)
	}
	if err = second.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error {
		known, err := lease.RejectionKnown(t.Context(), "rejected")
		if err != nil {
			return err
		}
		if !known {
			return fmt.Errorf("definite rejection was not durable")
		}
		return lease.Complete(t.Context(), "rejected")
	}); err != nil {
		t.Fatal(err)
	}

}

func TestLinearMirrorAdmissionSkipsLegacyArchive(t *testing.T) {
	_, url := testdb.Schema(t, "skip_legacy_archive")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	task := linearTask(t, st, TaskRefs{})
	tx, err := st.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = enqueueLinearOutboxTx(t.Context(), tx, task.ID, LinearOpTerminalArchive, LinearOutboxPayload{}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	found, err := st.ProcessNextLinearOutbox(t.Context(), func(context.Context, LinearOutbox) (LinearOutboxResult, error) {
		t.Fatal("legacy immediate archive reached callback")
		return LinearOutboxResult{}, nil
	})
	if err != nil || !found {
		t.Fatal(found, err)
	}
	rows, err := st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 1 || rows[0].State != "skipped" {
		t.Fatal(rows, err)
	}
}

func TestLinearMirrorAdmissionMigrationFromVersion30(t *testing.T) {
	_, url := testdb.Schema(t, "admission_upgrade")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err = st.pool.Exec(t.Context(), `DROP TABLE linear_create_admissions,linear_board_owners; DELETE FROM schema_version WHERE version=31`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = st.migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error { return lease.Reserve(t.Context(), "after-upgrade") }); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = st.pool.QueryRow(t.Context(), `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
}

func TestLinearMirrorAdmissionOwnershipAndVersions(t *testing.T) {
	_, url := testdb.Schema(t, "mirror_ownership")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	owner, other := linearTask(t, st, TaskRefs{}), linearTask(t, st, TaskRefs{})
	if err = st.EnsureLinearBoardOwnership(t.Context(), owner.ID, "owned-issue", "ROB-1"); err != nil {
		t.Fatal(err)
	}
	if err = st.EnsureLinearBoardOwnership(t.Context(), owner.ID, "owned-issue", "ROB-1"); err != nil {
		t.Fatal("idempotent ownership", err)
	}
	if err = st.EnsureLinearBoardOwnership(t.Context(), other.ID, "owned-issue", "ROB-1"); !errors.Is(err, ErrLinearIssueOwnership) {
		t.Fatal("reassigned issue", err)
	}
	if err = st.EnsureLinearBoardOwnership(t.Context(), owner.ID, "another-issue", "ROB-2"); !errors.Is(err, ErrLinearIssueOwnership) {
		t.Fatal("reassigned task", err)
	}
	st.EnableLinearBoard()
	if _, err = st.ClaimTask(t.Context(), owner.ID, "test", "owner-job", ""); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"in_progress", "verifying"} {
		if _, err = st.TransitionTask(t.Context(), owner.ID, state, "test", "", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.ListLinearOutbox(t.Context(), owner.ID)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	for i, row := range rows {
		if row.Payload.Board.Version != int64(i+1) {
			t.Fatalf("version %d: %+v", i, row)
		}
	}
	latest, found, err := st.GetLinearBoard(t.Context(), owner.ID)
	if err != nil || !found || latest.Version != 3 || latest.State != "In Review" {
		t.Fatal(latest, found, err)
	}
	// Preserve old duplicate mappings as evidence, but refuse using either owner.
	if _, err = st.pool.Exec(t.Context(), `INSERT INTO linear_issues VALUES($1,'owned-issue','ROB-1',now(),now())`, other.ID); err != nil {
		t.Fatal(err)
	}
	if err = st.EnsureLinearBoardOwnership(t.Context(), owner.ID, "owned-issue", "ROB-1"); !errors.Is(err, ErrLinearIssueOwnership) {
		t.Fatal("legacy duplicate accepted", err)
	}
}
