package store

import (
	"context"
	"errors"
	"slices"
	"time"
)

func (s *Store) getChangeStateMySQL(ctx context.Context, ref ChangeRef, opt ChangeReadOptions) (ChangeView, bool, error) {
	if err := checkChangeRef(ref); err != nil {
		return ChangeView{}, false, err
	}
	tx, err := s.beginSnapshotA5(ctx)
	if err != nil {
		return ChangeView{}, false, err
	}
	defer tx.Rollback(ctx)
	var v ChangeView
	c := &v.Change
	var checksAt, threadsAt, findingsAt, gateAt *time.Time
	var checksSrc, threadsSrc, findingsSrc, gateSrc *string
	var gateState, gateHead, gatePolicy *string
	var gateRaw []byte
	err = tx.QueryRow(ctx, `SELECT id,repo,pr_number,state,draft,title,author,base_ref,head_sha,base_sha,merge_sha,opened_at,merged_at,state_observed_at,state_source,updated_at,
	checks_observed_at,checks_source,threads_observed_at,threads_source,findings_observed_at,findings_source,gate_decided_at,gate_source,gate_state,gate_head,gate_reasons,gate_policy_revision
	FROM changes WHERE repo=? AND pr_number=?`, ref.Repo, ref.PR).Scan(&c.ID, &c.Repo, &c.PRNumber, &c.State, &c.Draft, &c.Title, &c.Author, &c.BaseRef, &c.HeadSHA, &c.BaseSHA, &c.MergeSHA,
		&c.OpenedAt, &c.MergedAt, &c.ObservedAt, &c.Source, &c.UpdatedAt, &checksAt, &checksSrc, &threadsAt, &threadsSrc, &findingsAt, &findingsSrc, &gateAt, &gateSrc,
		&gateState, &gateHead, &gateRaw, &gatePolicy)
	if errors.Is(err, ErrNoRows) {
		return ChangeView{}, false, nil
	}
	if err != nil {
		return ChangeView{}, false, err
	}
	c.PR = changeURL(c.Repo, c.PRNumber)
	v.Freshness = ChangeFreshness{State: freshness(c.ObservedAt, c.Source), Checks: freshness(checksAt, checksSrc), Threads: freshness(threadsAt, threadsSrc), Findings: freshness(findingsAt, findingsSrc), Gate: freshness(gateAt, gateSrc)}
	head := ""
	if c.HeadSHA != nil {
		head = *c.HeadSHA
	}
	if opt.Include.Gate && gateState != nil && gateHead != nil && gateAt != nil {
		g := ChangeGate{State: *gateState, Head: *gateHead, Current: *gateHead == head, Reasons: []string{}, PolicyRevision: gatePolicy, DecidedAt: *gateAt, Source: gateSrc}
		if _, err = scanJSONB(gateRaw, &g.Reasons); err != nil {
			return ChangeView{}, false, err
		}
		v.Gate = &g
	}
	if opt.Include.Checks {
		rows, e := tx.Query(ctx, `SELECT head_sha,name,state,conclusion,url,required,started_at,completed_at,observed_at,source,delivery_id FROM change_checks WHERE change_id=? AND NOT gone AND (? OR head_sha=?) ORDER BY (head_sha=?) DESC,head_sha,name`, c.ID, opt.AllHeads, head, head)
		if e != nil {
			return ChangeView{}, false, e
		}
		checks, e := collectRows(rows, func(r row) (ChangeCheck, error) {
			var k ChangeCheck
			err := r.Scan(&k.HeadSHA, &k.Name, &k.State, &k.Conclusion, &k.URL, &k.Required, &k.StartedAt, &k.CompletedAt, &k.ObservedAt, &k.Source, &k.DeliveryID)
			return k, err
		})
		if e != nil {
			return ChangeView{}, false, e
		}
		v.Checks = &checks
	}
	if opt.Include.Reviews {
		reviews, e := readReviewsMySQL(ctx, tx, c.ID)
		if e != nil {
			return ChangeView{}, false, e
		}
		v.Reviews = &reviews
	}
	if opt.Include.Threads {
		rows, e := tx.Query(ctx, `SELECT thread_id,url,path,line,author,author_kind,severity,resolved,outdated,replied,comment_count,last_comment_at,excerpt,redacted,observed_at,source FROM change_threads WHERE change_id=? AND NOT gone ORDER BY resolved,last_comment_at IS NULL,last_comment_at DESC,thread_id`, c.ID)
		if e != nil {
			return ChangeView{}, false, e
		}
		threads, e := collectRows(rows, func(r row) (ChangeThread, error) {
			var th ChangeThread
			err := r.Scan(&th.ThreadID, &th.URL, &th.Path, &th.Line, &th.Author, &th.AuthorKind, &th.Severity, &th.Resolved, &th.Outdated, &th.Replied, &th.CommentCount, &th.LastCommentAt, &th.Excerpt, &th.Redacted, &th.ObservedAt, &th.Source)
			return th, err
		})
		if e != nil {
			return ChangeView{}, false, e
		}
		v.Threads = &threads
	}
	if opt.Include.Findings {
		rows, e := tx.Query(ctx, `SELECT f.ext_id,f.location,f.severity,f.blocking,f.summary,f.url,f.author_replied OR COALESCE(t.replied,FALSE),
			CASE WHEN t.thread_id IS NOT NULL THEN IF(t.resolved OR t.gone,'resolved','open') WHEN f.reviewed_head=c.head_sha THEN 'open' ELSE 'superseded' END,
			COALESCE(f.thread_id,t.thread_id),f.body,f.redacted,f.reviewed_head,f.round,f.pass,f.lens,f.ordinal,f.review_id,f.observed_at,f.source
			FROM change_findings f JOIN changes c ON c.id=f.change_id LEFT JOIN change_threads t ON t.change_id=f.change_id AND t.thread_id=COALESCE(f.thread_id,f.ext_id)
			WHERE f.change_id=? ORDER BY f.review_id DESC,f.ordinal`, c.ID)
		if e != nil {
			return ChangeView{}, false, e
		}
		findings, e := collectRows(rows, func(r row) (ChangeFinding, error) {
			var f ChangeFinding
			err := r.Scan(&f.ID, &f.Location, &f.Severity, &f.Blocking, &f.Summary, &f.URL, &f.AuthorReplied, &f.Status, &f.ThreadID, &f.Body, &f.Redacted, &f.ReviewedHead, &f.Round, &f.Pass, &f.Lens, &f.Ordinal, &f.ReviewID, &f.ObservedAt, &f.Source)
			return f, err
		})
		if e != nil {
			return ChangeView{}, false, e
		}
		slices.SortStableFunc(findings, func(a, b ChangeFinding) int { return boolRank(b.Status == "open") - boolRank(a.Status == "open") })
		v.Findings = &findings
	}
	if err = tx.Commit(ctx); err != nil {
		return ChangeView{}, false, err
	}
	return v, true, nil
}

func readReviewsMySQL(ctx context.Context, q querier, changeID int64) ([]ChangeReview, error) {
	rows, err := q.Query(ctx, `SELECT id,reviewed_head,reviewed_base,patch_id,round,pass,lens,reviewer_job_id,family,profile,model,harness,effort,executor,fallback,policy_revision,contract_source,verdict,critical,high,medium,low,started_at,finished_at,recorded_at,reviewer,same_family,delta,quiet,outside_diff_blocking,patch_digest FROM reviews WHERE change_id=? ORDER BY id`, changeID)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (ChangeReview, error) {
		var rv ChangeReview
		err := r.Scan(&rv.ID, &rv.ReviewedHead, &rv.ReviewedBase, &rv.PatchID, &rv.Round, &rv.Pass, &rv.Lens, &rv.ReviewerJobID, &rv.Family, &rv.Profile, &rv.Model, &rv.Harness, &rv.Effort, &rv.Executor, &rv.Fallback, &rv.PolicyRevision, &rv.ContractSource, &rv.Verdict, &rv.Critical, &rv.High, &rv.Medium, &rv.Low, &rv.StartedAt, &rv.FinishedAt, &rv.RecordedAt, &rv.Reviewer, &rv.SameFamily, &rv.Delta, &rv.Quiet, &rv.OutsideDiffBlocking, &rv.PatchDigest)
		return rv, err
	})
}
