package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Document retention: the merge link (hk 6280, parent hk 6278, operator
// decision 493 A).
//
// The hk 5057 review/mwr-* leg links a document to its change through
// reviews.reviewer_job_id, which no production review row carries, so it
// matches nothing. The merge link reads the change from the mergewake job id
// instead. A review job (mwr-<pr>-<sha7>-p<n>-<unix>, or the compact
// mwr-<pr>-<sha7>p<n><suffix> current mergewake writes) files
// review/mwr-<job>, briefs/mwr-<job>, screens/mwr-<job>/... and
// reports/mwr-<job>, all with documents.job = <job>. A document under one of
// the three families in docRetentionMergeLinkFamilies is linked only on
// positive evidence:
//
//  1. documents.job is J and parses as mwr-<N digits>-<S 7 hex> followed by
//     '-' or 'p<digit>' (S is exactly the first 7 characters after N);
//  2. the documents with job = J under the four mwr- families (the three
//     below plus reports/mwr-, read only as evidence), together with the
//     doc_prune_log rows of deleted documents with job = J under the same
//     four families, carry exactly one distinct non-NULL task_id T (none:
//     unlinked; two or more: ambiguous). The log rows (hk 6289) keep the
//     evidence of a document another leg deleted first (the hk 5057 legs or
//     hk 3639 PruneDocuments): a deleted witness still links its siblings,
//     and a deleted document with a second task still makes the job
//     ambiguous. Rows logged before 20261125-doc-prune-log-evidence have NULL
//     job and task_id and are no evidence either way;
//  3. exactly one change C has a change_tasks row for T, pr_number N and a
//     reviews row whose reviewed_head starts with S (none: unlinked; two or
//     more: ambiguous).
//
// It is eligible when C is merged, merged_at is set and at or before the
// cutoff, and no yt_article_bindings row points at the document. Otherwise it
// is bound, fresh (merged after the cutoff) or unmerged (C open or closed).
//
// reports/mwr- (DocRetentionKeptPrefix) is never a family: the operator kept
// the reports, so no merge-link query selects one for deletion.
//
// The leg is an extra OR reason beside the hk 5057 legs, which are unchanged.
// A dry run always reports its per-family classification (merge_link); an
// apply run deletes by it only when the switch is on (mergeLinkApplyOn).
//
// Apply (switch on) picks whole jobs, so one job's eligible documents are
// never split across batches: before the batch transaction it lists the
// eligible ids of the first jobs (mergeLinkPickSQL); inside it, after the hk
// 5057 legs, it locks those rows by id and only then re-runs the whole
// classification restricted to them (mergeLinkRecheckSQL), keeping only the
// rows still eligible. It then admits a job's rows only when all of them fit
// the room left (admitMergeJobs, hk 6290): the batch size less the ids the hk
// 5057 legs chose, where the batch size is already DocRetentionBatch capped
// at the run's limit less its deletions. A job that does not fit is never
// split; it waits for a later batch or run, and the result counts it in
// merge_link_skipped_jobs. When no remaining job fits (a limit below the
// smallest eligible job) the merge leg deletes nothing and the run ends, so
// a run never loops on it. The pick puts jobs that fit a whole batch first,
// so a job larger than any batch never holds back the ones that fit. The recheck is a new statement after the locks are held
// (a fresh snapshot on PostgreSQL READ COMMITTED; on MySQL the transaction's
// first consistent read, since every earlier statement in it is a locking
// read), so a binding, an upsert or a second task committed before the lock is
// seen. A write committed after the recheck is the same as one committed
// after the delete.
//
// Cost: documents.job has no index, so the job -> task map is built once per
// statement by a grouped scan of the four key ranges (documents.key is
// uniquely indexed) and of the same ranges of doc_prune_log
// (doc_prune_log_evidence); no statement probes documents by job per row. A dry run
// adds one statement and an apply batch three (pick, lock, recheck).
const (
	// DocRetentionBriefsMwrPrefix holds the brief copy of a review job.
	DocRetentionBriefsMwrPrefix = "briefs/mwr-"
	// DocRetentionScreensMwrPrefix holds a review job's pane screens.
	DocRetentionScreensMwrPrefix = "screens/mwr-"
	// DocRetentionKeptPrefix holds review reports; retention never deletes
	// one. It is read only as task evidence for the merge link.
	DocRetentionKeptPrefix = "reports/mwr-"
	// DocRetentionMergeLinkApply turns the merge leg on for apply runs. It
	// is on since its gate passed (hk 6278, operator decision 493; findings
	// 2026-11-25/hk6278-retention-gate and the post-hk 6289 dry run). Dry runs
	// report the leg either way. Set it to false to stop the leg.
	DocRetentionMergeLinkApply = true
)

// docRetentionMergeLinkFamilies are the key prefixes the merge link may
// delete. reports/mwr- is deliberately absent (DocRetentionKeptPrefix).
var docRetentionMergeLinkFamilies = [3]string{DocRetentionMwrPrefix, DocRetentionBriefsMwrPrefix, DocRetentionScreensMwrPrefix}

// DocRetentionFamily classifies every document under one merge-link family.
// EligibleRows+Bound+Fresh+Unmerged+Unlinked+Ambiguous = Total. EligibleBytes
// is the body size of the eligible rows. Eligible counts the merge link only:
// a row the hk 5057 legs also match is counted here too.
type DocRetentionFamily struct {
	EligibleRows  int64 `json:"eligible_rows"`
	EligibleBytes int64 `json:"eligible_bytes"`
	Bound         int64 `json:"bound"`
	Fresh         int64 `json:"fresh"`
	Unmerged      int64 `json:"unmerged"`
	Unlinked      int64 `json:"unlinked"`
	Ambiguous     int64 `json:"ambiguous"`
	Total         int64 `json:"total"`
}

// mergeLinkApplyOn reports whether an apply run deletes by the merge link:
// DocRetentionMergeLinkApply unless a test set this store's override.
func (s *Store) mergeLinkApplyOn() bool {
	switch s.retentionMergeLinkOverride.Load() {
	case 1:
		return true
	case 2:
		return false
	}
	return DocRetentionMergeLinkApply
}

// setRetentionMergeLinkApply overrides DocRetentionMergeLinkApply for this
// store only, so parallel tests do not share a switch.
func (s *Store) setRetentionMergeLinkApply(on bool) {
	if on {
		s.retentionMergeLinkOverride.Store(1)
		return
	}
	s.retentionMergeLinkOverride.Store(2)
}

// mergeLinkSQL renders one merge-link statement for a dialect. Named
// parameters become $n (reused) on PostgreSQL and one ? per use on MySQL, with
// args in placeholder order.
type mergeLinkSQL struct {
	mysql bool
	vals  map[string]any
	pgPos map[string]int
	args  []any
}

func newMergeLinkSQL(mysql bool, cutoff time.Time) *mergeLinkSQL {
	b := &mergeLinkSQL{mysql: mysql, vals: map[string]any{"cutoff": cutoff.UTC()}, pgPos: map[string]int{}}
	for i, p := range []string{DocRetentionMwrPrefix, DocRetentionBriefsMwrPrefix, DocRetentionScreensMwrPrefix, DocRetentionKeptPrefix} {
		lo, hi := keyPrefixBounds(p)
		b.vals[fmt.Sprintf("lo%d", i)] = lo
		b.vals[fmt.Sprintf("hi%d", i)] = hi
	}
	return b
}

// inIDs is expr restricted to ids: = ANY($n) on PostgreSQL, IN (?,...) on
// MySQL. ids must not be empty.
func (b *mergeLinkSQL) inIDs(expr string, ids []int64) string {
	if b.mysql {
		for _, id := range ids {
			b.args = append(b.args, id)
		}
		return expr + " IN (" + placeholders(len(ids)) + ")"
	}
	b.args = append(b.args, ids)
	return fmt.Sprintf("%s = ANY($%d)", expr, len(b.args))
}

func (b *mergeLinkSQL) p(name string) string {
	v, ok := b.vals[name]
	if !ok {
		panic("mergeLinkSQL: unknown parameter " + name)
	}
	if b.mysql {
		b.args = append(b.args, v)
		return "?"
	}
	if n, ok := b.pgPos[name]; ok {
		return fmt.Sprintf("$%d", n)
	}
	b.args = append(b.args, v)
	b.pgPos[name] = len(b.args)
	return fmt.Sprintf("$%d", len(b.args))
}

// inRange is alias.key within prefix range i.
func (b *mergeLinkSQL) inRange(alias string, i int) string {
	k := alias + ".key COLLATE \"C\""
	if b.mysql {
		k = alias + ".`key`"
	}
	return b.colInRange(k, i)
}

// colInRange is the key expression k within prefix range i.
func (b *mergeLinkSQL) colInRange(k string, i int) string {
	return fmt.Sprintf("(%s >= %s AND %s < %s)", k, b.p(fmt.Sprintf("lo%d", i)), k, b.p(fmt.Sprintf("hi%d", i)))
}

// logInRanges is doc_prune_log row pl within any of the four mwr- ranges.
func (b *mergeLinkSQL) logInRanges() string {
	k := `pl.document_key COLLATE "C"`
	if b.mysql {
		k = "pl.document_key"
	}
	return b.colInRange(k, 0) + " OR " + b.colInRange(k, 1) + " OR " + b.colInRange(k, 2) + " OR " + b.colInRange(k, 3)
}

// cte is the WITH clause ending in ml(id, job, family, class): one row per
// document under the three families with its merge-link class.
func (b *mergeLinkSQL) cte() string {
	jobRE, prNum, sha7 := `x.job ~ '^mwr-[0-9]{1,9}-[0-9a-f]{7}(-|p[0-9])'`, `split_part(jt.job, '-', 2)::bigint`, `left(split_part(jt.job, '-', 3), 7) || '%'`
	if b.mysql {
		jobRE = `x.job REGEXP '^mwr-[0-9]{1,9}-[0-9a-f]{7}(-|p[0-9])'`
		prNum = `CAST(SUBSTRING_INDEX(SUBSTRING_INDEX(jt.job, '-', 2), '-', -1) AS UNSIGNED)`
		sha7 = `CONCAT(LEFT(SUBSTRING_INDEX(SUBSTRING_INDEX(jt.job, '-', 3), '-', -1), 7), '%')`
	}
	var sb strings.Builder
	// jt counts a job's tasks over its live documents and, as evidence that
	// survives a delete (hk 6289), the doc_prune_log rows another leg wrote
	// for the same job in the same four ranges. A log row without a task
	// (every row logged before 20261125-doc-prune-log-evidence) adds nothing.
	sb.WriteString("WITH jt AS (\n  SELECT x.job, COUNT(DISTINCT x.task_id) AS ntask, MIN(x.task_id) AS task_id\n  FROM (\n    SELECT xd.job, xd.task_id FROM documents xd\n    WHERE (")
	sb.WriteString(b.inRange("xd", 0) + " OR " + b.inRange("xd", 1) + " OR " + b.inRange("xd", 2) + " OR " + b.inRange("xd", 3))
	sb.WriteString(")\n    UNION ALL\n    SELECT pl.job, pl.task_id FROM doc_prune_log pl\n    WHERE pl.task_id IS NOT NULL AND (" + b.logInRanges() + ")\n  ) x\n")
	sb.WriteString("  WHERE " + jobRE + "\n  GROUP BY x.job\n")
	sb.WriteString("), jc AS (\n  SELECT jt.job, jt.ntask, COUNT(DISTINCT c.id) AS nchange, MIN(c.id) AS change_id\n  FROM jt\n")
	sb.WriteString("  LEFT JOIN change_tasks ct ON jt.ntask = 1 AND ct.task_id = jt.task_id\n")
	sb.WriteString("  LEFT JOIN changes c ON c.id = ct.change_id AND c.pr_number = " + prNum + "\n")
	sb.WriteString("    AND EXISTS (SELECT 1 FROM reviews r WHERE r.change_id = c.id AND r.reviewed_head LIKE " + sha7 + ")\n")
	sb.WriteString("  GROUP BY jt.job, jt.ntask\n")
	sb.WriteString("), ml AS (\n  SELECT d.id, d.job,\n")
	sb.WriteString("    CASE WHEN " + b.inRange("d", 0) + " THEN 'review/mwr-' WHEN " + b.inRange("d", 1) + " THEN 'briefs/mwr-' ELSE 'screens/mwr-' END AS family,\n")
	sb.WriteString(`    CASE WHEN jc.job IS NULL OR jc.ntask = 0 THEN 'unlinked'
      WHEN jc.ntask > 1 THEN 'ambiguous'
      WHEN jc.nchange = 0 THEN 'unlinked'
      WHEN jc.nchange > 1 THEN 'ambiguous'
      WHEN c.state <> 'merged' OR c.merged_at IS NULL THEN 'unmerged'
      WHEN c.merged_at > ` + b.p("cutoff") + ` THEN 'fresh'
      WHEN EXISTS (SELECT 1 FROM yt_article_bindings yb WHERE yb.document_id = d.id) THEN 'bound'
      ELSE 'eligible' END AS class
  FROM documents d
  LEFT JOIN jc ON jc.job = d.job
  LEFT JOIN changes c ON c.id = jc.change_id AND jc.nchange = 1
  WHERE (`)
	sb.WriteString(b.inRange("d", 0) + " OR " + b.inRange("d", 1) + " OR " + b.inRange("d", 2))
	sb.WriteString(")\n)\n")
	return sb.String()
}

// mergeLinkCountsSQL is the dry-run classification: one row per (family,
// class) with its row count and, for the eligible class, its body bytes.
func mergeLinkCountsSQL(mysql bool, cutoff time.Time) (string, []any) {
	b := newMergeLinkSQL(mysql, cutoff)
	q := b.cte()
	size := `octet_length(bd.body)`
	sum := `COALESCE(SUM(CASE WHEN ml.class = 'eligible' THEN (SELECT %s FROM documents bd WHERE bd.id = ml.id) ELSE 0 END), 0)`
	if mysql {
		size = `OCTET_LENGTH(bd.body)`
		sum = `CAST(` + sum + ` AS SIGNED)`
	} else {
		sum += `::bigint`
	}
	q += "SELECT ml.family, ml.class, COUNT(*), " + fmt.Sprintf(sum, size) + " FROM ml GROUP BY ml.family, ml.class"
	return q, b.args
}

// mergeLinkPickSQL lists (job, id) for every merge-link-eligible (so
// unbound) id of the first jobs eligible jobs: whole jobs, so apply never
// splits one across batches. Jobs whose eligible count fits capacity come
// first, each group ordered by its oldest eligible id, so a job too large for
// any batch never holds back the ones that fit.
func mergeLinkPickSQL(mysql bool, cutoff time.Time, jobs, capacity int) (string, []any) {
	b := newMergeLinkSQL(mysql, cutoff)
	b.vals["jobs"] = jobs
	b.vals["cap"] = capacity
	q := b.cte()
	q += "SELECT ml.job, ml.id FROM ml JOIN (SELECT m2.job, CASE WHEN COUNT(*) <= " + b.p("cap") + " THEN 0 ELSE 1 END AS over_cap, MIN(m2.id) AS first_id FROM ml m2 WHERE m2.class = 'eligible' GROUP BY m2.job ORDER BY over_cap, first_id LIMIT " + b.p("jobs") + ") pick ON pick.job = ml.job WHERE ml.class = 'eligible' ORDER BY pick.over_cap, pick.first_id, ml.id"
	return q, b.args
}

// mergeLinkRecheckSQL re-runs the classification for ids (already locked by
// the caller) and returns id, key, sha256, body size, job and task_id of
// those still eligible.
func mergeLinkRecheckSQL(mysql bool, cutoff time.Time, ids []int64) (string, []any) {
	b := newMergeLinkSQL(mysql, cutoff)
	q := b.cte()
	size, key := "octet_length(rd.body)", "rd.key"
	if mysql {
		size, key = "OCTET_LENGTH(rd.body)", "rd.`key`"
	}
	q += "SELECT rd.id, " + key + ", rd.sha256, " + size + ", rd.job, rd.task_id FROM ml JOIN documents rd ON rd.id = ml.id WHERE ml.class = 'eligible' AND " + b.inIDs("ml.id", ids) + " ORDER BY rd.id"
	return q, b.args
}

// mergeLinkCounts runs the dry-run classification for every family.
func (s *Store) mergeLinkCounts(ctx context.Context, cutoff time.Time) (map[string]DocRetentionFamily, error) {
	q, args := mergeLinkCountsSQL(s.dialect == mysqlDialect, cutoff)
	rs, err := s.db().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := make(map[string]DocRetentionFamily, len(docRetentionMergeLinkFamilies))
	for _, f := range docRetentionMergeLinkFamilies {
		out[f] = DocRetentionFamily{}
	}
	for rs.Next() {
		var family, class string
		var n, size int64
		if err = rs.Scan(&family, &class, &n, &size); err != nil {
			return nil, err
		}
		f, ok := out[family]
		if !ok {
			return nil, fmt.Errorf("merge link: unexpected family %q", family)
		}
		switch class {
		case "eligible":
			f.EligibleRows += n
			f.EligibleBytes += size
		case "bound":
			f.Bound += n
		case "fresh":
			f.Fresh += n
		case "unmerged":
			f.Unmerged += n
		case "unlinked":
			f.Unlinked += n
		case "ambiguous":
			f.Ambiguous += n
		default:
			return nil, fmt.Errorf("merge link: unexpected class %q", class)
		}
		f.Total += n
		out[family] = f
	}
	return out, rs.Err()
}

// mergeLinkPicked is the merge leg's pick for one batch: the picked jobs in
// admission order and their eligible ids.
type mergeLinkPicked struct {
	jobs []string
	ids  []int64
}

// mergeLinkPick returns the eligible ids of the first jobs eligible jobs,
// read outside the batch transaction (see the apply note above).
func (s *Store) mergeLinkPick(ctx context.Context, cutoff time.Time, jobs, capacity int) (mergeLinkPicked, error) {
	var out mergeLinkPicked
	q, args := mergeLinkPickSQL(s.dialect == mysqlDialect, cutoff, jobs, capacity)
	rs, err := s.db().Query(ctx, q, args...)
	if err != nil {
		return out, err
	}
	defer rs.Close()
	for rs.Next() {
		var job string
		var id int64
		if err = rs.Scan(&job, &id); err != nil {
			return out, err
		}
		if n := len(out.jobs); n == 0 || out.jobs[n-1] != job {
			out.jobs = append(out.jobs, job)
		}
		out.ids = append(out.ids, id)
	}
	return out, rs.Err()
}

// mergeLinkJobsPerBatch is how many jobs one apply batch picks for the merge
// leg: about a batch of documents at three families per job.
func mergeLinkJobsPerBatch(batch int) int {
	return max(1, batch/len(docRetentionMergeLinkFamilies))
}

// mergeLinkBatchIDs is the batch's merge-leg pick: empty when the leg is
// off, else the eligible ids of whole jobs, after the test seam.
func (s *Store) mergeLinkBatchIDs(ctx context.Context, cutoff time.Time, batch int) (mergeLinkPicked, error) {
	if !s.mergeLinkApplyOn() {
		return mergeLinkPicked{}, nil
	}
	picked, err := s.mergeLinkPick(ctx, cutoff, mergeLinkJobsPerBatch(batch), batch)
	if err != nil {
		return picked, err
	}
	if s.retentionMergeSeam != nil {
		s.retentionMergeSeam()
	}
	return picked, nil
}

// retentionRow is one document a retention batch deletes, with what its
// doc_prune_log row records.
type retentionRow struct {
	id   int64
	key  string
	sha  string
	size int64
	job  string
	task *int64
}

// admitMergeJobs is the merge leg's limit rule (hk 6290): it admits a job's
// rechecked eligible rows only when all of them fit the remaining capacity
// (the batch's DocRetentionBatch less the ids already chosen, which the
// caller has already capped at the run's limit less its deletions). A job
// that does not fit is never split: it is returned in skipped and stays for
// a later batch or run. Jobs are tried in order (the pick's order, then any
// job a row moved to before the recheck), and a smaller later job may still
// fit after a larger one is skipped.
func admitMergeJobs(order []string, rows []retentionRow, capacity int) (admitted []retentionRow, admittedJobs, skipped []string) {
	byJob := make(map[string][]retentionRow, len(order))
	jobs := append([]string(nil), order...)
	seen := make(map[string]bool, len(order))
	for _, j := range order {
		seen[j] = true
	}
	for _, r := range rows {
		if !seen[r.job] {
			seen[r.job] = true
			jobs = append(jobs, r.job)
		}
		byJob[r.job] = append(byJob[r.job], r)
	}
	for _, j := range jobs {
		rs := byJob[j]
		if len(rs) == 0 {
			continue
		}
		if len(rs) > capacity {
			skipped = append(skipped, j)
			continue
		}
		admitted = append(admitted, rs...)
		admittedJobs = append(admittedJobs, j)
		capacity -= len(rs)
	}
	return admitted, admittedJobs, skipped
}

// noteMergeSkips records, after a batch, which jobs the merge leg left whole
// for lack of room and which it deleted, and refreshes MergeLinkSkippedJobs:
// the jobs this run skipped and never deleted.
func (res *DocRetentionResult) noteMergeSkips(admitted, skipped []string) {
	if len(admitted) == 0 && len(skipped) == 0 {
		return
	}
	if res.mergeSkipped == nil {
		res.mergeSkipped = map[string]bool{}
	}
	for _, j := range skipped {
		res.mergeSkipped[j] = true
	}
	for _, j := range admitted {
		delete(res.mergeSkipped, j)
	}
	res.MergeLinkSkippedJobs = int64(len(res.mergeSkipped))
}

// withoutIDs is ids minus skip, in order.
func withoutIDs(ids []int64, skip map[int64]bool) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !skip[id] {
			out = append(out, id)
		}
	}
	return out
}
