package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSyncMutationJournal(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	task, err := s.CreateTask(ctx, Task{Lane: "sync-test", Title: "sync", Kind: "implement", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateTaskComment(ctx, task.ID, "test", "comment")
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := s.PutDocument(ctx, Document{Key: "sync/doc", Kind: "note", Body: "one", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	doc.Body = "two"
	if _, _, err = s.PutDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE tasks SET refs='{"decision_request":{"id":"request-one","status":"open"}}' WHERE id=$1`, task.ID)
	exec(`UPDATE tasks SET refs='{"decision_request":{"id":"request-one","status":"answered"}}' WHERE id=$1`, task.ID)
	exec(`UPDATE tasks SET refs='{"decision_request":{"id":"request-two","status":"open"}}' WHERE id=$1`, task.ID)
	exec(`DELETE FROM documents WHERE id=$1`, doc.ID)
	changes, meta, err := s.SyncDelta(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"task:I", "task_comment:I", "document:I", "document:U", "decision:I", "task:U", "decision:U", "task:U", "decision:D", "decision:I", "task:U", "document:D"}
	if len(changes) != len(want) {
		t.Fatalf("changes=%+v want=%v", changes, want)
	}
	for i, c := range changes {
		if c.Model+":"+c.Action != want[i] || c.SyncID <= 0 || c.At.IsZero() {
			t.Fatalf("change[%d]=%+v", i, c)
		}
		if i > 0 && c.SyncID <= changes[i-1].SyncID {
			t.Fatal("unordered")
		}
	}
	if meta.LastSyncID != meta.MaxSyncID {
		t.Fatal(meta)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE tasks SET title='rolled back' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	// Another connection must see neither the data nor the feed watermark.
	visible, m, err := s.SyncDelta(ctx, meta.MaxSyncID, 100)
	if err != nil || len(visible) != 0 || m.MaxSyncID != meta.MaxSyncID {
		t.Fatalf("uncommitted visible: %v %+v %v", visible, m, err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	visible, _, err = s.SyncDelta(ctx, meta.MaxSyncID, 100)
	if err != nil || len(visible) != 0 {
		t.Fatalf("rollback leaked: %v %v", visible, err)
	}
	// The existing append-only comment rule remains effective.
	if _, err = pool.Exec(ctx, `DELETE FROM task_comments WHERE task_id=$1`, task.ID); err == nil {
		t.Fatal("comment delete accepted")
	}
}

func TestSyncBootstrapDeltaConverges(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		if _, _, err := s.PutDocument(ctx, Document{Key: fmt.Sprintf("sync/%d", i), Kind: "note", Body: "body", CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.SyncBootstrap(ctx, "document", "", 1)
	if err != nil || first.Cursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	ids := map[string]bool{first.Rows[0].ID: true}
	if _, err = pool.Exec(ctx, `DELETE FROM documents WHERE id=$1::bigint`, first.Rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PutDocument(ctx, Document{Key: "sync/new", Kind: "note", Body: "new", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	cursor := first.Cursor
	for cursor != "" {
		page, err := s.SyncBootstrap(ctx, "document", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if page.MaxSyncID != first.MaxSyncID {
			t.Fatal("watermark changed")
		}
		for _, r := range page.Rows {
			ids[r.ID] = true
		}
		cursor = page.Cursor
	}
	after := first.MaxSyncID
	for {
		changes, meta, err := s.SyncDelta(ctx, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range changes {
			if c.Action == "D" {
				delete(ids, c.ModelID)
			} else {
				ids[c.ModelID] = true
			}
		}
		after = meta.LastSyncID
		if after == meta.MaxSyncID {
			break
		}
	}
	fresh, err := s.SyncBootstrap(ctx, "document", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(fresh.Rows) {
		t.Fatalf("ids=%v fresh=%v", ids, fresh.Rows)
	}
	for _, r := range fresh.Rows {
		if !ids[r.ID] {
			t.Fatalf("missing %s", r.ID)
		}
	}
	for _, model := range []string{"task", "task_comment", "decision"} {
		if _, err = s.SyncBootstrap(ctx, model, "", 100); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.SyncBootstrap(ctx, "task", first.Cursor, 1); !errors.Is(err, ErrInvalidSync) {
		t.Fatalf("cross-model cursor: %v", err)
	}
}

func TestSyncPruneReset(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	for i := 0; i < 2; i++ {
		if _, _, err := s.PutDocument(ctx, Document{Key: fmt.Sprintf("sync/%d", i), Kind: "note", Body: "body", CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	_, before, err := s.SyncDelta(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE sync_log SET at=now()-interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneSync(ctx, 30*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("prune=%d %v", n, err)
	}
	_, _, err = s.SyncDelta(ctx, 0, 100)
	var reset *SyncReset
	if !errors.As(err, &reset) || reset.MinSyncID != before.MaxSyncID+1 {
		t.Fatalf("reset=%+v err=%v", reset, err)
	}
	// An up-to-date client stays valid even when every log row was pruned.
	_, meta, err := s.SyncDelta(ctx, before.MaxSyncID, 100)
	if err != nil || meta.MaxSyncID != before.MaxSyncID {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	if _, _, err = s.PutDocument(ctx, Document{Key: "sync/next", Kind: "note", Body: "new", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	changes, _, err := s.SyncDelta(ctx, before.MaxSyncID, 100)
	if err != nil || len(changes) != 1 {
		t.Fatalf("next=%v %v", changes, err)
	}
}

func TestSyncCommitOrder(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	long, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer long.Rollback(ctx)
	insert := `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at) VALUES($1,'note','body','hash','test',now(),now()) RETURNING id`
	var firstID, secondID, lastID int64
	if err = long.QueryRow(ctx, insert, "sync/long-first").Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err = long.QueryRow(ctx, `SELECT count(*) FROM sync_log WHERE txid=pg_current_xact_id() AND sync_id IS NULL`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	// The older transaction has already written a source row and its journal.
	// A second writer must finish, including commit-time allocation, while it
	// remains open. Timeouts make an accidental early global lock fail fast.
	short, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer short.Rollback(ctx)
	if _, err = short.Exec(ctx, `SET LOCAL lock_timeout='1s'; SET LOCAL statement_timeout='5s'`); err != nil {
		t.Fatal(err)
	}
	if err = short.QueryRow(ctx, insert, "sync/short").Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	if err = short.Commit(ctx); err != nil {
		t.Fatalf("long transaction blocked unrelated commit: %v", err)
	}
	changes, meta, err := s.SyncDelta(ctx, 0, 100)
	if err != nil || len(changes) != 1 || changes[0].ModelID != fmt.Sprint(secondID) || meta.LastSyncID != meta.MaxSyncID {
		t.Fatalf("before long commit: %v %+v %v", changes, meta, err)
	}
	bootstrap, err := s.SyncBootstrap(ctx, "document", "", 100)
	if err != nil || bootstrap.MaxSyncID != meta.MaxSyncID || len(bootstrap.Rows) != 1 {
		t.Fatalf("bootstrap=%+v err=%v", bootstrap, err)
	}
	if err = long.QueryRow(ctx, insert, "sync/long-last").Scan(&lastID); err != nil {
		t.Fatal(err)
	}
	if err = long.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Resume from the earlier trailer: neither pending row may be skipped.
	changes, final, err := s.SyncDelta(ctx, meta.LastSyncID, 100)
	if err != nil || len(changes) != 2 || changes[0].ModelID != fmt.Sprint(firstID) || changes[1].ModelID != fmt.Sprint(lastID) || changes[0].SyncID <= meta.MaxSyncID || changes[1].SyncID <= changes[0].SyncID || final.LastSyncID != final.MaxSyncID {
		t.Fatalf("after long commit: %v %+v %v", changes, final, err)
	}
	var nullCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM sync_log WHERE sync_id IS NULL`).Scan(&nullCount); err != nil || nullCount != 0 {
		t.Fatalf("committed pending rows=%d err=%v", nullCount, err)
	}
}

func TestSyncPrunePendingRows(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at) VALUES('sync/pending','note','body','hash','test',now(),now())`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE sync_log SET at=now()-interval '31 days' WHERE txid=pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneSync(ctx, time.Nanosecond); err != nil || n != 0 {
		t.Fatalf("prune pending=%d err=%v", n, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	changes, meta, err := s.SyncDelta(ctx, 0, 100)
	if err != nil || len(changes) != 1 || meta.LastSyncID != meta.MaxSyncID {
		t.Fatalf("pending lost: %v %+v %v", changes, meta, err)
	}
}

func TestSyncModelDeleteAndDecisionRollback(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	id := seedTaskRow(t, pool, "sync-delete", "delete", "backlog")
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs='{"decision_request":{"id":"delete-request","status":"open"}}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, before, err := s.SyncDelta(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE tasks SET refs='{}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := s.SyncBootstrap(ctx, "decision", "", 100)
	if err != nil || len(page.Rows) != 1 || page.Rows[0].ID != "delete-request" {
		t.Fatalf("projection rollback=%+v err=%v", page, err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM tasks WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	changes, _, err := s.SyncDelta(ctx, before.MaxSyncID, 100)
	if err != nil || len(changes) != 2 {
		t.Fatalf("delete changes=%v err=%v", changes, err)
	}
	seen := map[string]bool{}
	for _, c := range changes {
		if c.Action != "D" || seen[c.Model] {
			t.Fatalf("unexpected delete %+v", c)
		}
		seen[c.Model] = true
	}
	if !seen["task"] || !seen["decision"] {
		t.Fatal(seen)
	}
	page, err = s.SyncBootstrap(ctx, "decision", "", 100)
	if err != nil || len(page.Rows) != 0 {
		t.Fatalf("deleted projection=%+v err=%v", page, err)
	}
}
