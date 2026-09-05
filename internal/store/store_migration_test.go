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
