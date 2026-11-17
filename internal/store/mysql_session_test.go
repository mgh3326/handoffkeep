//go:build mysql

package store

import (
	"database/sql"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLSessionSettings(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	// Hold both connections at once to check that setup is per connection.
	first, err := s.mysqlPool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.mysqlPool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		var isolation, zone string
		if err := conn.QueryRowContext(t.Context(), `SELECT @@transaction_isolation, @@time_zone`).Scan(&isolation, &zone); err != nil {
			t.Fatal(err)
		}
		if isolation != "READ-COMMITTED" || zone != "+00:00" {
			t.Fatalf("session settings: isolation=%q zone=%q", isolation, zone)
		}
	}
}
