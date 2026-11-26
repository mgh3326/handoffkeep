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
// unsent poll. Settlement must use notification_outbox_unsent_poll to range
// over the cutoff; the poll must use its id-ordered index to avoid filesort.
// The poll itself is read through ListUnsentNotifications, so its rows are
// checked as well.
func TestOutboxUnsentPollUsesIndex(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	cutoff := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	settledAt := cutoff.Add(-time.Hour)
	// One unsent row committed below the cutoff, inserted first so it holds the
	// lowest id: the live poll must exclude it, and the first settle must
	// settle it (hk 6912).
	if _, err := s.db().Exec(ctx, `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES(?,?,?,?,?)`,
		OutboxKindDotsReport, "plan-lane", "plan-late-pre-cutoff", "text", cutoff.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	const n = 20000
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
			case i%40 == 39:
				// During an outage, all live rows are at or after the cutoff.
				// Reverse their created_at order so id order cannot be inferred
				// from the range index.
				created = cutoff.Add(time.Duration(n-i) * time.Second)
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
		cutoff, 200); key != "notification_outbox_unsent_poll_order" {
		t.Errorf("unsent poll uses %q, want notification_outbox_unsent_poll_order", key)
	}
	if extra := relayPlanExtra(t, ctx, s, "notification_outbox",
		`SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE sent_at IS NULL AND skipped_at IS NULL AND created_at >= ? ORDER BY id ASC LIMIT ?`,
		cutoff, 200); strings.Contains(extra, "filesort") {
		t.Errorf("unsent poll plan = %q, want no filesort", extra)
	}

	// The poll the forwarder runs returns only the live rows at or after the
	// cutoff, in id order.
	got, err := s.ListUnsentNotifications(ctx, 200, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 {
		t.Fatalf("unsent rows = %d, want one 200-row poll batch", len(got))
	}
	for i, row := range got {
		if row.CreatedAt.Before(cutoff) || row.SentAt != nil || row.SkippedAt != nil {
			t.Fatalf("row %d = %+v, want a live row at or after the cutoff", i, row)
		}
		if i > 0 && got[i-1].ID >= row.ID {
			t.Fatalf("unsent rows not in id order: %d then %d", got[i-1].ID, row.ID)
		}
	}

	// The first settle settles the one pre-cutoff row; the second finds none.
	if settled, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || settled != 1 {
		t.Fatalf("settle = %d err = %v, want 1", settled, err)
	}
	if again, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || again != 0 {
		t.Fatalf("second settle = %d err = %v, want 0", again, err)
	}
}
