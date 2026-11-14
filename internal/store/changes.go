package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// The work record's changes (hk 3037; design hk:doc
// design/2026-11-05/hk-work-record-design-v1.1 §2). A change is one pull
// request, keyed by (repo, pr_number) from its URL and shared by every task
// that names it. mergewake's task events write it (recordChangeTx); an
// operator sets its merge-flow fields (SetChangeMergeFlow).

var (
	ErrChangeNotFound        = errors.New("change_not_found")
	ErrChangeDependencyCycle = errors.New("change_dependency_cycle")
)

// Change relations to a task.
const (
	ChangeImplements = "implements"
	ChangeReferences = "references"
)

// Change states.
const (
	ChangeOpen   = "open"
	ChangeMerged = "merged"
	ChangeClosed = "closed"
)

// MaxHoldReason is the byte cap of a change's hold_reason.
const MaxHoldReason = 300

// changeAdvisoryLock serializes the merge-flow writes. Two operators setting
// A->B and B->A at once would each pass the cycle check against the other's
// uncommitted state; one writer at a time makes the check exact.
const changeAdvisoryLock = 8241873041

// ChangeTask is one task linked to a change.
type ChangeTask struct {
	TaskID   int64  `json:"task_id"`
	Relation string `json:"relation"`
}

// ChangeReview is one stored review pass of a change.
type ChangeReview struct {
	ID             int64      `json:"id"`
	ReviewedHead   string     `json:"reviewed_head"`
	ReviewedBase   *string    `json:"reviewed_base,omitempty"`
	PatchID        *string    `json:"patch_id,omitempty"`
	Round          int        `json:"round"`
	Pass           int        `json:"pass"`
	Lens           string     `json:"lens"`
	ReviewerJobID  *string    `json:"reviewer_job_id,omitempty"`
	Family         *string    `json:"family,omitempty"`
	Profile        *string    `json:"profile,omitempty"`
	Model          *string    `json:"model,omitempty"`
	Harness        *string    `json:"harness,omitempty"`
	Effort         *string    `json:"effort,omitempty"`
	Executor       *string    `json:"executor,omitempty"`
	Fallback       *bool      `json:"fallback,omitempty"`
	PolicyRevision *string    `json:"policy_revision,omitempty"`
	ContractSource *string    `json:"contract_source,omitempty"`
	Verdict        *string    `json:"verdict,omitempty"`
	Critical       *int       `json:"critical,omitempty"`
	High           *int       `json:"high,omitempty"`
	Medium         *int       `json:"medium,omitempty"`
	Low            *int       `json:"low,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	RecordedAt     time.Time  `json:"recorded_at"`
	// Review-history fields (hk 3208); omitted when NULL (unknown).
	ReviewHistory
}

// Change is a pull request as the work record holds it.
type Change struct {
	ID                 int64          `json:"id"`
	Repo               string         `json:"repo"`
	PRNumber           int            `json:"pr_number"`
	PR                 string         `json:"pr"`
	State              string         `json:"state"`
	HeadSHA            *string        `json:"head_sha,omitempty"`
	BaseSHA            *string        `json:"base_sha,omitempty"`
	MergeSHA           *string        `json:"merge_sha,omitempty"`
	OpenedAt           time.Time      `json:"opened_at"`
	MergedAt           *time.Time     `json:"merged_at,omitempty"`
	RevertedByChangeID *int64         `json:"reverted_by_change_id,omitempty"`
	Priority           int            `json:"priority"`
	HoldReason         *string        `json:"hold_reason,omitempty"`
	DependsOnChangeID  *int64         `json:"depends_on_change_id,omitempty"`
	DependsOnState     *string        `json:"depends_on_state,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	Tasks              []ChangeTask   `json:"tasks"`
	Reviews            []ChangeReview `json:"reviews"`
	// Verification is the verdict on the current head, or nil; Verifications
	// are all verdicts, newest first (hk 3120).
	Verification  *Verification  `json:"verification"`
	Verifications []Verification `json:"verifications"`
	// PR state mergewake writes (hk 3198): the gate decision, draft flag, the
	// oldest written section's observed_at and the open thread and finding
	// counts. A section never written leaves its field unset.
	Draft        *bool      `json:"draft,omitempty"`
	GateState    *string    `json:"gate_state,omitempty"`
	FreshnessMin *time.Time `json:"freshness_min,omitempty"`
	OpenThreads  int        `json:"open_threads"`
	OpenFindings int        `json:"open_findings"`
}

// ChangeMergeFlow is the merge-flow field set. SetChangeMergeFlow replaces
// all of it: a zero Priority, an empty HoldReason and a nil DependsOnChange
// clear the field.
type ChangeMergeFlow struct {
	Priority        int    `json:"priority"`
	HoldReason      string `json:"hold_reason"`
	DependsOnChange *int64 `json:"depends_on_change"`
	DependsOnState  string `json:"depends_on_state"`
}

// parsePRURL splits a PR URL (taskEventPRRE shape) into repo and number. A
// number that does not fit the column is not a GitHub PR number; it reports
// false and the event is applied to the task without a change row.
func parsePRURL(url string) (repo string, n int, ok bool) {
	rest, found := strings.CutPrefix(url, "https://github.com/")
	if !found {
		return "", 0, false
	}
	i := strings.LastIndex(rest, "/pull/")
	if i < 0 {
		return "", 0, false
	}
	n64, err := strconv.ParseInt(rest[i+len("/pull/"):], 10, 32)
	if err != nil || n64 < 1 {
		return "", 0, false
	}
	return rest[:i], int(n64), true
}

func changeURL(repo string, n int) string {
	return "https://github.com/" + repo + "/pull/" + strconv.Itoa(n)
}

// canonicalPRURL normalizes numeric spellings accepted by parsePRURL before
// they are persisted as task delivery evidence.
func canonicalPRURL(pr string) string {
	repo, n, ok := parsePRURL(pr)
	if !ok {
		return pr
	}
	return changeURL(repo, n)
}

// nullStr maps "" to SQL NULL.
func nullStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// changeWrite is one upsert of a changes row.
type changeWrite struct {
	repo       string
	number     int
	state      string // ChangeOpen or ChangeMerged
	head, base string
	mergeSHA   string
	mergedAt   *time.Time
	// shaFromMerge lets head and base replace the stored ones on a merged
	// change: only the merge event does.
	shaFromMerge bool
}

// upsertChangeTx writes one change and returns its id. A new row is open
// unless the write says merged. An existing row never leaves merged, and a
// non-merge write never changes its state at all; a sent SHA replaces the
// stored one except on a merged change (shaFromMerge excepted).
func upsertChangeTx(ctx context.Context, tx pgx.Tx, w changeWrite, at, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO changes(repo,pr_number,state,head_sha,base_sha,merge_sha,opened_at,merged_at,created_at,updated_at)
VALUES($1,$2,$3::text,$4,$5,$6,$7,$8,$9,$9)
ON CONFLICT(repo,pr_number) DO UPDATE SET
	state = CASE WHEN $3::text='merged' THEN 'merged' ELSE changes.state END,
	merged_at = CASE WHEN $3::text='merged' THEN COALESCE(changes.merged_at,$8) ELSE changes.merged_at END,
	merge_sha = COALESCE($6,changes.merge_sha),
	head_sha = CASE WHEN changes.state='merged' AND NOT $10::boolean THEN changes.head_sha ELSE COALESCE($4,changes.head_sha) END,
	base_sha = CASE WHEN changes.state='merged' AND NOT $10::boolean THEN changes.base_sha ELSE COALESCE($5,changes.base_sha) END,
	updated_at = $9
RETURNING id`, w.repo, w.number, w.state, nullStr(w.head), nullStr(w.base), nullStr(w.mergeSHA), at, w.mergedAt, now, w.shaFromMerge).Scan(&id)
	return id, err
}

// linkChangeTaskTx links a task to a change. An existing link stays as it is
// (the first relation wins) unless reclassify is set, which makes the new
// relation replace it: the revert PR's own mergewake.pr event may have linked
// it as implements before the revert event names it.
func linkChangeTaskTx(ctx context.Context, tx pgx.Tx, changeID, taskID int64, relation string, reclassify bool, now time.Time) error {
	conflict := `DO NOTHING`
	if reclassify {
		conflict = `(change_id,task_id) DO UPDATE SET relation=EXCLUDED.relation`
	}
	_, err := tx.Exec(ctx, `INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES($1,$2,$3,$4) ON CONFLICT `+conflict, changeID, taskID, relation, now)
	return err
}

// recordChangeTx writes what a mergewake event says about its pull request:
// the change row, its link to task x and, on mergewake.review, its review
// rows. The caller has locked x under the task-mutation lock (level 2, see
// lockorder.go) and runs this for every event, redelivered or not applied
// too: the rows describe the PR, not the task's state. The two source-event
// tables stay out of it; applySourceEventTx is shared with sources that name
// no PR.
func recordChangeTx(ctx context.Context, tx pgx.Tx, x Task, in TaskSourceEvent, at, now time.Time) error {
	repo, n, ok := parsePRURL(in.PR)
	if !ok {
		return nil
	}
	w := changeWrite{repo: repo, number: n, state: ChangeOpen, head: in.HeadSHA, base: in.BaseSHA}
	switch in.Source {
	case SourceMergewakeMerge:
		w.state, w.mergedAt, w.mergeSHA, w.shaFromMerge = ChangeMerged, &at, in.MergeSHA, true
	case SourceMergewakeRevert:
		// pr is the reverted PR, which was merged; its sent SHAs describe the
		// revert PR (source_ref), recorded below.
		w.state, w.head, w.base = ChangeMerged, "", ""
	}
	id, err := upsertChangeTx(ctx, tx, w, at, now)
	if err != nil {
		return err
	}
	if err = linkChangeTaskTx(ctx, tx, id, x.ID, ChangeImplements, false, now); err != nil {
		return err
	}
	switch in.Source {
	case SourceMergewakeReview:
		for _, rv := range in.Reviews {
			if err = insertReviewTx(ctx, tx, id, rv, now); err != nil {
				return err
			}
		}
	case SourceMergewakeRevert:
		revRepo, revN, ok := parsePRURL(in.RevertPR)
		if !ok {
			return nil
		}
		revID, err := upsertChangeTx(ctx, tx, changeWrite{repo: revRepo, number: revN, state: ChangeMerged, head: in.HeadSHA, base: in.BaseSHA, mergedAt: &at}, at, now)
		if err != nil {
			return err
		}
		if revID != id {
			if err = linkChangeTaskTx(ctx, tx, revID, x.ID, ChangeReferences, true, now); err != nil {
				return err
			}
			// The first revert recorded stays; a later one does not replace it.
			if _, err = tx.Exec(ctx, `UPDATE changes SET reverted_by_change_id=$2, updated_at=$3 WHERE id=$1 AND reverted_by_change_id IS NULL`, id, revID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// reviewBareSQL is true for a reviews row that carries nothing but its key,
// apart from the review-history fields (reviewHistoryFillSQL): the row a
// findings write creates before any event reports the pass.
const reviewBareSQL = `reviews.reviewed_base IS NULL AND reviews.patch_id IS NULL AND reviews.reviewer_job_id IS NULL AND reviews.family IS NULL AND reviews.profile IS NULL AND reviews.model IS NULL AND reviews.harness IS NULL AND reviews.effort IS NULL AND reviews.executor IS NULL AND reviews.fallback IS NULL AND reviews.policy_revision IS NULL AND reviews.contract_source IS NULL AND reviews.verdict IS NULL AND reviews.critical IS NULL AND reviews.high IS NULL AND reviews.medium IS NULL AND reviews.low IS NULL AND reviews.started_at IS NULL AND reviews.finished_at IS NULL`

// reviewHistoryFillSQL fills the review-history columns of a bare row from
// EXCLUDED; a column already set keeps its value, so the first report of a
// field wins. reviewHistoryGivenSQL is true when EXCLUDED carries any of them.
const (
	reviewHistoryFillSQL  = `reviewer=COALESCE(reviews.reviewer,EXCLUDED.reviewer),same_family=COALESCE(reviews.same_family,EXCLUDED.same_family),delta=COALESCE(reviews.delta,EXCLUDED.delta),quiet=COALESCE(reviews.quiet,EXCLUDED.quiet),outside_diff_blocking=COALESCE(reviews.outside_diff_blocking,EXCLUDED.outside_diff_blocking),patch_digest=COALESCE(reviews.patch_digest,EXCLUDED.patch_digest)`
	reviewHistoryGivenSQL = `(EXCLUDED.reviewer IS NOT NULL OR EXCLUDED.same_family IS NOT NULL OR EXCLUDED.delta IS NOT NULL OR EXCLUDED.quiet IS NOT NULL OR EXCLUDED.outside_diff_blocking IS NOT NULL OR EXCLUDED.patch_digest IS NOT NULL)`
)

// args is the review-history values in column order: reviewer,
// same_family, delta, quiet, outside_diff_blocking, patch_digest.
func (h ReviewHistory) args() []any {
	return []any{h.Reviewer, h.SameFamily, h.Delta, h.Quiet, h.OutsideDiffBlocking, h.PatchDigest}
}

// insertReviewTx records one review pass. The first report of a pass wins and
// a redelivery changes nothing, except that a row carrying nothing but its key
// (a findings write got there first, ApplyChangeFindings) is filled in by the
// first report that carries something, review-history fields included; a
// field the findings write already set keeps its value.
func insertReviewTx(ctx context.Context, tx pgx.Tx, changeID int64, rv TaskEventReview, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO reviews(change_id,reviewed_head,reviewed_base,patch_id,round,pass,lens,reviewer_job_id,family,profile,model,harness,effort,executor,fallback,policy_revision,contract_source,verdict,critical,high,medium,low,started_at,finished_at,recorded_at,reviewer,same_family,delta,quiet,outside_diff_blocking,patch_digest)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31)
ON CONFLICT(change_id,reviewed_head,lens,round,pass) DO UPDATE SET reviewed_base=EXCLUDED.reviewed_base,patch_id=EXCLUDED.patch_id,reviewer_job_id=EXCLUDED.reviewer_job_id,family=EXCLUDED.family,profile=EXCLUDED.profile,model=EXCLUDED.model,harness=EXCLUDED.harness,effort=EXCLUDED.effort,executor=EXCLUDED.executor,fallback=EXCLUDED.fallback,policy_revision=EXCLUDED.policy_revision,contract_source=EXCLUDED.contract_source,verdict=EXCLUDED.verdict,critical=EXCLUDED.critical,high=EXCLUDED.high,medium=EXCLUDED.medium,low=EXCLUDED.low,started_at=EXCLUDED.started_at,finished_at=EXCLUDED.finished_at,`+reviewHistoryFillSQL+`
WHERE `+reviewBareSQL,
		append([]any{changeID, rv.ReviewedHead, nullStr(rv.ReviewedBase), nullStr(rv.PatchID), rv.Round, rv.Pass, rv.Lens, nullStr(rv.ReviewerJobID),
			nullStr(rv.Family), nullStr(rv.Profile), nullStr(rv.Model), nullStr(rv.Harness), nullStr(rv.Effort), nullStr(rv.Executor), rv.Fallback,
			nullStr(rv.PolicyRevision), nullStr(rv.ContractSource), nullStr(rv.Verdict), rv.Critical, rv.High, rv.Medium, rv.Low,
			rv.StartedAt, rv.FinishedAt, now}, rv.ReviewHistory.args()...)...)
	return err
}

const changeColumns = `id,repo,pr_number,state,head_sha,base_sha,merge_sha,opened_at,merged_at,reverted_by_change_id,priority,hold_reason,depends_on_change_id,depends_on_state,created_at,updated_at,
draft,gate_state,LEAST(state_observed_at,checks_observed_at,threads_observed_at,findings_observed_at,gate_decided_at),
(SELECT count(*) FROM change_threads t WHERE t.change_id=changes.id AND NOT t.resolved AND NOT t.gone),
(SELECT count(*) FROM change_findings f JOIN changes c ON c.id=f.change_id ` + findingThreadJoin + `
	WHERE f.change_id=changes.id AND ` + findingStatusSQL + `='open')`

func scanChange(row pgx.Row, c *Change) error {
	if err := row.Scan(&c.ID, &c.Repo, &c.PRNumber, &c.State, &c.HeadSHA, &c.BaseSHA, &c.MergeSHA, &c.OpenedAt, &c.MergedAt, &c.RevertedByChangeID, &c.Priority, &c.HoldReason, &c.DependsOnChangeID, &c.DependsOnState, &c.CreatedAt, &c.UpdatedAt, &c.Draft, &c.GateState, &c.FreshnessMin, &c.OpenThreads, &c.OpenFindings); err != nil {
		return err
	}
	c.PR = changeURL(c.Repo, c.PRNumber)
	c.Tasks, c.Reviews = []ChangeTask{}, []ChangeReview{}
	return nil
}

// ListChangesByTask lists the changes linked to a task, ordered by id.
func (s *Store) ListChangesByTask(ctx context.Context, taskID int64, limit int) ([]Change, error) {
	return s.listChangesWhere(ctx, s.pool, `id IN (SELECT change_id FROM change_tasks WHERE task_id=$1)`, limit, taskID)
}

// ListChangesByPR lists the change for a PR URL: none or one row.
func (s *Store) ListChangesByPR(ctx context.Context, pr string, limit int) ([]Change, error) {
	repo, n, ok := parsePRURL(pr)
	if !ok || !taskEventPRRE.MatchString(pr) {
		return nil, errors.New("invalid change query: pr must be a PR URL")
	}
	return s.listChangesWhere(ctx, s.pool, `repo=$1 AND pr_number=$2`, limit, repo, n)
}

type changeReader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// readReviews reads the reviews of the given changes ordered by id and calls
// add for each, in that order. The list read and the one-PR read share it.
func readReviews(ctx context.Context, q changeReader, changeIDs []int64, add func(changeID int64, r ChangeReview)) error {
	rows, err := q.Query(ctx, `SELECT change_id,id,reviewed_head,reviewed_base,patch_id,round,pass,lens,reviewer_job_id,family,profile,model,harness,effort,executor,fallback,policy_revision,contract_source,verdict,critical,high,medium,low,started_at,finished_at,recorded_at,reviewer,same_family,delta,quiet,outside_diff_blocking,patch_digest FROM reviews WHERE change_id=ANY($1) ORDER BY id`, changeIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r ChangeReview
		if err = rows.Scan(&id, &r.ID, &r.ReviewedHead, &r.ReviewedBase, &r.PatchID, &r.Round, &r.Pass, &r.Lens, &r.ReviewerJobID, &r.Family, &r.Profile, &r.Model, &r.Harness, &r.Effort, &r.Executor, &r.Fallback, &r.PolicyRevision, &r.ContractSource, &r.Verdict, &r.Critical, &r.High, &r.Medium, &r.Low, &r.StartedAt, &r.FinishedAt, &r.RecordedAt, &r.Reviewer, &r.SameFamily, &r.Delta, &r.Quiet, &r.OutsideDiffBlocking, &r.PatchDigest); err != nil {
			return err
		}
		add(id, r)
	}
	return rows.Err()
}

// listChangesWhere reads changes ordered by id, then their tasks and reviews,
// each ordered by id, and their verifications, newest first. where may use
// only $1..$len(args).
func (s *Store) listChangesWhere(ctx context.Context, q changeReader, where string, limit int, args ...any) ([]Change, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+changeColumns+` FROM changes WHERE `+where+fmt.Sprintf(` ORDER BY id LIMIT $%d`, len(args)+1), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Change, error) {
		var c Change
		return c, scanChange(row, &c)
	})
	if err != nil || len(out) == 0 {
		return []Change{}, err
	}
	ids := make([]int64, len(out))
	at := make(map[int64]int, len(out))
	for i, c := range out {
		ids[i], at[c.ID] = c.ID, i
		out[i].Verifications = []Verification{}
	}
	trows, err := q.Query(ctx, `SELECT change_id,task_id,relation FROM change_tasks WHERE change_id=ANY($1) ORDER BY change_id,task_id`, ids)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var id int64
		var t ChangeTask
		if err = trows.Scan(&id, &t.TaskID, &t.Relation); err != nil {
			trows.Close()
			return nil, err
		}
		out[at[id]].Tasks = append(out[at[id]].Tasks, t)
	}
	trows.Close()
	if err = trows.Err(); err != nil {
		return nil, err
	}
	err = readReviews(ctx, q, ids, func(changeID int64, r ChangeReview) {
		out[at[changeID]].Reviews = append(out[at[changeID]].Reviews, r)
	})
	if err != nil {
		return nil, err
	}
	vrows, err := q.Query(ctx, `SELECT `+verificationColumns+` FROM verifications WHERE change_id=ANY($1) ORDER BY recorded_at DESC, id DESC`, ids)
	if err != nil {
		return nil, err
	}
	for vrows.Next() {
		var v Verification
		if err = scanVerification(vrows, &v); err != nil {
			vrows.Close()
			return nil, err
		}
		c := &out[at[v.ChangeID]]
		v.Current = c.HeadSHA != nil && *c.HeadSHA == v.HeadSHA
		c.Verifications = append(c.Verifications, v)
		if v.Current {
			current := v
			c.Verification = &current
		}
	}
	vrows.Close()
	return out, vrows.Err()
}

// checkChangeMergeFlow validates a merge-flow field set that needs no
// database: the rest (the target exists, no cycle) is checked under the lock.
func checkChangeMergeFlow(in ChangeMergeFlow) error {
	if in.Priority < 0 || in.Priority > 1_000_000 {
		return errors.New("invalid change merge flow: priority must be 0 to 1000000")
	}
	if len(in.HoldReason) > MaxHoldReason || !validText(in.HoldReason, MaxHoldReason) {
		return fmt.Errorf("invalid change merge flow: hold_reason must be at most %d bytes", MaxHoldReason)
	}
	if err := guard.Reject(in.HoldReason); err != nil {
		return err
	}
	switch {
	case in.DependsOnChange == nil && in.DependsOnState != "":
		return errors.New("invalid change merge flow: depends_on_state needs depends_on_change")
	case in.DependsOnChange != nil && *in.DependsOnChange < 1:
		return errors.New("invalid change merge flow: depends_on_change must be positive")
	case in.DependsOnChange != nil && in.DependsOnState != "merged" && in.DependsOnState != "deployed":
		return errors.New("invalid change merge flow: depends_on_state must be merged or deployed")
	}
	return nil
}

// SetChangeMergeFlow replaces a change's priority, hold_reason and
// dependency; an omitted field is cleared. It is an operator control, not a
// tool record. It takes the merge-flow advisory lock, then locks the one
// changes row (lockorder.go).
func (s *Store) SetChangeMergeFlow(ctx context.Context, id int64, in ChangeMergeFlow) (Change, error) {
	if err := checkChangeMergeFlow(in); err != nil {
		return Change{}, err
	}
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Change{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, changeAdvisoryLock); err != nil {
		return Change{}, err
	}
	var locked int64
	if err = tx.QueryRow(ctx, `SELECT id FROM changes WHERE id=$1 FOR NO KEY UPDATE`, id).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Change{}, ErrChangeNotFound
		}
		return Change{}, err
	}
	var state *string
	if in.DependsOnChange != nil {
		target := *in.DependsOnChange
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM changes WHERE id=$1)`, target).Scan(&exists); err != nil {
			return Change{}, err
		}
		if !exists {
			return Change{}, ErrChangeNotFound
		}
		// Follow the target's own dependencies; reaching id closes a cycle.
		// UNION (not ALL) ends the walk even on a cycle that already exists.
		var cycle bool
		if err = tx.QueryRow(ctx, `WITH RECURSIVE chain(id) AS (
	SELECT $2::bigint
	UNION
	SELECT c.depends_on_change_id FROM changes c JOIN chain ON c.id=chain.id WHERE c.depends_on_change_id IS NOT NULL
) SELECT EXISTS(SELECT 1 FROM chain WHERE id=$1)`, id, target).Scan(&cycle); err != nil {
			return Change{}, err
		}
		if cycle {
			return Change{}, fmt.Errorf("%w: change %d would depend on itself through change %d", ErrChangeDependencyCycle, id, target)
		}
		state = &in.DependsOnState
	}
	if _, err = tx.Exec(ctx, `UPDATE changes SET priority=$2, hold_reason=$3, depends_on_change_id=$4, depends_on_state=$5, updated_at=$6 WHERE id=$1`,
		id, in.Priority, nullStr(in.HoldReason), in.DependsOnChange, state, now); err != nil {
		return Change{}, err
	}
	out, err := s.listChangesWhere(ctx, tx, `id=$1`, 1, id)
	if err != nil {
		return Change{}, err
	}
	if len(out) != 1 {
		return Change{}, ErrChangeNotFound
	}
	return out[0], tx.Commit(ctx)
}
