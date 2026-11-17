//go:build mysql

package store

import (
	"encoding/json"
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
