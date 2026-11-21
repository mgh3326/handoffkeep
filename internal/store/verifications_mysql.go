package store

import (
	"context"
	"errors"
	"time"
)

// recordVerificationMySQL is RecordVerification's MySQL path (hk 5914). The
// PostgreSQL statement is an ON CONFLICT DO NOTHING insert with RETURNING;
// MySQL has neither, so the three outcomes are read explicitly, as
// addTaskOriginMySQL does:
//
//   - the stub change takes an exclusive row lock on its changes row (ON
//     DUPLICATE KEY UPDATE id=id), which serializes writers of one PR, so the
//     verification read below cannot race another RecordVerification for it;
//   - SELECT ... FOR UPDATE finds the head's verdict. None: insert it. A
//     unique violation means a writer outside this lock inserted first, so the
//     row is read back; the payload check then runs as for an existing row;
//   - an existing row with the same payload is replayed; a different payload
//     is ErrVerificationExists unless the stored level is verifier-blocked, in
//     which case the row is updated in place.
//
// Locks, in order: at most one changes row, then at most one verifications row.
func (s *Store) recordVerificationMySQL(ctx context.Context, in VerificationInput, recordedBy string) (Verification, bool, error) {
	repo, n, _ := parsePRURL(in.PR)
	now := time.Now().UTC()
	var out Verification
	var created bool
	err := s.retryTx(ctx, func() error {
		out, created = Verification{}, false
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		// The stub never updates an existing row: an event writer that created
		// the change first keeps its head.
		if _, err = tx.Exec(ctx, `INSERT INTO changes(repo,pr_number,state,head_sha,opened_at,created_at,updated_at) VALUES(?,?,'open',?,?,?,?) ON DUPLICATE KEY UPDATE id=id`, repo, n, in.HeadSHA, now, now, now); err != nil {
			return err
		}
		var changeID int64
		var changeHead *string
		if err = tx.QueryRow(ctx, `SELECT id,head_sha FROM changes WHERE repo=? AND pr_number=?`, repo, n).Scan(&changeID, &changeHead); err != nil {
			return err
		}
		const byHead = `SELECT ` + verificationColumns + ` FROM verifications WHERE change_id=? AND head_sha=? FOR UPDATE`
		err = scanVerification(tx.QueryRow(ctx, byHead, changeID, in.HeadSHA), &out)
		if errors.Is(err, ErrNoRows) {
			args := []any{changeID, in.HeadSHA, in.Level, in.Verifier, nullStr(in.VerifierJobID), in.Family, nullStr(in.Profile), nullStr(in.Model), nullStr(in.Harness), in.Evidence, nullStr(in.CIResult), in.Source, recordedBy, now}
			_, err = tx.Exec(ctx, `INSERT INTO verifications(change_id,head_sha,level,verifier,verifier_job_id,family,profile,model,harness,evidence,ci_result,source,recorded_by,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
			switch {
			case err == nil:
				created = true
			case errors.Is(err, errUniqueViolation):
			default:
				return err
			}
			if err = scanVerification(tx.QueryRow(ctx, byHead, changeID, in.HeadSHA), &out); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if !created {
			switch {
			case out.samePayload(in):
			case out.Level == VerificationBlocked:
				if _, err = tx.Exec(ctx, `UPDATE verifications SET level=?,verifier=?,verifier_job_id=?,family=?,profile=?,model=?,harness=?,evidence=?,ci_result=?,source=?,recorded_by=?,recorded_at=? WHERE change_id=? AND head_sha=?`,
					in.Level, in.Verifier, nullStr(in.VerifierJobID), in.Family, nullStr(in.Profile), nullStr(in.Model), nullStr(in.Harness), in.Evidence, nullStr(in.CIResult), in.Source, recordedBy, now, changeID, in.HeadSHA); err != nil {
					return err
				}
				if err = scanVerification(tx.QueryRow(ctx, byHead, changeID, in.HeadSHA), &out); err != nil {
					return err
				}
			default:
				return ErrVerificationExists
			}
		}
		out.Current = changeHead != nil && *changeHead == out.HeadSHA
		return tx.Commit(ctx)
	})
	return out, created, err
}
