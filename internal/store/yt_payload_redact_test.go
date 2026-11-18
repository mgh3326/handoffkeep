package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// redactSeed prepares, claims and resolves one comment op: resolved at res,
// applied when ytID is not "" and rejected otherwise.
func redactSeed(t *testing.T, s *Store, key, ytID, reason string, at, res time.Time) ytwrite.Op {
	t.Helper()
	ctx := context.Background()
	l := s.YTWriteLedger()
	op, _, err := l.Prepare(ctx, at, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: key, PayloadSHA: "s" + key,
		Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: key, YTTargetID: "2-1", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, out, err := l.Claim(ctx, at, op.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim = %s %v", out, err)
	}
	if ytID != "" {
		op, err = l.Apply(ctx, res, op.ID, ytID, nil)
	} else {
		op, err = l.Reject(ctx, res, op.ID, reason, "test")
	}
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func redactExec(t *testing.T, s *Store, q string, args ...any) error {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), q, args...)
	return err
}

func wantRedactErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("err = %v, want it to contain %q", err, sub)
	}
}

// The widened guard allows exactly one more change: payload to NULL on an
// applied or rejected row resolved more than 30 days ago, with everything
// else unchanged.
func TestYTWriteOpPayloadRedactGuard(t *testing.T) {
	s := schemaStore(t, "ytredact")
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-31 * 24 * time.Hour)

	applied := redactSeed(t, s, "applied-old", "2-1", "", old, old)
	if err := redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, applied.ID); err != nil {
		t.Fatalf("redact applied: %v", err)
	}
	got, err := s.YTWriteLedger().ByID(ctx, applied.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Payload != nil || got.PayloadSHA != applied.PayloadSHA || got.State != ytwrite.StateApplied ||
		got.YTResultID != applied.YTResultID || got.ResolvedAt.IsZero() {
		t.Fatalf("redacted op = %+v, want NULL payload with sha and identity kept", got)
	}

	rejected := redactSeed(t, s, "rejected-old", "", "operator_not_applied", old, old)
	if err := redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, rejected.ID); err != nil {
		t.Fatalf("redact rejected: %v", err)
	}

	// A second redaction is refused: there is nothing left to change, and the
	// worker only selects rows with a payload.
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, applied.ID), "cannot change")

	// Rows younger than 30 days are never touched. The identity arm refuses
	// first: a payload change with no valid exception never reaches the
	// frozen-row check.
	young := redactSeed(t, s, "applied-now", "2-1", "", now, now)
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, young.ID), "immutable")
	almost := redactSeed(t, s, "rejected-29d", "", "gone", now.Add(-29*24*time.Hour), now.Add(-29*24*time.Hour))
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, almost.ID), "immutable")

	// Unresolved rows are never touched.
	prepared, _, err := s.YTWriteLedger().Prepare(ctx, now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend,
		Key: "prepared", PayloadSHA: "sp", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "prepared", YTTargetID: "2-1", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, prepared.ID), "immutable")
	if _, out, err := s.YTWriteLedger().Claim(ctx, now, prepared.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim = %s %v", out, err)
	}
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, prepared.ID), "immutable")
	unknown, _, err := s.YTWriteLedger().Prepare(ctx, now, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend,
		Key: "unknown", PayloadSHA: "su", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "unknown", YTTargetID: "2-1", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, out, err := s.YTWriteLedger().Claim(ctx, now, unknown.ID); err != nil || out != ytwrite.ClaimClaimed {
		t.Fatalf("claim = %s %v", out, err)
	}
	if _, err := s.YTWriteLedger().MarkUnknown(ctx, now, unknown.ID, now.Add(time.Hour), "lost", "test"); err != nil {
		t.Fatal(err)
	}
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL WHERE id=$1`, unknown.ID), "immutable")

	// The redaction is exactly payload to NULL: any companion change is refused.
	other := redactSeed(t, s, "applied-other", "2-1", "", old, old)
	for _, q := range map[string]string{
		"payload to a value": `payload='{"body":"y"}'`,
		"payload_sha":        `payload=NULL, payload_sha='z'`,
		"state":              `payload=NULL, state='rejected', resolved_at=now()`,
		"tenant":             `payload=NULL, tenant='other'`,
	} {
		wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET `+q+` WHERE id=$1`, other.ID), "immutable")
	}
	// A companion change fails the identity arm: redact needs every other
	// column unchanged, so the payload change has no valid exception.
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET payload=NULL, reason='late' WHERE id=$1`, other.ID), "immutable")
	// A change that leaves the payload alone passes identity and still hits
	// the frozen row.
	wantRedactErr(t, redactExec(t, s, `UPDATE yt_write_ops SET next_attempt_at=now() WHERE id=$1`, other.ID), "cannot change")
	// The log stays append-only.
	wantRedactErr(t, redactExec(t, s, `DELETE FROM yt_write_ops WHERE id=$1`, other.ID), "append-only")
	// A NULL payload reads back as nil with the receipt kept.
	if got, err := s.YTWriteLedger().ByID(ctx, applied.ID); err != nil || got.Payload != nil || got.PayloadSHA != "sapplied-old" {
		t.Fatalf("redacted read = %+v %v", got, err)
	}
}

// RedactYTPayloads redacts every eligible op in bounded batches: a full batch
// leaves the rest for the next tick (resumable), and a second run finds
// nothing (idempotent).
func TestRedactYTPayloadsBatch(t *testing.T) {
	s := schemaStore(t, "ytredactbatch")
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-31 * 24 * time.Hour)

	eligible := []ytwrite.Op{
		redactSeed(t, s, "batch-a1", "2-1", "", old, old),
		redactSeed(t, s, "batch-a2", "2-2", "", old, old),
		redactSeed(t, s, "batch-r1", "", "gone", old, old),
	}
	youngApplied := redactSeed(t, s, "batch-young", "2-3", "", now, now)
	youngRejected := redactSeed(t, s, "batch-29d", "", "gone", now.Add(-29*24*time.Hour), now.Add(-29*24*time.Hour))

	if n, err := s.RedactYTPayloads(ctx, now, 1); err != nil || n != 1 {
		t.Fatalf("first batch = %d %v, want 1", n, err)
	}
	if n, err := s.RedactYTPayloads(ctx, now, 10); err != nil || n != 2 {
		t.Fatalf("second batch = %d %v, want 2", n, err)
	}
	if n, err := s.RedactYTPayloads(ctx, now, 10); err != nil || n != 0 {
		t.Fatalf("third batch = %d %v, want 0", n, err)
	}
	for _, op := range eligible {
		got, err := s.YTWriteLedger().ByID(ctx, op.ID)
		if err != nil || got.Payload != nil || got.PayloadSHA != op.PayloadSHA || got.State != op.State {
			t.Fatalf("redacted %d = %+v %v", op.ID, got, err)
		}
	}
	for _, op := range []ytwrite.Op{youngApplied, youngRejected} {
		got, err := s.YTWriteLedger().ByID(ctx, op.ID)
		if err != nil || string(got.Payload) != string(op.Payload) {
			t.Fatalf("young %d = %+v %v", op.ID, got, err)
		}
	}
}

// A redacted op reads correctly everywhere the operator's view reaches it:
// the row scans with a nil payload, the view carries no payload, and the
// rejected alert pass still announces it.
func TestRedactedOpReads(t *testing.T) {
	s := schemaStore(t, "ytredactreads")
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-31 * 24 * time.Hour)

	rejected := redactSeed(t, s, "reads-r", "", "operator_not_applied", old, old)
	if _, err := s.RedactYTPayloads(ctx, now, 10); err != nil {
		t.Fatal(err)
	}
	got, err := s.YTWriteLedger().ByID(ctx, rejected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Payload != nil || got.PayloadSHA != rejected.PayloadSHA || got.State != ytwrite.StateRejected {
		t.Fatalf("redacted = %+v", got)
	}
	ops, err := s.ListYTWriteOps(ctx, YTOpFilter{State: "rejected", Limit: 10})
	if err != nil || len(ops) != 1 || ops[0].ID != rejected.ID || ops[0].Payload != nil {
		t.Fatalf("rejected view = %+v %v", ops, err)
	}
	view := ytwrite.ViewOf(ops[0])
	if view.ID != rejected.ID || view.State != ytwrite.StateRejected || view.Reason != "operator_not_applied" {
		t.Fatalf("view = %+v", view)
	}
	if _, err := s.EnqueueYTWriteOpAlerts(ctx, now); err != nil {
		t.Fatalf("alerts over a redacted op: %v", err)
	}
}
