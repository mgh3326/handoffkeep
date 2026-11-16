package store

// THROWAWAY diagnostic for hk 4922 (deleted before the PR): measures, on a
// production-like seed, what one write-worker pass costs each of the three
// hot tables. Runs only in CI (HANDOFFKEEP_TEST_DB_URL); no local server.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

type diagStat struct{ scans, tup int64 }

func (d diagStat) String() string {
	return fmt.Sprintf("scans=%d rows=%d", d.scans, d.tup)
}

// diagRead reads scan counters of every relation of kind in the current
// schema, after the backend's pending stats have been flushed.
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

func TestYTDiagWritePathScans(t *testing.T) {
	ctx := t.Context()
	s := schemaStore(t, "ytdiag")
	now := time.Now().UTC()

	// Production-like rows (pg_stat_user_tables on ncp, 12 days to
	// 2026-11-16): yt_article_bindings 15,657, yt_comment_write_links
	// 7,416, yt_write_ops 28,055.
	seed := []string{
		`INSERT INTO tasks (id, lane, title, kind, state, created_by, created_at, updated_at, project)
		 SELECT g, 'lane-'||g, 'task '||g, 'fix', 'backlog', 'test', now()-(g||' minutes')::interval, now(), 'proj'||(g%5)
		 FROM generate_series(1,2000) g`,
		`SELECT setval(pg_get_serial_sequence('tasks','id'), 2000)`,
		`INSERT INTO task_comments (id, task_id, body, author, created_at)
		 SELECT g, 1+(g%2000), 'comment '||g, 'operator', now()-(g||' seconds')::interval
		 FROM generate_series(1,10000) g`,
		`SELECT setval(pg_get_serial_sequence('task_comments','id'), 10000)`,
		`INSERT INTO yt_task_bindings (task_id, tenant, issue_id, project_id, bound_by, bound_at, origin, comments_from)
		 SELECT g, '` + ytcTenant + `', 'PROJ-'||g, 'PROJ', 'test', now(), 'import_verified', 0
		 FROM generate_series(1,2000) g`,
		`INSERT INTO yt_article_bindings (document_id, tenant, article_id, project_id, origin, bound_by, bound_at)
		 SELECT g, '` + ytcTenant + `', 'art-'||g, 'PROJ', 'import_verified', 'test', now()
		 FROM generate_series(1,15657) g`,
		// 28,055 ops: even ids are the comment principal's, odd the issue
		// principal's; all applied.
		`INSERT INTO yt_write_ops (principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
			yt_target_id, actor, state, yt_result_id, reason, attempts, created_at, next_attempt_at, resolved_at, dispatched_at)
		 SELECT CASE WHEN g%2=0 THEN 'hk:task-comments' ELSE 'hk:task-issues' END,
			CASE WHEN g%2=0 THEN 'comment_append' ELSE 'issue_create' END,
			CASE WHEN g%2=0 THEN 'comment:'||g ELSE 'issue:'||g END,
			repeat('a',64), '{}'::jsonb, 'task', (1+(g%2000))::text, '', 'operator',
			'applied', 'PROJ-'||g, '', 1,
			now()-(g||' seconds')::interval, now(), now(), now()
		 FROM generate_series(1,28055) g`,
		// Three article ops due now (the gate's per-pass evaluation).
		`INSERT INTO yt_write_ops (principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
			yt_target_id, actor, state, attempts, created_at, next_attempt_at)
		 VALUES ('hk:documents','article_create','document-article:1',repeat('a',64),'{}'::jsonb,'document','1','','operator','prepared',0, now()-interval '2 hours', now()-interval '1 minute'),
			('hk:documents','article_update','document-article:update:1',repeat('a',64),'{}'::jsonb,'document','2','art-2','operator','prepared',0, now()-interval '2 hours', now()-interval '1 minute'),
			('hk:documents','article_update','document-article:update:2',repeat('a',64),'{}'::jsonb,'document','3','art-3','operator','prepared',0, now()-interval '2 hours', now()-interval '1 minute')`,
		// 200 prepared comment ops on unbound tasks (held-op candidates).
		`INSERT INTO yt_write_ops (principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
			yt_target_id, actor, state, attempts, created_at, next_attempt_at)
		 SELECT 'hk:task-comments','comment_append','held:'||g,repeat('a',64),'{}'::jsonb,'task',(3000+g)::text,'','operator',
			'prepared',0, now()-interval '2 hours', now()+interval '1 hour'
		 FROM generate_series(1,200) g`,
		`INSERT INTO yt_comment_write_links (comment_id, op_id, created_at)
		 SELECT i, 2*i, now() FROM generate_series(1,7416) i`,
	}
	for _, q := range seed {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
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
		err := fn()
		diagFlush(ctx, s)
		aTab, aIdx := diagRead(ctx, s, "r"), diagRead(ctx, s, "i")
		t.Logf("== %s (err=%v)\n  tables:\n%s  indexes:\n%s", name, err, diagDiff(bTab, aTab), diagDiff(bIdx, aIdx))
	}

	step("Due", func() error { _, err := s.YTWriteLedger().Due(ctx, now, 200); return err })
	step("CatchUpYTComments", func() error { _, err := s.CatchUpYTComments(ctx, 200, nil); return err })
	step("RequeueYTMovedComments", func() error { _, err := s.RequeueYTMovedComments(ctx, ytcTenant, 200); return err })
	step("RunYTFeedConsumers", func() error { _, err := s.RunYTFeedConsumers(ctx, 200); return err })
	step("RecoverHeldYTOps", func() error { _, err := s.RecoverHeldYTOps(ctx, ytcTenant, now, YTHeldRecoveryGrace); return err })
	step("PrepareMappedYTIssues", func() error { _, err := s.PrepareMappedYTIssues(ctx, 200); return err })
	step("YTWriteOutboxStatus", func() error { _, err := s.YTWriteOutboxStatus(ctx, now); return err })

	explain := func(label, analyze, sql string, args ...any) {
		t.Helper()
		rows, err := s.pool.Query(ctx, "EXPLAIN "+analyze+" (COSTS OFF, SUMMARY OFF, TIMING OFF) "+sql, args...)
		if err != nil {
			t.Logf("== EXPLAIN %s: %v", label, err)
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
		t.Logf("== EXPLAIN %s\n%s", label, strings.Join(lines, "\n"))
	}

	var articleOp int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE kind='article_create'`).Scan(&articleOp); err != nil {
		t.Fatal(err)
	}
	explain("Claim admission (article_create)", "ANALYZE", `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND `+s.ytAdmitSQL("$2")+`)`, articleOp, ytcTenant)
	explain("requeue select", "ANALYZE", ytRequeueSelectSQL, YTCommentPrincipal, ytcTenant, "ambiguous_issue", YTIssuePrincipal, "issue_create", 200)
	explain("catch-up select", "ANALYZE", ytCatchUpSelectSQL, YouTrackOwnedActor, YTCommentPrincipal, "comment_append", "comment:", []int64{}, 200, false)
	explain("held candidates", "ANALYZE", `SELECT `+ytHeldOpColumns+` FROM yt_write_ops o
		WHERE o.state='prepared' AND o.tenant=$6 AND o.created_at <= $1 AND (o.created_at, o.id) > ($2, $3)
		AND o.kind = ANY($4) AND ($7 = '' OR o.entity_id = $7) AND `+ytHeldScopeSQL+` AND NOT `+s.ytAdmitSQL("$6")+` AND `+ytHeldKeptSQL+`
		ORDER BY o.created_at, o.id LIMIT $5`, now.Add(-YTHeldRecoveryGrace), time.Unix(0, 0).UTC(), int64(0),
		[]string{"comment_append", "issue_fields", "issue_move", "issue_links"}, 200, ytcTenant, "")
	explain("article binding update", "", `UPDATE yt_article_bindings SET content_sha=$4, needs_update=false
		WHERE document_id::text=$1 AND tenant=$2 AND article_id=$3`, "2", ytcTenant, "art-2", strings.Repeat("a", 64))
}
