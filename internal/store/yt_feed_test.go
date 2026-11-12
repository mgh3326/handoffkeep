package store

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// hk 3997: the change-feed tracker reads sync_log through one cursor per
// consumer and hands each consumer its changes once, in sync_id order.

func TestCollapseFeedRowsKeepsLastInOrder(t *testing.T) {
	rows := []FeedRow{
		{SyncID: 1, Model: "task", ModelID: "7", Action: "I"},
		{SyncID: 2, Model: "document", ModelID: "7", Action: "I"},
		{SyncID: 3, Model: "task", ModelID: "7", Action: "U"},
		{SyncID: 4, Model: "task", ModelID: "8", Action: "U"},
		{SyncID: 5, Model: "task", ModelID: "7", Action: "D"},
	}
	var got []int64
	for _, r := range collapseFeedRows(rows) {
		got = append(got, r.SyncID)
	}
	if want := []int64{2, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("collapsed sync ids = %v, want %v", got, want)
	}
}

func TestYTFeedConsumerRegistration(t *testing.T) {
	h := func(context.Context, pgx.Tx, []FeedRow) error { return nil }
	for _, tc := range []struct {
		name   string
		models []string
		handle FeedHandler
	}{
		{"Bad-Name", []string{"task"}, h},
		{"fields", nil, h},
		{"fields", []string{"tasks"}, h},
		{"fields", []string{"task", "task"}, h},
		{"fields", []string{"task"}, nil},
	} {
		if _, err := newYTFeedConsumer(tc.name, tc.models, tc.handle); err == nil {
			t.Fatalf("newYTFeedConsumer(%q, %v) accepted", tc.name, tc.models)
		}
	}
	if _, err := newYTFeedConsumer("task_fields", []string{"task", "document", "task_comment", "decision"}, h); err != nil {
		t.Fatal(err)
	}
	if got := SnapshotKey("task_fields", "42", 3, "abc"); got != "task_fields:42:3:abc" {
		t.Fatalf("SnapshotKey = %q", got)
	}
}

// ytFeedRecorder is a test consumer that records every batch it is given
// and fails while fail is set.
type ytFeedRecorder struct {
	batches [][]FeedRow
	fail    error
}

func (r *ytFeedRecorder) consumer(t *testing.T, name string, models ...string) ytFeedConsumer {
	t.Helper()
	c, err := newYTFeedConsumer(name, models, func(_ context.Context, _ pgx.Tx, rows []FeedRow) error {
		r.batches = append(r.batches, append([]FeedRow(nil), rows...))
		return r.fail
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ytFeedPassOK(t *testing.T, s *Store, c ytFeedConsumer) YTFeedPass {
	t.Helper()
	pass, err := s.runYTFeedConsumer(t.Context(), c, 100, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if pass.Busy {
		t.Fatal("feed lock busy")
	}
	return pass
}

func ytFeedMaxSyncID(t *testing.T, s *Store) int64 {
	t.Helper()
	var max int64
	if err := s.pool.QueryRow(t.Context(), `SELECT max_sync_id FROM sync_state WHERE singleton`).Scan(&max); err != nil {
		t.Fatal(err)
	}
	return max
}

func ytFeedCursor(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	var cur int64
	if err := s.pool.QueryRow(t.Context(), `SELECT last_sync_id FROM yt_feed_cursors WHERE consumer=$1`, name).Scan(&cur); err != nil {
		t.Fatal(err)
	}
	return cur
}

func ytFeedTask(t *testing.T, s *Store, title string) Task {
	t.Helper()
	task, err := s.CreateTask(t.Context(), Task{Lane: "yt-feed-test", Title: title, Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func ytFeedExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestYTFeedDeliversChangesOnceInOrder(t *testing.T) {
	s := schemaStore(t, "yt_feed_deliver")
	ctx := t.Context()
	old := ytFeedTask(t, s, "history")
	rec := &ytFeedRecorder{}
	c := rec.consumer(t, "test_all", "task", "document", "decision")

	// The first pass starts at max_sync_id: history is not the feed's.
	first := ytFeedPassOK(t, s, c)
	if !first.Created || first.Cursor != ytFeedMaxSyncID(t, s) || first.Cursor == 0 || len(rec.batches) != 0 {
		t.Fatalf("first pass = %+v, batches %v", first, rec.batches)
	}

	ytFeedExec(t, s, `UPDATE tasks SET title='renamed' WHERE id=$1`, old.ID)
	doc, _, err := s.PutDocument(ctx, Document{Key: "yt-feed/doc", Kind: "note", Body: "one", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ytFeedExec(t, s, `UPDATE tasks SET refs='{"decision_request":{"id":"feed-request","status":"open"}}' WHERE id=$1`, old.ID)
	// A comment is a model this consumer did not ask for.
	if _, err := s.CreateTaskComment(ctx, old.ID, "test", "not wanted"); err != nil {
		t.Fatal(err)
	}

	pass := ytFeedPassOK(t, s, c)
	if len(rec.batches) != 1 {
		t.Fatalf("batches = %v", rec.batches)
	}
	got := map[string]int{}
	batch := rec.batches[0]
	for i, r := range batch {
		got[r.Model+":"+r.ModelID]++
		if i > 0 && r.SyncID <= batch[i-1].SyncID {
			t.Fatalf("batch out of sync_id order: %+v", batch)
		}
	}
	want := map[string]int{"task:" + strconv.FormatInt(old.ID, 10): 1, "document:" + strconv.FormatInt(doc.ID, 10): 1, "decision:feed-request": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered %v, want %v (batch %+v)", got, want, batch)
	}
	// The task changed twice; the batch holds its last row.
	if pass.Read < 4 || pass.Handled != 3 {
		t.Fatalf("pass = %+v", pass)
	}
	// A short page moves the cursor to max_sync_id, past the comment rows
	// this consumer does not read.
	if max := ytFeedMaxSyncID(t, s); pass.Cursor != ytFeedCursor(t, s, "test_all") || pass.Cursor != max || max <= batch[len(batch)-1].SyncID {
		t.Fatalf("cursor = %d, stored %d, max_sync_id %d, last row %d", pass.Cursor, ytFeedCursor(t, s, "test_all"), max, batch[len(batch)-1].SyncID)
	}

	// Nothing new: no handler call, cursor unchanged.
	again := ytFeedPassOK(t, s, c)
	if len(rec.batches) != 1 || again.Read != 0 || again.Cursor != pass.Cursor {
		t.Fatalf("idle pass = %+v, batches %d", again, len(rec.batches))
	}
}

// An idle consumer keeps up with changes to models it does not read, so a
// prune of those rows is not a gap (mergewake finding on #315).
func TestYTFeedIdleConsumerFollowsPruneBoundary(t *testing.T) {
	s := schemaStore(t, "yt_feed_idle")
	task := ytFeedTask(t, s, "one")
	rec := &ytFeedRecorder{}
	c := rec.consumer(t, "test_docs", "document")
	ytFeedPassOK(t, s, c)
	for _, title := range []string{"two", "three"} {
		ytFeedExec(t, s, `UPDATE tasks SET title=$2 WHERE id=$1`, task.ID, title)
	}
	max := ytFeedMaxSyncID(t, s)
	if pass := ytFeedPassOK(t, s, c); pass.Read != 0 || pass.Cursor != max || ytFeedCursor(t, s, "test_docs") != max || len(rec.batches) != 0 {
		t.Fatalf("idle pass = %+v, stored %d, max_sync_id %d", pass, ytFeedCursor(t, s, "test_docs"), max)
	}
	ytFeedExec(t, s, `DELETE FROM sync_log WHERE sync_id<=$1`, max)
	ytFeedExec(t, s, `UPDATE sync_state SET pruned_through=$1 WHERE singleton`, max)
	doc, _, err := s.PutDocument(t.Context(), Document{Key: "yt-feed/idle", Kind: "note", Body: "one", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	pass := ytFeedPassOK(t, s, c)
	if pass.Pruned || len(rec.batches) != 1 || len(rec.batches[0]) != 1 || rec.batches[0][0].ModelID != strconv.FormatInt(doc.ID, 10) {
		t.Fatalf("pass after prune = %+v, batches %+v", pass, rec.batches)
	}
}

// A full page stops at its last row, even when max_sync_id is beyond it.
func TestYTFeedFullPageStopsAtLastRow(t *testing.T) {
	s := schemaStore(t, "yt_feed_page")
	a, b := ytFeedTask(t, s, "a"), ytFeedTask(t, s, "b")
	rec := &ytFeedRecorder{}
	c := rec.consumer(t, "test_page", "task")
	ytFeedPassOK(t, s, c)
	ytFeedExec(t, s, `UPDATE tasks SET title='a2' WHERE id=$1`, a.ID)
	ytFeedExec(t, s, `UPDATE tasks SET title='b2' WHERE id=$1`, b.ID)
	pass, err := s.runYTFeedConsumer(t.Context(), c, 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if pass.Read != 1 || pass.Cursor != rec.batches[0][0].SyncID || pass.Cursor >= ytFeedMaxSyncID(t, s) {
		t.Fatalf("full page = %+v, max_sync_id %d", pass, ytFeedMaxSyncID(t, s))
	}
	next, err := s.runYTFeedConsumer(t.Context(), c, 1, time.Now().UTC())
	if err != nil || next.Read != 1 || rec.batches[1][0].ModelID != strconv.FormatInt(b.ID, 10) {
		t.Fatalf("second page = %+v, %v, batches %+v", next, err, rec.batches)
	}
}

func TestYTFeedCollapsesRepeatedUpdates(t *testing.T) {
	s := schemaStore(t, "yt_feed_collapse")
	task := ytFeedTask(t, s, "one")
	rec := &ytFeedRecorder{}
	c := rec.consumer(t, "test_tasks", "task")
	ytFeedPassOK(t, s, c)
	for _, title := range []string{"two", "three", "four"} {
		ytFeedExec(t, s, `UPDATE tasks SET title=$2 WHERE id=$1`, task.ID, title)
	}
	pass := ytFeedPassOK(t, s, c)
	if len(rec.batches) != 1 || len(rec.batches[0]) != 1 || pass.Read != 3 || pass.Handled != 1 {
		t.Fatalf("pass = %+v, batches %+v", pass, rec.batches)
	}
	if r := rec.batches[0][0]; r.Model != "task" || r.ModelID != strconv.FormatInt(task.ID, 10) || r.Action != "U" || r.SyncID != ytFeedMaxSyncID(t, s) {
		t.Fatalf("collapsed row = %+v, max_sync_id %d", r, ytFeedMaxSyncID(t, s))
	}
}

func TestYTFeedHandlerErrorKeepsCursor(t *testing.T) {
	s := schemaStore(t, "yt_feed_error")
	task := ytFeedTask(t, s, "one")
	rec := &ytFeedRecorder{}
	c := rec.consumer(t, "test_error", "task")
	start := ytFeedPassOK(t, s, c).Cursor
	ytFeedExec(t, s, `UPDATE tasks SET title='two' WHERE id=$1`, task.ID)

	rec.fail = errors.New("boom")
	if _, err := s.runYTFeedConsumer(t.Context(), c, 100, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failing pass err = %v", err)
	}
	if cur := ytFeedCursor(t, s, "test_error"); cur != start {
		t.Fatalf("cursor moved to %d after a handler error, want %d", cur, start)
	}
	rec.fail = nil
	pass := ytFeedPassOK(t, s, c)
	if len(rec.batches) != 2 || !reflect.DeepEqual(rec.batches[0], rec.batches[1]) || pass.Cursor <= start {
		t.Fatalf("retry pass = %+v, batches %+v", pass, rec.batches)
	}

	// A handler error also rolls back what the handler wrote.
	wrote, err := newYTFeedConsumer("test_rollback", []string{"task"}, func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error {
		if _, err := tx.Exec(ctx, `INSERT INTO yt_snapshot_state(entity_kind,entity_id,kind,seq,last_sha,last_op_id) VALUES('task','1','test',1,'x',1)`); err != nil {
			return err
		}
		return errors.New("after write")
	})
	if err != nil {
		t.Fatal(err)
	}
	ytFeedPassOK(t, s, wrote)
	ytFeedExec(t, s, `UPDATE tasks SET title='three' WHERE id=$1`, task.ID)
	if _, err := s.runYTFeedConsumer(t.Context(), wrote, 100, time.Now().UTC()); err == nil {
		t.Fatal("pass succeeded")
	}
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM yt_snapshot_state`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("snapshot rows after a failed pass = %d, %v", n, err)
	}
}

func TestYTFeedPrunedBeyondCursorReadsNothing(t *testing.T) {
	s := schemaStore(t, "yt_feed_pruned")
	task := ytFeedTask(t, s, "one")
	rec := &ytFeedRecorder{}
	c := rec.consumer(t, "test_pruned", "task")
	start := ytFeedPassOK(t, s, c).Cursor
	for _, title := range []string{"two", "three"} {
		ytFeedExec(t, s, `UPDATE tasks SET title=$2 WHERE id=$1`, task.ID, title)
	}
	through := ytFeedMaxSyncID(t, s)
	ytFeedExec(t, s, `UPDATE sync_state SET pruned_through=$1 WHERE singleton`, through)
	logs := ytbLog(t)

	for i := 0; i < 2; i++ {
		pass := ytFeedPassOK(t, s, c)
		if !pass.Pruned || pass.PrunedThrough != through || pass.Read != 0 || pass.Cursor != start {
			t.Fatalf("pass %d = %+v", i, pass)
		}
	}
	if len(rec.batches) != 0 || ytFeedCursor(t, s, "test_pruned") != start {
		t.Fatalf("pruned feed read %v, cursor %d", rec.batches, ytFeedCursor(t, s, "test_pruned"))
	}
	line := "ytwrite.feed_pruned consumer=\"test_pruned\" cursor=" + strconv.FormatInt(start, 10) + " pruned_through=" + strconv.FormatInt(through, 10)
	if got := strings.Count(logs(), line); got != 2 {
		t.Fatalf("log has %d %q lines, want one per pass:\n%s", got, line, logs())
	}
	var alerts int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM notification_outbox WHERE kind=$1 AND event_id=$2`,
		OutboxKindYTWriteOp, "yt-feed-test_pruned-pruned-"+strconv.FormatInt(start, 10)).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("pruned alerts = %d, %v", alerts, err)
	}
}

// Production registers the task_fields consumer (hk 4003), the task_docs
// consumer (hk 4xxx) and the decision_issues consumer (hk 4054) and nothing
// else; with their modes off a pass creates their cursors and prepares
// nothing.
func TestRunYTFeedConsumersRegistered(t *testing.T) {
	if len(ytFeedConsumers) != 3 || ytFeedConsumers[ytFieldsConsumer].build == nil || ytFeedConsumers[ytDocCommentsConsumer].build == nil ||
		ytFeedConsumers[ytDecisionsConsumer].build == nil {
		t.Fatalf("production feed consumers: %v", ytFeedConsumers)
	}
	passOf := func(passes []YTFeedPass, name string) (YTFeedPass, bool) {
		for _, p := range passes {
			if p.Consumer == name {
				return p, true
			}
		}
		return YTFeedPass{}, false
	}
	s := schemaStore(t, "yt_feed_registered")
	task := ytFeedTask(t, s, "one")
	passes, err := s.RunYTFeedConsumers(t.Context(), 100)
	if err != nil || len(passes) != 3 {
		t.Fatalf("passes = %+v, %v", passes, err)
	}
	for _, name := range []string{ytFieldsConsumer, ytDocCommentsConsumer, ytDecisionsConsumer} {
		if p, ok := passOf(passes, name); !ok || !p.Created {
			t.Fatalf("first pass of %s = %+v, %v", name, p, ok)
		}
	}
	ytFeedExec(t, s, `UPDATE tasks SET title='two' WHERE id=$1`, task.ID)
	passes, err = s.RunYTFeedConsumers(t.Context(), 100)
	if err != nil || len(passes) != 3 {
		t.Fatalf("passes = %+v, %v", passes, err)
	}
	if p, ok := passOf(passes, ytFieldsConsumer); !ok || p.Handled != 1 {
		t.Fatalf("fields pass = %+v, %v", p, ok)
	}
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM yt_write_ops`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ops = %d, %v", n, err)
	}
}

func TestPrepareYTSnapshotSkipsIdenticalSHA(t *testing.T) {
	s := schemaStore(t, "yt_feed_snapshot")
	ctx := t.Context()
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	prepare := func(sha string) (ytwrite.Op, bool) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		op, prepared, err := prepareYTSnapshotTx(ctx, tx, now, ytwrite.NewOp{Principal: "p", Kind: "task_fields", PayloadSHA: sha,
			Payload: []byte(`{"sha":"` + sha + `"}`), EntityKind: "task", EntityID: "42", YTTargetID: "2-42", Actor: "a"}, sha)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return op, prepared
	}
	state := func() (int64, string, int64) {
		t.Helper()
		var seq, id int64
		var sha string
		if err := s.pool.QueryRow(ctx, `SELECT seq,last_sha,last_op_id FROM yt_snapshot_state WHERE entity_kind='task' AND entity_id='42' AND kind='task_fields'`).Scan(&seq, &sha, &id); err != nil {
			t.Fatal(err)
		}
		return seq, sha, id
	}
	a, ok := prepare("aaa")
	if !ok || a.Key != "task_fields:42:1:aaa" || a.State != ytwrite.StatePrepared {
		t.Fatalf("first prepare = %+v, %t", a, ok)
	}
	if seq, sha, id := state(); seq != 1 || sha != "aaa" || id != a.ID {
		t.Fatalf("state = %d/%s/%d, want 1/aaa/%d", seq, sha, id, a.ID)
	}
	// An identical sha (a no-op update) makes no op and leaves the state.
	if _, ok := prepare("aaa"); ok {
		t.Fatal("an identical sha prepared an op")
	}
	if seq, _, _ := state(); seq != 1 {
		t.Fatalf("seq after a skipped prepare = %d, want 1", seq)
	}
	b, ok := prepare("bbb")
	if !ok || b.ID == a.ID || b.Key != "task_fields:42:2:bbb" {
		t.Fatalf("changed sha prepare = %+v, %t", b, ok)
	}
	// A -> B -> A: the revert is a new op that renders A (hk 3997 ESC reply B).
	back, ok := prepare("aaa")
	if !ok || back.ID == a.ID || back.ID == b.ID || back.Key != "task_fields:42:3:aaa" || back.PayloadSHA != "aaa" || string(back.Payload) != string(a.Payload) {
		t.Fatalf("revert prepare = %+v, %t", back, ok)
	}
	if seq, sha, id := state(); seq != 3 || sha != "aaa" || id != back.ID {
		t.Fatalf("state = %d/%s/%d, want 3/aaa/%d", seq, sha, id, back.ID)
	}
	var ops int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM yt_write_ops WHERE kind='task_fields'`).Scan(&ops); err != nil || ops != 3 {
		t.Fatalf("ops = %d, %v", ops, err)
	}
}

func TestPutTranslationsTitleReachesFeed(t *testing.T) {
	s := schemaStore(t, "yt_feed_translations")
	ctx := t.Context()
	task := ytFeedTask(t, s, "feed title")
	other := ytFeedTask(t, s, "body only")
	id := strconv.FormatInt(task.ID, 10)
	before := ytFeedMaxSyncID(t, s)
	if err := s.PutTranslations(ctx, []Translation{
		{Entity: "task", EntityID: id, Field: "title", Lang: "ko", SourceSHA256: SourceSHA256(task.Title), Text: "피드 제목", Translator: "test"},
		{Entity: "task", EntityID: id, Field: "body", Lang: "ko", SourceSHA256: SourceSHA256("b"), Text: "본문", Translator: "test"},
		{Entity: "task", EntityID: strconv.FormatInt(other.ID, 10), Field: "body", Lang: "ko", SourceSHA256: SourceSHA256("b"), Text: "본문", Translator: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	changes, _, err := s.SyncDelta(ctx, before, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Model != "task" || changes[0].ModelID != id || changes[0].Action != "U" {
		t.Fatalf("feed after a title translation = %+v", changes)
	}
}

// ytSetOpState moves op id to state along the guard trigger's legal path.
func ytSetOpState(t *testing.T, s *Store, id int64, state ytwrite.State) {
	t.Helper()
	ctx := t.Context()
	exec := func(q string) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	switch state {
	case ytwrite.StatePrepared:
	case ytwrite.StateRejected:
		exec(`UPDATE yt_write_ops SET state='rejected', reason='field_refused', resolved_at=now() WHERE id=$1`)
	case ytwrite.StateDispatched:
		exec(`UPDATE yt_write_ops SET state='dispatched', dispatched_at=now() WHERE id=$1`)
	case ytwrite.StateRemoteUnknown:
		exec(`UPDATE yt_write_ops SET state='dispatched', dispatched_at=now() WHERE id=$1`)
		exec(`UPDATE yt_write_ops SET state='remote_unknown' WHERE id=$1`)
	case ytwrite.StateApplied:
		exec(`UPDATE yt_write_ops SET state='dispatched', dispatched_at=now() WHERE id=$1`)
		exec(`UPDATE yt_write_ops SET state='applied', yt_result_id='2-1', resolved_at=now() WHERE id=$1`)
	default:
		t.Fatalf("state %q", state)
	}
}

// An unchanged snapshot whose last op was rejected is prepared again (hk
// 4057); one whose last op is in any other state is not.
func TestPrepareYTSnapshotRetriesRejectedSHA(t *testing.T) {
	s := schemaStore(t, "yt_feed_snapshot_rejected")
	ctx := t.Context()
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	prepare := func(entity, sha string) (ytwrite.Op, bool) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		op, prepared, err := prepareYTSnapshotTx(ctx, tx, now, ytwrite.NewOp{Principal: "p", Kind: "task_fields", PayloadSHA: sha,
			Payload: []byte(`{"sha":"` + sha + `"}`), EntityKind: "task", EntityID: entity, YTTargetID: "2-" + entity, Actor: "a"}, sha)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return op, prepared
	}
	for i, state := range []ytwrite.State{ytwrite.StatePrepared, ytwrite.StateDispatched, ytwrite.StateRemoteUnknown, ytwrite.StateApplied} {
		entity := strconv.Itoa(100 + i)
		a, ok := prepare(entity, "aaa")
		if !ok {
			t.Fatalf("%s: first prepare skipped", state)
		}
		ytSetOpState(t, s, a.ID, state)
		if op, ok := prepare(entity, "aaa"); ok {
			t.Fatalf("%s + same sha prepared %+v", state, op)
		}
	}

	// Rejected + same sha: one new op per call, the next seq.
	a, _ := prepare("42", "aaa")
	ytSetOpState(t, s, a.ID, ytwrite.StateRejected)
	b, ok := prepare("42", "aaa")
	if !ok || b.ID == a.ID || b.Key != "task_fields:42:2:aaa" || b.State != ytwrite.StatePrepared {
		t.Fatalf("retry of a rejected sha = %+v, %t", b, ok)
	}
	// The retry is pending: a second call makes nothing.
	if op, ok := prepare("42", "aaa"); ok {
		t.Fatalf("second call prepared %+v", op)
	}
	// Rejected again: the next call prepares once more, and only once.
	ytSetOpState(t, s, b.ID, ytwrite.StateRejected)
	c, ok := prepare("42", "aaa")
	if !ok || c.Key != "task_fields:42:3:aaa" {
		t.Fatalf("retry after a second rejection = %+v, %t", c, ok)
	}
	if _, ok := prepare("42", "aaa"); ok {
		t.Fatal("a pending retry prepared again")
	}
	// A different sha prepares as before, whatever the last op's state.
	d, ok := prepare("42", "bbb")
	if !ok || d.Key != "task_fields:42:4:bbb" {
		t.Fatalf("changed sha = %+v, %t", d, ok)
	}
	var seq, last int64
	if err := s.pool.QueryRow(ctx, `SELECT seq,last_op_id FROM yt_snapshot_state WHERE entity_kind='task' AND entity_id='42' AND kind='task_fields'`).Scan(&seq, &last); err != nil || seq != 4 || last != d.ID {
		t.Fatalf("state = %d/%d, %v; want 4/%d", seq, last, err, d.ID)
	}
	var ops int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM yt_write_ops WHERE entity_id='42'`).Scan(&ops); err != nil || ops != 4 {
		t.Fatalf("ops = %d, %v", ops, err)
	}
}
