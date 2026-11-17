package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/mysqlurl"
)

const MySQLEnvVar = "HANDOFFKEEP_TEST_MYSQL_URL"

// MySQLDatabase creates an isolated database and returns its mysql URL. The
// caller must close its connections before cleanup drops the database.
func MySQLDatabase(t testing.TB) string {
	t.Helper()
	raw := os.Getenv(MySQLEnvVar)
	if raw == "" {
		if os.Getenv(NoDBVar) == "1" {
			t.Skipf("%s=1: this job runs without a database", NoDBVar)
		}
		if InCI() {
			t.Fatalf("%s must be set in CI; MySQL tests may not skip there", MySQLEnvVar)
		}
		t.Skipf("%s is not set; skipping MySQL test", MySQLEnvVar)
	}
	cfg, err := mysqlurl.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		db.Close()
		t.Fatal(err)
	}
	name := "hk_t_" + hex.EncodeToString(random[:])
	if _, err = db.ExecContext(context.Background(), "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var exists bool
		err := db.QueryRowContext(context.Background(), `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=? AND table_name='sync_log')`, name).Scan(&exists)
		if err != nil {
			t.Errorf("check sync_log existence: %v", err)
		} else if exists {
			var pending int64
			if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM `"+name+"`.sync_log WHERE sync_id IS NULL").Scan(&pending); err != nil {
				t.Errorf("check pending sync rows: %v", err)
			} else if pending != 0 {
				t.Errorf("%d sync_log rows have no sync_id", pending)
			}
		}
		if _, err := db.ExecContext(context.Background(), "DROP DATABASE `"+name+"`"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
