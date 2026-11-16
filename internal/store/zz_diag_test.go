package store

// THROWAWAY diagnostic for hk 4922 (deleted before the PR): measures, on a
// production-like seed, what one write-worker pass costs the three hot tables
// with and without the migration's indexes. Runs only in CI
// (HANDOFFKEEP_TEST_DB_URL); it starts no server.

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

type diagStat struct{ scans, tup int64 }

func (d diagStat) String() string { return fmt.Sprintf("scans=%d rows=%d", d.scans, d.tup) }

func diagRead(ctx context.Context, s *Store, kind string) map[string]diagStat {
	out := map[string]diagStat{}
	rows, err := s.pool.Query(ctx, `SELECT c.relname, pg_stat_get_numscans(c.oid), pg_stat_get_tuples_returned(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind = $1`, kind)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var st diagStat
		if err := rows.Scan(&name, &st.scans, &st.tup); err != nil {
			panic(err)
		}
		out[name] = st
	}
	return out
}

func diagFlush(ctx context.Context, s *Store) {
	if _, err := s.pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		panic(err)
	}
}

func diagDiff(before, after map[string]diagStat) string {
	names := make([]string, 0, len(after))
	for n := range after {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		x, y := before[n], after[n]
		if x.scans == y.scans && x.tup == y.tup {
			continue
		}
		fmt.Fprintf(&b, "    %-40s %s\n", n, diagStat{y.scans - x.scans, y.tup - x.tup})
	}
	if b.Len() == 0 {
		return "    (nothing)\n"
	}
	return b.String()
}

// diagRun seeds the production row counts, then measures one write-worker
// pass statement by statement, then explains the hot statements.
func diagRun(t *testing.T, prefix string, withIndexes bool) {
	t.Helper()
	ctx := t.Context()
	_, dbURL := testdb.Schema(t, prefix)
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("options", "-c statement_timeout=60s")
	u.RawQuery = q.Encode()
	s, err := Open(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	now := time.Now().UTC()

	if !withIndexes {
		for _, q := range []string{
			`DROP INDEX IF EXISTS yt_article_bindings_document_text`,
			`DROP INDEX IF EXISTS yt_write_ops_comment_requeue`,
		} {
			if _, err := s.pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}

	ytPlanSeed(t, s)
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_write_ops (principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
		yt_target_id, actor, state, attempts, created_at, next_attempt_at)
		SELECT 'hk:task-comments','comment_append','held:'||g,repeat('a',64),'{}'::jsonb,'task',(3000+g)::text,'','operator',
		'prepared',0, now()-interval '2 hours', now()+interval '1 hour'
		FROM generate_series(1,200) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}

	s.SetYTCommentsMode(YTCommentsOn)
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	s.SetYTMovesMode(YTMovesOn)

	step := func(name string, fn func() error) {
		t.Helper()
		diagFlush(ctx, s)
		bTab, bIdx := diagRead(ctx, s, "r"), diagRead(ctx, s, "i")
		started := time.Now()
		err := fn()
		elapsed := time.Since(started)
		diagFlush(ctx, s)
		aTab, aIdx := diagRead(ctx, s, "r"), diagRead(ctx, s, "i")
		t.Logf("== [%s] %s (err=%v, took %s)\n  tables:\n%s  indexes:\n%s", prefix, name, err, elapsed, diagDiff(bTab, aTab), diagDiff(bIdx, aIdx))
	}

	step("Due", func() error { _, err := s.YTWriteLedger().Due(ctx, now, 200); return err })
	step("CatchUpYTComments", func() error { _, err := s.CatchUpYTComments(ctx, 200, nil); return err })
	step("RequeueYTMovedComments", func() error { _, err := s.RequeueYTMovedComments(ctx, ytcTenant, 200); return err })
	step("RunYTFeedConsumers", func() error { _, err := s.RunYTFeedConsumers(ctx, 200); return err })
	step("RecoverHeldYTOps", func() error { _, err := s.RecoverHeldYTOps(ctx, ytcTenant, now, YTHeldRecoveryGrace); return err })
	step("PrepareMappedYTIssues", func() error { _, err := s.PrepareMappedYTIssues(ctx, 200); return err })
	step("YTWriteOutboxStatus", func() error { _, err := s.YTWriteOutboxStatus(ctx, now); return err })

	explain := func(label, sql string, args ...any) {
		t.Helper()
		rows, err := s.pool.Query(ctx, "EXPLAIN (COSTS OFF, SUMMARY OFF, TIMING OFF) "+sql, args...)
		if err != nil {
			t.Logf("== [%s] EXPLAIN %s: %v", prefix, label, err)
			return
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			lines = append(lines, l)
		}
		t.Logf("== [%s] EXPLAIN %s\n%s", prefix, label, strings.Join(lines, "\n"))
	}

	var articleOp int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE kind='article_create'`).Scan(&articleOp); err != nil {
		t.Fatal(err)
	}
	explain("Claim admission (article_create)",
		`SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND `+s.ytAdmitSQL("$2")+`)`, articleOp, ytcTenant)
	explain("requeue select", ytRequeueSelectSQL,
		YTCommentPrincipal, ytcTenant, YTIssuePrincipal, "issue_create", 200)
	explain("catch-up select", ytCatchUpSelectSQL,
		YouTrackOwnedActor, YTCommentPrincipal, "comment_append", "comment:", []int64{}, 200, false)
	explain("held candidates", `SELECT `+ytHeldOpColumns+` FROM yt_write_ops o
		WHERE o.state='prepared' AND o.tenant=$6 AND o.created_at <= $1 AND (o.created_at, o.id) > ($2, $3)
		AND o.kind = ANY($4) AND ($7 = '' OR o.entity_id = $7) AND `+ytHeldScopeSQL+` AND NOT `+s.ytAdmitSQL("$6")+` AND `+ytHeldKeptSQL+`
		ORDER BY o.created_at, o.id LIMIT $5`, now.Add(-YTHeldRecoveryGrace), time.Unix(0, 0).UTC(), int64(0),
		[]string{"comment_append", "issue_fields", "issue_move", "issue_links"}, 200, ytcTenant, "")
	explain("article binding update (pkey)",
		`UPDATE yt_article_bindings SET content_sha=$4, needs_update=false
		 WHERE document_id=$1 AND tenant=$2 AND article_id=$3`, int64(2), ytcTenant, "art-2", strings.Repeat("a", 64))
}

func TestYTDiagBeforeIndexes(t *testing.T) { diagRun(t, "ytbefore", false) }
func TestYTDiagAfterIndexes(t *testing.T)  { diagRun(t, "ytafter", true) }
