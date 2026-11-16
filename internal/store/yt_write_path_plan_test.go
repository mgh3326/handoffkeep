package store

import (
	"strings"
	"testing"
)

// TestYTWritePathPlanUsesIndexes is the regression test of hk 4922: on the
// row counts production held then (yt_article_bindings 15,657,
// yt_comment_write_links 7,416, yt_write_ops 28,055), the statements the
// YouTrack write worker runs every five seconds must read those tables
// through an index. Without one a single pass scanned a whole table:
// 959,016 sequential scans of yt_article_bindings read 13.85 billion rows in
// twelve days, and the requeue's candidate scan read its whole comment family
// of yt_write_ops (11.68 billion index entries) and joined
// yt_comment_write_links under it (1,847,809 whole scans).
//
// Each case names the index its statement has to use, or what its plan must
// not contain, so a predicate edited away from the index fails here rather
// than in production.
func TestYTWritePathPlanUsesIndexes(t *testing.T) {
	ctx := t.Context()
	s := schemaStore(t, "ytplan")
	ytPlanSeed(t, s)
	ytPlanMeasure(t, s)
	// The article gates are read with documents mode on (off, they are FALSE
	// and no plan would name the table).
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())

	var create, update int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE kind='article_create'`).Scan(&create); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE kind='article_update' ORDER BY id LIMIT 1`).Scan(&update); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		sql    string
		args   []any
		want   string
		forbid []string
	}{
		{
			// Claim's admission of an article_create op: the create gate.
			name:   "article_create admission",
			sql:    `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND ` + s.ytAdmitSQL("$2") + `)`,
			args:   []any{create, ytcTenant},
			want:   "yt_article_bindings_document_text",
			forbid: []string{"Seq Scan on yt_article_bindings"},
		},
		{
			// The same statement for an article_update op. The update gate's
			// tenant and article id are the unique key, so it may take that
			// index; what matters is that no gate scans the table whole.
			name:   "article_update admission",
			sql:    `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND ` + s.ytAdmitSQL("$2") + `)`,
			args:   []any{update, ytcTenant},
			forbid: []string{"Seq Scan on yt_article_bindings"},
		},
		{
			// The moved-comment requeue's candidate scan: the partial index,
			// and none of the whole-family read of the op log or the whole
			// read of the comment links it fed.
			name: "comment requeue candidates",
			sql:  ytRequeueSelectSQL,
			args: []any{YTCommentPrincipal, ytcTenant, YTIssuePrincipal, "issue_create", 200},
			want: "yt_write_ops_comment_requeue",
			forbid: []string{
				"Seq Scan on yt_comment_write_links",
				"Seq Scan on yt_write_ops",
				"yt_write_ops_principal_kind_idem_key_key",
			},
		},
	} {
		plan := ytPlanExplain(t, s, tc.sql, tc.args...)
		t.Logf("%s:\n%s", tc.name, plan)
		if tc.want != "" && !strings.Contains(plan, tc.want) {
			t.Errorf("%s does not use %s:\n%s", tc.name, tc.want, plan)
		}
		for _, f := range tc.forbid {
			if strings.Contains(plan, f) {
				t.Errorf("%s uses %s:\n%s", tc.name, f, plan)
			}
		}
	}

	// The article_update apply reads the binding by its primary key.
	plan := ytPlanExplain(t, s, `UPDATE yt_article_bindings SET content_sha=$4, needs_update=false
		WHERE document_id=$1 AND tenant=$2 AND article_id=$3`, int64(2), ytcTenant, "art-2", strings.Repeat("a", 64))
	t.Logf("article_update apply:\n%s", plan)
	if strings.Contains(plan, "Seq Scan on yt_article_bindings") {
		t.Errorf("the article_update apply scans yt_article_bindings whole:\n%s", plan)
	}
}

// ytPlanExplain returns the plan of sql with args as text. It plans only: the
// statement under test is the worker's, and what the test checks is the plan.
func ytPlanExplain(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), "EXPLAIN (COSTS OFF, SUMMARY OFF, TIMING OFF) "+sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// ytPlanMeasure collects statistics for the seed without rewriting any row,
// so the planner decides from production-like sizes (a plan test on an
// unanalyzed table proves nothing).
func ytPlanMeasure(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `ANALYZE`); err != nil {
		t.Fatal(err)
	}
}

// ytPlanSeed writes the rows production held when hk 4922 was written
// (pg_stat_user_tables on 2026-11-16, twelve days after the instance start):
// 15,657 article bindings, 7,416 comment write links and 28,055 write ops,
// the three article ops a pass finds prepared, and the 7,416 comment ops the
// links own. The supporting rows are production-shaped: 2,000 bound tasks
// with 10,000 comments, five per task, so the requeue and the catch-up join
// real rows instead of an empty database.
func ytPlanSeed(t *testing.T, s *Store) {
	t.Helper()
	seed := []string{
		`INSERT INTO tasks (id, lane, title, kind, state, created_by, created_at, updated_at, project)
		 SELECT g, 'lane-'||g, 'task '||g, 'fix', 'backlog', 'test', now()-(g||' minutes')::interval, now(), 'proj'||(g%5)
		 FROM generate_series(1,2000) g`,
		`SELECT setval(pg_get_serial_sequence('tasks','id'), 2000)`,
		`INSERT INTO task_comments (id, task_id, body, author, created_at)
		 SELECT g, 1+(g%2000), 'comment '||g, 'operator', now()-(g||' seconds')::interval
		 FROM generate_series(1,10000) g`,
		`SELECT setval(pg_get_serial_sequence('task_comments','id'), 10000)`,
		// 15,657 verified article bindings, one per document: the table the
		// three article cases plan against.
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
		// Three article ops due now: one bindingless document, two whose
		// binding names the op's target.
		`INSERT INTO yt_write_ops (principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
			yt_target_id, actor, state, attempts, created_at, next_attempt_at)
		 VALUES ('` + YTDocsPrincipal + `','article_create','document-article:1',repeat('a',64),'{}'::jsonb,'document','1','','operator','prepared',0, now()-interval '2 hours', now()-interval '1 minute'),
			('` + YTDocsPrincipal + `','article_update','document-article:update:1',repeat('a',64),'{}'::jsonb,'document','2','art-2','operator','prepared',0, now()-interval '2 hours', now()-interval '1 minute'),
			('` + YTDocsPrincipal + `','article_update','document-article:update:2',repeat('a',64),'{}'::jsonb,'document','3','art-3','operator','prepared',0, now()-interval '2 hours', now()-interval '1 minute')`,
		// The comment ops' ownership links, as mode on writes them.
		`INSERT INTO yt_comment_write_links (comment_id, op_id, created_at)
		 SELECT i, 2*i, now() FROM generate_series(1,7416) i`,
		// The tasks' bindings, as the applied issue_create ops published them,
		// with the comments above their watermark.
		`INSERT INTO yt_task_bindings (task_id, tenant, issue_id, project_id, bound_by, bound_at, origin, op_id, comments_from)
		 SELECT g, '` + ytcTenant + `', 'PROJ-'||g, 'PROJ', 'test', now(), 'op_applied',
			(SELECT min(id) FROM yt_write_ops WHERE kind='issue_create' AND entity_kind='task'
				AND entity_id = g::text AND state = 'applied'), 2
		 FROM generate_series(1,2000) g`,
	}
	for _, q := range seed {
		if _, err := s.pool.Exec(t.Context(), q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
}
