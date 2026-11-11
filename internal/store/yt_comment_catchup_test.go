package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// hk 3897: the comment worker's catch-up prepares the op of every comment on
// a bound task above the binding's comments_from that no op carries, with
// the enqueue's rules, once.

// ytcuRawComment writes a comment the way a writer that does not enqueue
// does (ReplyDots, the YouTrack activity pull): the row only.
func ytcuRawComment(t *testing.T, s *Store, task int64, author string) TaskComment {
	t.Helper()
	var c TaskComment
	if err := scanTaskComment(s.pool.QueryRow(t.Context(), `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4) RETURNING `+taskCommentColumns,
		task, "raw comment by "+author, author, time.Now().UTC()), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func ytcuPass(t *testing.T, s *Store, limit int, skip []int64) YTCatchUp {
	t.Helper()
	got, err := s.CatchUpYTComments(t.Context(), limit, skip)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func ytcuNoOp(t *testing.T, s *Store, name string, c TaskComment) {
	t.Helper()
	if op, ok := ytcOpFor(t, s, c.ID); ok {
		t.Errorf("%s: comment %d got op %+v", name, c.ID, op)
	}
}

// A comment the enqueue missed after the binding (written while the mode was
// not on, or by a writer that does not enqueue) gets exactly one op from one
// pass, targeting the binding's issue; a second pass prepares none. A comment
// at or below comments_from (written before the binding), one the YouTrack
// pull wrote and one the legacy push already mirrored get none, and the
// worker delivers each prepared op once.
func TestYTCommentCatchUpPreparesMissedComments(t *testing.T) {
	s := schemaStore(t, "ytcu_t1")
	ctx := t.Context()
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	early := ytbComment(t, s, task, "before the binding").TaskComment
	b := ytbBinding(task)
	ytbBind(t, s, b)
	stored, found, err := s.YTTaskBinding(ctx, task)
	if err != nil || !found || stored.CommentsFrom != early.ID {
		t.Fatalf("binding %+v %v %v: comments_from want %d, the task's highest comment id at publication", stored, found, err, early.ID)
	}
	modeOff := ytbComment(t, s, task, "after the binding, mode off").TaskComment
	dots := ytcuRawComment(t, s, task, "dots")
	pulled := ytcuRawComment(t, s, task, YouTrackOwnedActor)
	mirrored := ytbComment(t, s, task, "already pushed by the legacy push").TaskComment
	if err := s.LinkYouTrackComment(ctx, task, mirrored.ID, "4-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []TaskComment{early, modeOff, dots, mirrored} {
		ytcuNoOp(t, s, "before the pass", c)
	}

	for _, mode := range []YTCommentsMode{YTCommentsOff, YTCommentsCanary} {
		s.SetYTCommentsMode(mode)
		if got := ytcuPass(t, s, 200, nil); len(got.Prepared) != 0 || len(got.Skipped) != 0 {
			t.Fatalf("mode %s: a pass did %+v", mode, got)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 0 {
		t.Fatalf("passes outside mode on wrote %d ops", n)
	}

	s.SetYTCommentsMode(YTCommentsOn)
	logs := ytbLog(t)
	got := ytcuPass(t, s, 200, nil)
	if len(got.Prepared) != 2 || len(got.Skipped) != 0 {
		t.Fatalf("first pass %+v, want two ops", got)
	}
	for i, c := range []TaskComment{modeOff, dots} {
		op, ok := ytcOpFor(t, s, c.ID)
		if !ok || op.ID != got.Prepared[i].ID || op.State != ytwrite.StatePrepared || op.Key != YTCommentKey(c.ID) ||
			op.Tenant != b.Tenant || op.YTTargetID != b.IssueID || op.EntityKind != "task" || op.EntityID != strconv.FormatInt(task, 10) {
			t.Fatalf("comment %d (%d in order): op %+v %v, pass %+v", c.ID, i, op, ok, got.Prepared)
		}
		if op.CreatedAt.Before(c.CreatedAt) {
			t.Errorf("op %d created %s, before its comment (%s): it would look stale at once", op.ID, op.CreatedAt, c.CreatedAt)
		}
		want := "ytwrite.comment_catchup task_id=" + strconv.FormatInt(task, 10) + " comment_id=" + strconv.FormatInt(c.ID, 10) + " op_id=" + strconv.FormatInt(op.ID, 10)
		if !strings.Contains(logs(), want) {
			t.Errorf("log has no %q:\n%s", want, logs())
		}
	}
	for _, c := range []TaskComment{early, pulled, mirrored} {
		ytcuNoOp(t, s, "after the pass", c)
	}
	if row, ok, err := s.YTWriteLedger().MapGet(ctx, "task", strconv.FormatInt(task, 10)); err != nil || !ok || row.YTID != b.IssueID || row.OpID != got.Prepared[0].ID {
		t.Fatalf("task map anchor %+v %v %v", row, ok, err)
	}
	if again := ytcuPass(t, s, 200, nil); len(again.Prepared) != 0 || len(again.Skipped) != 0 {
		t.Fatalf("second pass %+v, want nothing", again)
	}
	// The legacy push no longer selects what the protocol now owns.
	legacy, err := s.UnpushedYouTrackComments(ctx, task, 0, 50)
	if err != nil || len(legacy) != 1 || legacy[0].ID != early.ID {
		t.Fatalf("legacy selection %+v %v, want only the comment from before the binding", legacy, err)
	}
	// A comment the enqueue copies is not prepared twice.
	live := ytbComment(t, s, task, "mode on")
	if again := ytcuPass(t, s, 200, nil); len(again.Prepared) != 0 {
		t.Fatalf("a pass after an enqueued comment %+v", again)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 3 {
		t.Fatalf("%d ops for three copied comments", n)
	}
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)
	for i := 0; i < 4; i++ {
		if _, err := p.Drive(ctx, 10); err != nil {
			t.Fatal(err)
		}
	}
	if cs := yt.Comments(b.IssueID); len(cs) != 3 || yt.Calls() != 3 {
		t.Fatalf("delivered %d comments in %d calls, want 3 (catch-up %d, %d and enqueue %d)", len(cs), yt.Calls(), modeOff.ID, dots.ID, live.ID)
	}
}

// A comment whose binding a local source disagrees with gets no op and one
// log line naming the source; passed back as skip, it is neither logged
// again nor crowds out a later comment of another task.
func TestYTCommentCatchUpSkipsAmbiguousBinding(t *testing.T) {
	s := schemaStore(t, "ytcu_t2")
	ctx := t.Context()
	ytbMapped(t, s)
	amb, ok := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(amb))
	ytbBind(t, s, ytbBinding(ok))
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: amb, IssueID: "9-" + strconv.FormatInt(amb, 10), DisplayRef: "OLD-1", Tenant: ytcTenant, Role: "mirror"}); err != nil {
		t.Fatal(err)
	}
	ambComment := ytcuRawComment(t, s, amb, "dots")
	okComment := ytcuRawComment(t, s, ok, "dots")
	s.SetYTCommentsMode(YTCommentsOn)
	logs := ytbLog(t)
	got := ytcuPass(t, s, 1, nil)
	if len(got.Prepared) != 0 || len(got.Skipped) != 1 || got.Skipped[0] != ambComment.ID {
		t.Fatalf("pass over the ambiguous comment %+v", got)
	}
	ytcuNoOp(t, s, "ambiguous", ambComment)
	want := "ytwrite.comment_catchup_skipped_ambiguous task_id=" + strconv.FormatInt(amb, 10) + " comment_id=" + strconv.FormatInt(ambComment.ID, 10) + " source=task_origins"
	if !strings.Contains(logs(), want) || strings.Contains(logs(), ambComment.Body) {
		t.Fatalf("log wants %q and no body:\n%s", want, logs())
	}
	got = ytcuPass(t, s, 1, got.Skipped)
	if len(got.Prepared) != 1 || len(got.Skipped) != 0 {
		t.Fatalf("pass with the skip list %+v, want the other task's comment", got)
	}
	if op, found := ytcOpFor(t, s, okComment.ID); !found || op.ID != got.Prepared[0].ID {
		t.Fatalf("other task's comment op %+v %v", op, found)
	}
	if n := strings.Count(logs(), want); n != 1 {
		t.Fatalf("the skip was logged %d times", n)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE entity_id=$1`, strconv.FormatInt(amb, 10)); n != 0 {
		t.Fatalf("%d ops on the ambiguous task", n)
	}
}

// A pass takes the oldest comments first, at most limit of them.
func TestYTCommentCatchUpOldestFirstAndBounded(t *testing.T) {
	s := schemaStore(t, "ytcu_t3")
	ytbMapped(t, s)
	first, second := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(first))
	ytbBind(t, s, ytbBinding(second))
	cs := []TaskComment{ytcuRawComment(t, s, second, "dots"), ytcuRawComment(t, s, first, "dots"), ytcuRawComment(t, s, second, "dots")}
	s.SetYTCommentsMode(YTCommentsOn)
	for i, want := range [][]TaskComment{cs[:2], cs[2:], nil} {
		got := ytcuPass(t, s, 2, nil)
		if len(got.Prepared) != len(want) {
			t.Fatalf("pass %d prepared %d, want %d", i, len(got.Prepared), len(want))
		}
		for j, c := range want {
			if op, ok := ytcOpFor(t, s, c.ID); !ok || op.ID != got.Prepared[j].ID {
				t.Fatalf("pass %d, comment %d: op %+v %v, prepared %+v", i, c.ID, op, ok, got.Prepared)
			}
		}
	}
}

// Two workers (blue-green) passing at once prepare one op per comment, and
// neither fails.
func TestYTCommentCatchUpConcurrentPassesPrepareOnce(t *testing.T) {
	s := schemaStore(t, "ytcu_t4")
	ytbMapped(t, s)
	var comments []TaskComment
	for i := 0; i < 4; i++ {
		task := ytcTask(t, s, ytbProject)
		ytbBind(t, s, ytbBinding(task))
		comments = append(comments, ytcuRawComment(t, s, task, "dots"), ytcuRawComment(t, s, task, "dots"))
	}
	s.SetYTCommentsMode(YTCommentsOn)
	var wg sync.WaitGroup
	var prepared atomic.Int64
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.CatchUpYTComments(t.Context(), 200, nil)
			if err != nil {
				errs <- err
				return
			}
			prepared.Add(int64(len(got.Prepared)))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != len(comments) || prepared.Load() != int64(len(comments)) {
		t.Fatalf("%d ops, %d reported prepared, for %d comments", n, prepared.Load(), len(comments))
	}
}

// A task with no hk project (the import's HK bucket) binds to a YouTrack
// project that no youtrack_projects row maps, and its comments are copied
// like any other's; bound to a mapped project, or once it gets a project, it
// disagrees.
func TestYTTaskBindingNoProjectBucket(t *testing.T) {
	s := schemaStore(t, "ytcu_t5")
	ctx := t.Context()
	ytbMapped(t, s)
	const bucketID = "0-1"
	noProject := func() int64 {
		task := ytcTask(t, s, ytbProject)
		if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project=NULL WHERE id=$1`, task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	task := noProject()
	b := ytbBinding(task)
	b.ProjectID = bucketID
	ytbBind(t, s, b)
	s.SetYTCommentsMode(YTCommentsOn)
	c := ytbComment(t, s, task, "no project, bucket issue")
	if op, ok := ytcOpFor(t, s, c.ID); !ok || op.State != ytwrite.StatePrepared || op.YTTargetID != b.IssueID {
		t.Fatalf("bucket binding: op %+v %v", op, ok)
	}

	mapped := noProject()
	wrong := ytbBinding(mapped)
	wrong.ProjectID = ytbProjectID
	if _, err := ytbAdd(t, s, wrong); !errors.Is(err, ErrYTBindingConflict) || !strings.Contains(err.Error(), ytSourceProject) {
		t.Fatalf("no-project task bound to a mapped project: %v, want %s conflict", err, ytSourceProject)
	}

	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project=$2 WHERE id=$1`, task, ytbProject); err != nil {
		t.Fatal(err)
	}
	if source, err := s.YTBindingDisagreement(ctx, b); err != nil || source != ytSourceProject {
		t.Fatalf("bucket binding of a task that got a project: %q %v", source, err)
	}
}

// The migration sets each existing binding's comments_from to its task's
// highest comment id (0 without comments), so the catch-up copies only what
// comes after it.
func TestYTBindingCommentsFromMigrationBackfills(t *testing.T) {
	_, url := testdb.Schema(t, "ytcu_mig")
	var without []migration
	for _, m := range migrationRegistry {
		if m.ID != YTBindingCommentsFromMigration {
			without = append(without, m)
		}
	}
	if len(without) != len(migrationRegistry)-1 {
		t.Fatalf("migration %s is not registered", YTBindingCommentsFromMigration)
	}
	old := openRegistry(t, url, without)
	withComments, without2, unbound := ytcTask(t, old, ytbProject), ytcTask(t, old, ytbProject), ytcTask(t, old, ytbProject)
	var last int64
	for _, task := range []int64{withComments, unbound, withComments} {
		last = ytcuRawComment(t, old, task, "worker").ID
	}
	for _, task := range []int64{withComments, without2} {
		// The previous release's insert, which names no comments_from.
		if _, err := old.pool.Exec(t.Context(), `INSERT INTO yt_task_bindings(task_id,tenant,issue_id,issue_readable,project_id,bound_by,bound_at)
			VALUES($1,$2,$3,'',$4,'old-release',now())`, task, ytcTenant, "2-"+strconv.FormatInt(task, 10), ytbProjectID); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	for i := 0; i < 2; i++ {
		s := openRegistry(t, url, migrationRegistry)
		for task, want := range map[int64]int64{withComments: last, without2: 0} {
			b, found, err := s.YTTaskBinding(t.Context(), task)
			if err != nil || !found || b.CommentsFrom != want {
				t.Fatalf("open %d: task %d binding %+v %v %v, want comments_from %d", i, task, b, found, err, want)
			}
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM schema_migrations WHERE id=$1`, YTBindingCommentsFromMigration); n != 1 {
			t.Fatalf("open %d: id recorded %d times", i, n)
		}
		if i == 1 {
			ytbMapped(t, s)
			after := ytcuRawComment(t, s, withComments, "dots")
			s.SetYTCommentsMode(YTCommentsOn)
			got := ytcuPass(t, s, 200, nil)
			if op, ok := ytcOpFor(t, s, after.ID); len(got.Prepared) != 1 || !ok || op.ID != got.Prepared[0].ID {
				t.Fatalf("catch-up after the migration %+v, op %+v %v", got, op, ok)
			}
		}
		s.Close()
	}
}

// The legacy push and the catch-up never both send one comment (hk 3897
// review). The legacy push keeps selecting every unlinked comment in every
// mode. A pass that finds comments while a `youtrack sync` run holds the sync
// lock defers and writes nothing; once the sync has finished, the comment it
// linked is left, even one on a binding that disagreed then and agrees now,
// and a link committed after the scan is seen under the lane lock.
func TestYTCommentCatchUpAndLegacyPushOwnOneEach(t *testing.T) {
	s := schemaStore(t, "ytcu_t6")
	ctx := t.Context()
	ytbMapped(t, s)
	agree, amb := ytcTask(t, s, ytbProject), ytcTask(t, s, ytbProject)
	ytbBind(t, s, ytbBinding(agree))
	ytbBind(t, s, ytbBinding(amb))
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project='ytcu-unmapped' WHERE id=$1`, amb); err != nil {
		t.Fatal(err)
	}
	agreeComment := ytcuRawComment(t, s, agree, "dots")
	ambComment := ytcuRawComment(t, s, amb, "dots")
	legacy := func(task int64) []int64 {
		t.Helper()
		got, err := s.UnpushedYouTrackComments(ctx, task, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		return ids
	}
	// Mode off: the legacy push still carries both.
	for task, c := range map[int64]TaskComment{agree: agreeComment, amb: ambComment} {
		if got := legacy(task); len(got) != 1 || got[0] != c.ID {
			t.Fatalf("mode off: the legacy push selects %v on task %d, want [%d]", got, task, c.ID)
		}
	}

	// A sync run holds the lock: the pass defers and writes nothing.
	s.SetYTCommentsMode(YTCommentsOn)
	logs := ytbLog(t)
	syncRun, held, err := s.TryYouTrackSyncLock(ctx)
	if err != nil || !held {
		t.Fatalf("sync lock: %v %v", held, err)
	}
	got := ytcuPass(t, s, 200, nil)
	if !got.Deferred || len(got.Prepared) != 0 || len(got.Skipped) != 0 {
		t.Fatalf("pass during a sync %+v, want deferred", got)
	}
	if !strings.Contains(logs(), "ytwrite.comment_catchup_deferred comments=2 reason=sync_running") {
		t.Fatalf("no deferral log:\n%s", logs())
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 0 {
		t.Fatalf("a deferred pass wrote %d ops", n)
	}
	// The sync posts and links both, then ends. The disagreement is fixed
	// after it; the catch-up, with no skip list, leaves both.
	for task, c := range map[int64]TaskComment{agree: agreeComment, amb: ambComment} {
		if err := s.LinkYouTrackComment(ctx, task, c.ID, "4-"+strconv.FormatInt(c.ID, 10), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := syncRun.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project=$2 WHERE id=$1`, amb, ytbProject); err != nil {
		t.Fatal(err)
	}
	if got := ytcuPass(t, s, 200, nil); got.Deferred || len(got.Prepared) != 0 || len(got.Skipped) != 0 {
		t.Fatalf("pass after the sync linked both %+v, want nothing", got)
	}
	ytcuNoOp(t, s, "linked by the legacy push", agreeComment)
	ytcuNoOp(t, s, "linked by the legacy push while ambiguous", ambComment)

	// No sync: the catch-up takes a new comment, and the legacy push no
	// longer selects it.
	fresh := ytcuRawComment(t, s, agree, "dots")
	if got := ytcuPass(t, s, 200, nil); len(got.Prepared) != 1 {
		t.Fatalf("pass with no sync running %+v, want one op", got)
	}
	if got := legacy(agree); len(got) != 0 {
		t.Fatalf("the legacy push selects %v after the catch-up linked %d", got, fresh.ID)
	}
	// The lock is released after the pass: a sync can start.
	next, held, err := s.TryYouTrackSyncLock(ctx)
	if err != nil || !held {
		t.Fatalf("sync lock after a pass: %v %v", held, err)
	}
	if err := next.Release(ctx); err != nil {
		t.Fatal(err)
	}

	// A legacy link that commits between the scan and the comment's
	// transaction: the read under the lane lock sees it.
	late := ytcuRawComment(t, s, agree, "dots")
	if err := s.LinkYouTrackComment(ctx, agree, late.ID, "4-late", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, prepared, _, err := catchUpYTComment(ctx, s.pool, late); err != nil || prepared {
		t.Fatalf("catch-up of a comment the legacy push linked: prepared %v, %v", prepared, err)
	}
	ytcuNoOp(t, s, "linked after the scan", late)
}

// A pass holds the sync lock's connection and runs each comment's
// transaction on it, so it finishes in a pool of one connection (review
// #309: it used to wait for a second connection forever).
func TestYTCommentCatchUpInOneConnectionPool(t *testing.T) {
	_, url := testdb.Schema(t, "ytcu_one")
	setup, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	ytbMapped(t, setup)
	task := ytcTask(t, setup, ytbProject)
	ytbBind(t, setup, ytbBinding(task))
	c := ytcuRawComment(t, setup, task, "dots")
	setup.Close()
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	s, err := Open(t.Context(), url+sep+"pool_max_conns=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetYTCommentsMode(YTCommentsOn)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := s.CatchUpYTComments(ctx, 200, nil)
	if err != nil || len(got.Prepared) != 1 {
		t.Fatalf("pass in a pool of one: %+v %v", got, err)
	}
	if op, ok := ytcOpFor(t, s, c.ID); !ok || op.ID != got.Prepared[0].ID {
		t.Fatalf("op %+v %v", op, ok)
	}
}
