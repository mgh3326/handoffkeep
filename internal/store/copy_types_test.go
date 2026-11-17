package store

import (
	"encoding/json"
	"reflect"
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

func TestCopyEvidenceRefsConvertsBetweenPostgresArrayAndMySQLJSON(t *testing.T) {
	const pgArray = `{"ref:a","ref,b","quoted\"ref"}`
	const jsonArray = `["ref:a","ref,b","quoted\"ref"]`
	mysqlValue, err := copyInsertValue(pgArray, "bench_area_exceptions", "evidence_refs", copyTextArray, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := mysqlValue.(string); !ok || got != jsonArray {
		t.Fatalf("PostgreSQL text array was not encoded as MySQL JSON: got %T(%v)", mysqlValue, mysqlValue)
	}
	postgresValue, err := copyInsertValue([]byte(jsonArray), "bench_area_exceptions", "evidence_refs", copyJSON, true, false)
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
	pgCanonical, err := canonicalCopyValue(pgArray, "bench_area_exceptions", "evidence_refs", copyTextArray, false)
	if err != nil {
		t.Fatal(err)
	}
	myCanonical, err := canonicalCopyValue([]byte(jsonArray), "bench_area_exceptions", "evidence_refs", copyJSON, true)
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
	insertedFloat, err := copyInsertValue([]byte("1.25"), "bench_scores", "score", copyFloat, true, false)
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
