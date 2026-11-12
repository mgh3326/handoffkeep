package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The per-kind authority row (hk 4122, docs/youtrack-write-protocol.md
// §Authority).

func ytaSides(t *testing.T, rows []YTAuthority) map[YTAuthorityKind]YTAuthoritySide {
	t.Helper()
	out := map[YTAuthorityKind]YTAuthoritySide{}
	for _, a := range rows {
		out[a.Kind] = a.Authority
	}
	if len(out) != len(YTAuthorityKinds) || len(rows) != len(YTAuthorityKinds) {
		t.Fatalf("rows %+v: want one per kind", rows)
	}
	return out
}

func ytaAll(t *testing.T, s *Store) map[YTAuthorityKind]YTAuthoritySide {
	t.Helper()
	rows, err := s.YTAuthorities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return ytaSides(t, rows)
}

func TestYTAuthorityDefaultRows(t *testing.T) {
	s := schemaStore(t, "yta_default")
	rows, err := s.YTAuthorities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range rows {
		if a.Kind != YTAuthorityKinds[i] || a.Authority != YTAuthorityHK || a.SwitchedAt != nil || a.SwitchedBy != "" || a.Note != "" {
			t.Fatalf("row %d = %+v", i, a)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_authority WHERE authority='hk'`); n != 4 {
		t.Fatalf("%d seeded hk rows, want 4", n)
	}
	// A missing row reads as hk.
	if _, err := s.pool.Exec(t.Context(), `DELETE FROM yt_authority WHERE kind='comments'`); err != nil {
		t.Fatal(err)
	}
	if side, err := s.YTAuthorityOf(t.Context(), YTAuthorityComments); err != nil || side != YTAuthorityHK {
		t.Fatalf("missing comments row = %q %v", side, err)
	}
	if got := ytaAll(t, s); got[YTAuthorityComments] != YTAuthorityHK {
		t.Fatalf("missing row lists as %v", got)
	}
	// A writer recreates the missing row (as hk) so it has a row to lock.
	task := ytcTask(t, s, "handoffkeep")
	if _, err := s.CreateTaskComment(t.Context(), task, "worker", "recreates the row"); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_authority WHERE kind='comments' AND authority='hk'`); n != 1 {
		t.Fatalf("comments row after a comment write: %d, want 1", n)
	}
	// The schema refuses an unknown kind or side.
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO yt_authority(kind) VALUES ('files')`); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE yt_authority SET authority='both' WHERE kind='tasks'`); err == nil {
		t.Fatal("unknown authority accepted")
	}
}

func TestYTAuthoritySetRefusals(t *testing.T) {
	s := schemaStore(t, "yta_refuse")
	ctx := t.Context()
	set := func(kinds []YTAuthorityKind, side YTAuthoritySide) ([]YTAuthority, []YTAuthority, error) {
		return s.SetYTAuthority(ctx, kinds, side, "operator", "drill")
	}
	refused := func(err error, want string) {
		t.Helper()
		if !errors.Is(err, ErrYTAuthorityRefused) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %v naming %q", err, ErrYTAuthorityRefused, want)
		}
	}
	unchanged := func() {
		t.Helper()
		for k, side := range ytaAll(t, s) {
			if side != YTAuthorityHK {
				t.Fatalf("%s is %s after a refused set", k, side)
			}
		}
	}

	// Invalid requests change nothing.
	for _, tc := range []struct {
		kinds    []YTAuthorityKind
		side     YTAuthoritySide
		by, note string
	}{
		{nil, YTAuthorityYouTrack, "operator", "n"},
		{[]YTAuthorityKind{"files"}, YTAuthorityYouTrack, "operator", "n"},
		{[]YTAuthorityKind{YTAuthorityComments, YTAuthorityComments}, YTAuthorityHK, "operator", "n"},
		{[]YTAuthorityKind{YTAuthorityComments}, "both", "operator", "n"},
		{[]YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "", "n"},
		{[]YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "operator", " "},
	} {
		if _, _, err := s.SetYTAuthority(ctx, tc.kinds, tc.side, tc.by, tc.note); err == nil || errors.Is(err, ErrYTAuthorityRefused) {
			t.Fatalf("set %+v = %v, want a validation error", tc, err)
		}
	}

	// Every write mode off: every kind is refused.
	for _, k := range YTAuthorityKinds {
		_, _, err := set([]YTAuthorityKind{k}, YTAuthorityYouTrack)
		refused(err, string(k)+": ")
	}
	unchanged()

	// Comments mode canary is not on.
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	c, err := s.CreateTaskComment(ctx, task, "worker", "pending copy")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("op %+v %v", op, ok)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "HANDOFFKEEP_YT_COMMENTS_MODE")

	// Mode on, but an op is prepared, then dispatched, then remote_unknown.
	s.SetYTCommentsMode(YTCommentsOn)
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "comments: 1 ops")
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='dispatched', dispatched_at=now() WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "comments: 1 ops")
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='remote_unknown' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	refused(err, "comments: 1 ops")
	unchanged()

	// An op of another kind's principal does not hold comments: once the
	// comment op is resolved, comments switch while tasks (modes off) are
	// still refused, and `all` changes nothing.
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state='rejected', reason='test', resolved_at=now() WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = set(YTAuthorityKinds, YTAuthorityYouTrack)
	refused(err, "tasks: ")
	unchanged()
	before, after, err := set([]YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack)
	if err != nil {
		t.Fatal(err)
	}
	if b, a := ytaSides(t, before), ytaSides(t, after); b[YTAuthorityComments] != YTAuthorityHK || a[YTAuthorityComments] != YTAuthorityYouTrack ||
		a[YTAuthorityTasks] != YTAuthorityHK || a[YTAuthorityDocuments] != YTAuthorityHK || a[YTAuthorityDecisions] != YTAuthorityHK {
		t.Fatalf("before %+v after %+v", before, after)
	}
	for _, a := range after {
		if a.Kind == YTAuthorityComments && (a.SwitchedAt == nil || a.SwitchedBy != "operator" || a.Note != "drill") {
			t.Fatalf("switched row %+v", a)
		}
	}

	// Tasks need both the issue and the field modes.
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	_, _, err = set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack)
	refused(err, "HANDOFFKEEP_YT_FIELDS_MODE")
	// With both modes on, the fields feed must have read every task change.
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	_, _, err = set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack)
	refused(err, "feed consumer "+ytFieldsConsumer+" has not run yet")
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_feed_cursors(consumer,last_sync_id,updated_at) VALUES ($1,0,now())`, ytFieldsConsumer); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack)
	refused(err, "changes to read")
	if _, err := s.pool.Exec(ctx, `UPDATE yt_feed_cursors SET last_sync_id=(SELECT coalesce(max(sync_id),0) FROM sync_log) WHERE consumer=$1`, ytFieldsConsumer); err != nil {
		t.Fatal(err)
	}
	// A prune past the cursor removed rows the feed never read: refused.
	var pruned int64
	if err := s.pool.QueryRow(ctx, `SELECT pruned_through FROM sync_state WHERE singleton`).Scan(&pruned); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE sync_state SET pruned_through=(SELECT last_sync_id+1 FROM yt_feed_cursors WHERE consumer=$1) WHERE singleton`, ytFieldsConsumer); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack)
	refused(err, "behind the sync_log prune")
	if _, err := s.pool.Exec(ctx, `UPDATE sync_state SET pruned_through=$1 WHERE singleton`, pruned); err != nil {
		t.Fatal(err)
	}
	if _, _, err := set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack); err != nil {
		t.Fatal(err)
	}
	// Decisions need their mode (hk 4054) and their feed caught up.
	_, _, err = set([]YTAuthorityKind{YTAuthorityDecisions}, YTAuthorityYouTrack)
	refused(err, "HANDOFFKEEP_YT_DECISIONS_MODE")
	s.SetYTDecisionsMode(YTDecisionsOn, ytcTenant, "0-9", "DEC")
	_, _, err = set([]YTAuthorityKind{YTAuthorityDecisions}, YTAuthorityYouTrack)
	refused(err, "feed consumer "+ytDecisionsConsumer+" has not run yet")
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_feed_cursors(consumer,last_sync_id,updated_at) SELECT $1, coalesce(max(sync_id),0), now() FROM sync_log`, ytDecisionsConsumer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := set([]YTAuthorityKind{YTAuthorityDecisions}, YTAuthorityYouTrack); err != nil {
		t.Fatal(err)
	}
	s.SetYTDecisionsMode(YTDecisionsOff, "", "", "")

	// A legacy sync, adopt or catch-up holding the sync lock refuses a switch
	// to youtrack; a rollback to hk does not need it.
	lock, held, err := s.TryYouTrackSyncLock(ctx)
	if err != nil || !held {
		t.Fatalf("sync lock %v %v", held, err)
	}
	if _, _, err := set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityHK); err != nil {
		t.Fatal(err)
	}
	_, _, err = set([]YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack)
	refused(err, "sync lock")
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}

	// Rollback to hk needs nothing, even with every mode off again.
	s.SetYTCommentsMode(YTCommentsOff)
	s.SetYTIssuesMode(YTIssuesOff, "")
	s.SetYTFieldsMode(YTFieldsOff, "")
	if _, _, err := set(YTAuthorityKinds, YTAuthorityHK); err != nil {
		t.Fatal(err)
	}
	unchanged()
}

// An op prepared in a transaction still open when the switch starts is
// counted: the switch waits for it (FOR SHARE in prepareYTOpTx) and refuses.
func TestYTAuthoritySwitchWaitsForOpInFlight(t *testing.T) {
	s := schemaStore(t, "yta_race")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsOn)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := prepareYTOpTx(ctx, tx, time.Now().UTC(), ytwrite.NewOp{Principal: YTCommentPrincipal, Kind: ytwrite.KindCommentAppend, Key: "in-flight",
		PayloadSHA: "x", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "999998", YTTargetID: "2-1", Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack, "operator", "race")
		done <- err
	}()
	// Wait until the switch waits on the authority row (or has finished).
	deadline := time.Now().Add(10 * time.Second)
	for waiting := false; !waiting && time.Now().Before(deadline); {
		select {
		case err := <-done:
			t.Fatalf("the switch did not wait for the op in flight: %v", err)
		default:
		}
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE NOT l.granted AND c.relname = 'yt_authority')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrYTAuthorityRefused) || !strings.Contains(err.Error(), "comments: 1 ops") {
		t.Fatalf("switch after the op committed = %v", err)
	}
	if side, err := s.YTAuthorityOf(ctx, YTAuthorityComments); err != nil || side != YTAuthorityHK {
		t.Fatalf("comments = %q %v", side, err)
	}
}

// An unsendable comment: under hk it commits with a born-rejected op, as
// today; under youtrack it is refused and nothing is written; back under hk
// it commits again.
func TestYTAuthorityUnsendableComment(t *testing.T) {
	s := schemaStore(t, "yta_comment")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	const quoted = "the marker looks like <!-- hk-op:12 --> in prose"
	assertBornRejected := func() {
		t.Helper()
		c, err := s.CreateTaskComment(ctx, task, "worker", quoted)
		if err != nil {
			t.Fatalf("hk: %v", err)
		}
		op, ok := ytcOpFor(t, s, c.ID)
		if !ok || op.State != ytwrite.StateRejected || op.Reason != ytwrite.ReasonInvalidComment {
			t.Fatalf("hk: op %+v %v", op, ok)
		}
	}
	assertBornRejected()

	s.SetYTCommentsMode(YTCommentsOn)
	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack, "operator", "drill"); err != nil {
		t.Fatal(err)
	}
	// Refused in every mode, on an enrolled and on an unenrolled task, through
	// REST and MCP writers alike.
	other := ytcTask(t, s, "handoffkeep")
	comments := ytcCount(t, s, `SELECT count(*) FROM task_comments`)
	ops := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`)
	for _, mode := range []YTCommentsMode{YTCommentsCanary, YTCommentsOn, YTCommentsOff} {
		s.SetYTCommentsMode(mode)
		for _, id := range []int64{task, other} {
			if _, err := s.CreateTaskComment(ctx, id, "worker", quoted); !errors.Is(err, ErrYTCommentUnsendable) || !strings.Contains(err.Error(), "marker") {
				t.Fatalf("mode %s task %d: err = %v", mode, id, err)
			}
		}
		// A sendable comment with no YouTrack copy (the task is not bound or
		// enrolled, or the mode is off) would stay only in PG: refused too.
		if _, err := s.CreateTaskComment(ctx, other, "worker", "sendable"); !errors.Is(err, ErrYTCommentUnsendable) {
			t.Fatalf("mode %s: sendable comment without a copy: err = %v", mode, err)
		}
		w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "yta-" + string(mode)}
		if _, _, err := s.CreateMCPTaskComment(ctx, task, quoted, w); !errors.Is(err, ErrYTCommentUnsendable) {
			t.Fatalf("mode %s mcp: err = %v", mode, err)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM task_comments`); n != comments {
		t.Fatalf("%d comments after refusals, want %d", n, comments)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != ops {
		t.Fatalf("%d ops after refusals, want %d", n, ops)
	}
	// A sendable comment is unaffected.
	s.SetYTCommentsMode(YTCommentsCanary)
	c, err := s.CreateTaskComment(ctx, task, "worker", "sendable")
	if err != nil {
		t.Fatal(err)
	}
	if op, ok := ytcOpFor(t, s, c.ID); !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("sendable under youtrack: op %+v %v", op, ok)
	}

	// The audit's enqueue writes nothing while comments are youtrack.
	if status, _, err := s.EnqueueYTAuditComment(ctx, c.ID); err != nil || status != YTAuditAuthority {
		t.Fatalf("audit enqueue under youtrack = %q %v", status, err)
	}
	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityHK, "operator", "rollback"); err != nil {
		t.Fatal(err)
	}
	assertBornRejected()
}

// A dots reply writes its comment directly (ReplyDots): under hk it gets no
// op, as today; under youtrack it gets its op in the reply's transaction, or
// the whole reply is refused when the comment would stay only in PG.
func TestYTAuthorityDotsReply(t *testing.T) {
	s := schemaStore(t, "yta_dots")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	reply := func(key, body string) error {
		_, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: body, IdempotencyKey: key, RunToken: started.RunToken})
		return err
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	ytcEnroll(t, s, task.ID)
	if err := reply("hk-progress", "under hk"); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 0 {
		t.Fatalf("hk: a dots reply prepared %d ops, want 0 (today's behaviour)", n)
	}

	s.SetYTCommentsMode(YTCommentsOn)
	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityComments}, YTAuthorityYouTrack, "operator", "drill"); err != nil {
		t.Fatal(err)
	}
	comments := ytcCount(t, s, `SELECT count(*) FROM task_comments WHERE task_id=$1`, task.ID)
	// Mode on and the task is not bound: no copy path, so the reply is refused.
	if err := reply("yt-unbound", "no copy path"); !errors.Is(err, ErrYTCommentUnsendable) {
		t.Fatalf("youtrack, unbound: err = %v", err)
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	if err := reply("yt-marker", "quotes <!-- hk-op:12 -->"); !errors.Is(err, ErrYTCommentUnsendable) {
		t.Fatalf("youtrack, unsendable body: err = %v", err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM task_comments WHERE task_id=$1`, task.ID); n != comments {
		t.Fatalf("%d comments after refused replies, want %d", n, comments)
	}
	// Enrolled in canary: the reply commits with its op.
	if err := reply("yt-enrolled", "copied"); err != nil {
		t.Fatal(err)
	}
	var commentID int64
	if err := s.pool.QueryRow(ctx, `SELECT max(id) FROM task_comments WHERE task_id=$1`, task.ID).Scan(&commentID); err != nil {
		t.Fatal(err)
	}
	if op, ok := ytcOpFor(t, s, commentID); !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("youtrack, enrolled: op %+v %v", op, ok)
	}
}

// A prune that removed every change the feed had not read leaves no backlog
// to count; the gate reads the prune boundary, the cursor and the backlog in
// one statement and refuses on the boundary (hk 4122 review r4545561733).
func TestYTAuthorityRefusesBacklogLostToPrune(t *testing.T) {
	s := schemaStore(t, "yta_prune")
	ctx := t.Context()
	s.SetYTIssuesMode(YTIssuesOn, ytcTenant)
	s.SetYTFieldsMode(YTFieldsOn, ytcTenant)
	ytcTask(t, s, "handoffkeep")
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_feed_cursors(consumer,last_sync_id,updated_at) VALUES ($1,0,now())`, ytFieldsConsumer); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.PruneSync(ctx, time.Nanosecond); err != nil || removed == 0 {
		t.Fatalf("prune removed %d rows: %v", removed, err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM sync_log WHERE model='task'`); n != 0 {
		t.Fatalf("%d task changes survived the prune", n)
	}
	_, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack, "operator", "drill")
	if !errors.Is(err, ErrYTAuthorityRefused) || !strings.Contains(err.Error(), "behind the sync_log prune") {
		t.Fatalf("switch with a pruned, unread backlog = %v", err)
	}
	// Once the consumer is past the boundary the gate passes.
	if _, err := s.pool.Exec(ctx, `UPDATE yt_feed_cursors SET last_sync_id=(SELECT pruned_through FROM sync_state WHERE singleton) WHERE consumer=$1`, ytFieldsConsumer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityTasks}, YTAuthorityYouTrack, "operator", "drill"); err != nil {
		t.Fatal(err)
	}
}
