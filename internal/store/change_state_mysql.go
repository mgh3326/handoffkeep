package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func lockChangeMySQL(ctx context.Context, tx transaction, b ChangeBatch, now time.Time) (changeLock, error) {
	_, err := tx.Exec(ctx, `INSERT INTO changes(repo,pr_number,state,head_sha,opened_at,created_at,updated_at) VALUES(?,?,'open',?,?,?,?) ON DUPLICATE KEY UPDATE id=id`, b.Repo, b.PR, nullStr(b.stubHead()), now, now, now)
	if err != nil {
		return changeLock{}, err
	}
	var c changeLock
	err = tx.QueryRow(ctx, `SELECT id,state,head_sha,base_sha,merge_sha,base_ref,title,author,merged_at,draft,state_observed_at,checks_observed_at,threads_observed_at,findings_observed_at,gate_decided_at FROM changes WHERE repo=? AND pr_number=? FOR UPDATE`, b.Repo, b.PR).Scan(&c.id, &c.state, &c.head, &c.base, &c.mergeSHA, &c.baseRef, &c.title, &c.author, &c.mergedAt, &c.draft, &c.stateObserved, &c.checksObserved, &c.threadsObserved, &c.findingsObserved, &c.gateDecided)
	return c, err
}

func updateChangeMySQL(ctx context.Context, tx transaction, id int64, touch *time.Time, cols ...setCol) error {
	sets := make([]string, 0, len(cols)+1)
	args := make([]any, 0, len(cols)*2+2)
	checks := make([]string, 0, len(cols))
	values := make([]any, 0, len(cols))
	for _, c := range cols {
		sets = append(sets, c.name+"=?")
		checks = append(checks, c.name+" <=> ?")
		args = append(args, c.val)
		values = append(values, c.val)
	}
	if touch != nil {
		sets = append(sets, "updated_at=?")
		args = append(args, *touch)
	}
	args = append(args, id)
	args = append(args, values...)
	query := `UPDATE changes SET ` + strings.Join(sets, ",") + ` WHERE id=?`
	if len(checks) > 0 {
		query += ` AND NOT (` + strings.Join(checks, " AND ") + `)`
	}
	_, err := tx.Exec(ctx, query, args...)
	return err
}

func applyChangeStateMySQL(ctx context.Context, tx transaction, c changeLock, w ChangeStateWrite, now time.Time) (ChangeSectionResult, error) {
	if olderThan(c.stateObserved, w.ObservedAt) {
		return staleResult, nil
	}
	title, redacted := screened(w.Title, func(s string) string { return capBytes(s, maxChangeTitle) })
	if w.Title == "" {
		title = c.title
	}
	state, head, base := w.State, nullStr(w.HeadSHA), nullStr(w.BaseSHA)
	if base == nil {
		base = c.base
	}
	if c.state == ChangeMerged && w.State != ChangeMerged {
		state, head, base = ChangeMerged, c.head, c.base
	}
	mergeSHA, mergedAt := c.mergeSHA, c.mergedAt
	if w.MergeSHA != "" {
		mergeSHA = &w.MergeSHA
	}
	if w.State == ChangeMerged && mergedAt == nil {
		at := w.ObservedAt
		if w.MergedAt != nil {
			at = *w.MergedAt
		}
		mergedAt = &at
	}
	pick := func(v string, old *string) *string {
		if v == "" {
			return old
		}
		return &v
	}
	draft := c.draft
	if w.Draft != nil {
		draft = w.Draft
	}
	err := updateChangeMySQL(ctx, tx, c.id, &now,
		setCol{"state", "", state}, setCol{"head_sha", "", head}, setCol{"base_sha", "", base}, setCol{"merge_sha", "", mergeSHA},
		setCol{"merged_at", "", mergedAt}, setCol{"draft", "", draft}, setCol{"base_ref", "", pick(w.BaseRef, c.baseRef)},
		setCol{"title", "", title}, setCol{"author", "", pick(w.Author, c.author)}, setCol{"state_observed_at", "", w.ObservedAt},
		setCol{"state_source", "", w.Source}, setCol{"state_delivery_id", "", nullStr(w.DeliveryID)})
	return ChangeSectionResult{Applied: true, Redacted: redacted}, err
}

func markGoneMySQL(ctx context.Context, tx transaction, table string, changeID int64, observed time.Time, source string, keyColumn string, keys []string, head string) error {
	query := `UPDATE ` + table + ` SET gone=TRUE,observed_at=?,source=? WHERE change_id=? AND NOT gone`
	args := []any{observed, source, changeID}
	if head != "" {
		query += ` AND head_sha=?`
		args = append(args, head)
	}
	if len(keys) > 0 {
		query += ` AND ` + keyColumn + ` NOT IN (` + placeholders(len(keys)) + `)`
		for _, key := range keys {
			args = append(args, key)
		}
	}
	_, err := tx.Exec(ctx, query, args...)
	return err
}

// applyChangeChecksMySQL mirrors applyChecksTx: per-check freshness (hk 7050)
// — a stored row is replaced only when its observed_at is not newer than the
// write's, the write is stale only when it is older than the checks watermark
// and every check it names already has a newer row, checks_observed_at is the
// max of the stored value and the write's observed_at, and a complete write
// marks gone only rows not newer than the write. The changes row lock keeps
// the newer set stable for the transaction, so the upsert itself needs no
// freshness clause (ON DUPLICATE KEY UPDATE has no WHERE).
func applyChangeChecksMySQL(ctx context.Context, tx transaction, c changeLock, w ChangeChecksWrite) (ChangeSectionResult, error) {
	names := make([]string, 0, len(w.Checks))
	for _, k := range w.Checks {
		names = append(names, k.Name)
	}
	// The newest complete set's mark is read as stored before this write, and
	// a check with no row that the mark judges is not inserted (hk 7056), as in
	// applyChecksTx.
	mark, err := checkSetObservedMySQL(ctx, tx, c.id, w.HeadSHA)
	if err != nil {
		return ChangeSectionResult{}, err
	}
	stored := map[string]time.Time{}
	if len(names) > 0 {
		args := make([]any, 0, len(names)+2)
		args = append(args, c.id, w.HeadSHA)
		for _, n := range names {
			args = append(args, n)
		}
		rows, err := tx.Query(ctx, `SELECT name,observed_at FROM change_checks WHERE change_id=? AND head_sha=? AND name IN (`+placeholders(len(names))+`)`, args...)
		if err != nil {
			return ChangeSectionResult{}, err
		}
		for rows.Next() {
			var n string
			var at time.Time
			if err := rows.Scan(&n, &at); err != nil {
				rows.Close()
				return ChangeSectionResult{}, err
			}
			stored[n] = at
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return ChangeSectionResult{}, err
		}
		rows.Close()
	}
	settled := settledChecks(names, stored, w, mark)
	if olderThan(c.checksObserved, w.ObservedAt) && allSettled(names, settled) {
		return staleResult, nil
	}
	for _, k := range w.Checks {
		if settled[k.Name] {
			continue
		}
		_, err := tx.Exec(ctx, `INSERT INTO change_checks(change_id,head_sha,name,state,conclusion,url,required,started_at,completed_at,observed_at,source,gone,delivery_id)
VALUES(?,?,?,?,?,?,?,?,?,?,?,FALSE,?) ON DUPLICATE KEY UPDATE
	delivery_id=IF(NOT (state <=> VALUES(state) AND conclusion <=> VALUES(conclusion) AND url <=> VALUES(url) AND required <=> VALUES(required) AND started_at <=> VALUES(started_at) AND completed_at <=> VALUES(completed_at) AND observed_at <=> VALUES(observed_at) AND source <=> VALUES(source) AND gone <=> FALSE) OR (VALUES(delivery_id) IS NOT NULL AND NOT (delivery_id <=> VALUES(delivery_id))),VALUES(delivery_id),delivery_id),
	state=VALUES(state),conclusion=VALUES(conclusion),url=VALUES(url),required=VALUES(required),started_at=VALUES(started_at),completed_at=VALUES(completed_at),observed_at=VALUES(observed_at),source=VALUES(source),gone=FALSE`, c.id, w.HeadSHA, k.Name, k.State, nullStr(k.Conclusion), nullStr(k.URL), k.Required, k.StartedAt, k.CompletedAt, w.ObservedAt, w.Source, nullStr(w.DeliveryID))
		if err != nil {
			return ChangeSectionResult{}, err
		}
	}
	if w.Complete {
		// Rows already gone also take the write's observed_at when it is newer,
		// as in applyChecksTx, so an older write cannot revive them.
		query := `UPDATE change_checks SET gone=TRUE,observed_at=?,source=? WHERE change_id=? AND head_sha=? AND observed_at<=? AND name NOT IN (` + placeholders(len(names)) + `) AND (NOT gone OR observed_at<?)`
		args := []any{w.ObservedAt, w.Source, c.id, w.HeadSHA, w.ObservedAt}
		for _, n := range names {
			args = append(args, n)
		}
		args = append(args, w.ObservedAt)
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			return ChangeSectionResult{}, err
		}
		// The mark only moves forward: the ON DUPLICATE KEY clause keeps a newer one.
		if _, err := tx.Exec(ctx, `INSERT INTO change_check_sets(change_id,head_sha,observed_at) VALUES(?,?,?)
ON DUPLICATE KEY UPDATE observed_at=IF(observed_at<VALUES(observed_at),VALUES(observed_at),observed_at)`, c.id, w.HeadSHA, w.ObservedAt); err != nil {
			return ChangeSectionResult{}, err
		}
	}
	if c.checksObserved == nil || w.ObservedAt.After(*c.checksObserved) {
		if err := updateChangeMySQL(ctx, tx, c.id, nil, setCol{"checks_observed_at", "", w.ObservedAt}, setCol{"checks_source", "", w.Source}); err != nil {
			return ChangeSectionResult{}, err
		}
	}
	return ChangeSectionResult{Applied: true}, nil
}

// checkSetObservedMySQL is checkSetObservedTx for MySQL (hk 7056).
func checkSetObservedMySQL(ctx context.Context, tx transaction, changeID int64, head string) (*time.Time, error) {
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT observed_at FROM change_check_sets WHERE change_id=? AND head_sha=?`, changeID, head).Scan(&at)
	if errors.Is(err, ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &at, nil
}

func applyChangeThreadsMySQL(ctx context.Context, tx transaction, c changeLock, w ChangeThreadsWrite) (ChangeSectionResult, error) {
	if olderThan(c.threadsObserved, w.ObservedAt) {
		return staleResult, nil
	}
	ids := make([]string, 0, len(w.Threads))
	redactedAny := false
	for _, t := range w.Threads {
		ids = append(ids, t.ThreadID)
		excerpt, redacted := screened(t.Excerpt, func(s string) string { return capRunes(s, MaxThreadExcerpt) })
		redactedAny = redactedAny || redacted
		_, err := tx.Exec(ctx, `INSERT INTO change_threads(change_id,thread_id,url,path,line,author,author_kind,severity,resolved,outdated,replied,comment_count,last_comment_at,excerpt,redacted,observed_at,source,gone)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,FALSE) ON DUPLICATE KEY UPDATE url=VALUES(url),path=VALUES(path),line=VALUES(line),author=VALUES(author),author_kind=VALUES(author_kind),severity=VALUES(severity),resolved=VALUES(resolved),outdated=VALUES(outdated),replied=VALUES(replied),comment_count=VALUES(comment_count),last_comment_at=VALUES(last_comment_at),excerpt=VALUES(excerpt),redacted=VALUES(redacted),observed_at=VALUES(observed_at),source=VALUES(source),gone=FALSE`, c.id, t.ThreadID, nullStr(t.URL), nullStr(t.Path), t.Line, nullStr(t.Author), t.AuthorKind, nullStr(t.Severity), t.Resolved, t.Outdated, t.Replied, t.CommentCount, t.LastCommentAt, excerpt, redacted, w.ObservedAt, w.Source)
		if err != nil {
			return ChangeSectionResult{}, err
		}
	}
	if w.Complete {
		if err := markGoneMySQL(ctx, tx, "change_threads", c.id, w.ObservedAt, w.Source, "thread_id", ids, ""); err != nil {
			return ChangeSectionResult{}, err
		}
	}
	err := updateChangeMySQL(ctx, tx, c.id, nil, setCol{"threads_observed_at", "", w.ObservedAt}, setCol{"threads_source", "", w.Source})
	return ChangeSectionResult{Applied: true, Redacted: redactedAny}, err
}

func ensureFindingReviewMySQL(ctx context.Context, tx transaction, c changeLock, w ChangeFindingsWrite, now time.Time) (int64, error) {
	values := append([]any{c.id, w.ReviewedHead, w.Round, w.Pass, w.Lens, now}, w.ReviewHistory.args()...)
	query := `INSERT INTO reviews(change_id,reviewed_head,round,pass,lens,recorded_at,reviewer,same_family,delta,quiet,outside_diff_blocking,patch_digest)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE id=id`
	if _, err := tx.Exec(ctx, query, values...); err != nil {
		return 0, err
	}
	var id int64
	var bare bool
	err := tx.QueryRow(ctx, `SELECT id,`+reviewBareMySQL+` FROM reviews WHERE change_id=? AND reviewed_head=? AND lens=? AND round=? AND pass=? FOR UPDATE`, c.id, w.ReviewedHead, w.Lens, w.Round, w.Pass).Scan(&id, &bare)
	if err != nil || !bare {
		return id, err
	}
	history := w.ReviewHistory
	if history == (ReviewHistory{}) {
		return id, nil
	}
	_, err = tx.Exec(ctx, `UPDATE reviews SET reviewer=COALESCE(reviewer,?),same_family=COALESCE(same_family,?),delta=COALESCE(delta,?),quiet=COALESCE(quiet,?),outside_diff_blocking=COALESCE(outside_diff_blocking,?),patch_digest=COALESCE(patch_digest,?) WHERE id=? AND `+reviewBareMySQL, append(history.args(), id)...)
	return id, err
}

func applyChangeFindingsMySQL(ctx context.Context, tx transaction, c changeLock, w ChangeFindingsWrite, now time.Time) (ChangeSectionResult, error) {
	if olderThan(c.findingsObserved, w.ObservedAt) {
		return staleResult, nil
	}
	source := w.Source
	if source == "" {
		source = defaultFindingSrc
	}
	reviewID, err := ensureFindingReviewMySQL(ctx, tx, c, w, now)
	if err != nil {
		return ChangeSectionResult{}, err
	}
	redactedAny := false
	for i, f := range w.Findings {
		summary, sr := screened(f.Summary, func(s string) string { return capRunes(s, MaxFindingSummary) })
		body, br := screened(f.Body, func(s string) string { return capBytes(s, MaxFindingBody) })
		redacted := sr || br
		redactedAny = redactedAny || redacted
		_, err = tx.Exec(ctx, `INSERT INTO change_findings(change_id,reviewed_head,round,pass,lens,ordinal,review_id,ext_id,location,severity,blocking,summary,url,author_replied,thread_id,body,redacted,observed_at,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE ordinal=ordinal`, c.id, w.ReviewedHead, w.Round, w.Pass, w.Lens, i, reviewID, f.ID, nullStr(f.Location), nullStr(f.Severity), f.Blocking, summary, nullStr(f.URL), f.AuthorReplied, nullStr(f.ThreadID), body, redacted, w.ObservedAt, source)
		if err != nil {
			return ChangeSectionResult{}, err
		}
	}
	err = updateChangeMySQL(ctx, tx, c.id, nil, setCol{"findings_observed_at", "", w.ObservedAt}, setCol{"findings_source", "", source})
	return ChangeSectionResult{Applied: true, Redacted: redactedAny}, err
}

func applyChangeGateMySQL(ctx context.Context, tx transaction, c changeLock, w ChangeGateWrite) (ChangeSectionResult, error) {
	if olderThan(c.gateDecided, w.DecidedAt) {
		return staleResult, nil
	}
	source := w.Source
	if source == "" {
		source = "review"
	}
	reasons := w.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	raw, err := json.Marshal(reasons)
	if err != nil {
		return ChangeSectionResult{}, err
	}
	err = updateChangeMySQL(ctx, tx, c.id, nil, setCol{"gate_state", "", w.State}, setCol{"gate_head", "", w.HeadSHA}, setCol{"gate_reasons", "", string(raw)}, setCol{"gate_decided_at", "", w.DecidedAt}, setCol{"gate_policy_revision", "", nullStr(w.PolicyRevision)}, setCol{"gate_source", "", source})
	return ChangeSectionResult{Applied: true}, err
}

func (s *Store) applyChangeBatchMySQL(ctx context.Context, b ChangeBatch) (ChangeWriteResult, error) {
	now := time.Now().UTC()
	if err := checkChangeBatch(b, now); err != nil {
		return ChangeWriteResult{}, err
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		return ChangeWriteResult{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	c, err := lockChangeMySQL(ctx, tx, b, now)
	if err != nil {
		return ChangeWriteResult{}, err
	}
	out := ChangeWriteResult{ChangeID: c.id, Sections: map[string]ChangeSectionResult{}}
	if b.State != nil {
		out.Sections[SectionState], err = applyChangeStateMySQL(ctx, tx, c, *b.State, now)
	}
	if err == nil && b.Checks != nil {
		out.Sections[SectionChecks], err = applyChangeChecksMySQL(ctx, tx, c, *b.Checks)
	}
	if err == nil && b.Threads != nil {
		out.Sections[SectionThreads], err = applyChangeThreadsMySQL(ctx, tx, c, *b.Threads)
	}
	if err == nil && b.Findings != nil {
		out.Sections[SectionFindings], err = applyChangeFindingsMySQL(ctx, tx, c, *b.Findings, now)
	}
	if err == nil && b.Gate != nil {
		out.Sections[SectionGate], err = applyChangeGateMySQL(ctx, tx, c, *b.Gate)
	}
	if err != nil {
		return ChangeWriteResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ChangeWriteResult{}, err
	}
	return out, nil
}
