package store

import "testing"

// 20261118-sync-bulk-load-skip: the PostgreSQL emit triggers journal exactly as
// before while handoffkeep.bulk_load is unset or off, and write nothing while
// it is on. The load flag is transaction-local, so the next statement journals
// again. Foreign keys are not part of the switch.
func TestSyncEmitSkipsOnlyWhileBulkLoadIsOn(t *testing.T) {
	s := schemaStore(t, "sync_bulk_load")
	ctx := t.Context()
	count := func(where string) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sync_log WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert := func(setting, title string) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if setting != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('handoffkeep.bulk_load', $1, true)`, setting); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tasks(lane,title,kind,state,refs,created_by,created_at,updated_at) VALUES('bulk','`+title+`','implement','backlog','{"decision_request":{"id":"bulk/`+title+`"}}','test',now(),now())`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	insert("", "unset")
	insert("off", "off")
	// Each unjournaled-or-journaled insert is one task row and one decision row.
	if got := count(`model='task'`); got != 2 {
		t.Fatalf("task feed rows with the flag unset or off = %d, want 2", got)
	}
	if got := count(`model='decision'`); got != 2 {
		t.Fatalf("decision feed rows with the flag unset or off = %d, want 2", got)
	}
	insert("on", "on")
	if got := count(`model='task'`); got != 2 {
		t.Fatalf("task feed rows after a bulk-load insert = %d, want 2 (unchanged)", got)
	}
	if got := count(`model='decision'`); got != 2 {
		t.Fatalf("decision feed rows after a bulk-load insert = %d, want 2 (unchanged)", got)
	}
	var projected int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sync_decisions WHERE id IN ('bulk/unset','bulk/off','bulk/on')`).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if projected != 2 {
		t.Fatalf("sync_decisions rows = %d, want 2: the bulk-load insert must not project its decision", projected)
	}
	insert("", "after")
	if got := count(`model='task'`); got != 3 {
		t.Fatalf("task feed rows after the flag ended = %d, want 3", got)
	}
	var pending int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sync_log WHERE sync_id IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("sync_log rows without a sync_id = %d (%v), want 0", pending, err)
	}
}
