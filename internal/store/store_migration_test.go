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
// fresh database. It proves historic job rows and their five-field conflict
// behavior survive every later additive relay migration.
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
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 14 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var constraintOID uint32
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='relay_events'::regclass AND conname='relay_events_kind_check'`).Scan(&constraintOID); err != nil {
		t.Fatal(err)
	}
	// A second open must see schema version 14 and skip the lock-heavy relay DDL.
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
	t.Logf("v6→v13 upgrade: historic_rows=%d job_attempts=%d lane_event_id=%d", count, job.Attempts, lane.ID)
}

func TestRelayEventsV12ToV13Upgrade(t *testing.T) {
	// A one-sided edit to either partial-index predicate can still appear to
	// work because PostgreSQL may infer a broader partial index from a
	// narrower ON CONFLICT target. Require exact equality in this duplicate
	// resend test so either one-sided mutant turns it red.
	if relayJobIndexPredicate != relayJobConflictPredicate {
		t.Fatalf("index predicate %q differs from ON CONFLICT predicate %q", relayJobIndexPredicate, relayJobConflictPredicate)
	}
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
	schema := fmt.Sprintf("relay_v13_%d", time.Now().UnixNano())
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
	// Restore the v12 relay DDL and marker in this throwaway schema, with old
	// job and lane rows present before invoking the actual v13 migration.
	for _, q := range []string{
		`INSERT INTO relay_events(kind,job_id,epoch,owner_lane,report_path,received_at) VALUES('job.completed','historic-job',1,'lane-a','report.md',now())`,
		`INSERT INTO relay_events(kind,job_id,epoch,owner_lane,event_id,text,received_at) VALUES('lane.event','',0,'lane-a','historic-lane','payload',now())`,
		`DELETE FROM schema_version WHERE version=13`,
		`ALTER TABLE relay_events DROP CONSTRAINT relay_events_kind_check`,
		`ALTER TABLE relay_events ADD CONSTRAINT relay_events_kind_check CHECK(kind IN ('job.completed','job.escalate','job.joined','lane.event'))`,
		`DROP INDEX relay_events_idempotency`,
		`CREATE UNIQUE INDEX relay_events_idempotency ON relay_events(kind,job_id,epoch,report_path,reason) WHERE kind IN ('job.completed','job.escalate','job.joined')`,
	} {
		if _, err = pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version, historic int
	// v13 re-ran and recorded itself once; max is 14 (#618) so assert the row.
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=13`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 13 rows=%d err=%v", version, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM relay_events WHERE job_id='historic-job' OR event_id='historic-lane'`).Scan(&historic); err != nil || historic != 2 {
		t.Fatalf("historic rows=%d err=%v", historic, err)
	}
	for _, kind := range []string{"job.lost", "job.revoked"} {
		x := RelayEvent{Kind: kind, JobID: kind, Epoch: 1, OwnerLane: "lane-a", Reason: "test", EventID: "00001-" + kind + ".json"}
		first, created, err := s.AppendRelayEvent(ctx, x)
		if err != nil || !created {
			t.Fatalf("%s insert=%+v created=%t err=%v", kind, first, created, err)
		}
		second, created, err := s.AppendRelayEvent(ctx, x)
		if err != nil || created || second.ID != first.ID {
			t.Fatalf("%s resend=%+v created=%t err=%v", kind, second, created, err)
		}
	}
	var constraintOID, indexOID, repeatedConstraintOID, repeatedIndexOID uint32
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='relay_events'::regclass AND conname='relay_events_kind_check'`).Scan(&constraintOID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT indexrelid FROM pg_index WHERE indexrelid='relay_events_idempotency'::regclass`).Scan(&indexOID); err != nil {
		t.Fatal(err)
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='relay_events'::regclass AND conname='relay_events_kind_check'`).Scan(&repeatedConstraintOID); err != nil || repeatedConstraintOID != constraintOID {
		t.Fatalf("repeat constraint oid=%d want=%d err=%v", repeatedConstraintOID, constraintOID, err)
	}
	if err = pool.QueryRow(ctx, `SELECT indexrelid FROM pg_index WHERE indexrelid='relay_events_idempotency'::regclass`).Scan(&repeatedIndexOID); err != nil || repeatedIndexOID != indexOID {
		t.Fatalf("repeat index oid=%d want=%d err=%v", repeatedIndexOID, indexOID, err)
	}
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
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 14 {
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
	if err = pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 14 {
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

// TestTaskEventsDecisionKindUpgradeToV14 starts from a database whose
// task_events.kind CHECK admits only transition/relane and holds history,
// and proves v14 admits 'decision' once, keeps every row, and still refuses
// an unknown kind.
func TestTaskEventsDecisionKindUpgradeToV14(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM schema_version WHERE version=14`,
		`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check`,
		`ALTER TABLE task_events ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane'))`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	x, err := s.CreateTask(ctx, Task{Lane: "v14-lane", Title: "history", Kind: "implement", CreatedBy: "v14"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTask(ctx, x.ID, "v14"); err != nil {
		t.Fatal(err)
	}
	insertDecision := func(kind string) error {
		_, err := pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,kind) VALUES($1,'claimed','claimed','v14','',now(),$2)`, x.ID, kind)
		return err
	}
	if insertDecision("decision") == nil {
		t.Fatal("pre-v14 constraint admitted a decision row")
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version, rows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=14`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 14 rows=%d err=%v", version, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1`, x.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("history rows=%d err=%v", rows, err)
	}
	if err = insertDecision("decision"); err != nil {
		t.Fatalf("v14 refused a decision row: %v", err)
	}
	if insertDecision("bogus") == nil {
		t.Fatal("v14 admitted an unknown kind")
	}
	// A second start sees v14 and does not repeat the lock-heavy swap.
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=14`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 14 rows after restart=%d err=%v", version, err)
	}
}

// TestV13ThenV14BothChecksHold is the deploy-order case for #627 (#47) and
// #618: a database that already recorded version 13 — #627's relay_events
// CHECK admitting job.lost/job.revoked, applied here as a stub of that shape
// — must still run this package's v14. Had #618 also used 13, the version
// gate would skip its DDL and task_events would refuse 'decision'. The test
// holds whether or not #627's own v13 code is present in this tree.
func TestV13ThenV14BothChecksHold(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for _, q := range []string{
		// Back to a v12 shape for both tables.
		`DELETE FROM schema_version WHERE version IN (13,14)`,
		`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check`,
		`ALTER TABLE task_events ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane'))`,
		`ALTER TABLE relay_events DROP CONSTRAINT relay_events_kind_check`,
		`ALTER TABLE relay_events ADD CONSTRAINT relay_events_kind_check CHECK(kind IN ('job.completed','job.escalate','job.joined','lane.event'))`,
		// #627's v13 (stub of its CHECK swap), deployed first.
		`ALTER TABLE relay_events DROP CONSTRAINT relay_events_kind_check`,
		`ALTER TABLE relay_events ADD CONSTRAINT relay_events_kind_check CHECK(kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked','lane.event'))`,
		`INSERT INTO schema_version(version) VALUES (13)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	x, err := s.CreateTask(ctx, Task{Lane: "v13v14-lane", Title: "stacked", Kind: "implement", CreatedBy: "v14"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,kind) VALUES($1,'backlog','backlog','v14','',now(),'decision')`, x.ID); err == nil {
		t.Fatal("decision row admitted before v14 ran")
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var v13, v14 int
	if err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE version=13), count(*) FILTER (WHERE version=14) FROM schema_version`).Scan(&v13, &v14); err != nil || v13 != 1 || v14 != 1 {
		t.Fatalf("versions 13=%d 14=%d err=%v", v13, v14, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,kind) VALUES($1,'backlog','backlog','v14','',now(),'decision')`, x.ID); err != nil {
		t.Fatalf("task_events refused 'decision' after v13 then v14: %v", err)
	}
	for _, kind := range []string{"job.lost", "job.revoked"} {
		if _, err := pool.Exec(ctx, `INSERT INTO relay_events(kind,job_id,owner_lane,received_at) VALUES($1,$2,'v14-lane',now())`, kind, "v13v14-"+kind); err != nil {
			t.Fatalf("relay_events refused %s after v14 ran: %v", kind, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO relay_events(kind,job_id,owner_lane,received_at) VALUES('job.bogus','x','v14-lane',now())`); err == nil {
		t.Fatal("relay_events admitted an unknown kind")
	}
}
