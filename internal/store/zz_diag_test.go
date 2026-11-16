package store

// THROWAWAY diagnostic for hk 4922 (deleted before the PR): prints the plans
// of the write worker's hot statements on a production-sized seed, before and
// after the migration's indexes. Runs only in CI (HANDOFFKEEP_TEST_DB_URL).

import (
	"strings"
	"testing"
	"time"
)

func TestYTDiagWritePathPlans(t *testing.T) {
	ctx := t.Context()
	s := schemaStore(t, "ytdiag")
	now := time.Now().UTC()

	ytPlanSeed(t, s)
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_write_ops (principal, kind, idem_key, payload_sha, payload, entity_kind, entity_id,
		yt_target_id, actor, state, attempts, created_at, next_attempt_at)
		SELECT 'hk:task-comments','comment_append','held:'||g,repeat('a',64),'{}'::jsonb,'task',(3000+g)::text,'','operator',
		'prepared',0, now()-interval '2 hours', now()+interval '1 hour'
		FROM generate_series(1,200) g`); err != nil {
		t.Fatal(err)
	}

	s.SetYTCommentsMode(YTCommentsOn)
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	s.SetYTMovesMode(YTMovesOn)

	var create, update int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE kind='article_create'`).Scan(&create); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE kind='article_update' ORDER BY id LIMIT 1`).Scan(&update); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"article_create admission", `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND ` + s.ytAdmitSQL("$2") + `)`, []any{create, ytcTenant}},
		{"article_update admission", `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND ` + s.ytAdmitSQL("$2") + `)`, []any{update, ytcTenant}},
		{"requeue select", ytRequeueSelectSQL, []any{YTCommentPrincipal, ytcTenant, YTIssuePrincipal, "issue_create", 200}},
		{"catch-up select", ytCatchUpSelectSQL, []any{YouTrackOwnedActor, YTCommentPrincipal, "comment_append", "comment:", []int64{}, 200, false}},
		{"held candidates", `SELECT ` + ytHeldOpColumns + ` FROM yt_write_ops o
			WHERE o.state='prepared' AND o.tenant=$6 AND o.created_at <= $1 AND (o.created_at, o.id) > ($2, $3)
			AND o.kind = ANY($4) AND ($7 = '' OR o.entity_id = $7) AND ` + ytHeldScopeSQL + ` AND NOT ` + s.ytAdmitSQL("$6") + ` AND ` + ytHeldKeptSQL + `
			ORDER BY o.created_at, o.id LIMIT $5`,
			[]any{now.Add(-YTHeldRecoveryGrace), time.Unix(0, 0).UTC(), int64(0),
				[]string{"comment_append", "issue_fields", "issue_move", "issue_links"}, 200, ytcTenant, ""}},
	}
	report := func(tag string, execute bool) {
		for _, tc := range cases {
			modes := []string{""}
			if execute {
				modes = append(modes, "ANALYZE")
			}
			for _, analyze := range modes {
				rows, err := s.pool.Query(ctx, "EXPLAIN "+analyze+" (COSTS OFF, SUMMARY OFF, TIMING OFF) "+tc.sql, tc.args...)
				if err != nil {
					t.Logf("== [%s] EXPLAIN %q %s: %v", tag, analyze, tc.name, err)
					continue
				}
				var lines []string
				for rows.Next() {
					var l string
					if err := rows.Scan(&l); err != nil {
						t.Fatalf("%s: %v", tc.name, err)
					}
					lines = append(lines, l)
				}
				rows.Close()
				t.Logf("== [%s] EXPLAIN %q %s\n%s", tag, analyze, tc.name, strings.Join(lines, "\n"))
			}
		}
	}

	if _, err := s.pool.Exec(ctx, `DROP INDEX IF EXISTS yt_article_bindings_document_text`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DROP INDEX IF EXISTS yt_write_ops_comment_requeue`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	// The "before" plans are not executed: the requeue's scan of every
	// comment op takes minutes on this seed, which is the finding.
	report("before", false)
	for _, q := range ytWritePathIndexStatements {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	report("after", true)
}
