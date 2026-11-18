package store

import "testing"

func TestOrderSQLRefusesOutOfOrderPlaceholders(t *testing.T) {
	mysql := &Store{dialect: mysqlDialect}
	if got := mysql.orderSQL(`UPDATE t SET a=$1,b=$2 WHERE id=$3`); got != `UPDATE t SET a=?,b=? WHERE id=?` {
		t.Fatalf("mysql text %q", got)
	}
	if got := (&Store{}).orderSQL(`SELECT 1 WHERE a=$1 AND b=$2`); got != `SELECT 1 WHERE a=$1 AND b=$2` {
		t.Fatalf("postgres text %q", got)
	}
	for _, bad := range []string{`UPDATE t SET a=$2 WHERE id=$1`, `SELECT $1, $1`, `SELECT $2`} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%q was accepted", bad)
				}
			}()
			mysql.orderSQL(bad)
		}()
	}
}
