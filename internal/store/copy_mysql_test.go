//go:build mysql

package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestCopyPostgresMySQLAndBackWithParity(t *testing.T) {
	_, pgURL := schemaStoreAt(t, "copy_roundtrip")
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	my.Close()
	pg, err := Open(t.Context(), pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	when := time.Date(2026, 11, 17, 4, 5, 6, 123456000, time.UTC)
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO quota_observations(id,account_key,kind,window_key,measured_at,value,received_at) VALUES(91001,'copy-test','manual','day', $1, $2, $1)`, when, json.RawMessage(`{"b":2,"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_reps(id,origin_id,profile,created_by,recorded_at,created_at,first_ci_green,completed,merged,reverted_within_7d) VALUES(92001,92001,'copy-test','copy-test',$1,$1,TRUE,17,FALSE,TRUE)`, when); err != nil {
		t.Fatal(err)
	}
	// B7 tables are seeded on the PostgreSQL side and copied in both directions
	// below. Keep values representative of their nullable and JSON columns.
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO jobs(job_id,task_id,host,state,role,created_by,created_at,updated_at) VALUES('copy-job',NULL,'copy-host','claimed','worker','copy-test',$1,$1)`, []any{when}},
		{`INSERT INTO relay_events(id,kind,job_id,epoch,owner_lane,machine,pane_id,report_path,report_last_line,question,pr,head,reason,event_id,text,event_time,received_at,attempts) VALUES(94001,'lane.event','',0,'copy-lane','','','','','','','','','copy-event','ready',NULL,$1,1)`, []any{when}},
		{`INSERT INTO notification_outbox(id,kind,target_lane,event_id,text,created_at) VALUES(94001,'dots_report','copy-lane','copy-outbox','ready',$1)`, []any{when}},
		{`INSERT INTO mcp_write_audit(id,tool,actor,channel,author,task_id,idempotency_key,result,at) VALUES(94001,'task_update','copy-actor','dots','dots',NULL,'copy-key','{"ok":true}'::jsonb,$1)`, []any{when}},
		{`INSERT INTO placement_decisions(id,job_id,attempt,task_id,decided_at,decided_on,tool_version,source,ranking,state_source,chosen_host,outcome,reason,profile,pool,account_key,family,model,role,pane_id,repo,candidates) VALUES(94001,'copy-job',1,NULL,$1,'copy-host','','auto','[]','copy',NULL,'failed','','','','','','','','','','[]'::jsonb)`, []any{when}},
		{`INSERT INTO attachments(sha256,size_bytes,mime,original_name,created_by,created_at) VALUES(repeat('b',64),3,'text/plain','copy.txt','copy-test',$1)`, []any{when}},
		{`INSERT INTO attachment_refs(sha256,ref_kind,ref_id) VALUES(repeat('b',64),'task','94001')`, nil},
		{`INSERT INTO attachment_uploads(id,staging_key,name,mime,size_bytes,sha256,ref_kind,ref_id,created_by,status,reason,created_at,expires_at,updated_at) VALUES('94000000000000000000000000000001','stage/copy','copy.txt','text/plain',3,repeat('b',64),'task','94001','copy-test','pending','',$1,$1,$1)`, []any{when}},
		{`INSERT INTO attachment_usage(month,puts,gets,bytes_added) VALUES('2026-11',1,2,3)`, nil},
		{`INSERT INTO ledger_cursors(consumer,acked_sync_id,acked_at,updated_by) VALUES('copy-consumer',0,$1,'copy-test')`, []any{when}},
		{`INSERT INTO sync_log(entry_id,sync_id,model,model_id,action,at) VALUES(94001,94001,'task','copy-task','U',$1)`, []any{when}},
		{`UPDATE sync_state SET max_sync_id=94001,pruned_through=0 WHERE singleton`, nil},
		{`INSERT INTO quota_leases(account_key,owner,holder,expires_at) VALUES('copy-account','copy-owner','copy-holder',$1)`, []any{when}},
		{`INSERT INTO quota_presence(host_id,account_key,last_seen) VALUES('copy-host','copy-account',$1)`, []any{when}},
		// A nonempty op ledger exercises the GENERATED ALWAYS identity insert
		// (OVERRIDING SYSTEM VALUE) on the MySQL to PostgreSQL leg, and the
		// seeded yt_authority rows exercise the upsert.
		{`INSERT INTO yt_write_ops(id,principal,kind,idem_key,payload_sha,payload,entity_kind,entity_id,actor,state,created_at,next_attempt_at,tenant)
			OVERRIDING SYSTEM VALUE VALUES(93001,'hk:task-comments','comment_append','task-comment:93001','sha-93001','{}','task','1','copy-test','prepared',$1,$1,'copy-tenant')`, []any{when}},
		{`INSERT INTO yt_write_op_alerts(op_id,alert_class,created_at) VALUES(93001,'stuck',$1)`, []any{when}},
		{`INSERT INTO yt_issue_intents(task_id,tenant,recorded_at) VALUES(1,'copy-tenant',$1)`, []any{when}},
		{`INSERT INTO yt_issue_comment_bounds(op_id,task_id,comments_from,recorded_at) VALUES(93001,1,7,$1)`, []any{when}},
		{`INSERT INTO yt_write_op_adoptions(op_id,attempts,yt_id,recorded_at) VALUES(93001,1,'CPY-1',$1)`, []any{when}},
		{`INSERT INTO yt_binding_history(id,task_id,tenant,issue_id,origin,action,actor,reason,at) VALUES(93001,1,'copy-tenant','CPY-1','import_verified','bound','copy-test','',$1)`, []any{when}},
		{`INSERT INTO yt_authority(kind,authority,switched_by,note) VALUES('comments','youtrack','copy-test','copy') ON CONFLICT (kind) DO NOTHING`, nil},
	} {
		if _, err := pg.pool.Exec(t.Context(), stmt.query, stmt.args...); err != nil {
			t.Fatalf("seed B7 copy row: %s: %v", stmt.query, err)
		}
	}
	forward, err := CopyStore(t.Context(), pgURL, myURL, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !forward.Match {
		t.Fatalf("PostgreSQL to MySQL parity mismatch: %+v", forward)
	}
	my, err = Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	var nextMySQLID int64
	if err := my.mysqlPool.QueryRowContext(t.Context(), `SELECT AUTO_INCREMENT FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='quota_observations'`).Scan(&nextMySQLID); err != nil {
		t.Fatal(err)
	}
	if nextMySQLID <= 91001 {
		t.Fatalf("MySQL AUTO_INCREMENT=%d, want past copied id 91001", nextMySQLID)
	}
	if err := my.mysqlPool.QueryRowContext(t.Context(), `SELECT AUTO_INCREMENT FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='sync_log'`).Scan(&nextMySQLID); err != nil {
		t.Fatal(err)
	}
	if nextMySQLID <= 94001 {
		t.Fatalf("MySQL sync_log AUTO_INCREMENT=%d, want past copied entry_id 94001", nextMySQLID)
	}
	my.Close()
	_, backURL := schemaStoreAt(t, "copy_reverse")
	back, err := Open(t.Context(), backURL)
	if err != nil {
		t.Fatal(err)
	}
	back.Close()
	reverse, err := CopyStore(t.Context(), myURL, backURL, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reverse.Match {
		t.Fatalf("MySQL to PostgreSQL parity mismatch: %+v", reverse)
	}
	back, err = Open(t.Context(), backURL)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	var nextPostgresID int64
	if err := back.pool.QueryRow(t.Context(), `SELECT nextval(pg_get_serial_sequence('quota_observations','id'))`).Scan(&nextPostgresID); err != nil {
		t.Fatal(err)
	}
	if nextPostgresID <= 91001 {
		t.Fatalf("PostgreSQL sequence=%d, want past copied id 91001", nextPostgresID)
	}
	if err := back.pool.QueryRow(t.Context(), `SELECT nextval(pg_get_serial_sequence('sync_log','entry_id'))`).Scan(&nextPostgresID); err != nil {
		t.Fatal(err)
	}
	if nextPostgresID <= 94001 {
		t.Fatalf("PostgreSQL sync_log sequence=%d, want past copied entry_id 94001", nextPostgresID)
	}
	if err := back.pool.QueryRow(t.Context(), `SELECT nextval(pg_get_serial_sequence('yt_write_ops','id'))`).Scan(&nextPostgresID); err != nil {
		t.Fatal(err)
	}
	if nextPostgresID <= 93001 {
		t.Fatalf("PostgreSQL yt_write_ops sequence=%d, want past copied id 93001", nextPostgresID)
	}
}

func TestCopySourceSnapshotIsRepeatableRead(t *testing.T) {
	pg, pgURL := schemaStoreAt(t, "copy_source_snapshot")
	defer pg.Close()
	when := time.Date(2026, 11, 18, 4, 5, 6, 123456000, time.UTC)
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_grades(profile,grade,boundary_version,deviation_ref,decided_at,decided_by) VALUES('copy-snapshot','A','','',$1,'test')`, when); err != nil {
		t.Fatal(err)
	}
	source, err := openCopyDB(t.Context(), pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	snapshot, err := beginCopySnapshot(t.Context(), source.db)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	var before []byte
	if err := snapshot.QueryRowContext(t.Context(), `SELECT grade FROM bench_grades WHERE profile='copy-snapshot'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(t.Context(), `UPDATE bench_grades SET grade='S' WHERE profile='copy-snapshot'`); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := snapshot.QueryRowContext(t.Context(), `SELECT grade FROM bench_grades WHERE profile='copy-snapshot'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("source snapshot changed during copy: before=%s after=%s", before, after)
	}
}

func TestCopyPostgresTargetKeepsQuotaForeignKeysEnforced(t *testing.T) {
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	defer my.Close()
	key := fmt.Sprintf("copy_fk_%d", time.Now().UnixNano())
	when := time.Date(2026, 11, 18, 5, 6, 7, 123456000, time.UTC)
	result, err := my.mysqlPool.ExecContext(t.Context(), `INSERT INTO quota_observations(account_key,kind,window_key,measured_at,value) VALUES(?,?,?,?,?)`, key, "manual", "day", when, json.RawMessage(`{"revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	observationID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := my.mysqlPool.ExecContext(t.Context(), `INSERT INTO quota_latest(account_key,kind,window_key,observation_id,measured_at,confirmed_at) VALUES(?,?,?,?,?,?)`, key, "manual", "day", observationID, when, when); err != nil {
		t.Fatal(err)
	}
	target, pgURL := schemaStoreAt(t, "copy_fk_enforced")
	defer target.Close()
	_, err = CopyStore(t.Context(), myURL, pgURL, []string{"quota_latest"}, false)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("PostgreSQL target did not enforce quota_latest foreign key: %v", err)
	}
	var rows int
	if err := target.pool.QueryRow(t.Context(), `SELECT count(*) FROM quota_latest`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("target has %d quota_latest rows after foreign key refusal", rows)
	}
}

func TestCopyRefusesOverlongMySQLColumnValue(t *testing.T) {
	_, pgURL := schemaStoreAt(t, "copy_overlong")
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	my.Close()
	pg, err := Open(t.Context(), pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	long := strings.Repeat("x", 201)
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_catalog(profile,effort,model_id,pool,grade,deviation_ref,decided_at,decided_by) VALUES($1,'','model','pool','A','ref',now(),'test')`, long); err != nil {
		t.Fatal(err)
	}
	_, err = CopyStore(t.Context(), pgURL, myURL, []string{"bench_catalog"}, false)
	if err == nil || !strings.Contains(err.Error(), "refusing bench_catalog.profile") {
		t.Fatalf("expected overlong-value refusal, got %v", err)
	}
}
