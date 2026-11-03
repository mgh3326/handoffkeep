package store

import (
	"context"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"testing"
	"time"
)

func TestR1LabelLookupDoesNotWaitForSlowCreate(t *testing.T) {
	_, url := testdb.Schema(t, "r1_label_concurrency")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Now().UTC()
	if err = st.ApplyLinearMetadata(t.Context(), []LinearMetadata{{Kind: "label", ID: "known", TeamID: "team", Name: "known", UpdatedAt: at}}); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := st.ResolveLinearBoardLabel(t.Context(), "team", "slow", func(context.Context) (LinearMetadata, error) {
			close(entered)
			<-release
			return LinearMetadata{Kind: "label", ID: "slow", TeamID: "team", Name: "slow", UpdatedAt: at}, nil
		})
		done <- err
	}()
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("creator did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	id, err := st.ResolveLinearBoardLabel(ctx, "team", "known", func(context.Context) (LinearMetadata, error) {
		t.Error("known label called Linear")
		return LinearMetadata{}, nil
	})
	if err != nil || id != "known" {
		t.Fatal("known metadata waited on another label's I/O", id, err)
	}
	id, err = st.ResolveLinearBoardLabel(ctx, "team", "other", func(context.Context) (LinearMetadata, error) {
		return LinearMetadata{Kind: "label", ID: "other", TeamID: "team", Name: "other", UpdatedAt: at}, nil
	})
	if err != nil || id != "other" {
		t.Fatal("other label serialized on slow I/O", id, err)
	}
}
func TestR1LegacyBoardReservationBackfillAndWebhook(t *testing.T) {
	_, url := testdb.Schema(t, "r1_legacy_reservation")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := linearTask(t, st, TaskRefs{})
	id := LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", task.ID))
	if _, err = st.pool.Exec(t.Context(), `INSERT INTO linear_create_admissions(issue_id,created_at) VALUES($1,now())`, id); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`DROP TABLE IF EXISTS linear_metadata,linear_metadata_state,linear_label_attempts,linear_label_failures,linear_issue_link_conflicts,linear_issue_webhook_versions,linear_board_stopped_tasks`,
		`DROP INDEX linear_issues_issue_id,linear_issues_identifier`,
		`ALTER TABLE linear_create_admissions DROP COLUMN board_task_id,DROP COLUMN awaiting_webhook`,
		`DELETE FROM schema_version WHERE version=32`,
	} {
		if _, err = st.pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err = st.migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var owner *int64
	if err = st.pool.QueryRow(t.Context(), `SELECT board_task_id FROM linear_create_admissions WHERE issue_id=$1`, id).Scan(&owner); err != nil || owner == nil || *owner != task.ID {
		t.Fatal("legacy reservation has no owner", owner, err)
	}
	// The old writer did not include the newer description marker. The matched
	// stable reservation itself is enough to route its signed webhook.
	if err = st.RecordLinearBoardIssue(t.Context(), LinearBoardIssueEvent{IssueID: id, Identifier: "ROB-789", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.GetLinearIssue(t.Context(), task.ID); err != nil || !found {
		t.Fatal("legacy webhook could not acknowledge reservation", found, err)
	}
	if pending, err := st.HasLinearCreateReservation(t.Context(), id); err != nil || pending {
		t.Fatal(pending, err)
	}
}
func TestR1AbsentReservationReleaseHasGraceAndBooksUnknownSlots(t *testing.T) {
	_, url := testdb.Schema(t, "r1_reservation_grace")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := LinearDecisionUUID("r1-unknown")
	if err = st.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error {
		if err := lease.Reserve(t.Context(), id); err != nil {
			return err
		}
		repair, ok := any(lease).(interface {
			ReleaseAbsent(context.Context, string) error
			ReservedCreates(context.Context) (int, error)
		})
		if !ok {
			t.Fatal("reservation has no bounded absence repair")
		}
		if count, err := repair.ReservedCreates(t.Context()); err != nil || count != 1 {
			t.Fatal("unknown outcome lost its capacity slot", count, err)
		}
		if err := repair.ReleaseAbsent(t.Context(), id); err == nil {
			t.Fatal("fresh in-flight outcome released")
		}
		if _, err := st.pool.Exec(t.Context(), `UPDATE linear_create_admissions SET created_at=now()-interval '3 minutes' WHERE issue_id=$1`, id); err != nil {
			return err
		}
		if err := repair.ReleaseAbsent(t.Context(), id); err != nil {
			return err
		}
		count, err := repair.ReservedCreates(t.Context())
		if err != nil || count != 0 {
			t.Fatal(count, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
