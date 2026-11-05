package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// The verification ledger (hk 3120). A verdict says how one head of a change
// was verified: at most one per (change_id, head_sha). A new head voids the
// verdict by read alone: Current is set only when the verdict's head is the
// change's head. CI is an input (ci_result), never a level.

// ErrVerificationExists refuses a different verdict for a head that already
// has one.
var ErrVerificationExists = errors.New("verification_exists")

// Verification levels.
const (
	VerificationLive      = "live-verified"
	VerificationUnitTest  = "unit-test-verified"
	VerificationTypeCheck = "type-check-only"
	VerificationBlocked   = "verifier-blocked"
	VerificationFailed    = "verifier-failed"
)

var (
	verificationLevels  = map[string]bool{VerificationLive: true, VerificationUnitTest: true, VerificationTypeCheck: true, VerificationBlocked: true, VerificationFailed: true}
	verificationCI      = map[string]bool{"success": true, "failure": true, "pending": true, "none": true}
	verificationSources = map[string]bool{"direct_merge": true, "mergewake": true}
	verifierRE          = regexp.MustCompile(`^[A-Za-z0-9._:@-]{1,128}$`)
	headSHARE           = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// maxEvidence is the byte cap of an https evidence link.
const maxEvidence = 512

// VerificationInput is one verdict as a writer sends it.
type VerificationInput struct {
	PR            string `json:"pr"`
	HeadSHA       string `json:"head_sha"`
	Level         string `json:"level"`
	Verifier      string `json:"verifier"`
	VerifierJobID string `json:"verifier_job_id,omitempty"`
	Family        string `json:"family"`
	Profile       string `json:"profile,omitempty"`
	Model         string `json:"model,omitempty"`
	Harness       string `json:"harness,omitempty"`
	Evidence      string `json:"evidence"`
	CIResult      string `json:"ci_result,omitempty"`
	Source        string `json:"source"`
}

// Verification is one stored verdict.
type Verification struct {
	ID            int64     `json:"id"`
	ChangeID      int64     `json:"change_id"`
	HeadSHA       string    `json:"head_sha"`
	Level         string    `json:"level"`
	Verifier      string    `json:"verifier"`
	VerifierJobID *string   `json:"verifier_job_id,omitempty"`
	Family        string    `json:"family"`
	Profile       *string   `json:"profile,omitempty"`
	Model         *string   `json:"model,omitempty"`
	Harness       *string   `json:"harness,omitempty"`
	Evidence      string    `json:"evidence"`
	CIResult      *string   `json:"ci_result,omitempty"`
	Source        string    `json:"source"`
	RecordedBy    string    `json:"recorded_by"`
	RecordedAt    time.Time `json:"recorded_at"`
	Current       bool      `json:"current"`
}

// samePayload reports whether v holds exactly what in sends.
func (v Verification) samePayload(in VerificationInput) bool {
	return v.Level == in.Level && v.Verifier == in.Verifier && v.Family == in.Family && v.Evidence == in.Evidence && v.Source == in.Source &&
		deref(v.VerifierJobID) == in.VerifierJobID && deref(v.Profile) == in.Profile && deref(v.Model) == in.Model &&
		deref(v.Harness) == in.Harness && deref(v.CIResult) == in.CIResult
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// checkEvidence accepts an https URL of at most maxEvidence bytes or
// "hk:doc <key>" with a valid document key.
func checkEvidence(e string) error {
	blank := func(r rune) bool { return r <= ' ' || r == 0x7f }
	if key, ok := strings.CutPrefix(e, "hk:doc "); ok {
		if !validDocKey(key) || strings.ContainsFunc(key, blank) {
			return errors.New("invalid verification: evidence hk:doc key is not a valid document key")
		}
		return nil
	}
	if !strings.HasPrefix(e, "https://") || len(e) <= len("https://") || len(e) > maxEvidence || strings.ContainsFunc(e, blank) {
		return fmt.Errorf("invalid verification: evidence must be an https URL of at most %d bytes or hk:doc <key>", maxEvidence)
	}
	return nil
}

// checkVerification validates a verdict before any database work.
func checkVerification(in VerificationInput) error {
	if _, _, ok := parsePRURL(in.PR); !ok || !taskEventPRRE.MatchString(in.PR) {
		return errors.New("invalid verification: pr must be a PR URL")
	}
	switch {
	case !headSHARE.MatchString(in.HeadSHA):
		return errors.New("invalid verification: head_sha must be 40 lowercase hex characters")
	case !verificationLevels[in.Level]:
		return errors.New("invalid verification: level must be live-verified, unit-test-verified, type-check-only, verifier-blocked or verifier-failed")
	case in.CIResult != "" && !verificationCI[in.CIResult]:
		return errors.New("invalid verification: ci_result must be success, failure, pending or none")
	case !verificationSources[in.Source]:
		return errors.New("invalid verification: source must be direct_merge or mergewake")
	case !verifierRE.MatchString(in.Verifier):
		return errors.New("invalid verification: verifier must match [A-Za-z0-9._:@-]{1,128}")
	case !verifierRE.MatchString(in.Family):
		return errors.New("invalid verification: family must match [A-Za-z0-9._:@-]{1,128}")
	}
	for _, x := range []string{in.VerifierJobID, in.Profile, in.Model, in.Harness} {
		if !validText(x, 128) {
			return errors.New("invalid verification: verifier_job_id, profile, model and harness are at most 128 bytes")
		}
	}
	if err := checkEvidence(in.Evidence); err != nil {
		return err
	}
	return guard.Reject(strings.Join([]string{in.PR, in.Verifier, in.VerifierJobID, in.Family, in.Profile, in.Model, in.Harness, in.Evidence}, "\n"))
}

const verificationColumns = `id,change_id,head_sha,level,verifier,verifier_job_id,family,profile,model,harness,evidence,ci_result,source,recorded_by,recorded_at`

func scanVerification(row pgx.Row, v *Verification) error {
	return row.Scan(&v.ID, &v.ChangeID, &v.HeadSHA, &v.Level, &v.Verifier, &v.VerifierJobID, &v.Family, &v.Profile, &v.Model, &v.Harness, &v.Evidence, &v.CIResult, &v.Source, &v.RecordedBy, &v.RecordedAt)
}

// RecordVerification stores the verdict for (in.PR, in.HeadSHA). A PR hk has
// not seen gets an open stub change at that head; an existing change is not
// modified. The same payload again returns the stored row with created
// false. A different payload is ErrVerificationExists unless the stored level
// is verifier-blocked (no verdict was given): then it is replaced in place.
// It locks at most one changes row (the stub upsert) and one verifications
// row, and no task row (lockorder.go).
func (s *Store) RecordVerification(ctx context.Context, in VerificationInput, recordedBy string) (Verification, bool, error) {
	if err := checkVerification(in); err != nil {
		return Verification{}, false, err
	}
	if !validText(recordedBy, 128) || recordedBy == "" {
		return Verification{}, false, errors.New("invalid verification: recorded_by is required")
	}
	repo, n, _ := parsePRURL(in.PR)
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Verification{}, false, err
	}
	defer tx.Rollback(ctx)
	var changeID int64
	var changeHead *string
	err = tx.QueryRow(ctx, `SELECT id,head_sha FROM changes WHERE repo=$1 AND pr_number=$2`, repo, n).Scan(&changeID, &changeHead)
	if errors.Is(err, pgx.ErrNoRows) {
		changeID, err = upsertChangeTx(ctx, tx, changeWrite{repo: repo, number: n, state: ChangeOpen, head: in.HeadSHA}, now, now)
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT head_sha FROM changes WHERE id=$1`, changeID).Scan(&changeHead)
		}
	}
	if err != nil {
		return Verification{}, false, err
	}
	args := []any{changeID, in.HeadSHA, in.Level, in.Verifier, nullStr(in.VerifierJobID), in.Family, nullStr(in.Profile), nullStr(in.Model), nullStr(in.Harness), in.Evidence, nullStr(in.CIResult), in.Source, recordedBy, now}
	var v Verification
	created := true
	err = scanVerification(tx.QueryRow(ctx, `INSERT INTO verifications(change_id,head_sha,level,verifier,verifier_job_id,family,profile,model,harness,evidence,ci_result,source,recorded_by,recorded_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT(change_id,head_sha) DO NOTHING
RETURNING `+verificationColumns, args...), &v)
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		if err = scanVerification(tx.QueryRow(ctx, `SELECT `+verificationColumns+` FROM verifications WHERE change_id=$1 AND head_sha=$2 FOR UPDATE`, changeID, in.HeadSHA), &v); err != nil {
			return Verification{}, false, err
		}
		switch {
		case v.samePayload(in):
		case v.Level == VerificationBlocked:
			err = scanVerification(tx.QueryRow(ctx, `UPDATE verifications SET level=$3,verifier=$4,verifier_job_id=$5,family=$6,profile=$7,model=$8,harness=$9,evidence=$10,ci_result=$11,source=$12,recorded_by=$13,recorded_at=$14
WHERE change_id=$1 AND head_sha=$2 RETURNING `+verificationColumns, args...), &v)
		default:
			return Verification{}, false, ErrVerificationExists
		}
	}
	if err != nil {
		return Verification{}, false, err
	}
	v.Current = changeHead != nil && *changeHead == v.HeadSHA
	return v, created, tx.Commit(ctx)
}
