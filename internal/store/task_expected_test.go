package store

import (
	"context"
	"testing"
)

func TestExpectedDurationValidation(t *testing.T) {
	for _, v := range []string{"4h", "1d", "30m", "2h30m"} {
		if !ValidExpected(v) {
			t.Errorf("rejected %q", v)
		}
	}
	for _, v := range []string{"", "0d", "-1h", "1.5d", "999999999999999999d", "garbage"} {
		if ValidExpected(v) {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestTaskExpectedMigrationAndRoundTrip(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM schema_version WHERE version=22; ALTER TABLE tasks DROP COLUMN expected`); err != nil {
		t.Fatal(err)
	}
	id := seedTaskRow(t, pool, "expected-lane", "legacy", "backlog")
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	legacy, found, err := s.GetTask(ctx, id)
	if err != nil || !found || legacy.Expected != nil {
		t.Fatalf("legacy=%+v found=%v err=%v", legacy, found, err)
	}
	value := "1d"
	project := testProjectName
	if _, err := pool.Exec(ctx, `INSERT INTO task_projects(name,created_by,created_at) VALUES($1,'test',now()) ON CONFLICT DO NOTHING`, project); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateTask(ctx, Task{Lane: "expected-lane", Title: "new", Kind: "implement", CreatedBy: "test", Project: &project, Expected: &value})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetTask(ctx, created.ID)
	if err != nil || !found || got.Expected == nil || *got.Expected != value {
		t.Fatalf("got=%+v found=%v err=%v", got, found, err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
