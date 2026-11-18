//go:build mysql

package store

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// ytAreaCopyTables is the YouTrack area plus the rows it depends on: a task,
// a document and a comment for the tables that reference them.
func ytAreaCopyTables() []string {
	var out []string
	for _, name := range StoreCopyTables() {
		if strings.HasPrefix(name, "yt_") || strings.HasPrefix(name, "youtrack_") ||
			name == "tasks" || name == "sync_decisions" || name == "documents" || name == "task_comments" {
			out = append(out, name)
		}
	}
	return out
}

// seedYTAreaPostgres fills every YouTrack table with raw SQL, including the
// rows the store's own methods cannot make in this shape: a released link
// beside a live one, a closed unowned row, a pruned feed cursor at a high
// watermark, and a family root whose op id is the op row it names.
func seedYTAreaPostgres(t *testing.T, pool *pgxpool.Pool, when time.Time) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO tasks(id,lane,title,kind,state,created_by,created_at,updated_at,last_activity_at) VALUES(95001,'lane-yt','copy task','implement','developing','test',$1,$1,$1)`, when)
	exec(`INSERT INTO documents(id,key,kind,session,job,body,sha256,created_by,created_at,updated_at,task_id,origin_class) VALUES(95001,'copy/yt/doc','brief','','','body','hash','test',$1,$1,95001,'authored')`, when)
	exec(`INSERT INTO task_comments(id,task_id,body,author,created_at) VALUES(95001,95001,'comment','test',$1)`, when)

	exec(`INSERT INTO yt_write_ops(id,principal,kind,idem_key,payload_sha,payload,entity_kind,entity_id,yt_target_id,actor,child_seq,state,yt_result_id,reason,last_error,attempts,reconciles,created_at,next_attempt_at,tenant)
		OVERRIDING SYSTEM VALUE VALUES(95001,'hk:documents','article_create','copy-op-1',repeat('a',64),'{"title":"t"}','document','95001','','test',0,'prepared','','','',0,0,$1,$1,'yt.example')`, when)
	exec(`INSERT INTO yt_entity_map(entity_kind,hk_key,yt_id,yt_parent_id,op_id,created_at,tenant) VALUES('document','95001','3-95','3-1',95001,$1,'yt.example')`, when)
	exec(`INSERT INTO yt_task_bindings(task_id,tenant,issue_id,issue_readable,project_id,bound_by,bound_at,comments_from,origin,op_id) VALUES(95001,'yt.example','2-95001','HK-95001','0-30','test',$1,95001,'import_verified',95001)`, when)
	exec(`INSERT INTO yt_comment_canary(task_id,tenant,issue_id,issue_readable,project_id,added_by,added_at) VALUES(95001,'yt.example','2-95001','HK-95001','0-30','test',$1)`, when)
	exec(`INSERT INTO yt_comment_write_links(comment_id,op_id,created_at) VALUES(95001,95001,$1)`, when)
	exec(`INSERT INTO yt_article_bindings(document_id,tenant,article_id,article_readable,project_id,part,generation,content_sha,origin,op_id,needs_update,bound_by,bound_at) VALUES(95001,'yt.example','3-95','HK-A-95','0-30',0,1,repeat('b',64),'op_applied',95001,TRUE,'test',$1)`, when)
	exec(`INSERT INTO yt_snapshot_state(entity_kind,entity_id,kind,seq,last_sha,last_op_id) VALUES('document','95001','article_update',2,repeat('c',64),95001)`)
	exec(`INSERT INTO yt_family_roots(tenant,project_id,family,article_id,op_id,created_at) VALUES('yt.example','0-30','briefs','3-1',95001,$1)`, when)
	exec(`INSERT INTO yt_feed_cursors(consumer,last_sync_id,updated_at) VALUES('document_articles',77,$1)`, when)
	exec(`INSERT INTO yt_binding_intents(task_id,tenant,state,reason,attempts,last_error,next_attempt_at,recorded_at) VALUES(95001,'yt.example','refused','ambiguous',2,'detail',$1,$1)`, when)
	exec(`INSERT INTO yt_binding_cursor(id,last_comment_id) VALUES(1,95001) ON CONFLICT (id) DO UPDATE SET last_comment_id=EXCLUDED.last_comment_id`)
	exec(`INSERT INTO yt_binding_history(id,task_id,tenant,issue_id,origin,action,actor,reason,at) VALUES(95001,95001,'yt.example','2-95001','import_verified','unbound','test','operator',$1)`, when)
	exec(`INSERT INTO yt_decision_bindings(decision_id,tenant,issue_id,issue_readable,project_id,origin,op_id,bound_by,bound_at) VALUES('95001','yt.example','2-95001','HK-95001','0-30','import_verified',95001,'test',$1)`, when)
	exec(`INSERT INTO yt_issue_intents(task_id,tenant,recorded_at) VALUES(95001,'yt.example',$1)`, when)
	exec(`INSERT INTO yt_issue_comment_bounds(op_id,task_id,comments_from,recorded_at) VALUES(95001,95001,95001,$1)`, when)
	exec(`INSERT INTO yt_write_op_alerts(op_id,alert_class,created_at) VALUES(95001,'stuck',$1)`, when)
	exec(`INSERT INTO yt_write_op_adoptions(op_id,attempts,yt_id,recorded_at) VALUES(95001,1,'2-95001',$1)`, when)
	exec(`INSERT INTO yt_link_plans(id,op_id,attempt,recover,plan,created_at) VALUES(95001,95001,0,0,'{"steps":[1,2]}',$1)`, when)
	exec(`INSERT INTO yt_link_steps(op_id,attempt,recover,step,marked_at,outcome) VALUES(95001,0,0,1,$1,'ok')`, when)
	exec(`INSERT INTO yt_link_owned(id,tenant,link_type,from_issue,to_issue,writer_task_id,target_task_id,kind,intended_op_id,intended_at,released_op_id,released_at) VALUES(95001,'yt.example','subtask','2-95001','2-96001',95001,96001,'parent',95001,$1,NULL,NULL)`, when)
	exec(`INSERT INTO yt_link_owned(id,tenant,link_type,from_issue,to_issue,writer_task_id,target_task_id,kind,intended_op_id,intended_at,released_op_id,released_at) VALUES(95002,'yt.example','subtask','2-95001','2-96002',95001,96002,'parent',95001,$1,95001,$1)`, when)
	// The released row and the live row share the link: only the live one is in
	// the functional unique key, which the copy must preserve.
	exec(`INSERT INTO yt_link_unowned(id,tenant,issue_id,link_type,side,other_issue,first_seen_op_id,last_seen_op_id,first_seen_at,last_seen_at,gone_at) VALUES(95001,'yt.example','2-95001','subtask','out','2-96001',95001,95001,$1,$1,NULL)`, when)
	exec(`INSERT INTO yt_link_unowned(id,tenant,issue_id,link_type,side,other_issue,first_seen_op_id,last_seen_op_id,first_seen_at,last_seen_at,gone_at) VALUES(95002,'yt.example','2-95001','subtask','out','2-96002',95001,95001,$1,$1,$1)`, when)
	exec(`INSERT INTO yt_links_sweep(singleton,generation,done_generation,after_task_id,updated_at) VALUES(TRUE,3,2,95001,$1) ON CONFLICT (singleton) DO UPDATE SET generation=EXCLUDED.generation, done_generation=EXCLUDED.done_generation, after_task_id=EXCLUDED.after_task_id, updated_at=EXCLUDED.updated_at`, when)
	exec(`INSERT INTO youtrack_projects(hk_project,yt_project_id,short_name,created_at,updated_at) VALUES('copy-project','0-99','CP',$1,$1)`, when)
	exec(`INSERT INTO youtrack_comment_links(hk_comment_id,task_id,youtrack_comment_id,created_at) VALUES(95001,95001,'2-95001-1',$1)`, when)
}

// The YouTrack area copies in both directions with parity: every row the
// seed writes is identical on the target, including the functional partial
// keys (a released link beside a live one) and the seeded tables.
func TestCopyYTAreaRoundTripWithParity(t *testing.T) {
	src, srcURL := schemaStoreAt(t, "copy_yt_src")
	when := time.Date(2026, 11, 18, 7, 8, 9, 123456000, time.UTC)
	seedYTAreaPostgres(t, src.pool, when)
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	my.Close()

	tables := ytAreaCopyTables()
	forward, err := CopyStore(t.Context(), srcURL, myURL, tables, false)
	if err != nil {
		t.Fatal(err)
	}
	if !forward.Match || len(forward.Tables) != len(tables) {
		t.Fatalf("PostgreSQL to MySQL parity mismatch: %+v", forward)
	}
	my, err = Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	var live, released int
	var cursor int64
	var nextPlan int64
	if err := my.mysqlPool.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM yt_link_owned WHERE released_op_id IS NULL),
		(SELECT COUNT(*) FROM yt_link_owned WHERE released_op_id IS NOT NULL),
		(SELECT last_comment_id FROM yt_binding_cursor WHERE id=1),
		(SELECT AUTO_INCREMENT FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='yt_link_plans')`).
		Scan(&live, &released, &cursor, &nextPlan); err != nil {
		my.Close()
		t.Fatal(err)
	}
	my.Close()
	if live != 1 || released != 1 || cursor != 95001 || nextPlan <= 95001 {
		t.Fatalf("MySQL target: live links=%d released=%d binding cursor=%d next plan id=%d", live, released, cursor, nextPlan)
	}

	back, backURL := schemaStoreAt(t, "copy_yt_back")
	reverse, err := CopyStore(t.Context(), myURL, backURL, tables, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reverse.Match || len(reverse.Tables) != len(tables) {
		t.Fatalf("MySQL to PostgreSQL parity mismatch: %+v", reverse)
	}
	var backLive, backFamily int
	if err := back.pool.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM yt_link_owned WHERE released_op_id IS NULL),
		(SELECT COUNT(*) FROM yt_family_roots WHERE tenant='yt.example' AND project_id='0-30' AND family='briefs')`).Scan(&backLive, &backFamily); err != nil {
		t.Fatal(err)
	}
	if backLive != 1 || backFamily != 1 {
		t.Fatalf("PostgreSQL target: live links=%d family roots=%d", backLive, backFamily)
	}
	// A copied append-only table stays writable for the one insert its own
	// path makes and refuses updates, on both targets.
	if _, err := back.pool.Exec(t.Context(), `INSERT INTO yt_binding_history(task_id,tenant,issue_id,origin,action,actor,reason,at) VALUES(95001,'yt.example','2-95001','import_verified','bound','test','',$1)`, when); err != nil {
		t.Fatalf("append yt_binding_history after copy: %v", err)
	}
	if _, err := back.pool.Exec(t.Context(), `UPDATE yt_binding_history SET reason='x' WHERE task_id=95001`); err == nil {
		t.Fatal("yt_binding_history append-only guard did not fire after copy")
	}
}
