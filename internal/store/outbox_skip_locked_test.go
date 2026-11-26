package store

import (
	"context"
	"testing"
	"time"
)

// TestSkipNotificationsBeforeSettlesAroundALockedRow pins hk 6920 on
// PostgreSQL: a pre-cutoff row another transaction holds is skipped, not
// waited on, so the settle returns and settles the rest; the next call, after
// the lock is released, settles the held row.
func TestSkipNotificationsBeforeSettlesAroundALockedRow(t *testing.T) {
	s := schemaStore(t, "outboxskiplock")
	checkSettleAroundHeldRow(t, s, func(ctx context.Context, event string) (func(), error) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var id int64
		if err := tx.QueryRow(ctx, `SELECT id FROM notification_outbox WHERE event_id=$1 FOR UPDATE`, event).Scan(&id); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		return func() { _ = tx.Rollback(context.Background()) }, nil
	})
}

// checkSettleAroundHeldRow is the scenario both dialects run (hk 6920). hold
// locks the named owed row in another transaction and returns its release.
func checkSettleAroundHeldRow(t *testing.T, s *Store, hold func(context.Context, string) (func(), error)) {
	t.Helper()
	ctx := t.Context()
	cutoff := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	insertOutboxRow(t, s, "skip-lock-held", cutoff.Add(-time.Hour))
	insertOutboxRow(t, s, "skip-lock-a", cutoff.Add(-time.Hour))
	insertOutboxRow(t, s, "skip-lock-b", cutoff.Add(-time.Minute))
	insertOutboxRow(t, s, "skip-lock-live", cutoff)

	release, err := hold(ctx, "skip-lock-held")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	// The settle returns inside its bound (a wait would hit the deadline and
	// fail) and settles the two pre-cutoff rows it can lock.
	bound, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	settled, err := s.SkipNotificationsBefore(bound, cutoff, OutboxSkipBeforeForwarder)
	if err != nil || settled != 2 {
		t.Fatalf("settle around a held row = %d err=%v, want 2 and no error", settled, err)
	}
	if !outboxSkipped(t, s, "skip-lock-a") || !outboxSkipped(t, s, "skip-lock-b") {
		t.Fatal("unlocked pre-cutoff rows were not settled in the same call")
	}
	if outboxSkipped(t, s, "skip-lock-held") {
		t.Fatal("the held row was settled while another transaction held it")
	}
	if outboxSkipped(t, s, "skip-lock-live") {
		t.Fatal("a row at the cutoff was settled")
	}

	// Once released, the next call settles the held row, and only it.
	release()
	if settled, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || settled != 1 {
		t.Fatalf("settle after release = %d err=%v, want 1 and no error", settled, err)
	}
	if !outboxSkipped(t, s, "skip-lock-held") {
		t.Fatal("the held row was not settled after its lock was released")
	}
	if again, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || again != 0 {
		t.Fatalf("second settle after release = %d err=%v, want 0", again, err)
	}
}

// insertOutboxRow writes one owed notification row created at at.
func insertOutboxRow(t *testing.T, s *Store, event string, at time.Time) {
	t.Helper()
	q := s.q(
		`INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES($1,$2,$3,$4,$5)`,
		`INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES(?,?,?,?,?)`)
	if _, err := s.db().Exec(t.Context(), q, OutboxKindDotsReport, "skip-lock-lane", event, "text", at); err != nil {
		t.Fatal(err)
	}
}

// outboxSkipped reports whether the row for event is settled as skipped.
func outboxSkipped(t *testing.T, s *Store, event string) bool {
	t.Helper()
	q := s.q(
		`SELECT COUNT(*) FROM notification_outbox WHERE event_id=$1 AND skipped_at IS NOT NULL`,
		`SELECT COUNT(*) FROM notification_outbox WHERE event_id=? AND skipped_at IS NOT NULL`)
	var n int64
	if err := s.db().QueryRow(t.Context(), q, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}
