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

const ytcTenant = "tenant.youtrack.cloud"

func ytcTask(t *testing.T, s *Store, project string) int64 {
	t.Helper()
	var id int64
	now := time.Now().UTC()
	if err := s.pool.QueryRow(t.Context(), `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at,project)
		VALUES('ytc-lane','ytc task','implement','backlog','test',$1,$1,$1,$2) RETURNING id`, now, project).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func ytcBinding(task int64) YTCommentCanary {
	id := strconv.FormatInt(task, 10)
	return YTCommentCanary{TaskID: task, Tenant: ytcTenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", AddedBy: "operator"}
}

func ytcEnroll(t *testing.T, s *Store, task int64) {
	t.Helper()
	if _, err := s.AddYTCommentCanary(t.Context(), ytcBinding(task)); err != nil {
		t.Fatalf("enroll %d: %v", task, err)
	}
}

func ytcCount(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ytcOpFor is the op linked to a comment, or ok false.
func ytcOpFor(t *testing.T, s *Store, commentID int64) (ytwrite.Op, bool) {
	t.Helper()
	var opID int64
	err := s.pool.QueryRow(t.Context(), `SELECT op_id FROM yt_comment_write_links WHERE comment_id=$1`, commentID).Scan(&opID)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return ytwrite.Op{}, false
		}
		t.Fatal(err)
	}
	op, err := s.YTWriteLedger().ByID(t.Context(), opID)
	if err != nil {
		t.Fatal(err)
	}
	return op, true
}

func TestOneOpPerPersistedComment(t *testing.T) {
	s := schemaStore(t, "ytc_one")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	a, err := s.CreateTaskComment(ctx, task, "worker-a", "첫 코멘트")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTaskComment(ctx, task, "worker-b", "둘째")
	if err != nil {
		t.Fatal(err)
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "mcp-key-1"}
	m, replayed, err := s.CreateMCPTaskComment(ctx, task, "MCP 코멘트", w)
	if err != nil || replayed {
		t.Fatalf("mcp comment %v %v", replayed, err)
	}
	again, replayed, err := s.CreateMCPTaskComment(ctx, task, "MCP 코멘트", w)
	if err != nil || !replayed || again.ID != m.ID {
		t.Fatalf("mcp replay %+v %v %v", again, replayed, err)
	}
	seen := map[int64]bool{}
	for _, c := range []TaskComment{a, b, m} {
		op, ok := ytcOpFor(t, s, c.ID)
		if !ok {
			t.Fatalf("comment %d has no op", c.ID)
		}
		if seen[op.ID] {
			t.Fatalf("op %d serves two comments", op.ID)
		}
		seen[op.ID] = true
		if op.Principal != YTCommentPrincipal || op.Key != YTCommentKey(c.ID) || op.Kind != ytwrite.KindCommentAppend ||
			op.EntityKind != "task" || op.EntityID != strconv.FormatInt(task, 10) || op.YTTargetID != "2-"+strconv.FormatInt(task, 10) ||
			op.Actor != c.Author || op.State != ytwrite.StatePrepared || !op.CreatedAt.Equal(c.CreatedAt.UTC()) {
			t.Fatalf("op for comment %d = %+v", c.ID, op)
		}
		text, err := ytwrite.RenderComment(op)
		if err != nil || !strings.Contains(text, "\n\n"+c.Body+"\n\n") {
			t.Fatalf("rendered %q: %v", text, err)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 3 {
		t.Fatalf("%d ops for three persisted comments (one replayed)", n)
	}
	// The first comment op anchors the task's map row.
	first, _ := ytcOpFor(t, s, a.ID)
	row, ok, err := s.YTWriteLedger().MapGet(ctx, "task", strconv.FormatInt(task, 10))
	if err != nil || !ok || row.YTID != "2-"+strconv.FormatInt(task, 10) || row.OpID != first.ID {
		t.Fatalf("task map row %+v %v %v", row, ok, err)
	}
}

func TestOpInsertFailureIsAtomic(t *testing.T) {
	s := schemaStore(t, "ytc_atomic")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION ytc_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected op insert failure'; END; $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE TRIGGER ytc_fail BEFORE INSERT ON yt_write_ops FOR EACH ROW EXECUTE FUNCTION ytc_fail()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTaskComment(ctx, task, "worker", "lost?"); err == nil {
		t.Fatal("comment succeeded although its op could not be stored")
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "mcp-atomic"}
	if _, _, err := s.CreateMCPTaskComment(ctx, task, "lost too?", w); err == nil {
		t.Fatal("MCP comment succeeded although its op could not be stored")
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM task_comments WHERE task_id=$1`, task); n != 0 {
		t.Fatalf("%d comments committed without their op", n)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM mcp_write_audit`); n != 0 {
		t.Fatalf("%d audit rows committed without their comment", n)
	}
}

// The op is written on the comment's own connection: a pool of one does not
// deadlock waiting for a second.
func TestCommentEnqueueUsesCallerTransaction(t *testing.T) {
	_, url := testdb.Schema(t, "ytc_pool")
	// Migrate with a normal pool first, so the pool of one only serves the
	// comment.
	migrated, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	migrated.Close()
	var s *Store
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	s, err = Open(t.Context(), url+sep+"pool_max_conns=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := s.CreateTaskComment(ctx, task, "worker", "one connection")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ytcOpFor(t, s, c.ID); !ok {
		t.Fatal("no op")
	}
}

func TestModeOffHasNoWriteSideEffects(t *testing.T) {
	s := schemaStore(t, "ytc_off")
	ctx := t.Context()
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	if n := ytcCount(t, s, `SELECT (SELECT count(*) FROM yt_write_ops)+(SELECT count(*) FROM yt_entity_map)+(SELECT count(*) FROM yt_comment_write_links)`); n != 0 {
		t.Fatalf("enrollment wrote %d op/map/link rows", n)
	}
	s.SetYTCommentsMode(YTCommentsOff)
	if _, err := s.CreateTaskComment(ctx, task, "worker", "off"); err != nil {
		t.Fatal(err)
	}
	w := MCPWrite{Tool: "task_comment", Actor: "operator@example.com", Channel: "chatgpt", Author: "dots", Key: "mcp-off"}
	if _, _, err := s.CreateMCPTaskComment(ctx, task, "off too", w); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT (SELECT count(*) FROM yt_write_ops)+(SELECT count(*) FROM yt_entity_map)+(SELECT count(*) FROM yt_comment_write_links)`); n != 0 {
		t.Fatalf("mode off wrote %d op/map/link rows", n)
	}
	// Canary mode, task not enrolled: no op either.
	s.SetYTCommentsMode(YTCommentsCanary)
	other := ytcTask(t, s, "handoffkeep")
	if _, err := s.CreateTaskComment(ctx, other, "worker", "not enrolled"); err != nil {
		t.Fatal(err)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops`); n != 0 {
		t.Fatalf("a task outside the canary got %d ops", n)
	}
}

// What PG accepts still commits unchanged; what cannot be sent becomes a
// locally rejected op, never a guessed target.
func TestCommentLocalRejects(t *testing.T) {
	s := schemaStore(t, "ytc_reject")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	quoted := "the marker looks like <!-- hk-op:12 --> in prose"
	c, err := s.CreateTaskComment(ctx, task, "worker", quoted)
	if err != nil || c.Body != quoted {
		t.Fatalf("PG refused text it accepts today: %+v %v", c, err)
	}
	op, ok := ytcOpFor(t, s, c.ID)
	if !ok || op.State != ytwrite.StateRejected || op.Reason != ytwrite.ReasonInvalidComment || op.Attempts != 0 {
		t.Fatalf("op for a marker-quoting body = %+v %v", op, ok)
	}

	// A task whose map row names another issue (written after enrollment,
	// which refuses it up front): rejected before any send.
	other := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, other)
	anchor, _, err := s.YTWriteLedger().Prepare(ctx, time.Now().UTC(), ytwrite.NewOp{Principal: "test", Kind: ytwrite.KindCommentAppend, Key: "anchor",
		PayloadSHA: "x", Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: "999999", YTTargetID: "2-1", Actor: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind,hk_key,yt_id,op_id,created_at) VALUES('task',$1,'2-55555',$2,now())`, strconv.FormatInt(other, 10), anchor.ID); err != nil {
		t.Fatal(err)
	}
	c2, err := s.CreateTaskComment(ctx, other, "worker", "mapped elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	op2, ok := ytcOpFor(t, s, c2.ID)
	if !ok || op2.State != ytwrite.StateRejected || op2.Reason != ytwrite.ReasonAmbiguousIssue {
		t.Fatalf("op for a disagreeing map = %+v %v", op2, ok)
	}
}

func TestLegacyMirrorCannotPostProtocolOwnedComment(t *testing.T) {
	s := schemaStore(t, "ytc_legacy")
	ctx := t.Context()
	task := ytcTask(t, s, "handoffkeep")
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: task, IssueID: "9-1", DisplayRef: "HK-1", Tenant: ytcTenant, Role: "mirror", SyncedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ytcEnroll(t, s, task)
	s.SetYTCommentsMode(YTCommentsCanary)
	owned, err := s.CreateTaskComment(ctx, task, "worker", "owned by the protocol")
	if err != nil {
		t.Fatal(err)
	}
	due, err := s.DueYouTrackPushTasks(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == task {
			t.Fatal("the legacy push picked a task whose only new comment the protocol owns")
		}
	}
	got, err := s.UnpushedYouTrackComments(ctx, task, 0, 50)
	if err != nil || len(got) != 0 {
		t.Fatalf("legacy push would post %+v (%v)", got, err)
	}
	s.SetYTCommentsMode(YTCommentsOff)
	free, err := s.CreateTaskComment(ctx, task, "worker", "not owned")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.UnpushedYouTrackComments(ctx, task, 0, 50)
	if err != nil || len(got) != 1 || got[0].ID != free.ID || got[0].ID == owned.ID {
		t.Fatalf("unowned comment not offered to the legacy push: %+v %v", got, err)
	}
}

func TestImportMarksProtocolOwnedComments(t *testing.T) {
	s := schemaStore(t, "ytc_import")
	ctx := t.Context()
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	free, err := s.CreateTaskComment(ctx, task, "worker", "imported normally")
	if err != nil {
		t.Fatal(err)
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	owned, err := s.CreateTaskComment(ctx, task, "worker", "hk writes this one")
	if err != nil {
		t.Fatal(err)
	}
	op, _ := ytcOpFor(t, s, owned.ID)
	check := func(name string, rows []ImportComment) {
		t.Helper()
		got := map[int64]int64{}
		for _, r := range rows {
			got[r.ID] = r.HKOpID
		}
		if len(got) != 2 || got[free.ID] != 0 || got[owned.ID] != op.ID {
			t.Fatalf("%s: hk_op_id by comment = %v, want %d -> 0 and %d -> %d", name, got, free.ID, owned.ID, op.ID)
		}
	}
	page, err := s.ImportCommentsPage(ctx, 0, 100, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	check("full page", page)
	updated, err := s.ImportCommentsUpdatedPage(ctx, []int64{task}, time.Time{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	check("update page", updated)
}

// ytcProtocol runs the protocol over the Postgres ledger with the fake
// YouTrack, admitting what the store admits.
func ytcProtocol(t *testing.T, s *Store, yt ytwrite.Remote, offset *atomic.Int64) *ytwrite.Protocol {
	t.Helper()
	p, err := ytwrite.New(ytwrite.Config{Ledger: s.YTWriteLedger(), Remote: yt,
		Now: func() time.Time { return time.Now().UTC().Add(time.Duration(offset.Load())) },
		Admit: func(ctx context.Context, op ytwrite.Op) error {
			task, _ := strconv.ParseInt(op.EntityID, 10, 64)
			ok, err := s.YTCommentAdmitted(ctx, task, op.YTTargetID, ytcTenant)
			if err == nil && !ok {
				err = errors.New("not admitted")
			}
			return err
		}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommentProtocolOverPostgres(t *testing.T) {
	s := schemaStore(t, "ytc_proto")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	issue := "2-" + strconv.FormatInt(task, 10)
	ytcEnroll(t, s, task)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	p := ytcProtocol(t, s, yt, &offset)

	c, err := s.CreateTaskComment(ctx, task, "worker", "happy path")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	op, _ := ytcOpFor(t, s, c.ID)
	cs := yt.Comments(issue)
	if op.State != ytwrite.StateApplied || len(cs) != 1 || op.YTResultID != cs[0].ID {
		t.Fatalf("happy path: op %+v, remote %+v", op, cs)
	}
	actor, at, id, ok := ytwrite.ParseProvenance(strings.SplitN(cs[0].Text, "\n", 2)[0])
	if !ok || actor != "worker" || id != op.ID || !at.Equal(c.CreatedAt.UTC().Truncate(time.Second)) || !strings.HasSuffix(cs[0].Text, ytwrite.OpMarker(op.ID)) {
		t.Fatalf("remote text %q", cs[0].Text)
	}

	yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	lost, err := s.CreateTaskComment(ctx, task, "worker", "lost reply")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, lost.ID); op.State != ytwrite.StateRemoteUnknown {
		t.Fatalf("lost reply: %+v", op)
	}
	offset.Store(int64(time.Minute))
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, lost.ID); op.State != ytwrite.StateApplied || yt.Calls() != 2 || yt.Commits() != 2 {
		t.Fatalf("reconciled by marker: %+v, calls %d commits %d", op, yt.Calls(), yt.Commits())
	}

	// Removing the task from the canary freezes sends; the op is not rejected.
	if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	if _, err := s.AddYTCommentCanary(ctx, ytcBinding(task)); err != nil {
		t.Fatal(err)
	}
	held, err := s.CreateTaskComment(ctx, task, "worker", "enrolled when written")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveYTCommentCanary(ctx, task); err != nil {
		t.Fatal(err)
	}
	offset.Store(int64(2 * time.Minute))
	if _, err := p.Drive(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if op, _ := ytcOpFor(t, s, held.ID); op.State != ytwrite.StatePrepared || yt.Calls() != 2 {
		t.Fatalf("held op: %+v, calls %d", op, yt.Calls())
	}
}

func TestTwoDispatchersOneCreate(t *testing.T) {
	s := schemaStore(t, "ytc_two")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task := ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, task)
	yt := ytwritetest.NewFake()
	yt.SetLatency(50 * time.Millisecond)
	var offset atomic.Int64
	a, b := ytcProtocol(t, s, yt, &offset), ytcProtocol(t, s, yt, &offset)
	for i := 0; i < 3; i++ {
		if _, err := s.CreateTaskComment(ctx, task, "worker", "race "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, p := range []*ytwrite.Protocol{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				_, _ = p.Drive(ctx, 10)
			}
		}()
	}
	wg.Wait()
	if yt.Calls() != 3 || yt.Commits() != 3 {
		t.Fatalf("two dispatchers made %d calls / %d comments for three ops", yt.Calls(), yt.Commits())
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE state='applied'`); n != 3 {
		t.Fatalf("%d applied", n)
	}
}

func TestListYTWriteOps(t *testing.T) {
	s := schemaStore(t, "ytc_list")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	t1, t2 := ytcTask(t, s, "handoffkeep"), ytcTask(t, s, "handoffkeep")
	ytcEnroll(t, s, t1)
	ytcEnroll(t, s, t2)
	var ids []int64
	for _, task := range []int64{t1, t1, t2} {
		c, err := s.CreateTaskComment(ctx, task, "worker", "x")
		if err != nil {
			t.Fatal(err)
		}
		op, _ := ytcOpFor(t, s, c.ID)
		ids = append(ids, op.ID)
	}
	rej, err := s.CreateTaskComment(ctx, t2, "worker", "<!-- hk-op:1 -->")
	if err != nil {
		t.Fatal(err)
	}
	rejOp, _ := ytcOpFor(t, s, rej.ID)
	list := func(f YTOpFilter) []int64 {
		t.Helper()
		ops, err := s.ListYTWriteOps(ctx, f)
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		var out []int64
		for _, op := range ops {
			out = append(out, op.ID)
		}
		return out
	}
	eq := func(a, b []int64) bool { return fmtIDs(a) == fmtIDs(b) }
	if got := list(YTOpFilter{}); !eq(got, ids) {
		t.Fatalf("default (pending) = %v, want %v", got, ids)
	}
	if got := list(YTOpFilter{State: "all"}); !eq(got, append(append([]int64{}, ids...), rejOp.ID)) {
		t.Fatalf("all = %v", got)
	}
	if got := list(YTOpFilter{State: "rejected"}); !eq(got, []int64{rejOp.ID}) {
		t.Fatalf("rejected = %v", got)
	}
	if got := list(YTOpFilter{TaskID: t1}); !eq(got, ids[:2]) {
		t.Fatalf("task filter = %v", got)
	}
	if got := list(YTOpFilter{AfterID: ids[0], Limit: 1}); !eq(got, ids[1:2]) {
		t.Fatalf("page = %v", got)
	}
	for _, bad := range []YTOpFilter{{State: "done"}, {Limit: YTOpListMax + 1}, {TaskID: -1}, {AfterID: -1}} {
		if _, err := s.ListYTWriteOps(ctx, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func fmtIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func TestYTCommentCanarySet(t *testing.T) {
	s := schemaStore(t, "ytc_set")
	ctx := t.Context()
	s.SetYTCommentsMode(YTCommentsCanary)
	task, other := ytcTask(t, s, "handoffkeep"), ytcTask(t, s, "handoffkeep")
	got, err := s.AddYTCommentCanary(ctx, ytcBinding(task))
	if err != nil || got.TaskID != task || got.IssueID != ytcBinding(task).IssueID || got.AddedAt.IsZero() {
		t.Fatalf("add = %+v %v", got, err)
	}
	if _, err := s.AddYTCommentCanary(ctx, ytcBinding(task)); err != nil {
		t.Fatalf("same binding again: %v", err)
	}
	moved := ytcBinding(task)
	moved.IssueID = "2-424242"
	if _, err := s.AddYTCommentCanary(ctx, moved); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("another binding for an enrolled task: %v", err)
	}
	stolen := ytcBinding(other)
	stolen.IssueID = ytcBinding(task).IssueID
	if _, err := s.AddYTCommentCanary(ctx, stolen); !errors.Is(err, ErrYTCanaryConflict) {
		t.Fatalf("an issue already bound to another task: %v", err)
	}
	if _, err := s.AddYTCommentCanary(ctx, ytcBinding(987654321)); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
	list, err := s.ListYTCommentCanary(ctx)
	if err != nil || len(list) != 1 || list[0].TaskID != task || list[0].Tenant != ytcTenant {
		t.Fatalf("list = %+v %v", list, err)
	}
	if ok, err := s.YTCommentAdmitted(ctx, task, ytcBinding(task).IssueID, ytcTenant); err != nil || !ok {
		t.Fatalf("admitted = %v %v", ok, err)
	}
	if ok, _ := s.YTCommentAdmitted(ctx, task, ytcBinding(task).IssueID, "other.youtrack.cloud"); ok {
		t.Fatal("admitted on another tenant")
	}
	s.SetYTCommentsMode(YTCommentsOff)
	if ok, _ := s.YTCommentAdmitted(ctx, task, ytcBinding(task).IssueID, ytcTenant); ok {
		t.Fatal("admitted with the gate off")
	}
	s.SetYTCommentsMode(YTCommentsCanary)
	if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || !removed {
		t.Fatalf("remove = %v %v", removed, err)
	}
	if removed, err := s.RemoveYTCommentCanary(ctx, task); err != nil || removed {
		t.Fatalf("second remove = %v %v", removed, err)
	}
	if ok, _ := s.YTCommentAdmitted(ctx, task, ytcBinding(task).IssueID, ytcTenant); ok {
		t.Fatal("admitted after removal")
	}
}

func TestYTCanaryCandidateReadsEveryMapping(t *testing.T) {
	s := schemaStore(t, "ytc_cand")
	ctx := t.Context()
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytc-mapped", ProjectID: "0-9", ShortName: "YTC"}); err != nil {
		t.Fatal(err)
	}
	mapped := ytcTask(t, s, "ytc-mapped")
	if _, err := s.UpsertYouTrackMirror(ctx, YouTrackMirrorInput{TaskID: mapped, IssueID: "9-1", DisplayRef: "YTC-1", Tenant: ytcTenant, Role: "mirror"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.YTCanaryCandidate(ctx, mapped)
	if err != nil || c.TaskID != mapped || c.Project != "ytc-mapped" || c.ShortName != "YTC" || c.ProjectID != "0-9" || c.MirrorIssueID != "9-1" || c.MirrorTenant != ytcTenant || c.MappedIssueID != "" {
		t.Fatalf("candidate = %+v %v", c, err)
	}
	// No OTHER fallback: an unmapped project has no short name.
	loose := ytcTask(t, s, "ytc-unmapped")
	c, err = s.YTCanaryCandidate(ctx, loose)
	if err != nil || c.Project != "ytc-unmapped" || c.ShortName != "" {
		t.Fatalf("unmapped candidate = %+v %v", c, err)
	}
	if _, err := s.YTCanaryCandidate(ctx, 987654321); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

const ytCommentCanaryMigrationID = "20261109-yt-comment-canary"

// A database already current on every other ID gains the new tables when the
// binary that registers this one opens it, and records the ID exactly once.
func TestCanaryMigrationUpgradesAlreadyCurrentDatabase(t *testing.T) {
	_, url := testdb.Schema(t, "ytc_mig")
	var without []migration
	for _, m := range migrationRegistry {
		if m.ID != ytCommentCanaryMigrationID {
			without = append(without, m)
		}
	}
	old := openRegistry(t, url, without)
	tables := func(s *Store) int {
		return ytcCount(t, s, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('yt_comment_canary','yt_comment_write_links')`)
	}
	if n := tables(old); n != 0 {
		t.Fatalf("tables exist before the migration: %d", n)
	}
	old.Close()
	for i := 0; i < 2; i++ {
		s := openRegistry(t, url, migrationRegistry)
		if n := tables(s); n != 2 {
			t.Fatalf("open %d: %d of 2 tables", i, n)
		}
		if n := ytcCount(t, s, `SELECT count(*) FROM schema_migrations WHERE id=$1`, ytCommentCanaryMigrationID); n != 1 {
			t.Fatalf("open %d: id recorded %d times", i, n)
		}
		if v := ytcCount(t, s, `SELECT max(version) FROM schema_version`); v != 53 {
			t.Fatalf("integer schema version = %d", v)
		}
		s.Close()
	}
}
