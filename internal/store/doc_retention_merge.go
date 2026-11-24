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
// instead. A review job (mwr-<pr>-<sha7>-p<n>-<unix>, jobs.go) files
// review/mwr-<job>, briefs/mwr-<job>, screens/mwr-<job>/... and
// reports/mwr-<job>, all with documents.job = <job>. A document under one of
// the three families in docRetentionMergeLinkFamilies is linked only on
// positive evidence:
//
//  1. documents.job is J and parses as mwr-<N digits>-<S 7 hex>-...;
//  2. the documents with job = J under the four mwr- families (the three
//     below plus reports/mwr-, read only as evidence) carry exactly one
//     distinct non-NULL task_id T (none: unlinked; two or more: ambiguous);
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
// Cost: documents.job has no index, so the job -> task map is built once per
// statement by a grouped scan of the four key ranges (documents.key is
// uniquely indexed); no statement probes documents by job per row. A dry run
// adds one statement and an apply batch two.
const (
	// DocRetentionBriefsMwrPrefix holds the brief copy of a review job.
	DocRetentionBriefsMwrPrefix = "briefs/mwr-"
	// DocRetentionScreensMwrPrefix holds a review job's pane screens.
	DocRetentionScreensMwrPrefix = "screens/mwr-"
	// DocRetentionKeptPrefix holds review reports; retention never deletes
	// one. It is read only as task evidence for the merge link.
	DocRetentionKeptPrefix = "reports/mwr-"
	// DocRetentionMergeLinkApply turns the merge leg on for apply runs. It
	// is off: a later PR turns it on after its gate (hk 6278). Dry runs
	// report the leg either way.
	DocRetentionMergeLinkApply = false
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
	return fmt.Sprintf("(%s >= %s AND %s < %s)", k, b.p(fmt.Sprintf("lo%d", i)), k, b.p(fmt.Sprintf("hi%d", i)))
}

// cte is the WITH clause ending in ml(id, family, class): one row per
// document under the three families with its merge-link class.
func (b *mergeLinkSQL) cte() string {
	jobRE, prNum, sha7 := `x.job ~ '^mwr-[0-9]{1,9}-[0-9a-f]{7}-'`, `split_part(jt.job, '-', 2)::bigint`, `split_part(jt.job, '-', 3) || '%'`
	if b.mysql {
		jobRE = `x.job REGEXP '^mwr-[0-9]{1,9}-[0-9a-f]{7}-'`
		prNum = `CAST(SUBSTRING_INDEX(SUBSTRING_INDEX(jt.job, '-', 2), '-', -1) AS UNSIGNED)`
		sha7 = `CONCAT(SUBSTRING_INDEX(SUBSTRING_INDEX(jt.job, '-', 3), '-', -1), '%')`
	}
	var sb strings.Builder
	sb.WriteString("WITH jt AS (\n  SELECT x.job, COUNT(DISTINCT x.task_id) AS ntask, MIN(x.task_id) AS task_id\n  FROM documents x\n  WHERE (")
	sb.WriteString(b.inRange("x", 0) + " OR " + b.inRange("x", 1) + " OR " + b.inRange("x", 2) + " OR " + b.inRange("x", 3))
	sb.WriteString(")\n  AND " + jobRE + "\n  GROUP BY x.job\n")
	sb.WriteString("), jc AS (\n  SELECT jt.job, jt.ntask, COUNT(DISTINCT c.id) AS nchange, MIN(c.id) AS change_id\n  FROM jt\n")
	sb.WriteString("  LEFT JOIN change_tasks ct ON jt.ntask = 1 AND ct.task_id = jt.task_id\n")
	sb.WriteString("  LEFT JOIN changes c ON c.id = ct.change_id AND c.pr_number = " + prNum + "\n")
	sb.WriteString("    AND EXISTS (SELECT 1 FROM reviews r WHERE r.change_id = c.id AND r.reviewed_head LIKE " + sha7 + ")\n")
	sb.WriteString("  GROUP BY jt.job, jt.ntask\n")
	sb.WriteString("), ml AS (\n  SELECT d.id,\n")
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

// mergeLinkCandidatesSQL lists the ids of merge-link-eligible (so unbound)
// documents, oldest id first, at most limit.
func mergeLinkCandidatesSQL(mysql bool, cutoff time.Time, limit int) (string, []any) {
	b := newMergeLinkSQL(mysql, cutoff)
	b.vals["limit"] = limit
	q := b.cte()
	q += "SELECT ml.id FROM ml WHERE ml.class = 'eligible' ORDER BY ml.id LIMIT " + b.p("limit")
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

// mergeLinkCandidateIDs returns up to want merge-link-eligible ids not in
// skip (rows an hk 5057 leg already chose for this batch), oldest first.
func mergeLinkCandidateIDs(ctx context.Context, query func(context.Context, string, ...any) (rows, error), mysql bool, cutoff time.Time, want int, skip map[int64]bool) ([]int64, error) {
	if want <= 0 {
		return nil, nil
	}
	q, args := mergeLinkCandidatesSQL(mysql, cutoff, want+len(skip))
	rs, err := query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var ids []int64
	for rs.Next() {
		var id int64
		if err = rs.Scan(&id); err != nil {
			return nil, err
		}
		if !skip[id] && len(ids) < want {
			ids = append(ids, id)
		}
	}
	return ids, rs.Err()
}
