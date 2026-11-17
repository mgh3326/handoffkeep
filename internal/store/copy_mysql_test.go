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
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_area_exceptions(profile,effort,area,action,evidence_refs,decision_ref,decided_by,decided_at,expires_with_model_id) VALUES('copy-test','','docs','exclude',ARRAY['ref:a','ref,b','quoted"ref'],'hk:task/1','test',$1,'model')`, when); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_area_exceptions(profile,effort,area,action,evidence_refs,decision_ref,decided_by,decided_at,expires_with_model_id) VALUES('copy-test-empty','','docs','prefer',ARRAY[]::TEXT[],'hk:task/2','test',$1,'model')`, when); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_reps(id,origin_id,profile,created_by,recorded_at,created_at,first_ci_green,completed,merged,reverted_within_7d) VALUES(92001,92001,'copy-test','copy-test',$1,$1,TRUE,17,FALSE,TRUE)`, when); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_scores(model_id,effort,harness,source,metric,score,captured_at,cost_per_task_usd,updated_by,updated_at) VALUES('copy-model','','','copy-test','metric',1.25,$1,2.5,'test',$1)`, when); err != nil {
		t.Fatal(err)
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
	my.Close()
	if nextMySQLID <= 91001 {
		t.Fatalf("MySQL AUTO_INCREMENT=%d, want past copied id 91001", nextMySQLID)
	}
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
	long := strings.Repeat("x", 129)
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_scores(model_id,effort,harness,source,metric,updated_by,captured_at,updated_at) VALUES($1,'','','source','metric','test',now(),now())`, long); err != nil {
		t.Fatal(err)
	}
	_, err = CopyStore(t.Context(), pgURL, myURL, []string{"bench_scores"}, false)
	if err == nil || !strings.Contains(err.Error(), "refusing bench_scores.model_id") {
		t.Fatalf("expected overlong-value refusal, got %v", err)
	}
}
