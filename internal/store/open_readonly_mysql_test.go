//go:build mysql

package store

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLOpenReadOnlyReadsAndRefusesWrites opens a MySQL store read-only, as
// the youtrack dry runs do. Before hk 5711 OpenReadOnly refused every mysql://
// URL, so each dry run failed once hk served from MySQL.
func TestMySQLOpenReadOnlyReadsAndRefusesWrites(t *testing.T) {
	ctx := t.Context()
	url := testdb.MySQLDatabase(t)
	rw, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, _, err := rw.PutDocument(ctx, Document{Key: "hk5711/doc-" + n, Kind: "note", Session: "s-" + n, Body: "body", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(ctx, url)
	if err != nil {
		t.Fatalf("OpenReadOnly on MySQL: %v", err)
	}
	defer ro.Close()
	applied, err := ro.MigrationApplied(ctx, YTWriteOpRequeueMigration)
	if err != nil || !applied {
		t.Fatalf("MigrationApplied = %v, %v; want true", applied, err)
	}
	var count int
	if err := ro.db().QueryRow(ctx, `SELECT COUNT(*) FROM documents WHERE `+"`key`"+`=?`, "hk5711/doc-"+n).Scan(&count); err != nil || count != 1 {
		t.Fatalf("read through the read-only store = %d, %v; want 1", count, err)
	}
	_, err = ro.db().Exec(ctx, `UPDATE documents SET body='changed' WHERE `+"`key`"+`=?`, "hk5711/doc-"+n)
	if err == nil || !strings.Contains(err.Error(), "1792") {
		t.Fatalf("write through the read-only store: err = %v, want MySQL 1792 (read-only transaction)", err)
	}
}
