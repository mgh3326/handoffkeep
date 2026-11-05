package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSyncMutationJournal(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	task, err := s.CreateTask(ctx, Task{Lane: "sync-test", Title: "sync", Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
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

func TestSyncDecisionDeleteAndTaskRetention(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	task, err := s.CreateTask(ctx, Task{Lane: "sync-delete", Title: "retained task", Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	id := task.ID
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
	// Task history is append-only: the cascading task delete must fail and
	// roll back both source and journal changes. Production closes tasks with
	// terminal transitions rather than hard deletion.
	if _, err = pool.Exec(ctx, `DELETE FROM tasks WHERE id=$1`, id); err == nil || !strings.Contains(err.Error(), "task_events is append-only") {
		t.Fatalf("task delete err=%v, want append-only refusal", err)
	}
	changes, _, err := s.SyncDelta(ctx, before.MaxSyncID, 100)
	if err != nil || len(changes) != 0 {
		t.Fatalf("refused delete leaked journal rows: %v %v", changes, err)
	}
	if _, found, err := s.GetSyncRow(ctx, "decision", "delete-request"); err != nil || !found {
		t.Fatalf("refused delete lost decision: found=%v err=%v", found, err)
	}
	// Removing the current request deletes only its decision projection.
	if _, err = pool.Exec(ctx, `UPDATE tasks SET refs='{}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	changes, afterDelete, err := s.SyncDelta(ctx, before.MaxSyncID, 100)
	if err != nil || len(changes) != 2 {
		t.Fatalf("decision deletion changes=%v err=%v", changes, err)
	}
	seen := map[string]string{}
	for _, c := range changes {
		if seen[c.Model] != "" {
			t.Fatalf("duplicate change %+v", c)
		}
		seen[c.Model] = c.Action
	}
	if seen["task"] != "U" || seen["decision"] != "D" {
		t.Fatalf("changes=%v", changes)
	}
	page, err = s.SyncBootstrap(ctx, "decision", "", 100)
	if err != nil || len(page.Rows) != 0 {
		t.Fatalf("deleted projection=%+v err=%v", page, err)
	}
	dropped, err := s.TransitionTask(ctx, id, "dropped", "test", "no longer needed", nil, "")
	if err != nil || dropped.State != "dropped" {
		t.Fatalf("terminal task=%+v err=%v", dropped, err)
	}
	changes, _, err = s.SyncDelta(ctx, afterDelete.MaxSyncID, 100)
	// The state update and subsequent activity-clock update each emit a task row.
	if err != nil || len(changes) != 2 {
		t.Fatalf("terminal changes=%v err=%v", changes, err)
	}
	for _, change := range changes {
		if change.Model != "task" || change.Action != "U" || change.ModelID != fmt.Sprint(id) {
			t.Fatalf("terminal change=%+v", change)
		}
	}
	if _, found, err := s.GetSyncRow(ctx, "task", fmt.Sprint(id)); err != nil || !found {
		t.Fatalf("terminal task not readable: found=%v err=%v", found, err)
	}
}

func TestSyncAllocationFollowsEntryOrder(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at) SELECT 'sync/order/'||n,'note','body','hash','test',now(),now() FROM generate_series(1,32) AS n`); err != nil {
		t.Fatal(err)
	}
	// Reverse the physical tuple order before allocation so a heap scan and
	// entry_id ordering disagree. All entries remain in this transaction.
	if _, err = tx.Exec(ctx, `DO $$ DECLARE pending RECORD; BEGIN FOR pending IN SELECT entry_id FROM sync_log WHERE txid=pg_current_xact_id() ORDER BY entry_id DESC LOOP UPDATE sync_log SET at=at WHERE entry_id=pending.entry_id; END LOOP; END $$`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT entry_id,sync_id FROM sync_log ORDER BY entry_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var previous int64
	count := 0
	for rows.Next() {
		var entryID, syncID int64
		if err = rows.Scan(&entryID, &syncID); err != nil {
			t.Fatal(err)
		}
		if syncID <= previous {
			t.Fatalf("entry_id=%d sync_id=%d follows sync_id=%d", entryID, syncID, previous)
		}
		previous = syncID
		count++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 32 {
		t.Fatalf("journal rows=%d want=32", count)
	}
	changes, meta, err := s.SyncDelta(ctx, 0, 100)
	if err != nil || len(changes) != count || meta.LastSyncID != previous {
		t.Fatalf("delta=%v metadata=%+v err=%v", changes, meta, err)
	}
}

func TestSyncBackfillDuplicateDecisionIDs(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	// Recreate the pre-sync database state in this isolated schema so the
	// real migration sees duplicate legacy IDs before any sync triggers exist.
	for _, query := range []string{
		`DROP FUNCTION handoffkeep_sync_emit(),handoffkeep_sync_allocate(),handoffkeep_sync_decision() CASCADE`,
		`DROP TABLE sync_log,sync_state,sync_decisions`,
		`DROP SEQUENCE sync_log_sync_id_seq`,
		`DELETE FROM schema_version WHERE version=30`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().UTC().Add(-time.Hour)
	var newest int64
	for i := 0; i < 3; i++ {
		id := seedTaskRow(t, pool, fmt.Sprintf("sync-legacy-%d", i), "legacy decision", "backlog")
		updated := at
		if i == 0 {
			updated = at.Add(-time.Hour)
		}
		if _, err := pool.Exec(ctx, `UPDATE tasks SET refs='{"decision_request":{"id":"shared/request","question":"legacy question"}}',updated_at=$2 WHERE id=$1`, id, updated); err != nil {
			t.Fatal(err)
		}
		newest = id
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var chosen int64
	if err := pool.QueryRow(ctx, `SELECT task_id FROM sync_decisions WHERE id='shared/request'`).Scan(&chosen); err != nil || chosen != newest {
		t.Fatalf("chosen task=%d want=%d err=%v", chosen, newest, err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	if _, found, err := s.GetSyncRow(ctx, "decision", "shared/request"); err != nil || !found {
		t.Fatalf("backfilled row found=%v err=%v", found, err)
	}
}

func TestSyncDecisionOwnershipMovesBetweenTasks(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	first, err := s.CreateTask(ctx, Task{Lane: "sync-owner-first", Title: "first owner", Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTask(ctx, Task{Lane: "sync-owner-second", Title: "second owner", Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(taskID int64, requestID, question string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=jsonb_build_object('decision_request',jsonb_build_object('id',$2::text,'question',$3::text)),updated_at=$4 WHERE id=$1`, taskID, requestID, question, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	claim(first.ID, "shared/request", "first question")
	// The second task already owns a different projection. Replacing it must
	// clear that row before transferring the shared ID's unique task_id.
	claim(second.ID, "second/request", "previous second question")
	claim(second.ID, "shared/request", "second question")
	assertOwner := func() {
		t.Helper()
		raw, found, err := s.GetSyncRow(ctx, "decision", "shared/request")
		if err != nil || !found {
			t.Fatalf("decision found=%v err=%v", found, err)
		}
		var row struct {
			TaskID   int64  `json:"task_id"`
			Question string `json:"question"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if row.TaskID != second.ID || row.Question != "second question" {
			t.Fatalf("wrong decision owner: %+v", row)
		}
	}
	assertOwner()
	if _, found, err := s.GetSyncRow(ctx, "decision", "second/request"); err != nil || found {
		t.Fatalf("previous projection found=%v err=%v", found, err)
	}
	_, before, err := s.SyncDelta(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs='{}' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	assertOwner()
	changes, after, err := s.SyncDelta(ctx, before.LastSyncID, 100)
	if err != nil || len(changes) != 1 || changes[0].Model != "task" || changes[0].Action != "U" {
		t.Fatalf("old owner removal affected shared decision: %v err=%v", changes, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs='{}' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetSyncRow(ctx, "decision", "shared/request"); err != nil || found {
		t.Fatalf("removed owner decision found=%v err=%v", found, err)
	}
	changes, _, err = s.SyncDelta(ctx, after.LastSyncID, 100)
	if err != nil || len(changes) != 2 || changes[0].Model != "decision" || changes[0].ModelID != "shared/request" || changes[0].Action != "D" || changes[1].Model != "task" || changes[1].Action != "U" {
		t.Fatalf("new owner removal changes=%v err=%v", changes, err)
	}
}
