//go:build mysql

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestOutboxUnsentPollUsesIndex seeds notification_outbox at production shape
// (hk 6398: about 890 rows, most of them settled) and checks the two
// statements the hub forwarder runs every 5 s: the pre-cutoff settle and the
// unsent poll. Each must use notification_outbox_unsent_poll instead of
// walking the whole table. The poll itself is read through
// ListUnsentNotifications, so its rows are checked as well.
func TestOutboxUnsentPollUsesIndex(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	cutoff := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	settledAt := cutoff.Add(-time.Hour)
	const n = 2000
	const batch = 500
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)
		var args []any
		for i := start; i < end; i++ {
			created := cutoff.Add(-time.Hour)
			var sent, skipped any
			var reason any
			switch {
			case i%40 < 36:
				sent = settledAt
			case i%40 < 39:
				skipped = settledAt
				reason = OutboxSkipBeforeForwarder
			case (i/40)%2 == 0:
				// Half the live rows queue at or after the cutoff.
				created = cutoff.Add(time.Hour)
			}
			args = append(args, OutboxKindDotsReport, "plan-lane", fmt.Sprintf("plan-evt-%d", i), "text", created, sent, skipped, reason)
		}
		q := `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at,sent_at,skipped_at,skip_reason) VALUES ` +
			strings.TrimSuffix(strings.Repeat(`(?,?,?,?,?,?,?,?),`, end-start), ",")
		if _, err := s.db().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}

	// Fresh statistics: the planner reads the index cardinalities, not the
	// rows just inserted. ANALYZE TABLE answers a result set, so it is read
	// and closed.
	ar, err := s.db().Query(ctx, `ANALYZE TABLE notification_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	ar.Close()

	if key := relayPlanKey(t, ctx, s, "notification_outbox",
		`UPDATE notification_outbox SET skipped_at=UTC_TIMESTAMP(6),skip_reason=? WHERE created_at < ? AND sent_at IS NULL AND skipped_at IS NULL`,
		OutboxSkipBeforeForwarder, cutoff); key != "notification_outbox_unsent_poll" {
		t.Errorf("pre-cutoff settle uses %q, want notification_outbox_unsent_poll", key)
	}
	if key := relayPlanKey(t, ctx, s, "notification_outbox",
		`SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE sent_at IS NULL AND skipped_at IS NULL AND created_at >= ? ORDER BY id ASC LIMIT ?`,
		cutoff, 200); key != "notification_outbox_unsent_poll" {
		t.Errorf("unsent poll uses %q, want notification_outbox_unsent_poll", key)
	}

	// The poll the forwarder runs returns only the live rows at or after the
	// cutoff, in id order.
	got, err := s.ListUnsentNotifications(ctx, 200, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 25 {
		t.Fatalf("unsent rows = %d, want 25 live rows at or after the cutoff", len(got))
	}
	for i, row := range got {
		if row.CreatedAt.Before(cutoff) || row.SentAt != nil || row.SkippedAt != nil {
			t.Fatalf("row %d = %+v, want a live row at or after the cutoff", i, row)
		}
		if i > 0 && got[i-1].ID >= row.ID {
			t.Fatalf("unsent rows not in id order: %d then %d", got[i-1].ID, row.ID)
		}
	}

	// The settle picks up the 25 live rows before the cutoff, then finds
	// nothing.
	if settled, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || settled != 25 {
		t.Fatalf("settle = %d err = %v, want 25", settled, err)
	}
	if again, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || again != 0 {
		t.Fatalf("second settle = %d err = %v, want 0", again, err)
	}
}
