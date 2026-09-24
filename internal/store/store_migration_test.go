package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRelayEventsV6ToV7Upgrade starts from the v6 relay table rather than a
// fresh v7 database. It proves historic job rows and their five-field conflict
// behavior survive the additive v7 migration.
func TestRelayEventsV6ToV7Upgrade(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL migration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("relay_v7_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `CREATE TABLE relay_events (id BIGSERIAL PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('job.completed','job.escalate','job.joined')), job_id TEXT NOT NULL, epoch INTEGER NOT NULL DEFAULT 0, owner_lane TEXT NOT NULL, machine TEXT NOT NULL DEFAULT '', pane_id TEXT NOT NULL DEFAULT '', report_path TEXT NOT NULL DEFAULT '', report_last_line TEXT NOT NULL DEFAULT '', question TEXT NOT NULL DEFAULT '', pr TEXT NOT NULL DEFAULT '', head TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', event_time TIMESTAMPTZ, received_at TIMESTAMPTZ NOT NULL, delivered_at TIMESTAMPTZ, delivered_to TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `CREATE UNIQUE INDEX relay_events_idempotency ON relay_events(kind, job_id, epoch, report_path, reason)`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO relay_events(kind,job_id,epoch,owner_lane,report_path,reason,received_at) VALUES('job.completed','old-job',1,'lane-a','report.md','',now())`); err != nil {
		t.Fatal(err)
	}

	s := &Store{pool: pool}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var constraintOID uint32
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='relay_events'::regclass AND conname='relay_events_kind_check'`).Scan(&constraintOID); err != nil {
		t.Fatal(err)
	}
	// A second open must see schema version 13 and skip the lock-heavy v7 DDL.
	// The constraint OID would change if it were dropped and re-added again.
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var repeatedConstraintOID uint32
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='relay_events'::regclass AND conname='relay_events_kind_check'`).Scan(&repeatedConstraintOID); err != nil || repeatedConstraintOID != constraintOID {
		t.Fatalf("v7 repeated constraint oid=%d want=%d err=%v", repeatedConstraintOID, constraintOID, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM relay_events WHERE kind='job.completed' AND job_id='old-job'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("historic rows=%d err=%v", count, err)
	}
	job, created, err := s.AppendRelayEvent(ctx, RelayEvent{Kind: "job.completed", JobID: "old-job", Epoch: 1, OwnerLane: "lane-a", ReportPath: "report.md"})
	if err != nil || created || job.Attempts != 1 {
		t.Fatalf("historic idempotency event=%+v created=%t err=%v", job, created, err)
	}
	lane, created, err := s.AppendRelayEvent(ctx, RelayEvent{Kind: "lane.event", OwnerLane: "lane-a", EventID: "producer-1", Text: "payload"})
	if err != nil || !created || lane.ID == 0 {
		t.Fatalf("lane event=%+v created=%t err=%v", lane, created, err)
	}
	t.Logf("v6→v7 upgrade: historic_rows=%d job_attempts=%d lane_event_id=%d", count, job.Attempts, lane.ID)
}

// TestTaskCommentsMigrationIsAdditiveAndIdempotent opens a database that
// already holds tasks, migrates twice, and proves the comment table and its
// append-only trigger exist once without claiming a schema version.
func TestTaskCommentsMigrationIsAdditiveAndIdempotent(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL migration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("task_comments_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, Task{Lane: "lane-a", Title: "existing", Kind: "implement", CreatedBy: "node"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateTaskComment(ctx, task.ID, "node", "before reopen")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var tables, triggers, version int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname=$1 AND tablename='task_comments'`, schema).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("tables=%d err=%v", tables, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname='task_comments_append_only' AND tgrelid='task_comments'::regclass`).Scan(&triggers); err != nil || triggers != 1 {
		t.Fatalf("triggers=%d err=%v", triggers, err)
	}
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	xs, err := s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(xs) != 1 || xs[0].ID != first.ID {
		t.Fatalf("comments=%+v err=%v", xs, err)
	}
}

// TestBenchCatalogV11ToV12Upgrade starts from a v9-shaped bench_grades table
// holding a historic row and proves the v12 migration copies it into
// bench_catalog as the profile-default row, survives a second migrate, and
// leaves later catalog edits untouched when the backfill cannot re-run.
func TestBenchCatalogV11ToV12Upgrade(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL migration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("bench_catalog_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `CREATE TABLE bench_grades (profile TEXT PRIMARY KEY, grade TEXT NOT NULL CHECK(grade IN ('S+','S','A+','A','B','C')), boundary_version TEXT NOT NULL DEFAULT '', deviation_ref TEXT NOT NULL, decided_at TIMESTAMPTZ NOT NULL, decided_by TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO bench_grades(profile,grade,boundary_version,deviation_ref,decided_at,decided_by) VALUES('devin-ds41','A+','2026-09-07','deviation-2026-09-07','2026-09-07T04:30:00Z','mac-personal')`); err != nil {
		t.Fatal(err)
	}

	s := &Store{pool: pool}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var profile, effort, grade, boundaryVersion string
	if err = pool.QueryRow(ctx, `SELECT profile,effort,grade,boundary_version FROM bench_catalog WHERE profile='devin-ds41'`).Scan(&profile, &effort, &grade, &boundaryVersion); err != nil {
		t.Fatal(err)
	}
	if effort != "" || grade != "A+" || boundaryVersion != "2026-09-07" {
		t.Fatalf("migrated catalog row=(%s,%s,%s,%s)", profile, effort, grade, boundaryVersion)
	}
	if _, err = pool.Exec(ctx, `UPDATE bench_catalog SET model_id='devin-ds41-model', pool='devin' WHERE profile='devin-ds41'`); err != nil {
		t.Fatal(err)
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version, rows, modelRows int
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 13 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=12`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 12 rows=%d err=%v", version, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM bench_catalog`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("catalog rows=%d err=%v", rows, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM bench_catalog WHERE profile='devin-ds41' AND model_id='devin-ds41-model'`).Scan(&modelRows); err != nil || modelRows != 1 {
		t.Fatalf("catalog edit preserved=%d err=%v", modelRows, err)
	}
	grades, err := s.ListBenchGrades(ctx)
	if err != nil || len(grades) != 1 || grades[0].Profile != "devin-ds41" || grades[0].Grade != "A+" {
		t.Fatalf("legacy grade projection=%+v err=%v", grades, err)
	}
	t.Logf("v11→v12 upgrade: catalog_rows=%d legacy_projection=%d", rows, len(grades))
}

// TestBenchCatalogEmptyMigration proves the v12 block is a no-op on a database
// that never had bench_grades rows: the table exists, empty, after one migrate.
func TestBenchCatalogEmptyMigration(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL migration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("bench_empty_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM bench_catalog`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("empty catalog rows=%d err=%v", rows, err)
	}
}

// TestTaskEventsDecisionKindV12ToV13Upgrade starts from a v12 database whose
// task_events.kind CHECK admits only transition/relane and holds history,
// and proves v13 admits 'decision' once, keeps every row, and still refuses
// an unknown kind.
func TestTaskEventsDecisionKindV12ToV13Upgrade(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM schema_version WHERE version=13`,
		`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check`,
		`ALTER TABLE task_events ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane'))`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	x, err := s.CreateTask(ctx, Task{Lane: "v13-lane", Title: "history", Kind: "implement", CreatedBy: "v13"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTask(ctx, x.ID, "v13"); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,kind) VALUES($1,'claimed','claimed','v13','',now(),'decision')`, x.ID); err == nil {
		t.Fatal("v12 constraint admitted a decision row")
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version, rows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=13`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 13 rows=%d err=%v", version, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, x.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("history rows=%d err=%v", rows, err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,kind) VALUES($1,'claimed','claimed','v13','',now(),'decision')`, x.ID); err != nil {
		t.Fatalf("v13 refused a decision row: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,kind) VALUES($1,'claimed','claimed','v13','',now(),'bogus')`, x.ID); err == nil {
		t.Fatal("v13 admitted an unknown kind")
	}
	// A second start sees v13 and does not repeat the lock-heavy swap.
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=13`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 13 rows after restart=%d err=%v", version, err)
	}
}
