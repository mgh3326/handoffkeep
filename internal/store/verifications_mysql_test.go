//go:build mysql

package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLRecordVerificationAndReadBack records verdicts on MySQL the way
// `handoffkeep changes verify` does, then reads the change back: the replay,
// the conflict, the blocked-then-replaced path and the current-head flag all
// behave as on PostgreSQL, and the change list carries the verdicts.
func TestMySQLRecordVerificationAndReadBack(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	repo := changeRepo("verify-mysql-" + n)
	pr := repoPR(repo, 1)
	input := func(head, level string) VerificationInput {
		return VerificationInput{PR: pr, HeadSHA: head, Level: level, Verifier: "lead", Family: "anthropic", Profile: "omp-opus@low",
			Evidence: "https://github.com/example/app/actions/runs/1", CIResult: "success", Source: "direct_merge"}
	}

	first, created, err := s.RecordVerification(ctx, input(testSHA1, VerificationUnitTest), "lead-client")
	if err != nil || !created || !first.Current || first.RecordedBy != "lead-client" {
		t.Fatalf("create: created=%v err=%v v=%+v", created, err, first)
	}
	replay, created, err := s.RecordVerification(ctx, input(testSHA1, VerificationUnitTest), "other-client")
	if err != nil || created || replay.ID != first.ID || replay.RecordedBy != "lead-client" {
		t.Fatalf("replay: created=%v err=%v v=%+v", created, err, replay)
	}
	if _, _, err = s.RecordVerification(ctx, input(testSHA1, VerificationLive), "lead-client"); !errors.Is(err, ErrVerificationExists) {
		t.Fatalf("different level on the same head: err=%v", err)
	}

	if _, created, err = s.RecordVerification(ctx, input(testSHA2, VerificationBlocked), "lead-client"); err != nil || !created {
		t.Fatalf("blocked: created=%v err=%v", created, err)
	}
	replaced, created, err := s.RecordVerification(ctx, input(testSHA2, VerificationLive), "lead-client")
	if err != nil || created || replaced.Level != VerificationLive {
		t.Fatalf("replace blocked: created=%v err=%v v=%+v", created, err, replaced)
	}

	changes, err := s.ListChangesByPR(ctx, pr, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes=%d, want 1", len(changes))
	}
	c := changes[0]
	// The change's head is the first head recorded (a stub never moves it), so
	// only testSHA1's verdict is current; testSHA2's is kept but not current.
	if len(c.Verifications) != 2 {
		t.Fatalf("verifications=%d, want 2 (one per head; blocked replaced by live)", len(c.Verifications))
	}
	if c.Verification == nil || c.Verification.HeadSHA != testSHA1 || c.Verification.Level != VerificationUnitTest || !c.Verification.Current {
		t.Fatalf("current verification=%+v", c.Verification)
	}
	for _, v := range c.Verifications {
		if v.Current != (v.HeadSHA == testSHA1) {
			t.Fatalf("verification %s current=%t", v.HeadSHA, v.Current)
		}
	}
}
