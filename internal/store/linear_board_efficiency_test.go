package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"testing"
	"time"
)

func TestLinearBoardWebhookIsOnlyActiveLinkWriter(t *testing.T) {
	_, url := testdb.Schema(t, "board_webhook_links")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := linearTask(t, st, TaskRefs{})
	id := LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", task.ID))
	if err = st.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error {
		reserved, err := lease.ReserveBoard(t.Context(), id, task.ID)
		if !reserved || err != nil {
			t.Fatal(reserved, err)
		}
		return lease.AwaitBoardWebhook(t.Context(), id)
	}); err != nil {
		t.Fatal(err)
	}
	if _, linked, err := st.GetLinearIssue(t.Context(), task.ID); err != nil || linked {
		t.Fatal("outbound mapped", linked, err)
	}
	at := time.Now().UTC()
	event := LinearBoardIssueEvent{IssueID: id, Identifier: "ROB-321", Description: fmt.Sprintf("<!-- work-board:task:%d -->", task.ID), At: at}
	if err = st.RecordLinearBoardIssue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if got, linked, err := st.GetLinearIssue(t.Context(), task.ID); err != nil || !linked || got.IssueID != id || got.Identifier != "ROB-321" {
		t.Fatal(got, linked, err)
	}
	if pending, err := st.HasLinearCreateReservation(t.Context(), id); err != nil || pending {
		t.Fatal(pending, err)
	}
	late := event
	late.At = at.Add(-time.Minute)
	late.Identifier = "ROB-111"
	if err = st.RecordLinearBoardIssue(t.Context(), late); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetLinearIssue(t.Context(), task.ID); got.Identifier != "ROB-321" {
		t.Fatal("late snapshot rollback", got)
	}
	event.Inactive = true
	event.At = at.Add(time.Second)
	if err = st.RecordLinearBoardIssue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if _, linked, err := st.GetLinearIssue(t.Context(), task.ID); err != nil || linked {
		t.Fatal("inactive mapped", linked, err)
	}
	if stopped, err := st.LinearBoardDeliveryStopped(t.Context(), task.ID); err != nil || !stopped {
		t.Fatal(stopped, err)
	}
	event.Inactive = false
	event.At = at.Add(2 * time.Second)
	if err = st.RecordLinearBoardIssue(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if _, linked, _ := st.GetLinearIssue(t.Context(), task.ID); linked {
		t.Fatal("inactive issue automatically remapped")
	}
}
func TestLinearBoardMetadataDurableBootstrapAndLabelFailure(t *testing.T) {
	_, url := testdb.Schema(t, "board_metadata")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().UTC()
	fetches := 0
	fetch := func(context.Context) ([]LinearMetadata, error) {
		fetches++
		return []LinearMetadata{{Kind: "team", ID: "t", Name: "ROB", UpdatedAt: at}, {Kind: "state", ID: "s", TeamID: "t", Name: "Old", UpdatedAt: at}}, nil
	}
	for range 2 {
		if err = st.EnsureLinearBoardMetadata(t.Context(), fetch); err != nil {
			t.Fatal(err)
		}
	}
	if fetches != 1 {
		t.Fatal(fetches)
	}
	newer := LinearMetadata{Kind: "state", ID: "s", TeamID: "t", Name: "New", UpdatedAt: at.Add(time.Second)}
	if err = st.ApplyLinearMetadata(t.Context(), []LinearMetadata{newer}); err != nil {
		t.Fatal(err)
	}
	if err = st.RefreshLinearBoardMetadata(t.Context(), fetch); err != nil {
		t.Fatal(err)
	}
	items, err := st.LinearBoardMetadata(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == "s" && item.Name != "New" {
			t.Fatal("refresh rolled newer webhook back", item)
		}
	}
	creates := 0
	create := func(context.Context) (LinearMetadata, error) {
		creates++
		return LinearMetadata{}, ErrLinearBoardLabelUnavailable
	}
	for range 10 {
		if _, err = st.ResolveLinearBoardLabel(t.Context(), "t", "fleet", create); !errors.Is(err, ErrLinearBoardLabelUnavailable) {
			t.Fatal(err)
		}
	}
	if creates != 1 {
		t.Fatal("per-task label creation", creates)
	}
	if err = st.ApplyLinearMetadata(t.Context(), []LinearMetadata{{Kind: "label", ID: "label", TeamID: "t", Name: "fleet", UpdatedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if id, err := st.ResolveLinearBoardLabel(t.Context(), "t", "fleet", create); err != nil || id != "label" {
		t.Fatal(id, err)
	}
}
func TestLinearBoardEfficiencyMigrationCollapsesOnceAndIndexesBothKeys(t *testing.T) {
	_, url := testdb.Schema(t, "board_efficiency_migration")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tasks := []Task{linearTask(t, st, TaskRefs{}), linearTask(t, st, TaskRefs{}), linearTask(t, st, TaskRefs{})}
	for _, sql := range []string{
		`DROP TABLE linear_metadata,linear_metadata_state,linear_label_failures,linear_issue_link_conflicts,linear_issue_webhook_versions,linear_board_stopped_tasks`,
		`DROP INDEX linear_issues_issue_id,linear_issues_identifier`,
		`ALTER TABLE linear_create_admissions DROP COLUMN board_task_id,DROP COLUMN awaiting_webhook`,
		`DELETE FROM schema_version WHERE version=32`,
	} {
		if _, err = st.pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	for i, task := range tasks {
		if _, err = st.pool.Exec(t.Context(), `INSERT INTO linear_issues VALUES($1,'shared','ROB-1',now(),now()+$2::int*interval '1 second')`, task.ID, i); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := st.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = enqueueLinearOutboxTx(t.Context(), tx, tasks[0].ID, LinearOpWorkBoard, LinearOutboxPayload{}); err != nil {
			t.Fatal(err)
		}
	}
	if err = enqueueLinearOutboxTx(t.Context(), tx, tasks[0].ID, LinearOpIssueState, LinearOutboxPayload{}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = st.migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var active, quarantined, pending, skipped, other int
	if err = st.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM linear_issues),(SELECT count(*) FROM linear_issue_link_conflicts),(SELECT count(*) FROM linear_outbox WHERE op='work_board' AND state='pending'),(SELECT count(*) FROM linear_outbox WHERE op='work_board' AND state='skipped'),(SELECT count(*) FROM linear_outbox WHERE op='issue_state' AND state='pending')`).Scan(&active, &quarantined, &pending, &skipped, &other); err != nil || active != 1 || quarantined != 2 || pending != 1 || skipped != 2 || other != 1 {
		t.Fatal(active, quarantined, pending, skipped, other, err)
	}
	for _, sql := range []string{`INSERT INTO linear_issues VALUES($1,'shared','ROB-2',now(),now())`, `INSERT INTO linear_issues VALUES($1,'other','ROB-1',now(),now())`} {
		if _, err = st.pool.Exec(t.Context(), sql, tasks[0].ID); err == nil {
			t.Fatal("unique reverse index missing", sql)
		}
	}
	rows, err := st.ListLinearOutbox(t.Context(), tasks[0].ID)
	if err != nil || rows[2].State != "pending" {
		t.Fatal(rows, err)
	}
}
