package store

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// TestYTWriteOpAlertsBatchPerWindowAndClass drives the alert worker on an
// injected clock. One notice per class per 10-minute window covers every op of
// the class not yet announced; an op is announced once per class; an op not
// old enough yet waits; an op a window's notice could not take waits for the
// next window; a remote_unknown op is not a stuck op.
func TestYTWriteOpAlertsBatchPerWindowAndClass(t *testing.T) {
	s := schemaStore(t, "ytwb")
	ctx := t.Context()
	l := s.YTWriteLedger()
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	// Each op has its own entity so the lane ordering never blocks a claim.
	prepare := func(key string, createdAt time.Time) ytwrite.Op {
		t.Helper()
		op, _, err := l.Prepare(ctx, createdAt, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: key, PayloadSHA: key,
			Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: key, YTTargetID: "2-" + key, Actor: "a"})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	claim := func(op ytwrite.Op, at time.Time) {
		t.Helper()
		if _, outcome, err := l.Claim(ctx, at, op.ID); err != nil || outcome != ytwrite.ClaimClaimed {
			t.Fatalf("claim op %d = %s, %v", op.ID, outcome, err)
		}
	}
	alerts := func(at time.Time) int {
		t.Helper()
		n, err := s.EnqueueYTWriteOpAlerts(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	id := func(op ytwrite.Op) string { return strconv.FormatInt(op.ID, 10) }

	prepared := prepare("prepared", t0.Add(-11*time.Minute))
	dispatched := prepare("dispatched", t0.Add(-11*time.Minute))
	claim(dispatched, t0.Add(-11*time.Minute))
	unknown := prepare("unknown", t0.Add(-11*time.Minute))
	claim(unknown, t0.Add(-11*time.Minute))
	if _, err := l.MarkUnknown(ctx, t0, unknown.ID, t0, ytwrite.ReasonTransport, "uncertain"); err != nil {
		t.Fatal(err)
	}
	rejected := prepare("rejected", t0)
	if _, err := l.Reject(ctx, t0, rejected.ID, ytwrite.ReasonOperatorNotDone, "checked"); err != nil {
		t.Fatal(err)
	}

	// 12:00: one stuck notice (prepared and dispatched) and one rejected notice.
	if n := alerts(t0); n != 2 {
		t.Fatalf("12:00 pass = %d notices, want 2", n)
	}
	if n := alerts(t0.Add(30 * time.Second)); n != 0 {
		t.Fatalf("12:00:30 pass = %d notices, want 0 (nothing new)", n)
	}
	late := prepare("late", t0.Add(-5*time.Minute))
	redispatched := prepare("redispatched", t0.Add(-time.Hour))
	claim(redispatched, t0.Add(4*time.Minute))
	// 12:05: late is old enough, but the 12:00 window already has its stuck
	// notice, so late waits for the next window.
	if n := alerts(t0.Add(5 * time.Minute)); n != 0 {
		t.Fatalf("12:05 pass = %d notices, want 0 (12:00 window taken)", n)
	}
	// 12:10: a new window takes late. redispatched was dispatched at 12:04, so
	// it is not stuck yet.
	if n := alerts(t0.Add(10 * time.Minute)); n != 1 {
		t.Fatalf("12:10 pass = %d notices, want 1", n)
	}
	// 12:15: redispatched is stuck now, but the 12:10 window is taken.
	if n := alerts(t0.Add(15 * time.Minute)); n != 0 {
		t.Fatalf("12:15 pass = %d notices, want 0 (12:10 window taken)", n)
	}
	// 12:20: the next window takes redispatched; nothing announced repeats.
	if n := alerts(t0.Add(20 * time.Minute)); n != 1 {
		t.Fatalf("12:20 pass = %d notices, want 1", n)
	}
	if n := alerts(t0.Add(30 * time.Minute)); n != 0 {
		t.Fatalf("12:30 pass = %d notices, want 0", n)
	}

	rows, err := s.pool.Query(ctx, `SELECT event_id, target_lane, text FROM notification_outbox WHERE kind=$1 ORDER BY id`, OutboxKindYTWriteOp)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ event, lane, text string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.event, &r.lane, &r.text); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	at := func(m int) int64 { return t0.Add(time.Duration(m) * time.Minute).Unix() }
	want := []row{
		{fmt.Sprintf("yt-ops-stuck-%d", at(0)), "handoffkeep-design",
			"[ytwrite] 2 stuck (dispatched: 1, prepared: 1) op ids " + id(prepared) + ", " + id(dispatched) + " | handoffkeep yt ops"},
		{fmt.Sprintf("yt-ops-rejected-%d", at(0)), "handoffkeep-design",
			"[ytwrite] 1 rejected (" + ytwrite.ReasonOperatorNotDone + ": 1) op ids " + id(rejected) + " | handoffkeep yt ops --state rejected"},
		{fmt.Sprintf("yt-ops-stuck-%d", at(10)), "handoffkeep-design",
			"[ytwrite] 1 stuck (prepared: 1) op ids " + id(late) + " | handoffkeep yt ops"},
		{fmt.Sprintf("yt-ops-stuck-%d", at(20)), "handoffkeep-design",
			"[ytwrite] 1 stuck (dispatched: 1) op ids " + id(redispatched) + " | handoffkeep yt ops"},
	}
	if len(got) != len(want) {
		t.Fatalf("outbox rows = %+v, want %d rows", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outbox row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The notice names at most ytWriteOpAlertIDsShown ids and stays within the
// lane-event byte cap, whatever the labels are.
func TestYTWriteOpAlertTextIsBounded(t *testing.T) {
	var cands []ytWriteOpAlertCandidate
	var wantIDs []string
	for i := 1; i <= 60; i++ {
		cands = append(cands, ytWriteOpAlertCandidate{id: int64(i), label: "http_400"})
		if i <= ytWriteOpAlertIDsShown {
			wantIDs = append(wantIDs, strconv.Itoa(i))
		}
	}
	want := "[ytwrite] 60 rejected (http_400: 60) op ids " + strings.Join(wantIDs, ", ") + " +10 more | handoffkeep yt ops --state rejected"
	if got := ytWriteOpAlertText(ytWriteOpAlertRejected, cands); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}

	// Forty long, distinct labels cannot fit in one notice.
	var long []ytWriteOpAlertCandidate
	for i := 0; i < 40; i++ {
		long = append(long, ytWriteOpAlertCandidate{id: int64(i + 1), label: fmt.Sprintf("label-%02d-%s", i, strings.Repeat("x", 60))})
	}
	text := ytWriteOpAlertText(ytWriteOpAlertRejected, long)
	if len(text) > RelayLaneEventMaxBytes || !strings.HasSuffix(text, "…") {
		t.Fatalf("long text = %d bytes, want at most %d and ending in an ellipsis: %q", len(text), RelayLaneEventMaxBytes, text)
	}
}
