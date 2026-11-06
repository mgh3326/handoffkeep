package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

func TestVerificationsMigration(t *testing.T) {
	s := schemaStore(t, "verifications_mig")
	ctx := t.Context()
	var found bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass(format('%I.%I', current_schema(), 'verifications')) IS NOT NULL`).Scan(&found); err != nil || !found {
		t.Fatalf("verifications: found=%v err=%v", found, err)
	}
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max(version)=%d err=%v, want %d", version, err, SchemaVersion)
	}
	now := time.Now().UTC()
	var id int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO changes(repo,pr_number,opened_at,created_at,updated_at) VALUES('example/vmig',1,$1,$1,$1) RETURNING id`, now).Scan(&id); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO verifications(change_id,head_sha,level,verifier,family,evidence,source,recorded_by,recorded_at) VALUES($1,$2,$3,'v','anthropic','https://example.com/e','direct_merge','tester',$4)`
	if _, err := s.pool.Exec(ctx, insert, id, testSHA1, VerificationLive, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, insert, id, testSHA1, VerificationUnitTest, now); err == nil {
		t.Fatal("(change_id, head_sha) is not unique")
	}
	if _, err := s.pool.Exec(ctx, insert, id, testSHA2, "ci-green", now); err == nil {
		t.Fatal("an unknown level was accepted")
	}
}

func TestRecordVerification(t *testing.T) {
	s := schemaStore(t, "verifications")
	ctx := t.Context()
	input := func(pr, head, level string) VerificationInput {
		return VerificationInput{PR: pr, HeadSHA: head, Level: level, Verifier: "lead", Family: "anthropic", Profile: "omp-opus@low",
			Evidence: "https://github.com/example/app/actions/runs/1", CIResult: "success", Source: "direct_merge"}
	}
	count := func(t *testing.T, changeID int64) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM verifications WHERE change_id=$1`, changeID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	repo := changeRepo("verify")
	pr := repoPR(repo, 1)
	var first Verification
	t.Run("create", func(t *testing.T) {
		v, created, err := s.RecordVerification(ctx, input(pr, testSHA1, VerificationUnitTest), "lead-client")
		if err != nil || !created {
			t.Fatalf("created=%v err=%v", created, err)
		}
		if v.Level != VerificationUnitTest || v.RecordedBy != "lead-client" || v.CIResult == nil || *v.CIResult != "success" || !v.Current {
			t.Fatalf("verification %+v", v)
		}
		first = v
	})
	t.Run("same payload replays", func(t *testing.T) {
		v, created, err := s.RecordVerification(ctx, input(pr, testSHA1, VerificationUnitTest), "other-client")
		if err != nil || created || v.ID != first.ID || v.RecordedBy != "lead-client" {
			t.Fatalf("created=%v err=%v v=%+v", created, err, v)
		}
	})
	t.Run("a different level on the same head", func(t *testing.T) {
		if _, _, err := s.RecordVerification(ctx, input(pr, testSHA1, VerificationLive), "lead-client"); !errors.Is(err, ErrVerificationExists) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("verifier-blocked is replaced", func(t *testing.T) {
		if _, created, err := s.RecordVerification(ctx, input(pr, testSHA2, VerificationBlocked), "lead-client"); err != nil || !created {
			t.Fatalf("blocked: created=%v err=%v", created, err)
		}
		v, created, err := s.RecordVerification(ctx, input(pr, testSHA2, VerificationLive), "lead-client")
		if err != nil || created || v.Level != VerificationLive {
			t.Fatalf("replace: created=%v err=%v v=%+v", created, err, v)
		}
		var n int
		if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM verifications WHERE change_id=$1 AND head_sha=$2`, v.ChangeID, testSHA2).Scan(&n); err != nil || n != 1 {
			t.Fatalf("rows=%d err=%v", n, err)
		}
		// Once a real verdict stands, it is not replaced.
		if _, _, err = s.RecordVerification(ctx, input(pr, testSHA2, VerificationBlocked), "lead-client"); !errors.Is(err, ErrVerificationExists) {
			t.Fatalf("re-block err=%v", err)
		}
	})
	t.Run("ci-green is not a level", func(t *testing.T) {
		for _, level := range []string{"ci-green", "ci_success", ""} {
			_, _, err := s.RecordVerification(ctx, input(pr, testSHA3, level), "lead-client")
			if err == nil || errors.Is(err, ErrVerificationExists) {
				t.Fatalf("level %q: err=%v", level, err)
			}
		}
		bad := input(pr, testSHA3, VerificationLive)
		bad.CIResult = "green"
		if _, _, err := s.RecordVerification(ctx, bad, "lead-client"); err == nil {
			t.Fatal("ci_result green accepted")
		}
		if n := count(t, first.ChangeID); n != 2 {
			t.Fatalf("rows=%d after refusals, want 2", n)
		}
	})
	t.Run("unknown PR gets a stub change", func(t *testing.T) {
		repo := changeRepo("stub")
		v, created, err := s.RecordVerification(ctx, input(repoPR(repo, 9), testSHA3, VerificationTypeCheck), "lead-client")
		if err != nil || !created || !v.Current {
			t.Fatalf("created=%v err=%v v=%+v", created, err, v)
		}
		c := mustChange(t, s, repo, 9)
		if c.ID != v.ChangeID || c.State != ChangeOpen || c.Head == nil || *c.Head != testSHA3 {
			t.Fatalf("change %+v", c)
		}
	})
	t.Run("an existing change is not modified", func(t *testing.T) {
		before := mustChange(t, s, repo, 1)
		if _, _, err := s.RecordVerification(ctx, input(pr, testSHA3, VerificationFailed), "lead-client"); err != nil {
			t.Fatal(err)
		}
		if after := mustChange(t, s, repo, 1); *after.Head != *before.Head {
			t.Fatalf("head moved from %s to %s", *before.Head, *after.Head)
		}
	})
	t.Run("secret-shaped evidence is refused", func(t *testing.T) {
		in := input(pr, "4444444444444444444444444444444444444444", VerificationLive)
		in.Evidence = "https://example.com/?k=" + "sk" + "-" + strings.Repeat("a", 40)
		_, _, err := s.RecordVerification(ctx, in, "lead-client")
		if _, ok := guard.SecretPattern(err); !ok {
			t.Fatalf("err=%v, want a secret refusal", err)
		}
	})
	t.Run("evidence shape", func(t *testing.T) {
		for _, e := range []string{"http://example.com/x", "https://", "https://?x", "https:///path", "hk:doc ", "hk:doc ../x", "https://example.com/" + strings.Repeat("x", 500), "see CI"} {
			in := input(pr, "5555555555555555555555555555555555555555", VerificationLive)
			in.Evidence = e
			if _, _, err := s.RecordVerification(ctx, in, "lead-client"); err == nil {
				t.Errorf("evidence %q accepted", e)
			}
		}
		in := input(pr, "5555555555555555555555555555555555555555", VerificationLive)
		in.Evidence = "hk:doc reports/hk3120/verify"
		if _, _, err := s.RecordVerification(ctx, in, "lead-client"); err != nil {
			t.Fatalf("hk:doc evidence: %v", err)
		}
	})
}
