package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestR2BoardGraceExpiresAndNonBoardRejectionsStillRelease(t *testing.T) {
	_, url := testdb.Schema(t, "r2_board_grace_expiry")
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, awaiting := range []bool{false, true} {
		for _, rejected := range []bool{false, true} {
			t.Run(fmt.Sprintf("awaiting=%t/rejected=%t", awaiting, rejected), func(t *testing.T) {
				task := linearTask(t, st, TaskRefs{})
				id := LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", task.ID))
				if err := st.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error {
					if ok, err := lease.ReserveBoard(t.Context(), id, task.ID); err != nil || !ok {
						return fmt.Errorf("reserve: %t, %v", ok, err)
					}
					if awaiting {
						if err := lease.AwaitBoardWebhook(t.Context(), id); err != nil {
							return err
						}
					}
					if rejected {
						if err := lease.Reject(t.Context(), id); err != nil {
							return err
						}
					}
					if err := lease.ReleaseAbsent(t.Context(), id); !errors.Is(err, ErrLinearReservationTooRecent) {
						t.Fatalf("fresh board reservation lost grace: %v", err)
					}
					if _, err := st.pool.Exec(t.Context(), `UPDATE linear_create_admissions SET created_at=clock_timestamp()-interval '3 minutes' WHERE issue_id=$1`, id); err != nil {
						return err
					}
					return lease.ReleaseAbsent(t.Context(), id)
				}); err != nil {
					t.Fatal(err)
				}
				if found, err := st.HasLinearCreateReservation(t.Context(), id); err != nil || found {
					t.Fatal("old reservation cannot be repaired", found, err)
				}
			})
		}
	}
	if err := st.SerializeLinearCreate(t.Context(), func(lease LinearCreateLease) error {
		const id = "known-non-board-rejection"
		if err := lease.Reserve(t.Context(), id); err != nil {
			return err
		}
		if err := lease.Reject(t.Context(), id); err != nil {
			return err
		}
		return lease.ReleaseAbsent(t.Context(), id)
	}); err != nil {
		t.Fatal("known non-board refusal acquired unnecessary grace", err)
	}
}
