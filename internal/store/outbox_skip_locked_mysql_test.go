//go:build mysql

package store

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestSkipNotificationsBeforeSettlesAroundALockedRowMySQL is the MySQL leg of
// hk 6920: the settle locks its rows with SKIP LOCKED, so a row another
// transaction holds is left for a later call instead of stalling the tick.
func TestSkipNotificationsBeforeSettlesAroundALockedRowMySQL(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	checkSettleAroundHeldRow(t, s, func(ctx context.Context, event string) (func(), error) {
		tx, err := s.mysqlPool.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM notification_outbox WHERE event_id=? FOR UPDATE`, event).Scan(&id); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		return func() { _ = tx.Rollback() }, nil
	})
}
