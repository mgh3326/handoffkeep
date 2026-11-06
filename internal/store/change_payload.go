package store

import (
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode"
)

// maxEventReviews caps reviews[] on one mergewake.review event.
const maxEventReviews = 16

var (
	// gitSHARE is a full git object id: head, base and merge SHAs.
	gitSHARE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// patchIDRE is a git patch-id, which may be shortened.
	patchIDRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	// reviewTokenRE is a lens, verdict or family: a short lower-case token.
	reviewTokenRE = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
	// reviewProfileRE is a launch profile such as sonnet@high.
	reviewProfileRE = regexp.MustCompile(`^[a-z0-9_.@-]{1,64}$`)
	// patchDigestRE is the digest half of mergewake's patch receipt
	// "<patch-id>:<digest>": lower-case hex.
	patchDigestRE = regexp.MustCompile(`^[0-9a-f]{12,128}$`)
)

const (
	maxReviewText  = 200  // model, harness, effort, executor, policy_revision, contract_source, reviewer_job_id
	maxReviewCount = 1e6  // a severity count above this is a unit mistake, not a count
	maxReviewRound = 1000 // round and pass
)

// TaskEventReview is one review pass inside a mergewake.review event. Counts
// are severity totals only; a nil count is unknown. The first write of a
// (reviewed_head, lens, round, pass) wins.
type TaskEventReview struct {
	ReviewedHead   string     `json:"reviewed_head"`
	ReviewedBase   string     `json:"reviewed_base,omitempty"`
	PatchID        string     `json:"patch_id,omitempty"`
	Round          int        `json:"round"`
	Pass           int        `json:"pass"`
	Lens           string     `json:"lens"`
	ReviewerJobID  string     `json:"reviewer_job_id,omitempty"`
	Family         string     `json:"family,omitempty"`
	Profile        string     `json:"profile,omitempty"`
	Model          string     `json:"model,omitempty"`
	Harness        string     `json:"harness,omitempty"`
	Effort         string     `json:"effort,omitempty"`
	Executor       string     `json:"executor,omitempty"`
	Fallback       *bool      `json:"fallback,omitempty"`
	PolicyRevision string     `json:"policy_revision,omitempty"`
	ContractSource string     `json:"contract_source,omitempty"`
	Verdict        string     `json:"verdict,omitempty"`
	Critical       *int       `json:"critical,omitempty"`
	High           *int       `json:"high,omitempty"`
	Medium         *int       `json:"medium,omitempty"`
	Low            *int       `json:"low,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ReviewHistory
}

// ReviewHistory is the optional review-history fields of a review pass
// (hk 3208). A nil field is unknown, never false or zero. The same fields ride
// on a findings write's review key (ChangeFindingsWrite).
type ReviewHistory struct {
	// Reviewer is the reviewer profile or login.
	Reviewer *string `json:"reviewer,omitempty"`
	// SameFamily says the pass's reviewer family equals the author's.
	SameFamily *bool `json:"same_family,omitempty"`
	// Delta marks a fix-delta pass, one that reviewed only the fix delta.
	Delta *bool `json:"delta,omitempty"`
	// Quiet marks a quiet pass.
	Quiet *bool `json:"quiet,omitempty"`
	// OutsideDiffBlocking counts blocking findings outside the diff.
	OutsideDiffBlocking *int `json:"outside_diff_blocking,omitempty"`
	// PatchDigest is the digest half of the patch receipt; PatchID stays the
	// patch-id half.
	PatchDigest *string `json:"patch_digest,omitempty"`
}

// check validates the fields that are set. It returns a plain message so the
// event path and the findings path can each wrap it in their own error.
func (h ReviewHistory) check() error {
	if h.Reviewer != nil && (*h.Reviewer == "" || !validReviewText(*h.Reviewer)) {
		return fmt.Errorf("reviewer must be non-empty printable text of at most %d bytes", maxReviewText)
	}
	if h.PatchDigest != nil && !patchDigestRE.MatchString(*h.PatchDigest) {
		return errors.New("patch_digest must be 12 to 128 lower-case hex digits")
	}
	if h.OutsideDiffBlocking != nil && (*h.OutsideDiffBlocking < 0 || *h.OutsideDiffBlocking > maxReviewCount) {
		return fmt.Errorf("outside_diff_blocking must be 0 to %d", int(maxReviewCount))
	}
	return nil
}

// checkEventChangeFields validates the work-record fields of an event whose
// source is already known to be a mergewake one.
func checkEventChangeFields(in TaskSourceEvent) error {
	for name, v := range map[string]string{"head_sha": in.HeadSHA, "base_sha": in.BaseSHA, "merge_sha": in.MergeSHA} {
		if v != "" && !gitSHARE.MatchString(v) {
			return fmt.Errorf("invalid task event: %s must be 40 lower-case hex digits", name)
		}
	}
	if in.MergeSHA != "" && in.Source != SourceMergewakeMerge {
		return errors.New("invalid task event: merge_sha belongs to mergewake.merge")
	}
	if len(in.Reviews) > 0 && in.Source != SourceMergewakeReview {
		return errors.New("invalid task event: reviews belong to mergewake.review")
	}
	if len(in.Reviews) > maxEventReviews {
		return fmt.Errorf("invalid task event: reviews has more than %d items", maxEventReviews)
	}
	for i, rv := range in.Reviews {
		if err := checkEventReview(rv); err != nil {
			return fmt.Errorf("invalid task event: reviews[%d]: %w", i, err)
		}
	}
	return nil
}

func checkEventReview(rv TaskEventReview) error {
	if !gitSHARE.MatchString(rv.ReviewedHead) {
		return errors.New("reviewed_head must be 40 lower-case hex digits")
	}
	if rv.ReviewedBase != "" && !gitSHARE.MatchString(rv.ReviewedBase) {
		return errors.New("reviewed_base must be 40 lower-case hex digits")
	}
	if rv.PatchID != "" && !patchIDRE.MatchString(rv.PatchID) {
		return errors.New("patch_id must be 7 to 64 lower-case hex digits")
	}
	if rv.Round < 1 || rv.Round > maxReviewRound || rv.Pass < 1 || rv.Pass > maxReviewRound {
		return fmt.Errorf("round and pass must be 1 to %d", maxReviewRound)
	}
	if !reviewTokenRE.MatchString(rv.Lens) {
		return errors.New("lens must be a short [a-z0-9_.-] token")
	}
	for name, v := range map[string]string{"verdict": rv.Verdict, "family": rv.Family} {
		if v != "" && !reviewTokenRE.MatchString(v) {
			return fmt.Errorf("%s must be a short [a-z0-9_.-] token", name)
		}
	}
	if rv.Profile != "" && !reviewProfileRE.MatchString(rv.Profile) {
		return errors.New("profile must be a short [a-z0-9_.@-] token")
	}
	for name, v := range map[string]string{"reviewer_job_id": rv.ReviewerJobID, "model": rv.Model, "harness": rv.Harness, "effort": rv.Effort, "executor": rv.Executor, "policy_revision": rv.PolicyRevision, "contract_source": rv.ContractSource} {
		if !validReviewText(v) {
			return fmt.Errorf("%s must be printable text of at most %d bytes", name, maxReviewText)
		}
	}
	for name, v := range map[string]*int{"critical": rv.Critical, "high": rv.High, "medium": rv.Medium, "low": rv.Low} {
		if v != nil && (*v < 0 || *v > maxReviewCount) {
			return fmt.Errorf("%s must be 0 to %d", name, int(maxReviewCount))
		}
	}
	if rv.StartedAt != nil && rv.FinishedAt != nil && rv.FinishedAt.Before(*rv.StartedAt) {
		return errors.New("finished_at is before started_at")
	}
	return rv.ReviewHistory.check()
}

func validReviewText(v string) bool {
	if len(v) > maxReviewText {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
