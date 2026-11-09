package store

import (
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestOriginsMigration(t *testing.T) {
	ctx := t.Context()
	pool, _ := testdb.SchemaPool(t, "origins_v41")
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	checkNames := func(where string) []string {
		rows, err := pool.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid='documents'::regclass AND contype='c' AND conkey=ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid='documents'::regclass AND attname='kind')] ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			names = append(names, n)
		}
		if len(names) != 1 || names[0] != "documents_kind_check" {
			t.Fatalf("%s: kind CHECK constraints %v, want exactly documents_kind_check", where, names)
		}
		return names
	}
	checkNames("fresh schema")

	// Rewind v41 to a v40 schema: the old inline-style CHECK (named by
	// Postgres, not by us), no new columns, no task_origins.
	for _, q := range []string{
		`DROP TABLE task_origins`,
		`ALTER TABLE documents DROP CONSTRAINT documents_kind_check`,
		`ALTER TABLE documents DROP COLUMN task_id, DROP COLUMN origin_class`,
		`ALTER TABLE documents ADD CHECK(kind IN ('brief','report','answer','handoff','note','other'))`,
		`DELETE FROM schema_version WHERE version=41`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rewind %q: %v", q, err)
		}
	}
	checkNames("rewound v40 schema")
	insertDoc := `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at) VALUES($1,'note','','','b',$2,$3,now(),now())`
	for key, by := range map[string]string{
		"dots/results/ZZZ-1/2026-11-02-0a1b2c3d": "linear-webhook",
		"dots/results/ZZZ-2/2026-11-02-0a1b2c3e": "someone-else",
		"notes/plain":                            "tester",
	} {
		if _, err := pool.Exec(ctx, insertDoc, key, strings.Repeat("0", 64), by); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("upgrade from v40: %v", err)
	}
	checkNames("upgraded schema")

	var version int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max(version)=%d err=%v, want %d", version, err, SchemaVersion)
	}

	// The backfill marks only the webhook's dots results.
	got := map[string]*string{}
	rows, err := pool.Query(ctx, `SELECT key, origin_class FROM documents`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		var class *string
		if err := rows.Scan(&key, &class); err != nil {
			t.Fatal(err)
		}
		got[key] = class
	}
	rows.Close()
	if c := got["dots/results/ZZZ-1/2026-11-02-0a1b2c3d"]; c == nil || *c != "import" {
		t.Errorf("webhook dots result origin_class=%v, want import", c)
	}
	for _, key := range []string{"dots/results/ZZZ-2/2026-11-02-0a1b2c3e", "notes/plain"} {
		if got[key] != nil {
			t.Errorf("%s origin_class=%v, want NULL (unknown)", key, *got[key])
		}
	}

	// task_origins and its unique key.
	var taskID int64
	if err := pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES('origins-test','t','implement','backlog','test',now(),now()) RETURNING id`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	insertOrigin := `INSERT INTO task_origins(task_id,provider,namespace,external_id,snapshot_at,created_by,created_at) VALUES($1,$2,'ns',$3,now(),'test',now())`
	if _, err := pool.Exec(ctx, insertOrigin, taskID, "linear", originUUID); err != nil {
		t.Fatalf("task_origins insert: %v", err)
	}
	if _, err := pool.Exec(ctx, insertOrigin, taskID, "linear", originUUID); err == nil {
		t.Error("a second row with the same (provider, namespace, external_id, task) was accepted")
	}
	if _, err := pool.Exec(ctx, insertOrigin, taskID, "jira", originUUID); err == nil {
		t.Error("an unknown provider was accepted")
	}
	if _, err := pool.Exec(ctx, insertOrigin, int64(999999999), "linear", originUUID); err == nil {
		t.Error("an origin of a missing task was accepted")
	}
	var idx int
	// Match the index by its key columns in the catalog, not by pg_indexes.indexdef: the view
	// runs pg_get_indexdef on rows of every schema before the filter applies, and a parallel
	// test dropping its own schema made that fail with "could not open relation with OID"
	// (hk 3554).
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index i
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = current_schema() AND t.relname = 'task_origins'
		  AND (SELECT array_agg(a.attname ORDER BY k.ord)
		         FROM unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
		         JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum) = ARRAY['provider','external_id']::name[]`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("(provider, external_id) index count=%d err=%v", idx, err)
	}

	// documents: the widened kind CHECK, task_id and origin_class.
	if _, err := pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at,task_id,origin_class) VALUES('d/design','design','b',$1,'test',now(),now(),$2,'authored')`, strings.Repeat("0", 64), taskID); err != nil {
		t.Errorf("a design document with task_id and origin_class: %v", err)
	}
	for _, kind := range []string{"decision", "research", "policy", "evidence"} {
		if _, err := pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at) VALUES($1,$2,'b',$3,'test',now(),now())`, "d/"+kind, kind, strings.Repeat("0", 64)); err != nil {
			t.Errorf("kind %s refused: %v", kind, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at) VALUES('d/bogus','bogus','b',$1,'test',now(),now())`, strings.Repeat("0", 64)); err == nil {
		t.Error("kind bogus was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at,origin_class) VALUES('d/class','note','b',$1,'test',now(),now(),'robot')`, strings.Repeat("0", 64)); err == nil {
		t.Error("an unknown origin_class was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at,task_id) VALUES('d/task','note','b',$1,'test',now(),now(),999999999)`, strings.Repeat("0", 64)); err == nil {
		t.Error("a document of a missing task was accepted")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND tablename='documents' AND indexname='documents_task'`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("documents_task index count=%d err=%v", idx, err)
	}
}
