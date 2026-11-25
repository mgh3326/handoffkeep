package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestCopyChecksumNormalizesMySQLBooleans(t *testing.T) {
	postgres, err := canonicalCopyValue(true, "bench_reps", "merged", copyBoolean, false)
	if err != nil {
		t.Fatal(err)
	}
	mysql, err := canonicalCopyValue([]byte("1"), "bench_reps", "merged", copyBoolean, true)
	if err != nil {
		t.Fatal(err)
	}
	postgresValue, err := json.Marshal(postgres)
	if err != nil {
		t.Fatal(err)
	}
	mysqlValue, err := json.Marshal(mysql)
	if err != nil {
		t.Fatal(err)
	}
	if string(postgresValue) != string(mysqlValue) {
		t.Fatalf("boolean encodings differ: PostgreSQL=%s MySQL=%s", postgresValue, mysqlValue)
	}
	inserted, err := copyInsertValue([]byte("0"), "bench_reps", "merged", copyBoolean, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := inserted.(bool); !ok || got {
		t.Fatalf("PostgreSQL insert value=%T(%v), want false", inserted, inserted)
	}
}

func TestCopyPostedConvertsBetweenPostgresArrayAndMySQLJSON(t *testing.T) {
	const pgArray = `{"ref:a","ref,b","quoted\"ref"}`
	const jsonArray = `["ref:a","ref,b","quoted\"ref"]`
	mysqlValue, err := copyInsertValue(pgArray, "task_thread_deliveries", "posted", copyTextArray, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := mysqlValue.(string); !ok || got != jsonArray {
		t.Fatalf("PostgreSQL text array was not encoded as MySQL JSON: got %T(%v)", mysqlValue, mysqlValue)
	}
	postgresValue, err := copyInsertValue([]byte(jsonArray), "task_thread_deliveries", "posted", copyJSON, true, false)
	if err != nil {
		t.Fatal(err)
	}
	array, ok := postgresValue.(pgtype.ArrayGetter)
	if !ok {
		t.Fatalf("MySQL JSON was not converted to a PostgreSQL array value: got %T", postgresValue)
	}
	if got := []string{array.Index(0).(string), array.Index(1).(string), array.Index(2).(string)}; !reflect.DeepEqual(got, []string{"ref:a", "ref,b", `quoted"ref`}) {
		t.Fatalf("converted PostgreSQL array=%v", got)
	}
	pgCanonical, err := canonicalCopyValue(pgArray, "task_thread_deliveries", "posted", copyTextArray, false)
	if err != nil {
		t.Fatal(err)
	}
	myCanonical, err := canonicalCopyValue([]byte(jsonArray), "task_thread_deliveries", "posted", copyJSON, true)
	if err != nil {
		t.Fatal(err)
	}
	pgBytes, err := json.Marshal(pgCanonical)
	if err != nil {
		t.Fatal(err)
	}
	myBytes, err := json.Marshal(myCanonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(pgBytes) != string(myBytes) {
		t.Fatalf("array checksum encodings differ: PostgreSQL=%s MySQL=%s", pgBytes, myBytes)
	}
}

func TestCopyNormalizesMySQLNumericColumns(t *testing.T) {
	integer, err := normalizeCopyValue([]byte("17"), copyInteger)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := integer.(int64); !ok || got != 17 {
		t.Fatalf("integer=%T(%v)", integer, integer)
	}
	insertedInteger, err := copyInsertValue([]byte("17"), "bench_reps", "completed", copyInteger, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := insertedInteger.(int64); !ok || got != 17 {
		t.Fatalf("integer insert value=%T(%v)", insertedInteger, insertedInteger)
	}
	floating, err := normalizeCopyValue([]byte("1.25"), copyFloat)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := floating.(float64); !ok || got != 1.25 {
		t.Fatalf("float=%T(%v)", floating, floating)
	}
	insertedFloat, err := copyInsertValue([]byte("1.25"), "bench_catalog", "score", copyFloat, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := insertedFloat.(float64); !ok || got != 1.25 {
		t.Fatalf("float insert value=%T(%v)", insertedFloat, insertedFloat)
	}
}

func TestCopyClassifiesCrossDialectScalarTypes(t *testing.T) {
	cases := []struct {
		mysql  bool
		data   string
		detail string
		want   copyValueKind
	}{
		{mysql: true, data: "tinyint", detail: "tinyint(1)", want: copyBoolean},
		{mysql: true, data: "int", detail: "int", want: copyInteger},
		{mysql: true, data: "double", detail: "double", want: copyFloat},
		{mysql: false, data: "boolean", detail: "bool", want: copyBoolean},
		{mysql: false, data: "integer", detail: "int4", want: copyInteger},
		{mysql: false, data: "double precision", detail: "float8", want: copyFloat},
	}
	for _, tc := range cases {
		got, err := classifyCopyColumn(tc.mysql, tc.data, tc.detail)
		if err != nil {
			t.Fatalf("classify %q: %v", tc.data, err)
		}
		if got != tc.want {
			t.Errorf("classify %q = %v, want %v", tc.data, got, tc.want)
		}
	}
}

func TestCopyChecksumIgnoresTextRowOrder(t *testing.T) {
	// These keys sort in opposite orders under common en_US and binary
	// collations. Identical rows must still produce identical checksums.
	spec := copyTableSpec{name: "bench_grades", columns: []string{"profile"}, order: []string{"profile"}}
	kinds := map[string]copyValueKind{"profile": copyText}
	postgresDB := sql.OpenDB(copyOrderConnector{rows: [][]driver.Value{{"haiku"}, {"Opus"}}})
	defer postgresDB.Close()
	postgres, err := checksum(context.Background(), postgresDB, false, spec, spec.columns, kinds, func([]any) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	mysqlDB := sql.OpenDB(copyOrderConnector{rows: [][]driver.Value{{"Opus"}, {"haiku"}}})
	defer mysqlDB.Close()
	mysql, err := checksum(context.Background(), mysqlDB, true, spec, spec.columns, kinds, func([]any) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if postgres != mysql {
		t.Fatalf("same rows in different collation orders have different checksums: PostgreSQL=%s MySQL=%s", postgres, mysql)
	}
}

func TestOpenCopyDBMalformedURLDoesNotEchoCredentials(t *testing.T) {
	const raw = "postgres://copy-user:secret%zz@localhost/handoffkeep"
	_, err := openCopyDB(context.Background(), raw)
	if err == nil {
		t.Fatal("openCopyDB accepted malformed URL")
	}
	if got := err.Error(); got != "malformed database URL" || strings.Contains(got, "secret") || strings.Contains(got, raw) {
		t.Fatalf("malformed URL error=%q, want a fixed message with no credentials", got)
	}
}

func TestSelectCopyTablesRejectsBlankSelection(t *testing.T) {
	_, err := selectCopyTables([]string{"", " \t", ""})
	if err == nil || err.Error() != "no tables selected" {
		t.Fatalf("selectCopyTables error=%v, want no tables selected", err)
	}
}

func TestSelectCopyTablesTasksRequiresSyncDecisions(t *testing.T) {
	_, err := selectCopyTables([]string{"tasks"})
	if err == nil || !strings.Contains(err.Error(), `"tasks"`) || !strings.Contains(err.Error(), `"sync_decisions"`) {
		t.Fatalf("selectCopyTables(tasks) error=%v, want tasks requires sync_decisions", err)
	}
	if _, err := selectCopyTables([]string{"tasks", "sync_decisions"}); err != nil {
		t.Fatalf("selectCopyTables(tasks, sync_decisions)=%v, want no error", err)
	}
	if wanted, err := selectCopyTables([]string{"sync_decisions"}); err != nil || !wanted["sync_decisions"] {
		t.Fatalf("selectCopyTables(sync_decisions)=%v, %v, want the projection alone accepted", wanted, err)
	}
	if wanted, err := selectCopyTables(nil); err != nil || !wanted["tasks"] || !wanted["sync_decisions"] {
		t.Fatalf("selectCopyTables(nil)=%v, %v, want the default to keep the companions together", wanted, err)
	}
}

// The companion refusal must fire before any database is opened: even with
// unreachable URLs the error names the missing table, not the connection.
func TestCopyStoreTasksWithoutSyncDecisionsRefusesBeforeOpen(t *testing.T) {
	_, err := CopyStore(t.Context(), "postgres://invalid.invalid/db", "mysql://invalid.invalid/db", []string{"tasks"}, false)
	if err == nil || !strings.Contains(err.Error(), `"sync_decisions"`) {
		t.Fatalf("CopyStore(tasks) error=%v, want the sync_decisions refusal before any open", err)
	}
}

func TestCopyEmptyPostedPreservesEmptyArray(t *testing.T) {
	mysqlValue, err := copyInsertValue([]string(nil), "task_thread_deliveries", "posted", copyTextArray, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := mysqlValue.(string); !ok || got != "[]" {
		t.Fatalf("empty PostgreSQL array encoded as MySQL JSON %T(%v), want []", mysqlValue, mysqlValue)
	}
	postgresValue, err := copyInsertValue([]byte("[]"), "task_thread_deliveries", "posted", copyJSON, true, false)
	if err != nil {
		t.Fatal(err)
	}
	refs, ok := postgresValue.(pgtype.FlatArray[string])
	if !ok || refs == nil || len(refs) != 0 {
		t.Fatalf("empty MySQL JSON array converted to PostgreSQL value %T(%v), want non-nil empty array", postgresValue, postgresValue)
	}
	pgCanonical, err := canonicalCopyValue([]string(nil), "task_thread_deliveries", "posted", copyTextArray, false)
	if err != nil {
		t.Fatal(err)
	}
	myCanonical, err := canonicalCopyValue([]byte("[]"), "task_thread_deliveries", "posted", copyJSON, true)
	if err != nil {
		t.Fatal(err)
	}
	pgRow, err := json.Marshal([]any{pgCanonical})
	if err != nil {
		t.Fatal(err)
	}
	myRow, err := json.Marshal([]any{myCanonical})
	if err != nil {
		t.Fatal(err)
	}
	if string(pgRow) != string(myRow) || string(pgRow) != `[[]]` {
		t.Fatalf("empty array checksum rows differ: PostgreSQL=%s MySQL=%s", pgRow, myRow)
	}
	if _, err := parseJSONTextArray([]byte("null")); err == nil {
		t.Fatal("JSON null was accepted as a posted array")
	}
}

type copyOrderConnector struct{ rows [][]driver.Value }

func (c copyOrderConnector) Connect(context.Context) (driver.Conn, error) {
	return copyOrderConn{rows: c.rows}, nil
}

func (copyOrderConnector) Driver() driver.Driver { return copyOrderDriver{} }

type copyOrderDriver struct{}

func (copyOrderDriver) Open(string) (driver.Conn, error) { return nil, driver.ErrSkip }

type copyOrderConn struct{ rows [][]driver.Value }

func (copyOrderConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (copyOrderConn) Close() error                        { return nil }
func (copyOrderConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c copyOrderConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &copyOrderRows{rows: c.rows}, nil
}

type copyOrderRows struct {
	rows  [][]driver.Value
	index int
}

func (*copyOrderRows) Columns() []string { return []string{"profile"} }
func (*copyOrderRows) Close() error      { return nil }
func (r *copyOrderRows) Next(dest []driver.Value) error {
	if r.index == len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.index])
	r.index++
	return nil
}

// A NULL JSON column (task_events.refs) must copy as NULL, not panic.
func TestCopyInsertValueKeepsNullJSON(t *testing.T) {
	for _, mysqlTarget := range []bool{false, true} {
		got, err := copyInsertValue(nil, "task_events", "refs", copyJSON, !mysqlTarget, mysqlTarget)
		if err != nil || got != nil {
			t.Fatalf("mysqlTarget=%v: got %v, %v; want nil, nil", mysqlTarget, got, err)
		}
	}
}

func TestCopyUpsertClause(t *testing.T) {
	spec := copyTableSpec{name: "task_projects", order: []string{"name"}}
	if got, want := copyUpsertClause(spec, []string{`"name"`, `"status"`}, false), ` ON CONFLICT ("name") DO UPDATE SET "status"=EXCLUDED."status"`; got != want {
		t.Errorf("postgres upsert = %q, want %q", got, want)
	}
	if got, want := copyUpsertClause(spec, []string{"`name`", "`status`"}, true), " ON DUPLICATE KEY UPDATE `status`=VALUES(`status`)"; got != want {
		t.Errorf("mysql upsert = %q, want %q", got, want)
	}
}
