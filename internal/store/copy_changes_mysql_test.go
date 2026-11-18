//go:build mysql

package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func seedChangesAreaPostgres(t *testing.T, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), query, args...); err != nil {
			t.Fatalf("seed query %q: %v", query, err)
		}
	}
	exec(`INSERT INTO tasks(id,lane,title,kind,state,created_by,created_at,updated_at) VALUES(92001,'copy','change copy','implement','developing','test',$1,$1)`, at)
	exec(`INSERT INTO changes(id,repo,pr_number,state,opened_at,created_at,updated_at,gate_state,gate_head,gate_reasons,gate_decided_at,gate_policy_revision,gate_source)
		VALUES(92001,'example/app',92001,'open',$1,$1,$1,'ready',repeat('a',40),'["ci_passed"]',$1,'copy-v1','review')`, at)
	exec(`INSERT INTO changes(id,repo,pr_number,state,opened_at,created_at,updated_at)
		VALUES(92002,'example/app',92002,'merged',$1,$1,$1)`, at)
	exec(`UPDATE changes SET reverted_by_change_id=92002,depends_on_change_id=92002,depends_on_state='merged' WHERE id=92001`)
	exec(`INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES(92001,92001,'implements',$1)`, at)
	exec(`INSERT INTO reviews(id,change_id,reviewed_head,round,pass,lens,recorded_at,reviewer,same_family,delta,quiet,outside_diff_blocking,patch_digest)
		VALUES(92001,92001,repeat('a',40),1,1,'correctness',$1,'openai',false,true,false,0,'digest-1')`, at)
	exec(`INSERT INTO change_checks(change_id,head_sha,name,state,observed_at,source,gone,delivery_id) VALUES(92001,repeat('a',40),'ci','completed',$1,'webhook',false,'delivery-1')`, at)
	exec(`INSERT INTO change_threads(change_id,thread_id,author_kind,resolved,observed_at,source) VALUES(92001,'NODE-1','human',false,$1,'review')`, at)
	exec(`INSERT INTO change_findings(change_id,reviewed_head,round,pass,lens,ordinal,review_id,ext_id,severity,blocking,observed_at,source)
		VALUES(92001,repeat('a',40),1,1,'correctness',0,92001,'NODE-1','high',true,$1,'review')`, at)
	exec(`INSERT INTO deploy_record_projection(key,sha256,record,decoded) VALUES('deploy/copy/20261117T120000Z',repeat('b',64),$1,true)`, []byte(`{"result":"success","included_prs":[]}`))
}

func TestCopyChangesAreaRoundTripWithParity(t *testing.T) {
	src, srcURL := schemaStoreAt(t, "copy_changes_src")
	at := time.Date(2026, 11, 17, 12, 0, 0, 123456000, time.UTC)
	seedChangesAreaPostgres(t, src.pool, at)
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	my.Close()
	tables := []string{"tasks", "sync_decisions", "changes", "change_tasks", "reviews", "change_checks", "change_threads", "change_findings", "change_gate_events", "deploy_record_projection", "deploy_services"}
	forward, err := CopyStore(t.Context(), srcURL, myURL, tables, false)
	if err != nil {
		t.Fatal(err)
	}
	if !forward.Match || len(forward.Tables) != len(tables) {
		t.Fatalf("PostgreSQL to MySQL parity=%+v", forward)
	}
	back, backURL := schemaStoreAt(t, "copy_changes_back")
	reverse, err := CopyStore(t.Context(), myURL, backURL, tables, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reverse.Match || len(reverse.Tables) != len(tables) {
		t.Fatalf("MySQL to PostgreSQL parity=%+v", reverse)
	}
	var findingCount, gateCount int
	if err = back.pool.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM change_findings),(SELECT COUNT(*) FROM change_gate_events)`).Scan(&findingCount, &gateCount); err != nil || findingCount != 1 || gateCount != 1 {
		t.Fatalf("copied rows findings=%d gates=%d err=%v", findingCount, gateCount, err)
	}
	var revertedBy, dependsOn int64
	if err = back.pool.QueryRow(t.Context(), `SELECT reverted_by_change_id,depends_on_change_id FROM changes WHERE id=92001`).Scan(&revertedBy, &dependsOn); err != nil || revertedBy != 92002 || dependsOn != 92002 {
		t.Fatalf("copied self references reverted_by=%d depends_on=%d err=%v", revertedBy, dependsOn, err)
	}
}
