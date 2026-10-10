package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres aborts a deadlocked or serialization-failing batch on its own —
// those SQLSTATEs map to ErrBenchRepsRetryable (a 503) while everything
// else passes through untouched.
func TestBenchRepsBatchErrRetryable(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		err := benchRepsBatchErr(fmt.Errorf("statement: %w", &pgconn.PgError{Code: code, Message: "deadlock detected"}))
		if !errors.Is(err, ErrBenchRepsRetryable) {
			t.Fatalf("SQLSTATE %s error %v not mapped to ErrBenchRepsRetryable", code, err)
		}
	}
	if err := benchRepsBatchErr(&pgconn.PgError{Code: "23505", Message: "duplicate key"}); errors.Is(err, ErrBenchRepsRetryable) {
		t.Fatalf("unique violation mapped to retryable: %v", err)
	}
	plain := errors.New("connection reset")
	if err := benchRepsBatchErr(plain); err != plain {
		t.Fatalf("non-Postgres error rewritten: %v", err)
	}
}

// sameBenchRepInput is the intra-batch resend rule: every carried field must
// be identical, recorded_at compares at the precision timestamptz keeps, and
// server-stamped fields are not inputs.
func TestSameBenchRepInput(t *testing.T) {
	str := func(v string) *string { return &v }
	num := func(v int) *int { return &v }
	big := func(v int64) *int64 { return &v }
	base := BenchRep{
		OriginID: 7, Profile: "p", ModelID: str("m"), TaskRef: str("t"), Tier: str("T2"), Role: str("impl"),
		Rounds: num(1), BlockersFound: num(0), Completed: num(1), InputTokens: big(10), OutputTokens: big(20),
		Notes: str("n"), RecordedAt: time.Date(2026, 9, 1, 10, 0, 0, 123456000, time.UTC),
		Effort: str("high"), Grade: str("A"), TableGrade: str("A"), CreatedBy: "c",
	}
	if !sameBenchRepInput(base, base) {
		t.Fatal("identical rep not equal to itself")
	}
	// Sub-microsecond spelling of the same stored instant is identical.
	b := base
	b.RecordedAt = base.RecordedAt.Add(999 * time.Nanosecond)
	if !sameBenchRepInput(base, b) {
		t.Fatal("sub-microsecond recorded_at treated as differing")
	}
	// A genuinely different instant differs.
	b = base
	b.RecordedAt = base.RecordedAt.Add(time.Microsecond)
	if sameBenchRepInput(base, b) {
		t.Fatal("recorded_at +1us treated as identical")
	}
	// nil vs set differs; nil vs nil does not.
	b = base
	b.Notes = nil
	if sameBenchRepInput(base, b) {
		t.Fatal("notes nil vs set treated as identical")
	}
	c := base
	c.Notes = nil
	if !sameBenchRepInput(b, c) {
		t.Fatal("notes nil vs nil treated as differing")
	}
	b = base
	b.ModelID = nil
	if sameBenchRepInput(base, b) {
		t.Fatal("model_id nil vs set treated as identical")
	}
	// id and created_at are server-stamped and never part of the comparison.
	b = base
	b.ID = 42
	b.CreatedAt = time.Now()
	if !sameBenchRepInput(base, b) {
		t.Fatal("server-owned fields affected input equality")
	}
}
