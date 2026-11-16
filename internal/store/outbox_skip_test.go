package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSkipNotificationsBeforeSettlesOnlyTheOldUnsentRows pins the backlog
// rule of hk 4986: forwarding starts at a cutoff and settles every unsent row
// older than it as skipped, leaving newer rows and rows the hub already
// received alone. Skipping is idempotent, and a skipped row can never gain
// sent_at: it was settled without being sent.
func TestSkipNotificationsBeforeSettlesOnlyTheOldUnsentRows(t *testing.T) {
	s, _ := schemaStoreAt(t, "outboxskip")
	ctx := t.Context()
	cutoff := time.Now().UTC().Truncate(time.Second)
	for _, row := range []struct {
		event string
		at    time.Time
		sent  bool
	}{
		{"skip-test-old", cutoff.Add(-time.Hour), false},
		{"skip-test-sent", cutoff.Add(-time.Hour), true},
		{"skip-test-edge", cutoff, false},
	} {
		var err error
		if row.sent {
			_, err = s.pool.Exec(ctx, `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at,sent_at,hub_row_id) VALUES('dots_report','skip-lane',$1,'t',$2,now(),7)`, row.event, row.at)
		} else {
			_, err = s.pool.Exec(ctx, `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES('dots_report','skip-lane',$1,'t',$2)`, row.event, row.at)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	skipped, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder)
	if err != nil || skipped != 1 {
		t.Fatalf("skipped=%d err=%v", skipped, err)
	}
	rows := map[string]NotificationOutbox{}
	for _, row := range outboxByPrefix(t, s.pool, "skip-test-") {
		rows[row.EventID] = row
	}
	if old, ok := rows["skip-test-old"]; !ok || old.SkippedAt == nil || old.SkipReason == nil || *old.SkipReason != OutboxSkipBeforeForwarder {
		t.Fatalf("old row = %+v, want skipped before the cutoff", old)
	} else if old.SentAt != nil || old.HubRowID != nil {
		t.Fatalf("a skipped row gained a receipt: %+v", old)
	}
	if sent, ok := rows["skip-test-sent"]; !ok || sent.SkippedAt != nil || sent.SentAt == nil {
		t.Fatalf("sent row = %+v, want untouched", sent)
	}
	if edge, ok := rows["skip-test-edge"]; !ok || edge.SkippedAt != nil {
		t.Fatalf("row at the cutoff = %+v, want live", edge)
	}

	// A restart with the same cutoff re-skips nothing.
	if again, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || again != 0 {
		t.Fatalf("second skip=%d err=%v", again, err)
	}

	// A skipped row is settled, not owed: the unsent list no longer counts it.
	unsent, err := s.ListUnsentNotifications(ctx, 100, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	var live []string
	for _, row := range unsent {
		if strings.HasPrefix(row.EventID, "skip-test-") {
			live = append(live, row.EventID)
		}
	}
	if len(live) != 1 || live[0] != "skip-test-edge" {
		t.Fatalf("unsent=%v, want only the row at the cutoff", live)
	}

	// Sending is what skipping ruled out: this release's store refuses a
	// receipt and an ack for a skipped row.
	if _, err := s.MarkNotificationSent(ctx, "skip-test-old", 9); !errors.Is(err, ErrOutboxConflict) {
		t.Fatalf("mark skipped row sent err=%v", err)
	}
	if _, err := s.MarkNotificationApplied(ctx, "skip-test-old", "skip-lane"); !errors.Is(err, ErrOutboxNotReceived) {
		t.Fatalf("apply skipped row err=%v", err)
	}
	if row := outboxByPrefix(t, s.pool, "skip-test-old")[0]; row.SentAt != nil || row.AppliedAt != nil || row.HubRowID != nil {
		t.Fatalf("a refused receipt changed the row: %+v", row)
	}

	// A row that commits after the settle but carries a pre-cutoff created_at
	// is still not listed: the cutoff is part of the read, so no backlog row
	// can slip through between the settle and the list.
	if _, err := s.pool.Exec(ctx, `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES('dots_report','skip-lane','skip-test-late','t',$1)`, cutoff.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	late, err := s.ListUnsentNotifications(ctx, 100, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range late {
		if row.EventID == "skip-test-late" {
			t.Fatalf("a pre-cutoff row that settled late was listed: %+v", row)
		}
	}
	// The next settle picks it up, so it is skipped rather than owed.
	if settled, err := s.SkipNotificationsBefore(ctx, cutoff, OutboxSkipBeforeForwarder); err != nil || settled != 1 {
		t.Fatalf("late settle=%d err=%v", settled, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE notification_outbox SET skipped_at=now() WHERE event_id='skip-test-edge'`); err == nil {
		t.Fatal("the table accepted a skip without a reason")
	}
}
