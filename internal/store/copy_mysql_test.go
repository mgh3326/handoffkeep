//go:build mysql

package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
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
	if _, err := pg.pool.Exec(t.Context(), `INSERT INTO bench_area_exceptions(profile,effort,area,action,evidence_refs,decision_ref,decided_by,decided_at,expires_with_model_id) VALUES('copy-test','','docs','require',ARRAY['ref:a','ref,b','quoted"ref'],'hk:task/1','test',$1,'model')`, when); err != nil {
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

func TestCopyPostgresReplicaRolePreflightRefusesBeforeWrites(t *testing.T) {
	myURL := testdb.MySQLDatabase(t)
	my, err := Open(t.Context(), myURL)
	if err != nil {
		t.Fatal(err)
	}
	my.Close()
	target, pgURL := schemaStoreAt(t, "copy_role_preflight")
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	role := "hk_copy_reader_" + hex.EncodeToString(random[:])
	password := hex.EncodeToString(random[:])
	admin := testdb.Conn(t)
	if _, err := admin.Exec(t.Context(), fmt.Sprintf(`CREATE ROLE %q LOGIN PASSWORD '%s'`, role, password)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %q`, role)) })
	u, err := url.Parse(pgURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	_, err = CopyStore(t.Context(), myURL, u.String(), []string{"quota_observations"}, false)
	if err == nil || !strings.Contains(err.Error(), "must allow SET session_replication_role = replica") {
		t.Fatalf("non-superuser target was not refused by preflight: %v", err)
	}
	var rows int
	if err := target.pool.QueryRow(t.Context(), `SELECT count(*) FROM quota_observations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("target has %d quota observations after privilege refusal", rows)
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
