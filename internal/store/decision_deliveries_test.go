package store

import (
	"testing"
	"time"
)

// Version 47 copies every decision_notify row into decision_deliveries: a
// channel status becomes applied_version on a pending row, unsent and failed
// keep their final state, and last_error keeps only its code.
func TestDecisionDeliveriesBackfill(t *testing.T) {
	s := schemaStore(t, "deliveries_backfill")
	ctx := t.Context()
	for _, q := range []string{`DROP TABLE decision_deliveries`, `DELETE FROM schema_version WHERE version=47`} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	type row struct {
		lastStatus, messageID, lastError string
		attempts                         int
		posted                           bool
	}
	seed := map[int64]row{
		1: {"", "", "discord webhook status 500", 3, false},
		2: {"open", "1001", "", 0, true},
		3: {"answered", "1002", "", 0, true},
		4: {"superseded", "", "", 0, false},
		5: {"expired", "1004", "", 0, true},
		6: {"unsent", "", "", 0, false},
		7: {"failed", "", "decision_notice_rejected: discord webhook status 404", 2, false},
	}
	for id, r := range seed {
		var posted *time.Time
		if r.posted {
			at := base.Add(time.Duration(id) * time.Minute)
			posted = &at
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO decision_notify(request_id,message_id,posted_at,last_status,edited_at,attempts,next_attempt_at,last_error,created_at)
			VALUES($1,$2,$3,$4,$3,$5,$6,$7,$8)`, id, r.messageID, posted, r.lastStatus, r.attempts, base.Add(time.Duration(id)*time.Hour), r.lastError, base); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version=%d SchemaVersion=%d err=%v", version, SchemaVersion, err)
	}
	want := map[int64]struct{ applied, state, code string }{
		1: {"", "pending", "discord webhook status 500"},
		2: {"open", "pending", ""},
		3: {"answered", "pending", ""},
		4: {"superseded", "pending", ""},
		5: {"expired", "pending", ""},
		6: {"", "unsent", ""},
		7: {"", "failed", "decision_notice_rejected"},
	}
	for id, w := range want {
		x, found, err := s.GetDecisionNotice(ctx, id)
		if err != nil || !found {
			t.Fatalf("request %d: found=%v err=%v", id, found, err)
		}
		r := seed[id]
		if x.Destination != DecisionDestinationDiscord || x.AppliedVersion != w.applied || x.DesiredVersion != w.applied || x.State != w.state || x.LastErrorCode != w.code ||
			x.MessageID != r.messageID || x.Attempts != r.attempts || !x.NextAttemptAt.Equal(base.Add(time.Duration(id)*time.Hour)) || (x.PostedAt != nil) != r.posted || (x.EditedAt != nil) != r.posted {
			t.Errorf("request %d (%q): %+v", id, r.lastStatus, x)
		}
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM decision_deliveries`).Scan(&n); err != nil || n != len(seed) {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

// Hygiene counts pending, lagging, unsent and failed rows separately: a
// lagging row is pending, behind its request, and over an hour past its next
// attempt.
func TestDecisionDeliveryHygiene(t *testing.T) {
	s := schemaStore(t, "deliveries_hygiene")
	ctx := t.Context()
	now := time.Now().UTC()
	h, err := s.DecisionDeliveryHygiene(ctx, now)
	if err != nil || len(h.Destinations) != 1 || h.Destinations[DecisionDestinationDiscord] != (DecisionDeliveryCounts{}) {
		t.Fatalf("empty: %+v err=%v", h, err)
	}
	set := map[string]string{
		"Q-20261105-01": `state='pending',applied_version='open',next_attempt_at=$2::timestamptz-interval '3 hours'`, // in sync
		"Q-20261105-02": `state='pending',applied_version='',next_attempt_at=$2::timestamptz-interval '2 hours'`,     // lagging
		"Q-20261105-03": `state='pending',applied_version='',next_attempt_at=$2::timestamptz-interval '30 minutes'`,  // due, not lagging yet
		"Q-20261105-04": `state='unsent',next_attempt_at=$2::timestamptz`,
		"Q-20261105-05": `state='failed',next_attempt_at=$2::timestamptz-interval '3 hours'`,
	}
	for id, assign := range set {
		if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: id, Lane: "hygiene-lane", Body: "question " + id}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `UPDATE decision_deliveries SET `+assign+`
			WHERE request_id=(SELECT id FROM decision_requests WHERE source='chat' AND source_ref=$1)`, id, now); err != nil {
			t.Fatal(err)
		}
	}
	h, err = s.DecisionDeliveryHygiene(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := h.Destinations[DecisionDestinationDiscord], (DecisionDeliveryCounts{Pending: 3, Lagging: 1, Unsent: 1, Failed: 1}); got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
